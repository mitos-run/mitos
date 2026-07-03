// cmd/grpc-exec-smoke proves the new gRPC runtime path (sandbox.v1.Sandbox)
// works against a REAL guest VM over vsock. It is the acceptance gate for Task
// 5.4: KVM-CI phase (issue #24 stage 5).
//
// Two sub-commands, each requires a running Firecracker VM whose guest agent
// serves the gRPC port (vsock.AgentGRPCPort = 53):
//
//	grpc-exec-smoke streaming <vsock-uds-path>
//	  Runs a command that emits output incrementally (a shell loop with sleep),
//	  asserts that the FIRST stdout chunk arrives before the command finishes
//	  (streaming delivery, not buffered), and asserts the correct exit code.
//
//	grpc-exec-smoke pty <vsock-uds-path>
//	  Opens an interactive PTY (ExecOpen with pty set), sends "echo grpc-pty-ok"
//	  followed by EOF (ctrl-d), verifies the echo appears in the PTY output, then
//	  sends a window-resize frame (ExecRequest.resize) and asserts no error is
//	  returned. This proves the PTY-in-Exec path on a real TTY over vsock gRPC.
//
// The streaming assertion is the key correctness property: it timestamps the
// moment the FIRST chunk arrives and the moment the final exit arrives. If the
// output was buffered and only flushed at command exit the two timestamps would
// be equal; a positive delta of at least one sleep interval proves incremental
// delivery. The actual minimum sleep in the command is 200ms so any sane check
// (we require >= 100ms delta) distinguishes streaming from buffering.
//
// Usage in CI: the kvm-test.yaml workflow calls this binary with each
// sub-command in turn against the vsock UDS left by a running Firecracker VM.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"

	"mitos.run/mitos/internal/vsock"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: grpc-exec-smoke <streaming|pty> <vsock-uds-path>")
		os.Exit(1)
	}
	sub := os.Args[1]
	udsPath := os.Args[2]

	cc, client, err := dialGuest(udsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL dial: %v\n", err)
		os.Exit(1)
	}
	defer cc.Close()

	switch sub {
	case "streaming":
		runStreaming(client)
	case "pty":
		runPTY(client)
	default:
		fmt.Fprintf(os.Stderr, "unknown sub-command %q (want streaming|pty)\n", sub)
		os.Exit(1)
	}
}

// dialGuest dials the guest gRPC server over the Firecracker vsock UDS. It
// retries for up to 30 seconds so the KVM phase does not need a separate
// wait-for-agent loop. Returns the raw *grpc.ClientConn (caller must Close)
// and a bound SandboxClient.
func dialGuest(udsPath string) (*grpc.ClientConn, sandboxv1.SandboxClient, error) {
	var (
		cc  *grpc.ClientConn
		err error
	)
	for attempt := 0; attempt < 15; attempt++ {
		cc, err = vsock.DialGRPCOverConn(func() (net.Conn, error) {
			return vsock.DialGRPCConn(udsPath, vsock.AgentGRPCPort)
		})
		if err == nil {
			return cc, sandboxv1.NewSandboxClient(cc), nil
		}
		fmt.Printf("dial attempt %d failed: %v (retrying in 2s)\n", attempt+1, err)
		time.Sleep(2 * time.Second)
	}
	return nil, nil, fmt.Errorf("dial failed after 15 attempts: %w", err)
}

