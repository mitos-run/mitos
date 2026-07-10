package husk

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"mitos.run/mitos/internal/pki"
	"mitos.run/mitos/internal/workspace"
)

// netTestPKI issues the husk server (forkd identity) and controller client
// leaves from a fresh CA, returning the server TLS config the husk control
// listener uses and the CA PEM the clients trust.
type netTestPKI struct {
	caPEM      []byte
	serverConf *tls.Config
	ctrlCert   tls.Certificate
}

func newNetTestPKI(t *testing.T) *netTestPKI {
	t.Helper()
	ca, err := pki.NewCA("husk-test")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	serverLeaf, err := ca.Issue(pki.ServerName)
	if err != nil {
		t.Fatalf("issue server leaf: %v", err)
	}
	ctrlLeaf, err := ca.Issue(pki.ControllerName)
	if err != nil {
		t.Fatalf("issue controller leaf: %v", err)
	}
	serverConf, err := pki.ServerTLSConfig(serverLeaf.CertPEM, serverLeaf.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatalf("server TLS config: %v", err)
	}
	ctrlCert, err := tls.X509KeyPair(ctrlLeaf.CertPEM, ctrlLeaf.KeyPEM)
	if err != nil {
		t.Fatalf("controller keypair: %v", err)
	}
	return &netTestPKI{caPEM: ca.CertPEM(), serverConf: serverConf, ctrlCert: ctrlCert}
}

func certPool(t *testing.T, caPEM []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("append CA to pool")
	}
	return pool
}

// clientConf builds a client TLS config trusting the CA and pinning the husk
// server identity, presenting the given client certificate.
func (p *netTestPKI) clientConf(t *testing.T, cert tls.Certificate) *tls.Config {
	t.Helper()
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      certPool(t, p.caPEM),
		ServerName:   pki.ServerName,
		MinVersion:   tls.VersionTLS13,
	}
}

// startServer serves ServeTLS on a fresh loopback listener and returns the dial
// address plus a stop function that cancels and waits for the server.
func startServer(t *testing.T, stub *Stub, p *netTestPKI) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ServeTLS(ctx, ln, stub, p.serverConf, AuthorizeControllerIdentity)
	}()
	return ln.Addr().String(), func() {
		cancel()
		<-done
	}
}

// activateClient runs the controller-side Activate exchange directly (mirroring
// internal/controller.ActivateHuskPod, kept here so the husk test does not
// import the controller). It dials over mTLS, sends the request, reads the
// result.
func activateClient(t *testing.T, addr string, clientConf *tls.Config, req ActivateRequest) (ActivateResult, error) {
	t.Helper()
	dialer := &tls.Dialer{Config: clientConf}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return ActivateResult{}, err
	}
	defer conn.Close()
	if err := WriteControlOp(conn, OpActivate); err != nil {
		return ActivateResult{}, err
	}
	if err := WriteRequest(conn, req); err != nil {
		return ActivateResult{}, err
	}
	return ReadResult(conn)
}

func TestServeTLSControllerRoundTrip(t *testing.T) {
	p := newNetTestPKI(t)
	vm := &fakeVMM{}
	n := &fakeNotifier{}
	stub := newTestStubWithNotifier(t, vm, readyOK, n)
	if err := stub.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	addr, stop := startServer(t, stub, p)
	defer stop()

	req := ActivateRequest{
		SnapshotDir: "/data/templates/tmpl-a/snapshot",
		Env:         map[string]string{"LANG": "C"},
		Secrets:     map[string]string{"API_KEY": "s3cr3t-value"},
	}
	res, err := activateClient(t, addr, p.clientConf(t, p.ctrlCert), req)
	if err != nil {
		t.Fatalf("activate over mTLS: %v", err)
	}
	if !res.OK {
		t.Fatalf("activate not OK: %q", res.Error)
	}
	// The stub received the secret-bearing request.
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.gotReq) != 1 {
		t.Fatalf("notifier saw %d requests, want 1", len(n.gotReq))
	}
	if n.gotReq[0].Secrets["API_KEY"] != "s3cr3t-value" {
		t.Fatalf("stub did not receive the secret")
	}
}

