package husk

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"mitos.run/mitos/internal/pki"
)

// controlIdleTimeout bounds how long an authenticated control connection may sit
// idle BETWEEN requests before the server reaps it. It exists only so a reused
// connection (the controller's connection pool keeps one authenticated stream
// open across RPCs) does not leak a wedged or forgotten socket; a one-shot
// client closes right after its single request and never reaches the timeout.
// It bounds the inter-request WAIT only, not the op itself: once a request is
// read, the deadline is cleared so a slow activate/fork is bounded by the
// caller's context deadline, not this idle window. The controller retires its
// pooled connection well before this (huskConnIdle) so it never writes onto a
// socket the server already idle-closed.
const controlIdleTimeout = 90 * time.Second

// controlOpTimeout bounds a single in-flight control op (request-body read +
// operation + result write) so a peer that half-sends a request or stops reading
// the reply cannot pin the serving goroutine forever. It is a finite socket
// backstop (unlike clearing the deadline); generous enough for the slowest
// activate/fork/workspace op the client itself would wait on.
const controlOpTimeout = 120 * time.Second

// AuthorizeControllerIdentity is the authorize hook for ServeTLS that mirrors
// forkd's RequireControllerIdentity interceptor: a control connection is
// accepted only when its VERIFIED mTLS peer presents the controller leaf's DNS
// SAN. The identity is read from VerifiedChains (set by the TLS handshake under
// RequireAndVerifyClientCert), never from the certificates a peer merely
// presented, so an unverified or wrong-CA peer can never satisfy it.
//
// An activate request delivers tenant SECRETS to a VM; this is the gate that
// ensures only the controller may drive that path. It returns an error (no
// secret material is in scope here) when the peer is missing an identity or is
// not the controller.
func AuthorizeControllerIdentity(state *tls.ConnectionState) error {
	if state == nil {
		return errors.New("husk control: no TLS state on connection")
	}
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		// Unreachable while the listener enforces RequireAndVerifyClientCert; kept
		// as defense in depth so a misconfigured TLS config still fails closed.
		return errors.New("husk control: client certificate required")
	}
	leaf := state.VerifiedChains[0][0]
	if len(leaf.DNSNames) == 0 || leaf.DNSNames[0] != pki.ControllerName {
		name := "<none>"
		if len(leaf.DNSNames) > 0 {
			name = leaf.DNSNames[0]
		}
		return fmt.Errorf("husk control: peer %q may not activate this husk", name)
	}
	return nil
}

// ServeTLS serves the line-delimited JSON control protocol (control.go's
// ReadRequest / WriteResult) over an mTLS net.Listener, dispatching each
// accepted connection to serveControlConn, which authorizes the verified peer
// once and then serves one or more requests (activate/fork/spawn/workspace) on
// that connection until the peer closes it or it sits idle.
//
// SECURITY: this is the network control channel that activates a dormant VM
// with tenant secrets. tlsConf MUST require and verify client certificates
// (build it with pki.ServerTLSConfig). authorize MUST verify the peer identity
// (use AuthorizeControllerIdentity so only the controller may activate); a nil
// authorize is rejected because an unauthenticated activate channel that
// delivers secrets is unacceptable. A connection whose handshake fails, or
// whose verified peer is not authorized, is closed without ever reading an
// ActivateRequest, so secrets are never accepted from an unauthenticated peer.
//
// Like Stub.Serve, a husk pod is LONG-LIVED: a successful activate does not end
// ServeTLS. It keeps holding the active VM and rejecting further activate
// attempts (via Activate's state check) until ctx is cancelled or the listener
// closes. It never tears the VM down; the caller (cmd/husk-stub) calls
// stub.Close on shutdown.
//
// Secret and entropy VALUES are never logged here: per-connection failures are
// reported to stderr by operation only, with the transport error, never the
// request payload.
func ServeTLS(ctx context.Context, ln net.Listener, stub *Stub, tlsConf *tls.Config, authorize func(*tls.ConnectionState) error) error {
	if tlsConf == nil {
		return errors.New("husk control: refusing to serve network control without TLS")
	}
	if authorize == nil {
		return errors.New("husk control: refusing to serve network control without an authorize hook")
	}

	tlsLn := tls.NewListener(ln, tlsConf)

	// Unblock Accept when the context is cancelled.
	go func() {
		<-ctx.Done()
		_ = tlsLn.Close()
	}()

	for {
		conn, err := tlsLn.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("husk control: accept connection: %w", err)
		}
		serveControlConn(ctx, conn, stub, authorize)
	}
}

