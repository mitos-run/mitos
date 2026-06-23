//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"mitos.run/mitos/internal/vsock"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// grpc_server.go adds the gRPC runtime protocol to the guest agent. It runs on
// a SEPARATE vsock port (vsock.AgentGRPCPort = 53) ALONGSIDE the legacy
// JSON-lines accept loop on vsock.AgentPort, which remains in force during the
// wire migration (issue #24). Both the public sandbox.v1.Sandbox service and
// the host-trusted sandbox.internal.v1.Control service are served on this one
// gRPC server: inside the VM the vsock channel is reachable only by the host
// (forkd) over Firecracker's virtio-vsock, so colocating them on one in-guest
// port does not widen exposure. forkd routes Configure/NotifyForked to this
// internal-only channel and never re-exposes Control on its public :9091 edge.
//
// Transport credentials are insecure: the microVM boundary is the isolation,
// the exact same posture as the JSON-lines path, which has no in-guest auth.
// vsock is not reachable from tenant code in other sandboxes, the host network,
// or the internet. mTLS over vsock is a later hardening slice
// (docs/threat-model.md, internal/vsock/grpcconn.go).

// newGuestGRPCServer builds the grpc.Server with both guest services
// registered. It is split from the listen/serve wiring so a test can register
// the same services on its own listener.
func newGuestGRPCServer() *grpc.Server {
	s := grpc.NewServer()
	sandboxv1.RegisterSandboxServer(s, &sandboxServer{})
	internalv1.RegisterControlServer(s, &controlServer{})
	return s
}

// startGRPCServer listens on the dedicated gRPC vsock port and serves the
// runtime protocol. It is best-effort and non-fatal, like the self-service
// socket: a listen failure is logged and the legacy JSON-lines loop is
// unaffected. It blocks in Serve, so callers run it in a goroutine.
func startGRPCServer() {
	listener, err := listenVsock(vsock.AgentGRPCPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: gRPC listen error: %v\n", err)
		return
	}
	srv := newGuestGRPCServer()
	fmt.Println("sandbox-agent: gRPC runtime protocol ready on vsock port", vsock.AgentGRPCPort)
	if err := srv.Serve(listener); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: gRPC serve error: %v\n", err)
	}
}

// sandboxServer implements sandbox.v1.Sandbox. Only Exec is implemented in this
// slice; every other RPC inherits codes.Unimplemented from the embedded
// UnimplementedSandboxServer until its follow-up slice wires it (file IO,
// archive, watch, processes, signal, port-forward, run-code, the budget-gated
// self-service RPCs, and vitals). Embedding by value is required by the
// generated forward-compat contract.
type sandboxServer struct {
	sandboxv1.UnimplementedSandboxServer
}

// grpcExecSink adapts the shared exec engine (runExecStream) to the gRPC Exec
// reply stream. It reuses the exact spawn/env/process-group/exit-code logic of
// the JSON path; only the emission target differs. Sink calls are already
// serialized by runExecStream's mutex, so stream.Send is never called
// concurrently. A send error is recorded so the engine's later emissions become
// no-ops and the RPC returns it.
type grpcExecSink struct {
	stream sandboxv1.Sandbox_ExecServer
	mu     sync.Mutex
	err    error
}

func (s *grpcExecSink) chunk(stream vsock.StreamName, data []byte) {
	var msg *sandboxv1.ExecResponse
	if stream == vsock.StreamStderr {
		msg = &sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stderr{Stderr: data}}
	} else {
		msg = &sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stdout{Stdout: data}}
	}
	s.send(msg)
}

func (s *grpcExecSink) exit(exitCode int, execTimeMs float64, spawnErr string) {
	s.send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{
		ExitCode:   int32(exitCode),
		ExecTimeMs: execTimeMs,
		Error:      spawnErr,
	}}})
}

func (s *grpcExecSink) send(msg *sandboxv1.ExecResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	if err := s.stream.Send(msg); err != nil {
		s.err = err
	}
}

func (s *grpcExecSink) sendErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Exec runs a command and streams its IO over the bidi stream. The first client
// message MUST carry the open oneof; this slice runs the non-PTY shell exec by
// REUSING runExecStream, the same engine the JSON /v1/exec/stream path uses, so
// the env merge (configured env + per-call env), working dir, process-group
// kill on cancel/timeout, and exit-code mapping are byte-for-byte identical and
// secret env values are never logged. The client's ctx cancel (hang-up)
// propagates into runExecStream to kill the process tree. PTY exec and argv
// (shell-less) exec are deferred to a follow-up slice and return Unimplemented.
func (s *sandboxServer) Exec(stream sandboxv1.Sandbox_ExecServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "exec: first message recv: %v", err)
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "exec: first message must carry the open oneof")
	}
	if open.GetPty() != nil {
		return status.Error(codes.Unimplemented, "exec: PTY exec over gRPC is not implemented in this slice; use the JSON PTY path")
	}
	if len(open.GetArgs()) > 0 {
		return status.Error(codes.Unimplemented, "exec: argv (shell-less) exec is not implemented in this slice; pass the command as a single shell string")
	}

	// Translate the open frame into the existing vsock.ExecRequest the shared
	// engine consumes. Env values may be secret and are copied verbatim into the
	// process environment without being logged.
	req := &vsock.ExecRequest{
		Command:    open.GetCommand(),
		WorkingDir: open.GetCwd(),
		Timeout:    int(open.GetTimeoutSeconds()),
		Env:        envVarsToMap(open.GetEnv()),
	}

	sink := &grpcExecSink{stream: stream}
	runExecStream(stream.Context(), req, sink)
	if sendErr := sink.sendErr(); sendErr != nil {
		return status.Errorf(codes.Unavailable, "exec: stream send failed: %v", sendErr)
	}
	return nil
}

