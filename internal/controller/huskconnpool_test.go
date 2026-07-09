package controller

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mitos.run/mitos/internal/husk"
	"mitos.run/mitos/internal/pki"
)

// loopHuskServer is an mTLS husk control server that speaks the real husk wire
// codec and serves MULTIPLE requests per accepted connection (mirroring
// husk.ServeTLS's keep-alive loop), so the controller HuskConnPool's reuse is
// exercised end to end over the genuine protocol. accepts counts TCP
// connections (a reused connection counts once), so a pool that reuses shows
// accepts=1 for many RPCs. closeAfter, when > 0, closes a connection after
// serving that many requests to simulate a husk restart / server idle-close and
// force the pool to re-dial.
type loopHuskServer struct {
	addr       string
	stop       func()
	closeAfter int

	accepts   int64
	forkReqs  int64
	spawnReqs int64
	actReqs   int64

	mu      sync.Mutex
	secrets []string // captured to prove secret-bearing reuse works; never logged
}

func newLoopHuskServer(t *testing.T, p *huskClientPKI, closeAfter int) *loopHuskServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsLn := tls.NewListener(ln, p.serverConf)
	s := &loopHuskServer{addr: ln.Addr().String(), closeAfter: closeAfter}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, aerr := tlsLn.Accept()
			if aerr != nil {
				return
			}
			atomic.AddInt64(&s.accepts, 1)
			go s.serve(conn)
		}
	}()
	s.stop = func() {
		_ = tlsLn.Close()
		<-done
	}
	return s
}

func (s *loopHuskServer) serve(conn net.Conn) {
	defer conn.Close()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return
	}
	if err := tlsConn.HandshakeContext(context.Background()); err != nil {
		return
	}
	state := tlsConn.ConnectionState()
	// The reused connection is authorized ONCE with the same controller-identity
	// gate the real server uses; an unauthorized peer never reaches a request.
	if err := husk.AuthorizeControllerIdentity(&state); err != nil {
		return
	}
	br := bufio.NewReader(conn)
	served := 0
	for {
		op, err := husk.ReadControlOp(br)
		if err != nil {
			return
		}
		switch op {
		case husk.OpForkSnapshot:
			req, rerr := husk.ReadForkSnapshotRequestReader(br)
			if rerr != nil {
				return
			}
			atomic.AddInt64(&s.forkReqs, 1)
			// Echo the request's SnapshotDir so a caller can prove the response is
			// ITS OWN (no frame interleaving under concurrency).
			_ = husk.WriteForkSnapshotResult(conn, husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir})
		case husk.OpSpawnVM:
			req, rerr := husk.ReadSpawnVMRequestReader(br)
			if rerr != nil {
				return
			}
			atomic.AddInt64(&s.spawnReqs, 1)
			s.mu.Lock()
			s.secrets = append(s.secrets, req.Activate.Secrets["API_KEY"])
			s.mu.Unlock()
			_ = husk.WriteSpawnVMResult(conn, husk.SpawnVMResult{OK: true, VMID: req.VMID, VsockPath: "/run/husk/" + req.VMID + "/vsock.sock"})
		case husk.OpActivate:
			req, rerr := husk.ReadActivateRequestReader(br)
			if rerr != nil {
				return
			}
			atomic.AddInt64(&s.actReqs, 1)
			s.mu.Lock()
			s.secrets = append(s.secrets, req.Secrets["API_KEY"])
			s.mu.Unlock()
			_ = husk.WriteResult(conn, husk.ActivateResult{OK: true, VsockPath: "/run/husk/vsock.sock"})
		case husk.OpRemoveForkSnapshot:
			req, rerr := husk.ReadForkSnapshotRequestReader(br) // remove shares the fork-snapshot request shape
			if rerr != nil {
				return
			}
			_ = husk.WriteForkSnapshotResult(conn, husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir})
		default:
			return
		}
		served++
		if s.closeAfter > 0 && served >= s.closeAfter {
			return
		}
	}
}

// TestHuskConnPoolReusesConnection proves the pool reuses ONE authenticated mTLS
// connection across two RPCs to the SAME husk address: a fork-snapshot then a
// spawn-vm (the co-located fork's two RPCs, both to the source pod) are served
// on a single accepted connection, so the second RPC pays no TCP+TLS handshake.
func TestHuskConnPoolReusesConnection(t *testing.T) {
	p := newHuskClientPKI(t)
	s := newLoopHuskServer(t, p, 0)
	defer s.stop()

	pool := NewHuskConnPool()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fres, err := pool.ForkSnapshotOnHusk(ctx, s.addr, p.clientConf, husk.ForkSnapshotRequest{ForkID: "f1", SnapshotDir: "/snap/f1"})
	if err != nil || !fres.OK {
		t.Fatalf("fork-snapshot over pool: res=%+v err=%v", fres, err)
	}
	sres, err := pool.SpawnVMOnHusk(ctx, s.addr, p.clientConf, husk.SpawnVMRequest{
		VMID:     "vm-1",
		Activate: husk.ActivateRequest{SnapshotDir: "/snap/f1", Secrets: map[string]string{"API_KEY": "s3cr3t"}},
	})
	if err != nil || !sres.OK {
		t.Fatalf("spawn-vm over pool: res=%+v err=%v", sres, err)
	}

	if got := atomic.LoadInt64(&s.accepts); got != 1 {
		t.Fatalf("pool must reuse ONE connection for both RPCs, got %d accepts", got)
	}
	if f := atomic.LoadInt64(&s.forkReqs); f != 1 {
		t.Fatalf("server saw %d fork-snapshot requests, want 1", f)
	}
	if sp := atomic.LoadInt64(&s.spawnReqs); sp != 1 {
		t.Fatalf("server saw %d spawn-vm requests, want 1", sp)
	}
	// The secret rode the REUSED connection intact.
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.secrets) != 1 || s.secrets[0] != "s3cr3t" {
		t.Fatalf("secret did not survive connection reuse: %v", s.secrets)
	}
}