func TestServeTLSDispatchesForkSnapshot(t *testing.T) {
	// A serving stub holding an ACTIVE fake VM answers a fork-snapshot op over the
	// mTLS control channel: it pauses, snapshots, resumes, and replies OK.
	p := newNetTestPKI(t)
	f := &fakeVMM{}
	stub := &Stub{state: StateActive, vm: f}

	addr, stop := startServer(t, stub, p)
	defer stop()

	dir := t.TempDir()
	dialer := &tls.Dialer{Config: p.clientConf(t, p.ctrlCert)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := WriteControlOp(conn, OpForkSnapshot); err != nil {
		t.Fatalf("WriteControlOp: %v", err)
	}
	if err := WriteForkSnapshotRequest(conn, ForkSnapshotRequest{ForkID: "fork-1", SnapshotDir: dir}); err != nil {
		t.Fatalf("WriteForkSnapshotRequest: %v", err)
	}
	res, err := ReadForkSnapshotResult(conn)
	if err != nil {
		t.Fatalf("ReadForkSnapshotResult: %v", err)
	}
	if !res.OK {
		t.Fatalf("fork-snapshot op not OK: %+v", res)
	}
	if !f.paused {
		t.Fatalf("source VM not paused via the control channel")
	}
}

// spawnVMClient runs the controller-side spawn-vm exchange directly (mirroring
// internal/controller.SpawnVMOnHusk, kept here so the husk test does not import
// the controller). It dials over mTLS, writes the op + request, reads the result.
func spawnVMClient(t *testing.T, addr string, clientConf *tls.Config, req SpawnVMRequest) (SpawnVMResult, error) {
	t.Helper()
	dialer := &tls.Dialer{Config: clientConf}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return SpawnVMResult{}, err
	}
	defer conn.Close()
	if err := WriteControlOp(conn, OpSpawnVM); err != nil {
		return SpawnVMResult{}, err
	}
	if err := WriteSpawnVMRequest(conn, req); err != nil {
		return SpawnVMResult{}, err
	}
	return ReadSpawnVMResult(conn)
}

// TestServeTLSDispatchesSpawnVM proves the spawn-vm control op end to end: a
// SpawnVMRequest written by the client is read by the server dispatch and drives
// prepareInstance + activateInstance for the named vmID on a MultiVM stub, so an
// ADDITIONAL same-tenant VM comes up in the running pod and the result carries its
// vsock path.
func TestServeTLSDispatchesSpawnVM(t *testing.T) {
	p := newNetTestPKI(t)
	vms := map[string]*fakeVMM{}
	stub := newMultiVMTestStub(t, vms)
	// The pod is already serving its primary VM.
	if err := stub.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := stub.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"}); err != nil {
		t.Fatalf("Activate default: %v", err)
	}

	addr, stop := startServer(t, stub, p)
	defer stop()

	const second = "fork-7"
	res, err := spawnVMClient(t, addr, p.clientConf(t, p.ctrlCert), SpawnVMRequest{
		VMID:     second,
		Activate: ActivateRequest{SnapshotDir: "/snap"},
	})
	if err != nil {
		t.Fatalf("spawn-vm over mTLS: %v", err)
	}
	if !res.OK {
		t.Fatalf("spawn-vm not OK: %q", res.Error)
	}
	if res.VsockPath == "" {
		t.Fatalf("spawn-vm result missing vsock path: %+v", res)
	}
	if res.VMID != second {
		t.Fatalf("spawn-vm result VMID = %q, want %q", res.VMID, second)
	}
	// The additional VM prepared + activated in the pod, its own instance.
	if got := stub.instances[vmID(second)].state; got != StateActive {
		t.Fatalf("spawned instance state = %s, want active", got)
	}
	// The primary VM is undisturbed and the spawned VM owns a distinct VMM.
	if got := stub.instances[defaultVMID].state; got != StateActive {
		t.Fatalf("primary instance state = %s, want active", got)
	}
	if _, ok := vms["husk-test-fork-7"]; !ok {
		t.Fatalf("spawned vmID must derive a distinct per-VM config ID, got keys %v", vms)
	}
}

