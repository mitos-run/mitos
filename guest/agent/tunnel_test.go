//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"

	"mitos.run/mitos/internal/vsock"
)

// tunnelTargetPort extracts the port from a 127.0.0.1:port listener address.
func tunnelTargetPort(t *testing.T, l net.Listener) int {
	t.Helper()
	return l.Addr().(*net.TCPAddr).Port
}

// TestHandleTunnelEchoesBidirectionally starts a loopback echo server, drives
// handleTunnel over an in-process pipe, and asserts bytes round-trip both ways
// through the tunnel after a successful ack.
func TestHandleTunnelEchoesBidirectionally(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						c.Write(append([]byte("re:"), buf[:n]...))
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()

	server, client := net.Pipe()
	go func() {
		defer server.Close()
		handleTunnel(server, &vsock.TunnelRequest{Port: tunnelTargetPort(t, echo)})
	}()
	defer client.Close()

	br := bufio.NewReader(client)
	ackLine, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	var ack vsock.TunnelAck
	if err := json.Unmarshal(ackLine, &ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if !ack.OK {
		t.Fatalf("ack not ok: %+v", ack)
	}

	if _, err := client.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, 16)
	n, err := br.Read(got)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got[:n]) != "re:hi" {
		t.Fatalf("round trip = %q, want %q", got[:n], "re:hi")
	}
}

// TestHandleTunnelRefusesNonListeningPort asserts a tunnel to a port with no
// listener returns a clean ack with OK=false and a cause, not a hang.
func TestHandleTunnelRefusesNonListeningPort(t *testing.T) {
	// Bind then close to obtain a port that is reliably not listening.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := tunnelTargetPort(t, l)
	l.Close()

	server, client := net.Pipe()
	go func() {
		defer server.Close()
		handleTunnel(server, &vsock.TunnelRequest{Port: port})
	}()
	defer client.Close()

	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(client)
	ackLine, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	var ack vsock.TunnelAck
	if err := json.Unmarshal(ackLine, &ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if ack.OK {
		t.Fatal("expected ack OK=false for a non-listening guest port")
	}
	if ack.Error == "" {
		t.Fatal("refused ack must carry a cause")
	}
}

// TestHandleTunnelRejectsInvalidPort asserts an out-of-range port is refused at
// the ack without attempting any dial.
func TestHandleTunnelRejectsInvalidPort(t *testing.T) {
	server, client := net.Pipe()
	go func() {
		defer server.Close()
		handleTunnel(server, &vsock.TunnelRequest{Port: 0})
	}()
	defer client.Close()

	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(client)
	ackLine, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	var ack vsock.TunnelAck
	_ = json.Unmarshal(ackLine, &ack)
	if ack.OK {
		t.Fatal("expected ack OK=false for an invalid port")
	}
}