// runStreaming proves that the gRPC Exec path delivers stdout INCREMENTALLY.
//
// Command: a small shell loop that prints a line and sleeps 200ms, four
// times in total. Total runtime is ~800ms. A streaming transport delivers
// each line as it is printed; a buffered transport delivers all four lines at
// exit. The assertion checks that the first stdout chunk arrives at least 100ms
// before the exit message so the test passes even on a slow CI runner.
func runStreaming(client sandboxv1.SandboxClient) {
	// The busybox rootfs built by the kvm phase has sh, echo, and sleep.
	// Four lines, 200ms apart, total ~800ms.
	const cmd = `for i in 1 2 3 4; do echo "grpc-stream-line-$i"; sleep 0.2; done`

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := client.Exec(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL Exec open stream: %v\n", err)
		os.Exit(1)
	}

	// Send the open request (non-PTY exec).
	if err := stream.Send(&sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{
			Open: &sandboxv1.ExecOpen{
				Command:        cmd,
				TimeoutSeconds: 20,
			},
		},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL ExecRequest send: %v\n", err)
		os.Exit(1)
	}
	// Signal no stdin: half-close the send side.
	if err := stream.CloseSend(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL CloseSend: %v\n", err)
		os.Exit(1)
	}

	var (
		firstChunkAt time.Time
		exitAt       time.Time
		lines        []string
		exitCode     int32
	)

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL Recv: %v\n", err)
			os.Exit(1)
		}
		switch v := msg.Msg.(type) {
		case *sandboxv1.ExecResponse_Stdout:
			if firstChunkAt.IsZero() {
				firstChunkAt = time.Now()
			}
			lines = append(lines, strings.TrimRight(string(v.Stdout), "\n"))
		case *sandboxv1.ExecResponse_Stderr:
			// stderr is unexpected for this command but not fatal.
			fmt.Printf("stderr chunk: %q\n", v.Stderr)
		case *sandboxv1.ExecResponse_Exit:
			exitAt = time.Now()
			exitCode = v.Exit.ExitCode
		}
	}

	// --- assertions ---

	if exitCode != 0 {
		fmt.Fprintf(os.Stderr, "FAIL streaming: exit_code=%d want 0\n", exitCode)
		os.Exit(1)
	}
	fmt.Printf("PASS streaming: exit_code=0\n")

	// Expect four output lines.
	if len(lines) < 4 {
		fmt.Fprintf(os.Stderr, "FAIL streaming: got %d lines, want 4; output: %v\n", len(lines), lines)
		os.Exit(1)
	}
	for i, want := range []string{"grpc-stream-line-1", "grpc-stream-line-2", "grpc-stream-line-3", "grpc-stream-line-4"} {
		if !strings.Contains(strings.Join(lines, "\n"), want) {
			fmt.Fprintf(os.Stderr, "FAIL streaming: line %d: want %q in %v\n", i+1, want, lines)
			os.Exit(1)
		}
	}
	fmt.Printf("PASS streaming: received 4 expected output lines\n")

	// Streaming assertion: first chunk must arrive at least 100ms before the
	// exit message. The loop has 4 iterations x 200ms = ~800ms total; a chunked
	// stream delivers the first line after the first echo (~0ms) and the exit
	// after all four sleeps (~800ms). A buffered delivery would have
	// firstChunkAt == exitAt (both set on the single flush at exit). 100ms is
	// a generous floor that distinguishes the two even on a slow CI runner.
	if firstChunkAt.IsZero() || exitAt.IsZero() {
		fmt.Fprintf(os.Stderr, "FAIL streaming: missing timing timestamps (firstChunk=%v exit=%v)\n", firstChunkAt, exitAt)
		os.Exit(1)
	}
	delta := exitAt.Sub(firstChunkAt)
	const minStreamingDelta = 100 * time.Millisecond
	if delta < minStreamingDelta {
		fmt.Fprintf(os.Stderr, "FAIL streaming: first chunk arrived only %v before exit (want >= %v); output was likely buffered, not streamed\n", delta, minStreamingDelta)
		os.Exit(1)
	}
	fmt.Printf("PASS streaming: first chunk arrived %v before exit (>= %v, proves incremental delivery)\n", delta, minStreamingDelta)

	fmt.Println("================================")
	fmt.Println("  gRPC streaming Exec proven: incremental delivery over vsock gRPC")
	fmt.Println("================================")
}