// TestServeTLSSpawnVMRejectedOnSingleVMStub proves the fail-closed flag gate: a
// spawn-vm op against a stub NOT running in multi-VM mode is refused, and no VM is
// spawned. The single-VM pod owns exactly one VM and must never be driven to host
// a second over the wire.
func TestServeTLSSpawnVMRejectedOnSingleVMStub(t *testing.T) {
	p := newNetTestPKI(t)
	stub := newTestStubWithNotifier(t, &fakeVMM{}, readyOK, &fakeNotifier{})
	if stub.multiVM {
		t.Fatal("newTestStubWithNotifier must be single-VM")
	}
	if err := stub.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	addr, stop := startServer(t, stub, p)
	defer stop()

	res, err := spawnVMClient(t, addr, p.clientConf(t, p.ctrlCert), SpawnVMRequest{
		VMID:     "fork-7",
		Activate: ActivateRequest{SnapshotDir: "/snap"},
	})
	if err != nil {
		t.Fatalf("spawn-vm exchange: %v", err)
	}
	if res.OK {
		t.Fatalf("spawn-vm on a single-VM stub must fail closed, got OK: %+v", res)
	}
	if !strings.Contains(res.Error, "multi-VM") {
		t.Fatalf("spawn-vm rejection must name the multi-VM gate, got %q", res.Error)
	}
	// No instances scaffold is allocated on the single-VM path, so nothing spawned.
	if stub.instances != nil {
		t.Fatalf("single-VM stub must not spawn a VM (instances = %v)", stub.instances)
	}
	// The pod's one VM is undisturbed by the refused spawn.
	if stub.State() != StateDormant {
		t.Fatalf("single-VM pod state = %s, want dormant (refused spawn must not touch it)", stub.State())
	}
}

// TestServeTLSSpawnVMRejectsInvalidVMID proves an unsafe vmID is refused
// fail-closed by the checkVMID gate before any per-VM path is derived from it, so
// a traversing id can never spawn a VM over the control channel.
func TestServeTLSSpawnVMRejectsInvalidVMID(t *testing.T) {
	p := newNetTestPKI(t)
	vms := map[string]*fakeVMM{}
	stub := newMultiVMTestStub(t, vms)
	addr, stop := startServer(t, stub, p)
	defer stop()

	res, err := spawnVMClient(t, addr, p.clientConf(t, p.ctrlCert), SpawnVMRequest{
		VMID:     "../evil",
		Activate: ActivateRequest{SnapshotDir: "/snap"},
	})
	if err != nil {
		t.Fatalf("spawn-vm exchange: %v", err)
	}
	if res.OK {
		t.Fatalf("spawn-vm with an unsafe vmID must fail closed, got OK: %+v", res)
	}
	if !strings.Contains(res.Error, "invalid vm id") {
		t.Fatalf("spawn-vm rejection must name the invalid vm id, got %q", res.Error)
	}
	// No instance was created for the unsafe id, and no VM was started for it.
	stub.mu.Lock()
	_, exists := stub.instances["../evil"]
	stub.mu.Unlock()
	if exists {
		t.Fatal("an instance was created for an unsafe vmID")
	}
	if len(vms) != 0 {
		t.Fatalf("no VMM must start for a rejected vmID, got %v", vms)
	}
}