// TestHuskConnPoolRedialsAfterServerClose proves the pool re-dials when a REUSED
// connection is dead: the server closes each connection after one request
// (simulating a husk restart / idle-close), so the pool's cached connection is
// stale on the second RPC. The pool must transparently re-dial and the RPC must
// still succeed. accepts=2 proves the re-dial happened.
func TestHuskConnPoolRedialsAfterServerClose(t *testing.T) {
	p := newHuskClientPKI(t)
	s := newLoopHuskServer(t, p, 1) // close after every single request
	defer s.stop()

	pool := NewHuskConnPool()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if fres, err := pool.ForkSnapshotOnHusk(ctx, s.addr, p.clientConf, husk.ForkSnapshotRequest{ForkID: "f1", SnapshotDir: "/snap/f1"}); err != nil || !fres.OK {
		t.Fatalf("first fork-snapshot: res=%+v err=%v", fres, err)
	}
	// The server closed that connection; the pool still holds it. This second RPC
	// must detect the dead reused connection and re-dial.
	if fres, err := pool.ForkSnapshotOnHusk(ctx, s.addr, p.clientConf, husk.ForkSnapshotRequest{ForkID: "f2", SnapshotDir: "/snap/f2"}); err != nil || !fres.OK {
		t.Fatalf("second fork-snapshot (after server close) must re-dial and succeed: res=%+v err=%v", fres, err)
	}
	if got := atomic.LoadInt64(&s.accepts); got != 2 {
		t.Fatalf("pool must re-dial after a dead reused connection, got %d accepts (want 2)", got)
	}
}

// TestHuskConnPoolConcurrent proves concurrent RPCs to the SAME husk never
// interleave frames on the shared connection: N goroutines each run a
// fork-snapshot carrying a UNIQUE SnapshotDir, and each must get ITS OWN dir
// echoed back. A frame interleave would hand a goroutine another's response.
// Run with -race to catch data races on the pooled connection.
func TestHuskConnPoolConcurrent(t *testing.T) {
	p := newHuskClientPKI(t)
	s := newLoopHuskServer(t, p, 0)
	defer s.stop()

	pool := NewHuskConnPool()
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			dir := fmt.Sprintf("/snap/f%d", i)
			res, err := pool.ForkSnapshotOnHusk(ctx, s.addr, p.clientConf, husk.ForkSnapshotRequest{ForkID: fmt.Sprintf("f%d", i), SnapshotDir: dir})
			if err != nil {
				errs <- fmt.Errorf("rpc %d: %w", i, err)
				return
			}
			if !res.OK || res.SnapshotDir != dir {
				errs <- fmt.Errorf("rpc %d got wrong response (interleave?): %+v want dir %q", i, res, dir)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&s.forkReqs); got != n {
		t.Fatalf("server saw %d fork-snapshot requests, want %d", got, n)
	}
}

// TestHuskConnPoolEnforcesMTLSIdentity proves every pooled dial is mTLS
// authenticated: a client leaf from a DIFFERENT CA (so the husk server's
// ClientCAs reject it) fails the handshake, and no request is served. Reuse
// never weakens the identity gate; it only changes WHEN the verified handshake
// happens.
func TestHuskConnPoolEnforcesMTLSIdentity(t *testing.T) {
	p := newHuskClientPKI(t)
	s := newLoopHuskServer(t, p, 0)
	defer s.stop()

	// A rogue client cert from an untrusted CA, but trusting the real server CA so
	// the CLIENT accepts the server; the SERVER must reject the rogue client.
	rogueCA, err := pki.NewCA("attacker")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	rogueLeaf, err := rogueCA.Issue(pki.ControllerName)
	if err != nil {
		t.Fatalf("issue rogue leaf: %v", err)
	}
	rogueCert, err := tls.X509KeyPair(rogueLeaf.CertPEM, rogueLeaf.KeyPEM)
	if err != nil {
		t.Fatalf("rogue keypair: %v", err)
	}
	rogueConf := &tls.Config{
		Certificates: []tls.Certificate{rogueCert},
		RootCAs:      huskCertPool(t, p.caPEM),
		ServerName:   pki.ServerName,
		MinVersion:   tls.VersionTLS13,
	}

	pool := NewHuskConnPool()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = pool.ForkSnapshotOnHusk(ctx, s.addr, rogueConf, husk.ForkSnapshotRequest{ForkID: "f1", SnapshotDir: "/snap/f1"})
	if err == nil {
		t.Fatalf("pool must reject a wrong-CA client cert at the handshake")
	}
	if got := atomic.LoadInt64(&s.forkReqs); got != 0 {
		t.Fatalf("an unauthenticated peer must never have a request served, got %d", got)
	}
}