// serveControlConn completes the mTLS handshake ONCE, authorizes the verified
// peer ONCE, then serves requests on the connection in a loop: read op, dispatch,
// write result, repeat until the peer closes it (EOF), it sits idle past
// controlIdleTimeout, or a framing/transport error desyncs the stream. Any
// handshake or authorization failure closes the connection without reading a
// request, so a secret-bearing request is never accepted from an unauthenticated
// or unauthorized peer.
//
// The loop makes the connection REUSABLE for the controller's connection pool
// (one authenticated stream carries many RPCs, saving a TCP+TLS handshake per
// op) while staying byte-for-byte backward compatible with a one-shot client: a
// client that writes a single request and closes drives exactly one iteration,
// then the next ReadControlOp sees EOF and the connection closes, identical in
// effect to the previous one-request-per-connection behavior. The identity is
// verified once and remains the verified peer for the connection's whole life;
// there is no per-request re-auth because the mTLS session does not change.
//
// Errors are logged by operation only; the request payload (env/secrets/entropy)
// is never logged.
func serveControlConn(ctx context.Context, conn net.Conn, stub *Stub, authorize func(*tls.ConnectionState) error) {
	defer conn.Close()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		fmt.Fprintln(os.Stderr, "husk control: non-TLS connection rejected")
		return
	}
	// Force the handshake now so VerifiedChains is populated before authorize.
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "husk control: TLS handshake: %v\n", err)
		return
	}
	state := tlsConn.ConnectionState()
	if err := authorize(&state); err != nil {
		// Authorization failed: do NOT read the request, so no secret material is
		// accepted from this peer. The error names only the identity.
		fmt.Fprintf(os.Stderr, "husk control: %v\n", err)
		return
	}

	// One persistent reader for the connection's whole life so a reused stream
	// decodes cleanly across requests (bufio keeps any buffered surplus between
	// reads; a fresh reader per request would drop it).
	br := bufio.NewReader(conn)
	for {
		// Bound only the WAIT for the next request so an idle or wedged reused
		// connection is reaped. A one-shot client's close surfaces here as EOF.
		_ = conn.SetReadDeadline(time.Now().Add(controlIdleTimeout))
		op, err := ReadControlOp(br)
		if err != nil {
			// EOF (peer closed / one-shot), idle timeout, or a cancelled server:
			// close quietly. A genuine framing/decode error is logged (never the
			// payload) so a desync is visible.
			var nerr net.Error
			if errors.Is(err, io.EOF) || (errors.As(err, &nerr) && nerr.Timeout()) || ctx.Err() != nil {
				return
			}
			fmt.Fprintf(os.Stderr, "husk control: read control op: %v\n", err)
			return
		}
		// The op arrived: re-arm a finite per-op deadline (both read + write) so a
		// slow activate/fork is allowed but a peer that half-sends the request body
		// or stops reading the reply cannot pin this goroutine forever. ctx bounds
		// the operation logic; this bounds the raw socket I/O ctx cannot unblock.
		_ = conn.SetDeadline(time.Now().Add(controlOpTimeout))
		if !dispatchControlOp(ctx, conn, br, op, stub) {
			// A request-read or result-write error left the stream in an unknown
			// framing state; close instead of reading the next op off a desynced
			// stream.
			return
		}
	}
}

