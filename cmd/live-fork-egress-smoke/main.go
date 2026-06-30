// Command live-fork-egress-smoke is the KVM acceptance gate for issue #336: a
// LIVE fork (Engine.ForkRunning) of a NETWORKED sandbox routed through the
// per-sandbox egress proxy. It proves end to end, on real Firecracker, that a
// running networked sandbox can be live-forked safely: the child gets a fresh
// per-fork network identity, egresses on its OWN upstream connection through the
// proxy, does not ride the parent's held keep-alive, and passes the
// fork-correctness handshake (reseeded RNG + a stepped clock).
//
// This binary is the ForkRunning networked-SUCCESS end-to-end test: it drives
// engine.ForkRunning on a networked source with the egress proxy active (not
// merely prepareForkNetwork), which is the path Task 7's review flagged as
// missing a live KVM proof.
//
// Topology (all host-side pieces live in THIS process):
//
//	guest (busybox) --HTTP_PROXY--> sentinel:port
//	    --nftables DNAT per fork--> fork gateway:port
//	    --> per-node egress proxy (internal/egressproxy) --net.Dial--> upstream stub
//
// The upstream stub is a tiny host HTTP server bound on loopback that records
// every distinct inbound TCP connection (a fresh proxy->stub dial happens for
// each guest connection). The guest reaches it ONLY through the proxy: the
// proxy dials the stub host-side, so a fresh stub connection is the observable
// signal that a guest opened a new, independent upstream.
//
// Assertions (exit 1 on failure; setup errors exit 2, mirroring net-fork-smoke):
//
//	(a) parent AND child both get a 200 (body "hello") through independent egress.
//	(b) parent and child have DISTINCT tap/MAC/guest-IP (neither the placeholder)
//	    and the upstream connections never collide on a remote 4-tuple.
//	(c) the stub observes a NEW upstream connection attributable to the child: the
//	    parent holds a keep-alive tunnel open across the fork, and the child's
//	    request arrives on a DISTINCT connection while the parent's held tunnel
//	    stays open. This proves "child has independent egress on a fresh
//	    connection at the stub." It does NOT independently falsify fd-inheritance:
//	    a fresh wget always opens its own socket regardless of whether
//	    ResetUpstreams ran. The specific "captured upstream fd is reset and not
//	    reused" property is gated by the ResetUpstreams=true assertion (~line 231)
//	    and the ResetUpstreams unit tests in internal/fork and guest/agent-rs.
//	(d) the child's fork-correctness handshake reports ReseededRNG true and the
//	    child guest wall clock is stepped to within a few seconds of the host.
//
// This binary only does real work on a KVM host (it needs /dev/kvm plus the host
// network stack: tap creation, nftables, and the proxy listener, exactly as
// forkd uses). It compiles on any platform so cross-build checks pass; the proxy
// listener itself is linux-only (internal/egressproxy/listener_other.go stubs
// it elsewhere), so the binary is run only by the KVM CI phase.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mitos.run/mitos/internal/egressproxy"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/guestgrpc"
	"mitos.run/mitos/internal/netconf"
	"mitos.run/mitos/internal/network"
	"mitos.run/mitos/internal/vsock"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

const placeholderMAC = "02:00:00:00:00:01"