// envVarsToMap flattens the repeated EnvVar list into the map shape the shared
// exec engine and guestenv.Merge consume. On a duplicate key the last entry
// wins, matching map-merge semantics. Values may be secret and are never logged.
func envVarsToMap(vars []*sandboxv1.EnvVar) map[string]string {
	if len(vars) == 0 {
		return nil
	}
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		if v == nil {
			continue
		}
		m[v.GetKey()] = v.GetValue()
	}
	return m
}

// controlServer implements sandbox.internal.v1.Control, the host-trusted
// control channel. Every method REUSES the existing guest handlers
// (handleConfigure, handleNotifyForked) and the same uptime source as the JSON
// ping, so the secret handling and the fork-correctness reseed are byte-for-byte
// equivalent to the legacy path. This service is served ONLY on the in-guest
// vsock gRPC port and is never exposed on the public Sandbox surface or forkd
// :9091.
type controlServer struct {
	internalv1.UnimplementedControlServer
}

// Ping returns the agent uptime, the same value the JSON TypePing handler
// returns (time.Since(startTime)). Carries no secrets.
func (c *controlServer) Ping(_ context.Context, _ *internalv1.PingRequest) (*internalv1.PingResponse, error) {
	return &internalv1.PingResponse{UptimeSeconds: uptimeSeconds()}, nil
}

// Configure delivers claim-time env and secrets to the guest. THIS RPC CARRIES
// SECRET VALUES. It reuses handleConfigure verbatim: the same additive merge
// into configuredEnv under configuredMu, the same secrets-only-in-env handling,
// and the same key-count-only logging. Values are never logged or echoed. A
// non-OK reuse result becomes an Internal error whose message carries only the
// non-secret reason string from the handler.
func (c *controlServer) Configure(_ context.Context, req *internalv1.ConfigureRequest) (*internalv1.ConfigureResponse, error) {
	resp := handleConfigure(&vsock.ConfigureRequest{
		Env:     req.GetEnv(),
		Secrets: req.GetSecrets(),
	})
	if !resp.OK {
		return nil, status.Errorf(codes.Internal, "configure: %s", resp.Error)
	}
	return &internalv1.ConfigureResponse{}, nil
}

// NotifyForked applies the post-restore fork-correctness repairs. It reuses
// handleNotifyForked verbatim, so the RNDADDENTROPY reseed (fail-closed), the
// CLOCK_REALTIME step, the fork-generation write, the network reconfigure, the
// volume mounts, and the SIGUSR2 userspace reseed are byte-for-byte identical to
// the JSON path. Entropy and the absolute clock value are never logged; the
// response carries only the applied-step magnitude, the reseed boolean, and the
// signaled-process count.
func (c *controlServer) NotifyForked(_ context.Context, req *internalv1.NotifyForkedRequest) (*internalv1.NotifyForkedResponse, error) {
	vreq := &vsock.NotifyForkedRequest{
		Generation:         req.GetGeneration(),
		HostWallClockNanos: req.GetHostWallClockNanos(),
		Entropy:            req.GetEntropy(),
		Network:            notifyForkedNetworkFromProto(req.GetNetwork()),
		Volumes:            volumeMountsFromProto(req.GetVolumes()),
	}
	resp := handleNotifyForked(vreq)
	if !resp.OK {
		return nil, status.Errorf(codes.Internal, "notify_forked: %s", resp.Error)
	}
	out := resp.NotifyForked
	if out == nil {
		out = &vsock.NotifyForkedResponse{}
	}
	return &internalv1.NotifyForkedResponse{
		AppliedClockStepNanos: out.AppliedClockStepNanos,
		ReseededRng:           out.ReseededRNG,
		SignaledProcesses:     int32(out.SignaledProcesses),
	}, nil
}

// notifyForkedNetworkFromProto maps the proto network identity to the internal
// vsock shape the existing configureNetwork handler consumes. All fields are
// plain addresses (no secrets). Returns nil when the host delivered no network
// config, preserving the JSON path's nil-means-no-op behavior.
func notifyForkedNetworkFromProto(n *internalv1.NotifyForkedNetwork) *vsock.NotifyForkedNetwork {
	if n == nil {
		return nil
	}
	return &vsock.NotifyForkedNetwork{
		GuestIP:    n.GetGuestIp(),
		GatewayIP:  n.GetGatewayIp(),
		PrefixLen:  int(n.GetPrefixLen()),
		GuestMAC:   n.GetGuestMac(),
		ResolverIP: n.GetResolverIp(),
	}
}

// volumeMountsFromProto maps the proto per-fork mount table to the internal
// vsock shape mountVolumes consumes. All fields are config values, not secrets.
func volumeMountsFromProto(entries []*internalv1.VolumeMountEntry) []vsock.VolumeMountEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]vsock.VolumeMountEntry, 0, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		out = append(out, vsock.VolumeMountEntry{
			Device:    e.GetDevice(),
			MountPath: e.GetMountPath(),
			ReadOnly:  e.GetReadOnly(),
		})
	}
	return out
}