// dispatchControlOp reads the request for op off the shared reader, runs the
// stub operation, and writes the result to conn. It returns true when the
// request-response frame completed cleanly (the connection may serve the next
// op) and false when a request read or result write failed, which desyncs the
// stream and requires closing the connection. Errors are logged by operation
// only; the request payload (env/secrets/entropy) is never logged.
func dispatchControlOp(ctx context.Context, conn net.Conn, br *bufio.Reader, op string, stub *Stub) bool {
	switch op {
	case OpActivate:
		req, rerr := readActivateRequest(br)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "husk control: read activate request: %v\n", rerr)
			return false
		}
		res, _ := stub.Activate(ctx, req)
		if werr := WriteResult(conn, res); werr != nil {
			fmt.Fprintf(os.Stderr, "husk control: write activate result: %v\n", werr)
			return false
		}
		return true
	case OpForkSnapshot:
		req, rerr := readForkSnapshotRequest(br)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "husk control: read fork-snapshot request: %v\n", rerr)
			return false
		}
		res, _ := stub.ForkSnapshot(ctx, req)
		if werr := WriteForkSnapshotResult(conn, res); werr != nil {
			fmt.Fprintf(os.Stderr, "husk control: write fork-snapshot result: %v\n", werr)
			return false
		}
		return true
	case OpRemoveForkSnapshot:
		req, rerr := readRemoveForkSnapshotRequest(br)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "husk control: read remove-fork-snapshot request: %v\n", rerr)
			return false
		}
		rmErr := stub.RemoveForkSnapshot(ForkSnapshotRequest{ForkID: req.ForkID, SnapshotDir: req.SnapshotDir})
		out := ForkSnapshotResult{OK: rmErr == nil, SnapshotDir: req.SnapshotDir}
		if rmErr != nil {
			out.Error = rmErr.Error()
		}
		if werr := WriteForkSnapshotResult(conn, out); werr != nil {
			fmt.Fprintf(os.Stderr, "husk control: write remove-fork-snapshot result: %v\n", werr)
			return false
		}
		return true
	case OpDehydrateWorkspace:
		req, rerr := readDehydrateWorkspaceRequest(br)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "husk control: read dehydrate-workspace request: %v\n", rerr)
			return false
		}
		res, _ := stub.DehydrateWorkspace(ctx, req)
		if werr := WriteDehydrateWorkspaceResult(conn, res); werr != nil {
			fmt.Fprintf(os.Stderr, "husk control: write dehydrate-workspace result: %v\n", werr)
			return false
		}
		return true
	case OpHydrateWorkspace:
		req, rerr := readHydrateWorkspaceRequest(br)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "husk control: read hydrate-workspace request: %v\n", rerr)
			return false
		}
		res, _ := stub.HydrateWorkspace(ctx, req)
		if werr := WriteHydrateWorkspaceResult(conn, res); werr != nil {
			fmt.Fprintf(os.Stderr, "husk control: write hydrate-workspace result: %v\n", werr)
			return false
		}
		return true
	case OpSpawnVM:
		req, rerr := readSpawnVMRequest(br)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "husk control: read spawn-vm request: %v\n", rerr)
			return false
		}
		// SpawnVM fails closed on a non-multi-vm stub and validates req.VMID with
		// checkVMID before deriving any path from it, so a single-VM pod never
		// spawns a second VM and an unsafe vmID is refused. The request payload
		// (env/secrets/token) is never logged.
		res := stub.SpawnVM(ctx, req)
		if werr := WriteSpawnVMResult(conn, res); werr != nil {
			fmt.Fprintf(os.Stderr, "husk control: write spawn-vm result: %v\n", werr)
			return false
		}
		return true
	default:
		// An unknown op on a shared stream cannot be safely skipped (its request
		// framing is unknown), so the connection is closed.
		fmt.Fprintf(os.Stderr, "husk control: unknown control op %q\n", op)
		return false
	}
}
