// Package guestgrpc provides a reusable host-side gRPC client for the guest
// agent's gRPC services (sandbox.v1.Sandbox and sandbox.internal.v1.Control).
//
// Transport choice: grpc-go over a raw net.Conn, identical to the pattern
// used in internal/vsock/grpcconn.go. For vsock-based (Firecracker) transport
// use Dial; for unix-socket transport (standalone sandbox-server and tests)
// use DialUnix. WaitReady and WaitReadyUnix add retry-with-backoff so callers
// can tolerate the brief window after a VM restore before the guest agent is
// ready to accept connections.
//
// Security note: transport credentials are insecure because the vsock channel
// runs inside the Firecracker VM trust boundary and is not reachable from the
// host network or tenant code in other sandboxes.
package guestgrpc

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"

	"mitos.run/mitos/internal/vsock"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// Client holds typed gRPC clients for the guest agent's two services.
// Conn is the underlying *grpc.ClientConn; Close it when done.
type Client struct {
	Conn    *grpc.ClientConn
	Sandbox sandboxv1.SandboxClient
	Control internalv1.ControlClient
}

// Close shuts down the underlying gRPC connection. Safe to call more than once;
// subsequent calls after the first may return a non-nil error which callers may
// ignore.
func (c *Client) Close() error {
	return c.Conn.Close()
}

// buildClient wraps a *grpc.ClientConn into a Client by constructing the two
// typed service stubs.
func buildClient(cc *grpc.ClientConn) *Client {
	return &Client{
		Conn:    cc,
		Sandbox: sandboxv1.NewSandboxClient(cc),
		Control: internalv1.NewControlClient(cc),
	}
}

// Dial dials the guest agent gRPC service over the Firecracker vsock UDS at
// vsockPath, performing the "CONNECT <port>\n" / "OK\n" preamble before
// handing the raw conn to grpc-go. vsock.AgentGRPCPort is used as the guest
// vsock port.
//
// The returned Client owns the connection; call Close when done.
func Dial(vsockPath string) (*Client, error) {
	conn, err := vsock.DialGRPCConn(vsockPath, vsock.AgentGRPCPort)
	if err != nil {
		return nil, fmt.Errorf("guestgrpc dial vsock %s: %w", vsockPath, err)
	}
	capturedConn := conn
	cc, err := vsock.DialGRPCOverConn(func() (net.Conn, error) {
		c := capturedConn
		capturedConn = nil
		if c == nil {
			return nil, fmt.Errorf("guestgrpc vsock dialer: reconnect not supported")
		}
		return c, nil
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("guestgrpc wrap grpc conn (vsock %s): %w", vsockPath, err)
	}
	return buildClient(cc), nil
}

// DialUnix dials the guest agent gRPC service over a plain unix socket at
// sockPath (no Firecracker CONNECT preamble). This is the path used by the
// standalone sandbox-server's local-testing fallback and by tests that run an
// in-process gRPC server on a temp unix socket.
//
// The returned Client owns the connection; call Close when done.
func DialUnix(sockPath string) (*Client, error) {
	conn, err := vsock.DialGRPCConnUnix(sockPath)
	if err != nil {
		return nil, fmt.Errorf("guestgrpc dial unix %s: %w", sockPath, err)
	}
	capturedConn := conn
	cc, err := vsock.DialGRPCOverConn(func() (net.Conn, error) {
		c := capturedConn
		capturedConn = nil
		if c == nil {
			return nil, fmt.Errorf("guestgrpc unix dialer: reconnect not supported")
		}
		return c, nil
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("guestgrpc wrap grpc conn (unix %s): %w", sockPath, err)
	}
	return buildClient(cc), nil
}

// retryInterval is the fixed sleep between connection attempts. Mirrors the
// 20 ms interval used by cmd/bench connectGRPCWithRetry and
// internal/husk productionGuestReady.
const retryInterval = 20 * time.Millisecond

// WaitReady dials the guest agent gRPC service over vsock at vsockPath,
// retrying at fixed intervals until the Control.Ping RPC succeeds, ctx is
// cancelled, or timeout elapses. It returns a ready Client whose HTTP/2
// handshake has been completed.
//
// The retry loop mirrors the pattern in cmd/bench connectGRPCWithRetry: each
// attempt opens a fresh vsock conn, wraps it in a *grpc.ClientConn, and calls
// Ping to verify liveness. On failure the conn is closed and the attempt is
// retried after retryInterval.
func WaitReady(ctx context.Context, vsockPath string, timeout time.Duration) (*Client, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("guestgrpc wait-ready vsock %s: %w", vsockPath, ctx.Err())
		}
		if time.Now().After(deadline) {
			break
		}

		client, err := Dial(vsockPath)
		if err != nil {
			lastErr = err
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("guestgrpc wait-ready vsock %s: %w", vsockPath, ctx.Err())
			case <-time.After(retryInterval):
			}
			continue
		}

		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, pingErr := client.Control.Ping(pingCtx, &internalv1.PingRequest{})
		cancel()
		if pingErr == nil {
			return client, nil
		}
		client.Close() //nolint:errcheck // best-effort; conn is being replaced
		lastErr = pingErr
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("guestgrpc wait-ready vsock %s: %w", vsockPath, ctx.Err())
		case <-time.After(retryInterval):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return nil, fmt.Errorf("guestgrpc wait-ready vsock %s after %s: %w", vsockPath, timeout, lastErr)
}

// WaitReadyUnix is WaitReady over a plain unix socket (no Firecracker preamble).
// It is used by the standalone sandbox-server and tests.
func WaitReadyUnix(ctx context.Context, sockPath string, timeout time.Duration) (*Client, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("guestgrpc wait-ready unix %s: %w", sockPath, ctx.Err())
		}
		if time.Now().After(deadline) {
			break
		}

		client, err := DialUnix(sockPath)
		if err != nil {
			lastErr = err
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("guestgrpc wait-ready unix %s: %w", sockPath, ctx.Err())
			case <-time.After(retryInterval):
			}
			continue
		}

		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, pingErr := client.Control.Ping(pingCtx, &internalv1.PingRequest{})
		cancel()
		if pingErr == nil {
			return client, nil
		}
		client.Close() //nolint:errcheck // best-effort; conn is being replaced
		lastErr = pingErr
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("guestgrpc wait-ready unix %s: %w", sockPath, ctx.Err())
		case <-time.After(retryInterval):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return nil, fmt.Errorf("guestgrpc wait-ready unix %s after %s: %w", sockPath, timeout, lastErr)
}
