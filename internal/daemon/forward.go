package daemon

import (
	"context"
	"fmt"
	"net"
	"sync"

	"mitos.run/mitos/internal/sandboxrpc"
)

// defaultMaxForwards is the per-sandbox ceiling on concurrent OPEN port forwards
// (issue #228). Each forward holds a host TCP listener plus one gRPC tunnel
// goroutine pair per accepted connection, so an unbounded number would exhaust
// host sockets and goroutines. It mirrors the streaming-exec ceiling intent
// (production-blocker #2): a sandbox cannot exhaust host resources via forwards.
const defaultMaxForwards = 16

// portForward is one live host-side forward (issue #228): a host TCP listener on
// loopback bridged over a per-connection gRPC PortForward tunnel to a guest
// loopback port. It tracks every accepted host connection so Close tears the
// whole forward down with no goroutine or fd leak.
type portForward struct {
	guestPort int
	listener  net.Listener
	hostAddr  string

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// SetMaxForwardsPerSandbox sets the per-sandbox ceiling on concurrent OPEN port
// forwards (issue #228). A NEW forward over the cap is rejected. n<=0 disables
// the cap (unbounded). Must be called before the API serves requests; the field
// is not synchronized.
func (api *SandboxAPI) SetMaxForwardsPerSandbox(n int) {
	api.maxForwards = n
}

// ForwardPort opens a host TCP listener on 127.0.0.1:0 and bridges every
// accepted connection over a fresh gRPC PortForward tunnel to the guest's
// 127.0.0.1:guestPort (issue #228). It returns the host address (host:port) the
// caller dials. The listener and all its tunnels are tracked under sandboxID and
// torn down by CloseForwards (which UnregisterSandbox calls on terminate), so no
// host listener or tunnel goroutine outlives the sandbox.
//
// The host listener binds to loopback ONLY: the standalone server has no token
// on this path (the same tokenless trust model as the rest of the standalone
// server), so a loopback bind keeps the forward reachable only from the host
// running the server, never from the network. The guest dial is forced to
// loopback by the guest agent. A guest port that is not listening surfaces as a
// per-connection tunnel error (the host connection is closed), not a hang.
//
// It fails fast (before opening a listener) when the sandbox has no registered
// stream path or agent, when guestPort is out of range, or when the sandbox is
// already at the per-sandbox forward cap.
func (api *SandboxAPI) ForwardPort(sandboxID string, guestPort int) (string, error) {
	if guestPort < 1 || guestPort > 65535 {
		return "", fmt.Errorf("guest port %d out of range 1-65535", guestPort)
	}

	// Require a registered sandbox and stream path before binding a host socket,
	// so a forward for an unknown sandbox fails cleanly instead of opening a
	// listener whose every connection would error.
	if err := api.checkSandboxRegistered(sandboxID); err != nil {
		return "", err
	}
	api.mu.RLock()
	_, hasPath := api.streamPaths[sandboxID]
	api.mu.RUnlock()
	if !hasPath {
		return "", fmt.Errorf("sandbox %s has no stream path; cannot open a port forward", sandboxID)
	}

	// Enforce the per-sandbox forward cap and reserve a slot atomically so two
	// concurrent ForwardPort calls cannot both pass the check.
	api.mu.Lock()
	if api.maxForwards > 0 && len(api.forwards[sandboxID]) >= api.maxForwards {
		api.mu.Unlock()
		return "", fmt.Errorf("sandbox %s is at its port-forward limit of %d", sandboxID, api.maxForwards)
	}
	api.mu.Unlock()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("open host forward listener: %w", err)
	}
	pf := &portForward{
		guestPort: guestPort,
		listener:  lis,
		hostAddr:  lis.Addr().String(),
		conns:     make(map[net.Conn]struct{}),
	}

	api.mu.Lock()
	// Re-check the cap under the same lock the listener is registered under, then
	// commit the forward. (A racing CloseForwards cannot have run yet because the
	// forward is not yet registered.)
	if api.maxForwards > 0 && len(api.forwards[sandboxID]) >= api.maxForwards {
		api.mu.Unlock()
		lis.Close()
		return "", fmt.Errorf("sandbox %s is at its port-forward limit of %d", sandboxID, api.maxForwards)
	}
	api.forwards[sandboxID] = append(api.forwards[sandboxID], pf)
	api.mu.Unlock()

	go api.acceptForward(sandboxID, pf)
	return pf.hostAddr, nil
}