func TestServeTLSDispatchesDehydrateAndHydrateWorkspace(t *testing.T) {
	// A serving stub holding an ACTIVE fake VM answers dehydrate-workspace then
	// hydrate-workspace over the mTLS control channel: the dehydrate captures the
	// guest /workspace into the node CAS and returns a manifest digest; the hydrate
	// reads it back and untars it into the guest.
	p := newNetTestPKI(t)
	agent := &fakeWorkspaceAgent{tar: tarOf(t, map[string]string{"main.go": "package main"})}
	stub, _ := newWorkspaceStub(t, agent)

	addr, stop := startServer(t, stub, p)
	defer stop()

	dial := func() net.Conn {
		dialer := &tls.Dialer{Config: p.clientConf(t, p.ctrlCert)}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return conn
	}

	conn := dial()
	if err := WriteControlOp(conn, OpDehydrateWorkspace); err != nil {
		t.Fatalf("WriteControlOp dehydrate: %v", err)
	}
	if err := WriteDehydrateWorkspaceRequest(conn, DehydrateWorkspaceRequest{}); err != nil {
		t.Fatalf("WriteDehydrateWorkspaceRequest: %v", err)
	}
	dres, err := ReadDehydrateWorkspaceResult(conn)
	conn.Close()
	if err != nil {
		t.Fatalf("ReadDehydrateWorkspaceResult: %v", err)
	}
	if !dres.OK || dres.ManifestDigest == "" {
		t.Fatalf("dehydrate-workspace op not OK: %+v", dres)
	}

	conn = dial()
	if err := WriteControlOp(conn, OpHydrateWorkspace); err != nil {
		t.Fatalf("WriteControlOp hydrate: %v", err)
	}
	if err := WriteHydrateWorkspaceRequest(conn, HydrateWorkspaceRequest{ManifestDigest: dres.ManifestDigest}); err != nil {
		t.Fatalf("WriteHydrateWorkspaceRequest: %v", err)
	}
	hres, err := ReadHydrateWorkspaceResult(conn)
	conn.Close()
	if err != nil {
		t.Fatalf("ReadHydrateWorkspaceResult: %v", err)
	}
	if !hres.OK {
		t.Fatalf("hydrate-workspace op not OK: %+v", hres)
	}
	if agent.untarPath != workspace.WorkspacePath {
		t.Fatalf("hydrate did not untar into the guest workspace, got %q", agent.untarPath)
	}
}

