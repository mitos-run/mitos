//go:build linux

package main

import "testing"

// TestVsockAddrsNonNil is the regression guard for the PID-1 kernel-panic bug:
// grpc.Server.Serve derefs Listener.Addr() (and per-connection LocalAddr/
// RemoteAddr) during channelz registration. When these returned nil, Serve
// panicked, and because the agent is PID 1 the panic kernel-panicked the whole
// microVM at boot. They MUST return a non-nil net.Addr. The Addr methods are
// pure (no fd access), so zero-value structs are safe to exercise here.
func TestVsockAddrsNonNil(t *testing.T) {
	var l vsockListener
	if l.Addr() == nil {
		t.Fatal("vsockListener.Addr() must be non-nil: grpc.Serve derefs it and a nil return panics PID 1")
	}
	var c vsockConn
	if c.LocalAddr() == nil {
		t.Fatal("vsockConn.LocalAddr() must be non-nil")
	}
	if c.RemoteAddr() == nil {
		t.Fatal("vsockConn.RemoteAddr() must be non-nil")
	}
}