func main() {
	image := flag.String("image", "", "rootfs.ext4 path (agent as /init, with busybox incl. wget and nc) to build the template from")
	dataDir := flag.String("data-dir", "", "engine data directory")
	fcBin := flag.String("firecracker", "/usr/local/bin/firecracker", "path to the firecracker binary")
	kernel := flag.String("kernel", "", "path to the guest kernel (vmlinux)")
	agentBin := flag.String("agent-bin", "", "path to the guest agent binary")
	sentinel := flag.String("proxy-sentinel", "169.254.169.2", "fork-stable sentinel proxy address (DNATed per fork to the fork gateway)")
	proxyPort := flag.Int("proxy-port", 3128, "TCP port the per-node egress proxy listens on")
	flag.Parse()
	if *image == "" || *dataDir == "" || *kernel == "" || *agentBin == "" {
		fmt.Fprintln(os.Stderr, "live-fork-egress-smoke: --image, --data-dir, --kernel and --agent-bin are required")
		os.Exit(2)
	}
	if err := run(*image, *dataDir, *fcBin, *kernel, *agentBin, *sentinel, *proxyPort); err != nil {
		fmt.Fprintf(os.Stderr, "live-fork-egress-smoke: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("live-fork-egress-smoke: PASS: live fork of a networked sandbox egresses through the proxy on a fresh, independent connection")
}

func run(image, dataDir, fcBin, kernel, agentBin, sentinel string, proxyPort int) error {
	sentinelIP := net.ParseIP(sentinel)
	if sentinelIP == nil {
		return setupErr(fmt.Errorf("invalid --proxy-sentinel %q", sentinel))
	}

	// Host-side upstream stub. Bound on loopback: the proxy (this process) dials
	// it host-side, so it never needs to be guest-reachable directly. It counts
	// every distinct inbound TCP connection so the test can prove a fresh
	// connection vs a reused one.
	stub, err := newUpstreamStub()
	if err != nil {
		return setupErr(fmt.Errorf("start upstream stub: %w", err))
	}
	defer stub.Close()
	fmt.Printf("live-fork-egress-smoke: upstream stub on %s\n", stub.addr)

	// Per-fork networking, exactly as forkd assembles it, plus the egress proxy.
	alloc, err := netconf.NewAllocator("10.203.0.0/24", "lfesmoke")
	if err != nil {
		return setupErr(fmt.Errorf("new allocator: %w", err))
	}
	mgr := network.NewManager(network.Options{SubnetCIDR: "10.203.0.0/16", EnableForwarding: true})
	registry := egressproxy.NewRegistry()

	engine, err := fork.NewEngine(dataDir, fcBin, kernel, firecracker.JailerConfig{}, fork.EngineOpts{
		AllowUnverified: true,
		AgentBinPath:    agentBin,
		NetManager:      mgr,
		NetAllocator:    alloc,
		EgressProxy:     registry,
		ProxySentinel:   sentinelIP,
		ProxyPort:       proxyPort,
	})
	if err != nil {
		return setupErr(fmt.Errorf("new engine: %w", err))
	}

	// Start the per-node egress proxy listener, mirroring forkd: it dials every
	// upstream host-side through a bounded net.Dialer (so a forked guest never
	// inherits an open upstream) and attributes each connection by source IP.
	proxy := egressproxy.NewProxy(registry, netEgressDialer{d: net.Dialer{Timeout: 30 * time.Second}}, noopEgressLogger{})
	defer func() { _ = proxy.Close() }()
	proxyErr := make(chan error, 1)
	go func() { proxyErr <- proxy.ListenAndServe(net.JoinHostPort("", strconv.Itoa(proxyPort))) }()
	// Give the listener a moment to bind, and fail fast if it could not.
	select {
	case err := <-proxyErr:
		return setupErr(fmt.Errorf("egress proxy listener: %w", err))
	case <-time.After(500 * time.Millisecond):
	}

	templateID := "lfe-tmpl"
	if err := engine.CreateTemplate(templateID, image, nil, nil, nil, nil); err != nil {
		return setupErr(fmt.Errorf("create template: %w", err))
	}

	// Boot the networked SOURCE sandbox. EgressPolicy "deny": with the proxy on,
	// the per-fork nftables chain DNATs the sentinel to the gateway and accepts
	// the proxy port ahead of the allowlist drop, so the guest egresses through
	// the proxy, not the chain (the design under test).
	netOpts := &fork.NetworkOpts{EgressPolicy: "deny"}
	srcRes, err := engine.Fork(templateID, "lfe-src", fork.ForkOpts{Network: netOpts})
	if err != nil {
		return setupErr(fmt.Errorf("fork source: %w", err))
	}
	defer func() { _ = engine.Terminate("lfe-src") }()
	if srcRes.GuestNetwork == nil {
		return setupErr(fmt.Errorf("source fork carried no guest network; networking did not engage"))
	}
	proxyEndpoint := srcRes.GuestNetwork.ProxyEndpoint
	if proxyEndpoint == "" {
		return setupErr(fmt.Errorf("source fork carried no proxy endpoint; the egress proxy did not engage"))
	}
	fmt.Printf("live-fork-egress-smoke: source proxy endpoint %s\n", proxyEndpoint)

	// Deliver the SOURCE's fork-correctness + network handshake (cold fork: no
	// upstream reset). The agent addresses eth0 and records the proxy endpoint.
	srcClient, err := connect(srcRes.VsockPath)
	if err != nil {
		return setupErr(fmt.Errorf("connect source guest: %w", err))
	}
	defer func() { _ = srcClient.Close() }()
	if err := notifyForked(srcClient, 1, srcRes.GuestNetwork); err != nil {
		return setupErr(fmt.Errorf("notify-forked source: %w", err))
	}

	// HOLD a keep-alive connection open from the SOURCE through the proxy to the
	// stub, on a SEPARATE guest connection so it does not serialize with the
	// request execs. nc opens a CONNECT tunnel to the stub and holds it (stdin
	// kept open by sleep), so the proxy holds one upstream socket at the stub for
	// the whole test. This is the captured upstream the child must NOT inherit.
	holdClient, err := connect(srcRes.VsockPath)
	if err != nil {
		return setupErr(fmt.Errorf("connect source hold channel: %w", err))
	}
	defer func() { _ = holdClient.Close() }()
	sentHost, sentPort, err := net.SplitHostPort(proxyEndpoint)
	if err != nil {
		return setupErr(fmt.Errorf("split proxy endpoint %q: %w", proxyEndpoint, err))
	}
	stubHostPort := stub.addr
	holdCmd := fmt.Sprintf(
		"{ printf 'CONNECT %s HTTP/1.1\\r\\nHost: %s\\r\\n\\r\\n'; sleep 600; } | nc %s %s",
		stubHostPort, stubHostPort, sentHost, sentPort,
	)
	holdCtx, holdCancel := context.WithCancel(context.Background())
	defer holdCancel()
	if err := startHeld(holdCtx, holdClient, holdCmd); err != nil {
		return setupErr(fmt.Errorf("start held tunnel: %w", err))
	}

	// Wait for the held tunnel to land at the stub (a connection that sends no
	// HTTP request, so it stays open). This is the parent's captured upstream.
	if err := waitForOpenConn(stub, 15*time.Second); err != nil {
		return fmt.Errorf("held keep-alive never reached the stub through the proxy: %w", err)
	}
	fmt.Printf("live-fork-egress-smoke: source holds a keep-alive tunnel (stub open conns >= 1)\n")

	// Source egress warm-up: a request through the proxy must return 200 + body.
	if err := proxiedGet(srcClient, proxyEndpoint, stubHostPort); err != nil {
		return fmt.Errorf("source egress warm-up failed: %w", err)
	}
	fmt.Printf("live-fork-egress-smoke: PASS source egress (200 through the proxy)\n")
	baselineRequests := atomic.LoadInt64(&stub.requests)

	// === LIVE FORK the running networked source through the egress proxy. ===
	// This is the path under test: ForkRunning checkpoints the running source and
	// forks it; with the proxy active the child gets a fresh per-fork identity, a
	// NIC rebind, and ResetUpstreams=true (captured sockets die).
	childRes, err := engine.ForkRunning("lfe-src", "lfe-child", true)
	if err != nil {
		return fmt.Errorf("ForkRunning of the networked source through the proxy: %w", err)
	}
	defer func() { _ = engine.Terminate("lfe-child") }()
	if childRes.GuestNetwork == nil {
		return fmt.Errorf("live fork carried no guest network; the proxy-gated network path did not run")
	}
	if !childRes.GuestNetwork.ResetUpstreams {
		return fmt.Errorf("live fork did not set ResetUpstreams; captured upstream sockets would leak into the child")
	}
	if childRes.GuestNetwork.ProxyEndpoint == "" {
		return fmt.Errorf("live fork carried no child proxy endpoint; ForkRunning did not assign the child's egress proxy")
	}

	// Deliver the CHILD's fork-correctness + network handshake (live fork: reset
	// upstreams). Assert reseed and a stepped clock (assertion d).
	childClient, err := connect(childRes.VsockPath)
	if err != nil {
		return setupErr(fmt.Errorf("connect child guest: %w", err))
	}
	defer func() { _ = childClient.Close() }()
	hostBeforeNanos := time.Now().UnixNano()
	if err := notifyForked(childClient, 2, childRes.GuestNetwork); err != nil {
		return fmt.Errorf("child fork-correctness handshake: %w", err)
	}
	fmt.Printf("live-fork-egress-smoke: PASS child reseeded RNG after live fork (assertion d, RNG)\n")
	if err := assertClockStepped(childClient, hostBeforeNanos); err != nil {
		return fmt.Errorf("child clock step: %w", err)
	}
	fmt.Printf("live-fork-egress-smoke: PASS child wall clock stepped to host time (assertion d, clock)\n")

	// (b) Distinct per-fork identity: parent and child differ on MAC and IP, and
	// neither carries the shared placeholder MAC.
	parentMAC, parentIP, err := readEth0(srcClient)
	if err != nil {
		return fmt.Errorf("read parent eth0: %w", err)
	}
	childMAC, childIP, err := readEth0(childClient)
	if err != nil {
		return fmt.Errorf("read child eth0: %w", err)
	}
	fmt.Printf("live-fork-egress-smoke: parent eth0 MAC=%s IP=%s | child eth0 MAC=%s IP=%s\n", parentMAC, parentIP, childMAC, childIP)
	if parentMAC == "" || childMAC == "" {
		return fmt.Errorf("a fork reported an empty eth0 MAC (parent=%q child=%q)", parentMAC, childMAC)
	}
	if parentMAC == placeholderMAC || childMAC == placeholderMAC {
		return fmt.Errorf("a fork still has the shared placeholder MAC %s (parent=%s child=%s)", placeholderMAC, parentMAC, childMAC)
	}
	if parentMAC == childMAC {
		return fmt.Errorf("parent and child share the same guest MAC %s: per-fork identity violated", parentMAC)
	}
	if parentIP == "" || childIP == "" || parentIP == childIP {
		return fmt.Errorf("parent and child do not have distinct guest IPs (parent=%q child=%q)", parentIP, childIP)
	}
	fmt.Printf("live-fork-egress-smoke: PASS distinct tap/MAC/guest-IP (assertion b)\n")

	// (a) Independent egress: parent then child each get a 200 + body through the
	// proxy. (c) The child's request must arrive on a FRESH upstream connection.
	if err := proxiedGet(srcClient, proxyEndpoint, stubHostPort); err != nil {
		return fmt.Errorf("parent egress after live fork failed: %w", err)
	}
	afterParentRequests := atomic.LoadInt64(&stub.requests)
	fmt.Printf("live-fork-egress-smoke: PASS parent egress after live fork (assertion a, parent)\n")

	if err := proxiedGet(childClient, proxyEndpoint, stubHostPort); err != nil {
		return fmt.Errorf("child egress after live fork failed: %w", err)
	}
	afterChildRequests := atomic.LoadInt64(&stub.requests)
	fmt.Printf("live-fork-egress-smoke: PASS child egress after live fork (assertion a, child)\n")

	// (c) The child has independent egress on a fresh distinct connection at the
	// stub while the parent's held keep-alive stays open: the request count
	// strictly increased at the child step (confirming a new upstream TCP
	// connection was opened), the held tunnel remains open (confirming the parent
	// survived), and no two connections share a remote 4-tuple.
	//
	// NOTE: this connection-count check proves "child used a fresh connection,"
	// not "captured upstream fd was reset." A fresh wget always opens its own
	// socket regardless of whether ResetUpstreams ran. The fd-reset property is
	// gated by the ResetUpstreams=true assertion above and the ResetUpstreams
	// unit tests in internal/fork and guest/agent-rs.
	if afterParentRequests <= baselineRequests {
		return fmt.Errorf("parent request did not produce a fresh upstream connection (baseline=%d after-parent=%d)", baselineRequests, afterParentRequests)
	}
	if afterChildRequests <= afterParentRequests {
		return fmt.Errorf("child request did not produce a NEW upstream connection; the captured upstream socket may have been reused (after-parent=%d after-child=%d)", afterParentRequests, afterChildRequests)
	}
	if open := stub.openCount(); open < 1 {
		return fmt.Errorf("the parent's held keep-alive connection did not survive the child live fork (stub open conns=%d)", open)
	}
	if dup := stub.duplicateRemotes(); dup != "" {
		return fmt.Errorf("two upstream connections shared a remote 4-tuple (%s): socket collision", dup)
	}
	fmt.Printf("live-fork-egress-smoke: PASS child egress on a fresh connection, parent keep-alive held, no 4-tuple collision (assertion c)\n")

	return nil
}

func setupErr(err error) error {
	fmt.Fprintf(os.Stderr, "live-fork-egress-smoke: SETUP: %v\n", err)
	os.Exit(2)
	return err
}

// notifyForked delivers the daemon's fork-correctness + per-fork network
// handshake: fresh CRNG entropy plus this fork's eth0 config (including the
// proxy endpoint and the reset-upstreams flag). It asserts the guest reseeded.
func notifyForked(client *guestgrpc.Client, generation uint64, gn *vsock.NotifyForkedNetwork) error {
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return fmt.Errorf("entropy: %w", err)
	}
	var protoNetwork *internalv1.NotifyForkedNetwork
	if gn != nil {
		protoNetwork = &internalv1.NotifyForkedNetwork{
			GuestIp:        gn.GuestIP,
			GatewayIp:      gn.GatewayIP,
			PrefixLen:      int32(gn.PrefixLen), //nolint:gosec // network prefix (0-32)
			GuestMac:       gn.GuestMAC,
			ResolverIp:     gn.ResolverIP,
			ProxyEndpoint:  gn.ProxyEndpoint,
			ResetUpstreams: gn.ResetUpstreams,
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Control.NotifyForked(ctx, &internalv1.NotifyForkedRequest{
		Generation:         generation,
		HostWallClockNanos: time.Now().UnixNano(),
		Entropy:            entropy,
		Network:            protoNetwork,
	})
	if err != nil {
		return fmt.Errorf("notify-forked rpc: %w", err)
	}
	if resp == nil || !resp.GetReseededRng() {
		return fmt.Errorf("guest did not reseed its RNG after fork")
	}
	return nil
}

// assertClockStepped reads the child guest wall clock after the handshake and
// asserts it stepped forward to within a few seconds of the host clock, the
// observable proof that the clock resync ran (fork-correctness section 1). A
// generous 10s window tolerates a slow CI runner.
func assertClockStepped(client *guestgrpc.Client, hostBeforeNanos int64) error {
	out, err := execOut(client, "date +%s")
	if err != nil {
		return fmt.Errorf("read guest wall clock: %w", err)
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return fmt.Errorf("parse guest wall clock %q: %w", strings.TrimSpace(out), err)
	}
	guestNanos := secs * int64(time.Second)
	hostNanos := time.Now().UnixNano()
	diff := guestNanos - hostNanos
	if diff < 0 {
		diff = -diff
	}
	const maxSkew = int64(10 * time.Second)
	if guestNanos < hostBeforeNanos-maxSkew || diff > maxSkew {
		return fmt.Errorf("guest wall clock not stepped to host time (guest=%ds host=%ds skew=%dns > %dns)", secs, hostNanos/int64(time.Second), diff, maxSkew)
	}
	return nil
}

// readEth0 returns the guest's lowercased eth0 MAC and its CIDR address.
func readEth0(client *guestgrpc.Client) (mac, ip string, err error) {
	mac, err = execOut(client, "cat /sys/class/net/eth0/address")
	if err != nil {
		return "", "", fmt.Errorf("read eth0 MAC: %w", err)
	}
	ip, err = execOut(client, "ip -o -4 addr show dev eth0 | awk '{print $4}'")
	if err != nil {
		return "", "", fmt.Errorf("read eth0 IP: %w", err)
	}
	return strings.TrimSpace(strings.ToLower(mac)), strings.TrimSpace(ip), nil
}

// proxiedGet issues an HTTP GET to the stub THROUGH the proxy (http_proxy points
// at the sentinel, DNATed per fork to the gateway). It asserts the body the stub
// returns, proving a 200 round trip on a fresh upstream connection.
func proxiedGet(client *guestgrpc.Client, proxyEndpoint, stubHostPort string) error {
	cmd := fmt.Sprintf("http_proxy=http://%s wget -q -O - http://%s/", proxyEndpoint, stubHostPort)
	out, err := execOut(client, cmd)
	if err != nil {
		return fmt.Errorf("proxied GET: %w", err)
	}
	if !strings.Contains(out, stubBody) {
		return fmt.Errorf("proxied GET returned unexpected body %q (want %q)", strings.TrimSpace(out), stubBody)
	}
	return nil
}

// startHeld opens an exec stream for a long-running command and keeps it open by
// draining its frames in a goroutine until ctx is cancelled. It is used to hold
// a keep-alive connection open in the guest for the duration of the test.
func startHeld(ctx context.Context, client *guestgrpc.Client, command string) error {
	stream, err := client.Sandbox.ExecStream(ctx, &sandboxv1.ExecStreamRequest{
		Command:        command,
		Cwd:            "/",
		TimeoutSeconds: 600,
	})
	if err != nil {
		return fmt.Errorf("open held exec stream: %w", err)
	}
	go func() {
		for {
			if _, rerr := stream.Recv(); rerr != nil {
				return
			}
		}
	}()
	return nil
}

func execOut(client *guestgrpc.Client, command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stream, err := client.Sandbox.ExecStream(ctx, &sandboxv1.ExecStreamRequest{
		Command:        command,
		Cwd:            "/",
		TimeoutSeconds: 60,
	})
	if err != nil {
		return "", fmt.Errorf("exec stream: %w", err)
	}
	var stdout, stderr strings.Builder
	var exitCode int32
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("recv exec frame: %w", err)
		}
		switch m := msg.Msg.(type) {
		case *sandboxv1.ExecResponse_Stdout:
			stdout.Write(m.Stdout)
		case *sandboxv1.ExecResponse_Stderr:
			stderr.Write(m.Stderr)
		case *sandboxv1.ExecResponse_Exit:
			exitCode = m.Exit.GetExitCode()
			if spawnErr := m.Exit.GetError(); spawnErr != "" {
				return stdout.String(), fmt.Errorf("exec spawn error: %s", spawnErr)
			}
		}
	}
	if exitCode != 0 {
		return stdout.String(), fmt.Errorf("command %q exited %d: %s", command, exitCode, stderr.String())
	}
	return stdout.String(), nil
}

func connect(udsPath string) (*guestgrpc.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return guestgrpc.WaitReady(ctx, udsPath, 30*time.Second)
}

// --- host-side egress proxy seams, mirroring forkd ---

// netEgressDialer opens upstream sockets through the host's net.Dialer so the
// host process owns every upstream connection; a forked guest never inherits an
// already-open upstream.
type netEgressDialer struct {
	d net.Dialer
}

func (n netEgressDialer) Dial(ctx context.Context, hostport string) (net.Conn, error) {
	return n.d.DialContext(ctx, "tcp", hostport)
}

// noopEgressLogger discards egress events: this smoke asserts at the stub, and
// the redaction contract is unit-tested in internal/egressproxy. It records
// sandbox ID, host:port, and byte counts only, never secrets.
type noopEgressLogger struct{}

func (noopEgressLogger) Egress(sandboxID, hostport string, bytesUp, bytesDown int64) {}

// --- host-side upstream stub ---

const stubBody = "hello"

// upstreamStub is a tiny host HTTP server the proxy dials. It records every
// distinct inbound TCP connection so the test can prove a fresh connection vs a
// reused one. A connection that sends an HTTP request line gets a 200 + body
// and closes; a connection that sends nothing (the held CONNECT tunnel) stays
// open and is counted as an open connection.
type upstreamStub struct {
	ln   net.Listener
	addr string

	requests int64 // atomic: connections that sent an HTTP request line

	mu       sync.Mutex
	open     int            // currently-open connections
	remotes  map[string]int // remote addr -> times seen (collision detection)
	dupFound string
}

func newUpstreamStub() (*upstreamStub, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	s := &upstreamStub{ln: ln, addr: ln.Addr().String(), remotes: make(map[string]int)}
	go s.serve()
	return s, nil
}

func (s *upstreamStub) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *upstreamStub) handle(conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	s.mu.Lock()
	s.open++
	s.remotes[remote]++
	if s.remotes[remote] > 1 && s.dupFound == "" {
		s.dupFound = remote
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.open--
		s.mu.Unlock()
	}()

	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		// No request arrived (the held CONNECT tunnel): block until the peer
		// closes, keeping this counted as an open connection.
		_, _ = io.Copy(io.Discard, br)
		return
	}
	if strings.HasPrefix(line, "GET") {
		// Drain the rest of the request headers, then answer 200 + body.
		for {
			h, herr := br.ReadString('\n')
			if herr != nil || strings.TrimRight(h, "\r\n") == "" {
				break
			}
		}
		atomic.AddInt64(&s.requests, 1)
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(stubBody), stubBody)
		return
	}
	// Anything else: hold open until the peer closes.
	_, _ = io.Copy(io.Discard, br)
}

func (s *upstreamStub) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open
}

func (s *upstreamStub) duplicateRemotes() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dupFound
}

func (s *upstreamStub) Close() {
	_ = s.ln.Close()
}

// waitForOpenConn waits until the stub has at least one open connection (the
// held keep-alive tunnel landed through the proxy).
func waitForOpenConn(stub *upstreamStub, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if stub.openCount() >= 1 {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("no open connection at the stub within %s", timeout)
}
