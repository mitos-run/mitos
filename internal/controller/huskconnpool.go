package controller

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"mitos.run/mitos/internal/husk"
)

// huskConnIdle is how long the controller keeps an authenticated husk control
// connection in the pool before it re-dials rather than reuse a possibly stale
// one. It is deliberately SHORTER than the husk server's inter-request idle
// timeout (husk.controlIdleTimeout, 90s) so the controller retires a connection
// before the server would idle-close it, so it never writes a request onto a
// socket the server already closed.
const huskConnIdle = 30 * time.Second

// huskConn is a single authenticated mTLS control connection to one husk pod,
// paired with the persistent bufio.Reader its results are decoded through. It is
// NOT safe for concurrent use; the pool serializes access with a per-address
// mutex so at most one request/response is ever in flight on it, which is what
// keeps two concurrent forks from interleaving frames on one stream.
type huskConn struct {
	conn net.Conn
	br   *bufio.Reader
}

// huskConnEntry is the pool's per-address slot: a mutex that serializes all RPCs
// to that husk (one in-flight frame at a time), the live connection (nil until
// first dialed and after any drop), and the time it was last used successfully
// for idle retirement.
type huskConnEntry struct {
	mu       sync.Mutex
	conn     *huskConn
	lastUsed time.Time
}

// close tears down the entry's connection and marks the slot empty so the next
// use re-dials. The caller must hold e.mu.
func (e *huskConnEntry) close() {
	if e.conn != nil {
		_ = e.conn.conn.Close()
		e.conn = nil
	}
}

// HuskConnPool reuses one authenticated mTLS control connection per husk pod
// across control-plane RPCs, so a co-located fork (fork-snapshot then spawn-vm,
// both to the SAME source pod) pays ONE TCP+TLS handshake instead of one per
// RPC. The default one-shot huskclient.go functions (dial, one request, close)
// remain the byte-for-byte fallback; a pool is wired into the reconciler only
// behind the --husk-conn-reuse flag so it can be canaried and rolled back.
//
// SECURITY: reuse changes only WHEN the handshake happens, never WHETHER it is
// verified. Every dial goes through tlsConf, which pins the husk server identity
// and presents the controller client leaf the husk server authorizes
// (AuthorizeControllerIdentity), so a reused connection is the SAME authenticated
// peer the husk verified at dial time; the mTLS session does not change under it.
// A nil tlsConf is refused by each seam method before any dial, so the control
// channel is never driven unauthenticated. One request/response is in flight per
// connection (the per-address mutex), so concurrent forks never interleave
// frames. Secret VALUES are never logged: errors carry only the operation, the
// address, and the transport error, never the request payload.
//
// The pool holds at most one connection per distinct husk address. A dead or
// restarted husk's connection is dropped on the next use (re-dial on error) or
// when it exceeds huskConnIdle; a slot whose pod is gone keeps only a tiny
// nil-connection struct, reclaimed if the process restarts.
type HuskConnPool struct {
	entries sync.Map // addr string -> *huskConnEntry
}

// NewHuskConnPool returns an empty pool ready to serve the husk control seams.
func NewHuskConnPool() *HuskConnPool {
	return &HuskConnPool{}
}

// dialHuskConn opens a fresh authenticated mTLS control connection to addr. The
// returned huskConn carries a persistent reader so a reused stream decodes
// cleanly across results.
func dialHuskConn(ctx context.Context, addr string, tlsConf *tls.Config) (*huskConn, error) {
	dialer := &tls.Dialer{Config: tlsConf}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial husk control %s: %w", addr, err)
	}
	return &huskConn{conn: conn, br: bufio.NewReader(conn)}, nil
}

// do runs one control exchange (fn) against the pooled connection to addr under
// the per-address lock, so concurrent RPCs to the SAME husk never interleave
// frames on one stream. It dials on first use, reuses the authenticated
// connection afterwards, and transparently RE-DIALS once if a REUSED connection
// fails (a husk restart or a server idle-close between uses): the request still
// succeeds on the fresh connection. A FRESHLY dialed connection that fails
// returns the error, because that is a real transport or protocol failure, not
// staleness. Any failure drops the connection so a desynced stream is never
// reused.
func (p *HuskConnPool) do(ctx context.Context, addr string, tlsConf *tls.Config, fn func(*huskConn) error) error {
	ei, _ := p.entries.LoadOrStore(addr, &huskConnEntry{})
	e := ei.(*huskConnEntry)
	e.mu.Lock()
	defer e.mu.Unlock()

	// Retire an idle connection before use so we never write onto a socket the
	// husk server may already have idle-closed on its own timeout.
	if e.conn != nil && time.Since(e.lastUsed) > huskConnIdle {
		e.close()
	}

	reused := e.conn != nil
	if !reused {
		c, err := dialHuskConn(ctx, addr, tlsConf)
		if err != nil {
			return err
		}
		e.conn = c
	}

	if err := p.run(ctx, e.conn, fn); err == nil {
		e.lastUsed = time.Now()
		return nil
	} else if !reused {
		// A fresh connection failed: a real transport/protocol error. Drop it and
		// report; do not mask it behind a retry.
		e.close()
		return err
	} else {
		// A reused connection failed: the husk may have restarted or idle-closed
		// between uses. Drop it, re-dial ONCE, and retry so transient staleness
		// does not fail the RPC.
		e.close()
		c, derr := dialHuskConn(ctx, addr, tlsConf)
		if derr != nil {
			return fmt.Errorf("re-dial husk control %s after reuse error (%v): %w", addr, err, derr)
		}
		e.conn = c
		if rerr := p.run(ctx, e.conn, fn); rerr != nil {
			e.close()
			return rerr
		}
		e.lastUsed = time.Now()
		return nil
	}
}