// sendOrFail sends one ExecRequest frame and exits with a diagnosable message
// on failure. gRPC returns a bare io.EOF from SendMsg whenever the server has
// already terminated the RPC; the real terminal status is only observable via
// RecvMsg. Without this, a server-side failure (for example the guest agent
// failing openpty because devpts is not mounted, issue #535) surfaces as an
// opaque "EOF" and the root cause is invisible in CI logs.
func sendOrFail(stream sandboxv1.Sandbox_ExecClient, what string, req *sandboxv1.ExecRequest) {
	err := stream.Send(req)
	if err == nil {
		return
	}
	if err == io.EOF {
		// Drain any buffered frames; the RPC is already terminated so Recv
		// returns the terminal status (or a clean io.EOF) promptly.
		for {
			_, rerr := stream.Recv()
			if rerr == io.EOF {
				fmt.Fprintf(os.Stderr, "FAIL %s: server closed the stream (clean EOF) before the frame was accepted\n", what)
				os.Exit(1)
			}
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "FAIL %s: server terminated the stream: %v\n", what, rerr)
				os.Exit(1)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", what, err)
	os.Exit(1)
}

// runPTY proves the PTY-in-Exec path over vsock gRPC:
//  1. Open an ExecOpen with a PtyOptions set (initial 80x24 window).
//  2. Write "echo grpc-pty-ok" + newline as stdin bytes so the shell runs it.
//  3. Write a ctrl-d (EOF) as stdin_close to end the shell session.
//  4. Collect PTY stdout until the exit message arrives.
//  5. Assert the echo output appears in the collected PTY output.
//  6. Send a window-resize (cols=120, rows=40) before CloseSend and assert no
//     error, proving the resize frame is accepted by the gRPC transport.
//
// A PTY merges stdout/stderr onto one stream, so all output arrives as
// ExecResponse.stdout bytes.
func runPTY(client sandboxv1.SandboxClient) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := client.Exec(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL Exec open PTY stream: %v\n", err)
		os.Exit(1)
	}

	// 1. Open with PTY (80x24 window, default shell /bin/sh).
	sendOrFail(stream, "ExecRequest PTY open send", &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{
			Open: &sandboxv1.ExecOpen{
				Command: "/bin/sh",
				Pty: &sandboxv1.PtyOptions{
					Term: "xterm-256color",
					Size: &sandboxv1.WindowSize{Cols: 80, Rows: 24},
				},
				TimeoutSeconds: 15,
			},
		},
	})

	// Give the shell a moment to start its prompt before we send input.
	time.Sleep(200 * time.Millisecond)

	// 2. Send the command as stdin bytes.
	sendOrFail(stream, "send PTY stdin (command)", &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Stdin{
			Stdin: []byte("echo grpc-pty-ok\n"),
		},
	})

	// Small pause so the shell can run the echo before we send ctrl-d.
	time.Sleep(200 * time.Millisecond)

	// 6. Send a window resize before closing (proves the resize frame path).
	sendOrFail(stream, "send PTY resize", &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Resize{
			Resize: &sandboxv1.WindowSize{Cols: 120, Rows: 40},
		},
	})

	// 3. Signal EOF on stdin (ctrl-d closes the shell session).
	sendOrFail(stream, "send PTY stdin_close", &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_StdinClose{StdinClose: true},
	})
	if err := stream.CloseSend(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL CloseSend PTY: %v\n", err)
		os.Exit(1)
	}

	// 4. Collect output until the exit message.
	var (
		output   strings.Builder
		exitCode int32 = -1
	)
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL PTY Recv: %v\n", err)
			os.Exit(1)
		}
		switch v := msg.Msg.(type) {
		case *sandboxv1.ExecResponse_Stdout:
			output.Write(v.Stdout)
		case *sandboxv1.ExecResponse_Stderr:
			// PTY merges stderr onto stdout; unexpected but not fatal.
			output.Write(v.Stderr)
		case *sandboxv1.ExecResponse_Exit:
			exitCode = v.Exit.ExitCode
		}
	}

	// 5. Assert the echo output appears in the collected PTY output.
	got := output.String()
	if !strings.Contains(got, "grpc-pty-ok") {
		fmt.Fprintf(os.Stderr, "FAIL PTY: expected \"grpc-pty-ok\" in PTY output; got: %q\n", got)
		os.Exit(1)
	}
	fmt.Printf("PASS PTY: \"grpc-pty-ok\" echoed in PTY output\n")

	// A shell that exits cleanly returns 0. Exit code -1 means we never saw an
	// exit message, which is also a failure.
	if exitCode != 0 {
		fmt.Fprintf(os.Stderr, "FAIL PTY: exit_code=%d want 0\n", exitCode)
		os.Exit(1)
	}
	fmt.Printf("PASS PTY: exit_code=0\n")
	fmt.Println("PASS PTY: resize frame accepted without error")

	fmt.Println("================================")
	fmt.Println("  gRPC interactive PTY proven: echo, resize, clean exit over vsock gRPC")
	fmt.Println("================================")
}
