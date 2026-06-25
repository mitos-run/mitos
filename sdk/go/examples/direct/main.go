// Direct-mode example: the standalone sandbox-server, no Kubernetes required.
//
// Run a sandbox-server (cmd/sandbox-server) and then:
//
//	go run ./sdk/go/examples/direct [base_url]
//
// It creates a template, forks a sandbox, runs a command two ways (buffered Exec
// and streaming ExecStream over the Connect runtime protocol, issue #24), checks
// the result, and terminates. The base URL comes from argv[1], else
// MITOS_BASE_URL, else http://localhost:8080. This example is executed end to end
// against a real KVM sandbox-server by the kvm-test sdk-go-example phase, so it
// is kept runnable, not just illustrative.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	mitos "github.com/mitos-run/mitos/sdk/go"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "example FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("go direct example OK")
}

func run() error {
	baseURL := "http://localhost:8080"
	if len(os.Args) > 1 {
		baseURL = os.Args[1]
	} else if env := os.Getenv("MITOS_BASE_URL"); env != "" {
		baseURL = env
	}
	ctx := context.Background()
	server := mitos.NewSandboxServer(mitos.WithBaseURL(baseURL))

	if _, err := server.CreateTemplate(ctx, "python-312"); err != nil {
		return fmt.Errorf("create template: %w", err)
	}
	sb, err := server.Fork(ctx, "python-312", "example-sandbox")
	if err != nil {
		return fmt.Errorf("fork: %w", err)
	}
	defer func() {
		if err := sb.Terminate(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "terminate:", err)
		} else {
			fmt.Println("sandbox terminated")
		}
	}()

	// Buffered Exec (runs over the Connect ExecStream RPC, then collected).
	res, err := sb.Exec(ctx, "echo hello from the sandbox")
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	fmt.Printf("exec exit=%d stdout=%q\n", res.ExitCode, res.Stdout)
	if res.ExitCode != 0 {
		return fmt.Errorf("exec returned exit %d", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hello from the sandbox") {
		return fmt.Errorf("unexpected stdout %q", res.Stdout)
	}

	// Streaming ExecStream: output arrives incrementally as it is produced.
	stream, err := sb.ExecStream(ctx, "for i in 1 2 3; do echo line $i; done")
	if err != nil {
		return fmt.Errorf("exec stream: %w", err)
	}
	defer func() { _ = stream.Close() }()
	var streamed strings.Builder
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("stream recv: %w", err)
		}
		streamed.Write(chunk.Stdout)
	}
	fmt.Printf("stream exit=%d lines=%q\n", stream.Result().ExitCode, streamed.String())
	if stream.Result().ExitCode != 0 {
		return fmt.Errorf("stream returned exit %d", stream.Result().ExitCode)
	}
	for _, want := range []string{"line 1", "line 2", "line 3"} {
		if !strings.Contains(streamed.String(), want) {
			return fmt.Errorf("streamed output missing %q: %q", want, streamed.String())
		}
	}
	return nil
}