// TestServeTLSKeepAliveMultipleRequests proves the control server serves MORE
// than one request on a SINGLE authenticated connection: the mTLS handshake and
// the controller-identity authorization happen ONCE, then two fork-snapshot ops
// are pipelined request-response on the same conn and BOTH succeed. This is the
// server half of the controller connection pool: one handshake, many RPCs. It
// also proves no frame desync across requests, because each result decodes
// cleanly after the previous one on the shared stream.
func TestServeTLSKeepAliveMultipleRequests(t *testing.T) {
	p := newNetTestPKI(t)
	f := &fakeVMM{}
	stub := &Stub{state: StateActive, vm: f}

	addr, stop := startServer(t, stub, p)
	defer stop()

	dialer := &tls.Dialer{Config: p.clientConf(t, p.ctrlCert)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// A persistent reader for the reused stream: results must decode one line at a
	// time without over-reading into the next response.
	br := bufio.NewReader(conn)
	for i := 0; i < 3; i++ {
		dir := t.TempDir()
		if err := WriteControlOp(conn, OpForkSnapshot); err != nil {
			t.Fatalf("req %d WriteControlOp: %v", i, err)
		}
		if err := WriteForkSnapshotRequest(conn, ForkSnapshotRequest{ForkID: "fork", SnapshotDir: dir}); err != nil {
			t.Fatalf("req %d WriteForkSnapshotRequest: %v", i, err)
		}
		res, err := ReadForkSnapshotResultReader(br)
		if err != nil {
			t.Fatalf("req %d ReadForkSnapshotResultReader: %v", i, err)
		}
		if !res.OK {
			t.Fatalf("req %d fork-snapshot not OK: %+v", i, res)
		}
		// The echoed dir proves this response belongs to THIS request (no desync).
		if res.SnapshotDir != dir {
			t.Fatalf("req %d result dir = %q, want %q (frame desync)", i, res.SnapshotDir, dir)
		}
	}
}

// TestServeTLSKeepAliveClosesOnPeerEOF proves a one-shot client stays byte-for-
// byte compatible: a client that sends a single request and closes drives one
// iteration, and the server loop then sees EOF and closes without error. The
// server goroutine must not wedge or panic on the closed connection.
func TestServeTLSKeepAliveClosesOnPeerEOF(t *testing.T) {
	p := newNetTestPKI(t)
	stub := &Stub{state: StateActive, vm: &fakeVMM{}}
	addr, stop := startServer(t, stub, p)
	defer stop()

	logged := captureStderr(t, func() {
		dir := t.TempDir()
		res, err := func() (ForkSnapshotResult, error) {
			dialer := &tls.Dialer{Config: p.clientConf(t, p.ctrlCert)}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, derr := dialer.DialContext(ctx, "tcp", addr)
			if derr != nil {
				return ForkSnapshotResult{}, derr
			}
			defer conn.Close()
			if werr := WriteControlOp(conn, OpForkSnapshot); werr != nil {
				return ForkSnapshotResult{}, werr
			}
			if werr := WriteForkSnapshotRequest(conn, ForkSnapshotRequest{ForkID: "fork", SnapshotDir: dir}); werr != nil {
				return ForkSnapshotResult{}, werr
			}
			return ReadForkSnapshotResult(conn)
		}()
		if err != nil {
			t.Fatalf("one-shot fork-snapshot: %v", err)
		}
		if !res.OK {
			t.Fatalf("one-shot fork-snapshot not OK: %+v", res)
		}
		// Let the server loop observe the peer close and exit its iteration.
		time.Sleep(50 * time.Millisecond)
	})
	// A clean peer close is NOT an error and must not be logged as one.
	if strings.Contains(logged, "read control op") {
		t.Fatalf("clean one-shot close logged a read error:\n%s", logged)
	}
}

func TestServeTLSRejectsNoClientCert(t *testing.T) {
	p := newNetTestPKI(t)
	n := &fakeNotifier{}
	stub := newTestStubWithNotifier(t, &fakeVMM{}, readyOK, n)
	if err := stub.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	addr, stop := startServer(t, stub, p)
	defer stop()

	// No client certificate: the server requires and verifies one, so the
	// handshake fails. The activate request must never be processed.
	noCertConf := &tls.Config{
		RootCAs:    certPool(t, p.caPEM),
		ServerName: pki.ServerName,
		MinVersion: tls.VersionTLS13,
	}
	_, err := activateClient(t, addr, noCertConf, ActivateRequest{
		SnapshotDir: "/data/snap",
		Secrets:     map[string]string{"API_KEY": "leak-me"},
	})
	if err == nil {
		t.Fatalf("expected handshake failure for missing client cert")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.calls != 0 {
		t.Fatalf("stub processed an unauthenticated request (%d notifier calls)", n.calls)
	}
}

func TestServeTLSRejectsWrongCA(t *testing.T) {
	p := newNetTestPKI(t)
	stub := newTestStubWithNotifier(t, &fakeVMM{}, readyOK, &fakeNotifier{})
	if err := stub.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	addr, stop := startServer(t, stub, p)
	defer stop()

	// A controller leaf from a DIFFERENT CA: the server's ClientCAs pool does
	// not trust it, so verification fails at the handshake.
	otherCA, err := pki.NewCA("attacker")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	otherLeaf, err := otherCA.Issue(pki.ControllerName)
	if err != nil {
		t.Fatalf("issue rogue leaf: %v", err)
	}
	rogueCert, err := tls.X509KeyPair(otherLeaf.CertPEM, otherLeaf.KeyPEM)
	if err != nil {
		t.Fatalf("rogue keypair: %v", err)
	}
	_, err = activateClient(t, addr, p.clientConf(t, rogueCert), ActivateRequest{SnapshotDir: "/data/snap"})
	if err == nil {
		t.Fatalf("expected handshake failure for wrong-CA client cert")
	}
}

func TestAuthorizeControllerIdentity(t *testing.T) {
	// nil state and an empty verified chain both fail closed.
	if err := AuthorizeControllerIdentity(nil); err == nil {
		t.Fatalf("nil state must be rejected")
	}
	if err := AuthorizeControllerIdentity(&tls.ConnectionState{}); err == nil {
		t.Fatalf("empty verified chain must be rejected")
	}

	// A verified chain whose leaf SAN is the husk server, not the controller, is
	// rejected by the identity check (defense in depth behind the TLS EKU split).
	ca, err := pki.NewCA("husk-test")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	serverLeaf, err := ca.Issue(pki.ServerName)
	if err != nil {
		t.Fatalf("issue server leaf: %v", err)
	}
	block, _ := pem.Decode(serverLeaf.CertPEM)
	if block == nil {
		t.Fatalf("decode server cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse server cert: %v", err)
	}
	wrongPeer := &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}
	if err := AuthorizeControllerIdentity(wrongPeer); err == nil {
		t.Fatalf("server identity must be rejected as an activate peer")
	}

	// The controller leaf is accepted.
	ctrlLeaf, err := ca.Issue(pki.ControllerName)
	if err != nil {
		t.Fatalf("issue controller leaf: %v", err)
	}
	cblock, _ := pem.Decode(ctrlLeaf.CertPEM)
	ccert, err := x509.ParseCertificate(cblock.Bytes)
	if err != nil {
		t.Fatalf("parse controller cert: %v", err)
	}
	ctrlPeer := &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{ccert}}}
	if err := AuthorizeControllerIdentity(ctrlPeer); err != nil {
		t.Fatalf("controller identity must be accepted: %v", err)
	}
}

