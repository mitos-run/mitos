package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	connect "connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	v1 "mitos.run/mitos/api/v1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// execResult is the folded sandbox exec result: the exit code and the captured
// streams. It is the same shape the caller expects; the runtime call now rides
// the Connect sandbox.v1.Sandbox/ExecStream RPC.
type execResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// runExec resolves the sandbox's endpoint and per-sandbox bearer token, then
// runs the command over the sandbox HTTP API and streams the result to
// stdout/stderr. The exit code of the in-sandbox command becomes this process's
// exit code so scripts can branch on it.
func runExec(namespace, name string, cmd []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ref, endpoint, token, err := resolveSandboxAuth(ctx, c, namespace, name)
	if err != nil {
		return err
	}

	res, err := execSandbox(ctx, http.DefaultClient, endpoint, token, ref, strings.Join(cmd, " "))
	if err != nil {
		return err
	}
	if res.Stdout != "" {
		fmt.Fprint(os.Stdout, res.Stdout)
	}
	if res.Stderr != "" {
		fmt.Fprint(os.Stderr, res.Stderr)
	}
	if res.ExitCode != 0 {
		os.Exit(res.ExitCode)
	}
	return nil
}

// resolveSandboxAuth reads the Sandbox and its owned <sandbox>-sandbox-token
// Secret to recover the sandbox ref, the forkd HTTP endpoint, and the bearer
// token the sandbox API requires. The ref mirrors the SDK: Status.SandboxID,
// falling back to the sandbox name. A missing sandbox, a not-running sandbox,
// or a missing token Secret each returns a clear, actionable error rather than
// a hang.
func resolveSandboxAuth(ctx context.Context, c client.Client, namespace, name string) (ref, endpoint, token string, err error) {
	var sandbox v1.Sandbox
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", "", fmt.Errorf("sandbox %q not found in namespace %q", name, namespace)
		}
		return "", "", "", fmt.Errorf("get sandbox: %w", err)
	}
	if sandbox.Status.Phase != v1.SandboxReady {
		return "", "", "", fmt.Errorf("sandbox %q is %s, not Ready: exec needs a running sandbox", name, orUnknownPhase(sandbox.Status.Phase))
	}
	endpoint = sandbox.Status.Endpoint
	if endpoint == "" {
		return "", "", "", fmt.Errorf("sandbox %q has no endpoint yet: exec needs a running sandbox", name)
	}
	ref = sandbox.Status.SandboxID
	if ref == "" {
		ref = sandbox.Name
	}

	secretName := name + "-sandbox-token"
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", "", fmt.Errorf("token Secret %q not found: the sandbox API requires a bearer token; wait for the sandbox to go Ready or check the controller", secretName)
		}
		return "", "", "", fmt.Errorf("get token Secret %q: %w", secretName, err)
	}
	tokenBytes, ok := secret.Data["token"]
	if !ok || len(tokenBytes) == 0 {
		return "", "", "", fmt.Errorf("token Secret %q has no token key: cannot authenticate to the sandbox API", secretName)
	}
	// The token VALUE is the bearer credential. It is never logged or echoed.
	return ref, endpoint, string(tokenBytes), nil
}

// orUnknownPhase renders a SandboxPhase, or "Unknown" when empty, so the
// not-running error always names a phase.
func orUnknownPhase(p v1.SandboxPhase) string {
	if p == "" {
		return "Unknown"
	}
	return string(p)
}

// execSandbox runs command in the sandbox over the Connect
// sandbox.v1.Sandbox/ExecStream RPC (the HTTP/1.1-reachable non-interactive exec,
// the same RPC the Go SDK uses) and folds the streamed ExecResponse frames into a
// single execResult: stdout chunks and stderr chunks are accumulated and the
// terminal exit frame supplies the exit code. The per-sandbox bearer token and
// sandbox id ride on the Authorization and X-Sandbox-Id headers (the SAME gate
// the SDK uses; auth is never bypassed). A rejected token (Connect
// unauthenticated) maps to the same 401 message as before; any other failure is
// wrapped. The token value never appears in an error.
//
// httpc may be nil, in which case http.DefaultClient is used.
func execSandbox(ctx context.Context, httpc *http.Client, endpoint, token, ref, command string) (execResult, error) {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	cli := sandboxv1connect.NewSandboxClient(httpc, "http://"+endpoint)

	req := connect.NewRequest(&sandboxv1.ExecStreamRequest{Command: command})
	req.Header().Set("Authorization", "Bearer "+token)
	req.Header().Set("X-Sandbox-Id", ref)

	stream, err := cli.ExecStream(ctx, req)
	if err != nil {
		return execResult{}, execConnectError(endpoint, token, err)
	}
	defer func() { _ = stream.Close() }()

	var res execResult
	var stdout, stderr strings.Builder
	for stream.Receive() {
		msg := stream.Msg()
		if out := msg.GetStdout(); len(out) > 0 {
			stdout.Write(out)
		}
		if errOut := msg.GetStderr(); len(errOut) > 0 {
			stderr.Write(errOut)
		}
		if exit := msg.GetExit(); exit != nil {
			res.ExitCode = int(exit.GetExitCode())
		}
	}
	if err := stream.Err(); err != nil {
		return execResult{}, execConnectError(endpoint, token, err)
	}

	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	return res, nil
}

// execConnectError maps a Connect ExecStream failure to the user-facing message,
// preserving the legacy behavior: an unauthenticated code becomes the 401
// "rejected the bearer token" message, anything else is wrapped with the
// endpoint. The token value is redacted from any wrapped cause so it never
// reaches an error string a caller may log.
func execConnectError(endpoint, token string, err error) error {
	if connect.CodeOf(err) == connect.CodeUnauthenticated {
		return fmt.Errorf("sandbox API rejected the bearer token (401): the token Secret may be stale")
	}
	safe := err
	if token != "" {
		safe = errors.New(strings.ReplaceAll(err.Error(), token, "[REDACTED]"))
	}
	return fmt.Errorf("reach sandbox API at %s: %w (is the sandbox running and the endpoint routable?)", endpoint, safe)
}