// TestHuskConnPoolRefusesNilTLS proves each seam refuses a nil TLS config so the
// control channel is never driven unauthenticated, matching the one-shot
// huskclient.go functions.
func TestHuskConnPoolRefusesNilTLS(t *testing.T) {
	pool := NewHuskConnPool()
	ctx := context.Background()
	if _, err := pool.ActivateHuskPod(ctx, "10.0.0.1:9443", nil, husk.ActivateRequest{Secrets: map[string]string{"API_KEY": "x"}}); err == nil {
		t.Fatal("ActivateHuskPod must refuse a nil TLS config")
	}
	if _, err := pool.ForkSnapshotOnHusk(ctx, "10.0.0.1:9443", nil, husk.ForkSnapshotRequest{}); err == nil {
		t.Fatal("ForkSnapshotOnHusk must refuse a nil TLS config")
	}
	if _, err := pool.SpawnVMOnHusk(ctx, "10.0.0.1:9443", nil, husk.SpawnVMRequest{}); err == nil {
		t.Fatal("SpawnVMOnHusk must refuse a nil TLS config")
	}
	if _, err := pool.RemoveForkSnapshotOnHusk(ctx, "10.0.0.1:9443", nil, husk.RemoveForkSnapshotRequest{}); err == nil {
		t.Fatal("RemoveForkSnapshotOnHusk must refuse a nil TLS config")
	}
}

// TestHuskConnPoolErrorHidesSecret proves an RPC error never carries the request
// payload: when the server drops the connection after the handshake, the
// activate result read fails, and the returned error names only the operation
// and address, never the secret.
func TestHuskConnPoolErrorHidesSecret(t *testing.T) {
	p := newHuskClientPKI(t)
	// A server that accepts the mTLS connection then closes WITHOUT replying, so
	// the pool's result read fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsLn := tls.NewListener(ln, p.serverConf)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, aerr := tlsLn.Accept()
			if aerr != nil {
				return
			}
			if tc, ok := conn.(*tls.Conn); ok {
				_ = tc.HandshakeContext(context.Background())
			}
			_ = conn.Close() // drop without answering
		}
	}()
	defer func() { _ = tlsLn.Close(); <-done }()

	pool := NewHuskConnPool()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const secret = "TOPSECRET-do-not-leak"
	_, err = pool.ActivateHuskPod(ctx, ln.Addr().String(), p.clientConf, husk.ActivateRequest{
		SnapshotDir: "/snap",
		Secrets:     map[string]string{"API_KEY": secret},
	})
	if err == nil {
		t.Fatal("activate against a dropping server must fail")
	}
	if got := err.Error(); strings.Contains(got, secret) {
		t.Fatalf("error leaked the secret: %q", got)
	}
}

// TestHuskConnPoolRetiresIdleConnection proves an idle connection past
// huskConnIdle is retired and re-dialed rather than reused, so the controller
// never writes onto a socket the husk server may have idle-closed.
func TestHuskConnPoolRetiresIdleConnection(t *testing.T) {
	p := newHuskClientPKI(t)
	s := newLoopHuskServer(t, p, 0)
	defer s.stop()

	pool := NewHuskConnPool()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if fres, err := pool.ForkSnapshotOnHusk(ctx, s.addr, p.clientConf, husk.ForkSnapshotRequest{ForkID: "f1", SnapshotDir: "/snap/f1"}); err != nil || !fres.OK {
		t.Fatalf("first fork-snapshot: res=%+v err=%v", fres, err)
	}
	// Force the pooled entry to look idle by backdating its lastUsed.
	ei, ok := pool.entries.Load(s.addr)
	if !ok {
		t.Fatal("pool did not cache a connection entry")
	}
	e := ei.(*huskConnEntry)
	e.mu.Lock()
	e.lastUsed = time.Now().Add(-2 * huskConnIdle)
	e.mu.Unlock()

	if fres, err := pool.ForkSnapshotOnHusk(ctx, s.addr, p.clientConf, husk.ForkSnapshotRequest{ForkID: "f2", SnapshotDir: "/snap/f2"}); err != nil || !fres.OK {
		t.Fatalf("second fork-snapshot after idle retire: res=%+v err=%v", fres, err)
	}
	if got := atomic.LoadInt64(&s.accepts); got != 2 {
		t.Fatalf("an idle-retired connection must be re-dialed, got %d accepts (want 2)", got)
	}
}