func TestServeTLSRefusesWithoutTLSOrAuthorize(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	stub := newTestStubWithNotifier(t, &fakeVMM{}, readyOK, &fakeNotifier{})
	if err := ServeTLS(context.Background(), ln, stub, nil, AuthorizeControllerIdentity); err == nil {
		t.Fatalf("ServeTLS must refuse a nil TLS config")
	}
	p := newNetTestPKI(t)
	if err := ServeTLS(context.Background(), ln, stub, p.serverConf, nil); err == nil {
		t.Fatalf("ServeTLS must refuse a nil authorize hook")
	}
}

// TestServeTLSNoSecretInLogs runs a full activate plus a rejected handshake
// while capturing os.Stderr, and asserts the secret value never appears in any
// log output.
func TestServeTLSNoSecretInLogs(t *testing.T) {
	p := newNetTestPKI(t)
	stub := newTestStubWithNotifier(t, &fakeVMM{}, readyOK, &fakeNotifier{})
	if err := stub.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	logged := captureStderr(t, func() {
		addr, stop := startServer(t, stub, p)
		defer stop()

		const secret = "TOPSECRET-do-not-log"
		if _, err := activateClient(t, addr, p.clientConf(t, p.ctrlCert), ActivateRequest{
			SnapshotDir: "/data/snap",
			Secrets:     map[string]string{"API_KEY": secret},
		}); err != nil {
			t.Fatalf("activate: %v", err)
		}
		// Drive a rejected (no client cert) connection to exercise the error log
		// path; the request payload must still never be logged.
		noCertConf := &tls.Config{RootCAs: certPool(t, p.caPEM), ServerName: pki.ServerName, MinVersion: tls.VersionTLS13}
		_, _ = activateClient(t, addr, noCertConf, ActivateRequest{Secrets: map[string]string{"API_KEY": secret}})
		// Give the server goroutine a moment to flush its rejection log.
		time.Sleep(50 * time.Millisecond)
	})

	if strings.Contains(logged, "TOPSECRET-do-not-log") {
		t.Fatalf("secret value leaked into logs:\n%s", logged)
	}
}

// captureStderr redirects os.Stderr around fn and returns everything written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w

	var buf safeBuf
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = bufCopy(&buf, r)
	}()

	fn()

	os.Stderr = orig
	_ = w.Close()
	wg.Wait()
	_ = r.Close()
	return buf.String()
}

func bufCopy(dst *safeBuf, r *os.File) (int64, error) {
	buf := make([]byte, 4096)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = dst.Write(buf[:n])
			total += int64(n)
		}
		if err != nil {
			return total, err
		}
	}
}

// safeBuf is a goroutine-safe bytes.Buffer for capturing concurrent log writes.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