// acceptForward is the host listener accept loop for one forward: each accepted
// host connection is bridged over a fresh gRPC PortForward tunnel to the guest
// port. The loop ends when the listener is closed (CloseForwards).
func (api *SandboxAPI) acceptForward(sandboxID string, pf *portForward) {
	for {
		hostConn, err := pf.listener.Accept()
		if err != nil {
			return // listener closed: the forward is being torn down
		}
		go api.bridgeForwardConn(sandboxID, pf, hostConn)
	}
}

// bridgeForwardConn opens a gRPC PortForward tunnel to the guest port and
// splices the accepted host connection to it bidirectionally. A tunnel open
// failure (the guest port is not listening) closes the host connection with no
// hang. The host connection is tracked on pf so CloseForwards tears it down,
// and deregistered when the bridge ends.
func (api *SandboxAPI) bridgeForwardConn(sandboxID string, pf *portForward, hostConn net.Conn) {
	defer hostConn.Close()

	// Register the host connection so CloseForwards can close it even mid-copy.
	if !pf.trackConn(hostConn) {
		return // the forward was closed between Accept and here
	}
	defer pf.untrackConn(hostConn)

	// Dial a fresh gRPC PortForward stream to the guest. Use a background context
	// so the tunnel lifetime is not tied to an HTTP request context; the tunnel
	// is torn down by hostConn.Close() (tracked on pf) or CloseForwards.
	g := newVsockGuestConn(api, sandboxID).(*vsockGuestConn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pfStream, err := g.PortForward(ctx, uint32(pf.guestPort))
	if err != nil {
		return // guest refused (port not listening); host connection closed
	}
	defer pfStream.Close()

	// Splice host<->tunnel bidirectionally. Both goroutines close both sides when
	// they finish so a half-close promptly tears the other down. Bytes are never
	// logged (secret hygiene).
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			cancel()
			pfStream.Close() //nolint:errcheck // teardown
			hostConn.Close()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Guest-to-host: receive gRPC frames and write raw bytes to the host conn.
	go func() {
		defer wg.Done()
		defer closeBoth()
		for {
			frame, recvErr := pfStream.Recv()
			if recvErr != nil {
				return
			}
			if frame.Close {
				return
			}
			if len(frame.Data) > 0 {
				if _, werr := hostConn.Write(frame.Data); werr != nil {
					return
				}
			}
		}
	}()

	// Host-to-guest: read raw bytes from the host conn and send as gRPC frames.
	go func() {
		defer wg.Done()
		defer closeBoth()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := hostConn.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if serr := pfStream.Send(chunk); serr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	wg.Wait()
}

// trackConn registers c on the forward so a teardown can close it. It returns
// false if the forward is already closed, in which case the caller must abandon
// c.
func (pf *portForward) trackConn(c net.Conn) bool {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if pf.closed {
		return false
	}
	pf.conns[c] = struct{}{}
	return true
}

func (pf *portForward) untrackConn(c net.Conn) {
	pf.mu.Lock()
	delete(pf.conns, c)
	pf.mu.Unlock()
}

// close shuts the listener and every tracked connection for this forward.
// Idempotent.
func (pf *portForward) close() {
	pf.mu.Lock()
	if pf.closed {
		pf.mu.Unlock()
		return
	}
	pf.closed = true
	conns := make([]net.Conn, 0, len(pf.conns))
	for c := range pf.conns {
		conns = append(conns, c)
	}
	pf.conns = nil
	pf.mu.Unlock()

	_ = pf.listener.Close()
	for _, c := range conns {
		_ = c.Close()
	}
}

// CloseForwards closes every live host-side port forward for sandboxID: the host
// listeners and all in-flight tunnels. It is called by UnregisterSandbox so a
// terminate leaves no host listener or tunnel goroutine behind. Safe to call for
// a sandbox with no forwards.
func (api *SandboxAPI) CloseForwards(sandboxID string) {
	api.mu.Lock()
	pfs := api.forwards[sandboxID]
	delete(api.forwards, sandboxID)
	api.mu.Unlock()

	for _, pf := range pfs {
		pf.close()
	}
}

// portForwardStream is the interface used by bridgeForwardConn to splice bytes,
// exposed for testing without a real gRPC connection.
var _ sandboxrpc.PortForwardStream = (*grpcPortForwardStream)(nil)