// run bounds the exchange by the caller's context deadline (so a wedged husk
// cannot block the reconcile) and invokes fn on the connection.
func (p *HuskConnPool) run(ctx context.Context, hc *huskConn, fn func(*huskConn) error) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = hc.conn.SetDeadline(deadline)
	}
	return fn(hc)
}

// ActivateHuskPod runs the Activate exchange over the pooled connection. Its
// signature matches the one-shot ActivateHuskPod so it satisfies the reconciler
// huskActivator seam. req carries tenant SECRETS, so a nil tlsConf is refused.
func (p *HuskConnPool) ActivateHuskPod(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error) {
	if tlsConf == nil {
		return husk.ActivateResult{}, fmt.Errorf("activate husk pod %s: refusing to send activation secrets over an unauthenticated channel", addr)
	}
	var res husk.ActivateResult
	err := p.do(ctx, addr, tlsConf, func(hc *huskConn) error {
		if err := husk.WriteControlOp(hc.conn, husk.OpActivate); err != nil {
			return fmt.Errorf("send activate op to %s: %w", addr, err)
		}
		if err := husk.WriteRequest(hc.conn, req); err != nil {
			return fmt.Errorf("send activate request to %s: %w", addr, err)
		}
		r, err := husk.ReadResultReader(hc.br)
		if err != nil {
			return fmt.Errorf("read activate result from %s: %w", addr, err)
		}
		res = r
		return nil
	})
	return res, err
}

// ForkSnapshotOnHusk runs the fork-snapshot exchange over the pooled connection.
// Its signature matches the one-shot ForkSnapshotOnHusk (the huskForkSnapshotter
// seam). req carries no secrets; a nil tlsConf is still refused because the same
// control surface delivers secrets on activate.
func (p *HuskConnPool) ForkSnapshotOnHusk(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
	if tlsConf == nil {
		return husk.ForkSnapshotResult{}, fmt.Errorf("fork-snapshot on husk %s: refusing to drive the control channel unauthenticated", addr)
	}
	var res husk.ForkSnapshotResult
	err := p.do(ctx, addr, tlsConf, func(hc *huskConn) error {
		if err := husk.WriteControlOp(hc.conn, husk.OpForkSnapshot); err != nil {
			return fmt.Errorf("send fork-snapshot op to %s: %w", addr, err)
		}
		if err := husk.WriteForkSnapshotRequest(hc.conn, req); err != nil {
			return fmt.Errorf("send fork-snapshot request to %s: %w", addr, err)
		}
		r, err := husk.ReadForkSnapshotResultReader(hc.br)
		if err != nil {
			return fmt.Errorf("read fork-snapshot result from %s: %w", addr, err)
		}
		res = r
		return nil
	})
	return res, err
}

// SpawnVMOnHusk runs the spawn-vm exchange over the pooled connection. Its
// signature matches the one-shot SpawnVMOnHusk (the huskVMSpawner seam).
// req.Activate carries tenant SECRETS, so a nil tlsConf is refused.
func (p *HuskConnPool) SpawnVMOnHusk(ctx context.Context, addr string, tlsConf *tls.Config, req husk.SpawnVMRequest) (husk.SpawnVMResult, error) {
	if tlsConf == nil {
		return husk.SpawnVMResult{}, fmt.Errorf("spawn-vm on husk %s: refusing to drive the control channel unauthenticated", addr)
	}
	var res husk.SpawnVMResult
	err := p.do(ctx, addr, tlsConf, func(hc *huskConn) error {
		if err := husk.WriteControlOp(hc.conn, husk.OpSpawnVM); err != nil {
			return fmt.Errorf("send spawn-vm op to %s: %w", addr, err)
		}
		if err := husk.WriteSpawnVMRequest(hc.conn, req); err != nil {
			return fmt.Errorf("send spawn-vm request to %s: %w", addr, err)
		}
		r, err := husk.ReadSpawnVMResultReader(hc.br)
		if err != nil {
			return fmt.Errorf("read spawn-vm result from %s: %w", addr, err)
		}
		res = r
		return nil
	})
	return res, err
}

// RemoveForkSnapshotOnHusk runs the remove-fork-snapshot exchange over the pooled
// connection. Its signature matches the one-shot RemoveForkSnapshotOnHusk (the
// huskForkSnapshotRemover seam). A nil tlsConf is refused.
func (p *HuskConnPool) RemoveForkSnapshotOnHusk(ctx context.Context, addr string, tlsConf *tls.Config, req husk.RemoveForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
	if tlsConf == nil {
		return husk.ForkSnapshotResult{}, fmt.Errorf("remove fork-snapshot on husk %s: refusing to drive the control channel unauthenticated", addr)
	}
	var res husk.ForkSnapshotResult
	err := p.do(ctx, addr, tlsConf, func(hc *huskConn) error {
		if err := husk.WriteControlOp(hc.conn, husk.OpRemoveForkSnapshot); err != nil {
			return fmt.Errorf("send remove-fork-snapshot op to %s: %w", addr, err)
		}
		if err := husk.WriteRemoveForkSnapshotRequest(hc.conn, req); err != nil {
			return fmt.Errorf("send remove-fork-snapshot request to %s: %w", addr, err)
		}
		r, err := husk.ReadForkSnapshotResultReader(hc.br)
		if err != nil {
			return fmt.Errorf("read remove-fork-snapshot result from %s: %w", addr, err)
		}
		res = r
		return nil
	})
	return res, err
}
