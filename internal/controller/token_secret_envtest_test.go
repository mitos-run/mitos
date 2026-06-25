package controller_test

// Envtest coverage for per-sandbox bearer tokens: a Ready claim (and a
// Ready fork) must produce an owned <name>-sandbox-token Secret whose token
// round-trips against the fake forkd's real HTTP sandbox API. The fake has
// no guest agent, so "auth passed" shows up as the 404 agent-missing error,
// never as a 401.

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func waitClaimReady(t *testing.T, name string) *v1.Sandbox {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1.SandboxReady {
				return &got
			}
			if got.Status.Phase == v1.SandboxFailed {
				t.Fatalf("claim failed: %+v", got.Status)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("claim %s did not become Ready within 15s", name)
	return nil
}

func waitTokenSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var s corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &s); err == nil {
			return &s
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("token secret %s not created within 10s", name)
	return nil
}

// execStatus runs an exec against the sandbox API at endpoint over the Connect
// sandbox.v1.Sandbox/ExecStream RPC (the runtime path forkd and the SDKs use;
// the legacy /v1/exec JSON route is retired, issue #358). The per-sandbox bearer
// token and sandbox id ride the Authorization and X-Sandbox-Id headers, the SAME
// gate the JSON route enforced. It returns the terminal Connect error code and
// the error string (empty on success). A rejected token surfaces as
// connect.CodeUnauthenticated, the Connect successor to the legacy 401; an
// authed exec against a mock-engine sandbox with no reachable guest agent
// surfaces as connect.CodeUnavailable, the proof that auth passed. The bearer
// token VALUE is never logged.
func execStatus(t *testing.T, endpoint, sandboxID, bearer string) (connect.Code, string) {
	t.Helper()
	cli := sandboxv1connect.NewSandboxClient(http.DefaultClient, "http://"+endpoint)
	req := connect.NewRequest(&sandboxv1.ExecStreamRequest{Command: "true"})
	if bearer != "" {
		req.Header().Set("Authorization", "Bearer "+bearer)
	}
	req.Header().Set("X-Sandbox-Id", sandboxID)

	stream, err := cli.ExecStream(context.Background(), req)
	if err != nil {
		return connect.CodeOf(err), err.Error()
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() {
		// Drain any frames; on the mock engine no agent answers, so the stream
		// terminates with an error below.
	}
	if err := stream.Err(); err != nil {
		return connect.CodeOf(err), err.Error()
	}
	return 0, ""
}

func assertHex64(t *testing.T, token string) {
	t.Helper()
	if len(token) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(token))
	}
	if _, err := hex.DecodeString(token); err != nil {
		t.Fatalf("token is not hex: %v", err)
	}
}

func TestClaimReadyCreatesOwnedTokenSecretAndGatesHTTP(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "tok-node-1", "tok-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "tok-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "tok-claim", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "tok-pool"}},
		},
	}
	for _, obj := range []client.Object{pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
	})

	got := waitClaimReady(t, "tok-claim")
	if got.Status.Endpoint == "" {
		t.Fatal("ready claim has empty endpoint")
	}

	c := newCoreClient(t)
	secret := waitTokenSecret(t, c, "tok-claim-sandbox-token")

	token := string(secret.Data["token"])
	assertHex64(t, token)
	if ep := string(secret.Data["endpoint"]); ep != got.Status.Endpoint {
		t.Fatalf("secret endpoint = %q, want %q", ep, got.Status.Endpoint)
	}

	owner := metav1.GetControllerOf(secret)
	if owner == nil || owner.Kind != "Sandbox" || owner.Name != "tok-claim" {
		t.Fatalf("secret controller owner = %+v, want Sandbox tok-claim", owner)
	}

	// Token never in status or conditions.
	for _, cond := range got.Status.Conditions {
		if cond.Message != "" && bytes.Contains([]byte(cond.Message), []byte(token)) {
			t.Fatal("token leaked into a condition message")
		}
	}

	// Round-trip against the fake forkd's real Connect handler. Without the
	// bearer: unauthenticated. With it: auth passes; on the mock engine the
	// sandbox has no reachable guest agent, so the proof that auth passed is a
	// CodeUnavailable (guest unreachable) rather than a CodeUnauthenticated.
	code, body := execStatus(t, got.Status.Endpoint, got.Status.SandboxID, "")
	if code != connect.CodeUnauthenticated {
		t.Fatalf("exec without token: code = %v, body = %s, want unauthenticated", code, body)
	}
	code, body = execStatus(t, got.Status.Endpoint, got.Status.SandboxID, "0000000000000000000000000000000000000000000000000000000000000000")
	if code != connect.CodeUnauthenticated {
		t.Fatalf("exec with wrong token: code = %v, body = %s, want unauthenticated", code, body)
	}
	code, body = execStatus(t, got.Status.Endpoint, got.Status.SandboxID, token)
	if code == connect.CodeUnauthenticated {
		t.Fatalf("exec with correct token: code = unauthenticated, body = %s, want auth to pass (a non-auth error on the mock engine)", body)
	}
	if code != connect.CodeUnavailable {
		t.Fatalf("exec with token: code = %v, body = %s, want unavailable (auth passed, no reachable guest on mock engine)", code, body)
	}
}

func TestForkReadyCreatesOwnedTokenSecret(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "tokf-node-1", "tokf-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "tokf-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "tokf-claim", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "tokf-pool"}},
		},
	}
	for _, obj := range []client.Object{pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
	})

	waitClaimReady(t, "tokf-claim")

	forkObj := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "tokf-fork", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "tokf-claim"}},
			Replicas: 1,
		},
	}
	if err := k8sClient.Create(ctx, forkObj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, forkObj) })

	var forkInfo *v1.SandboxChild
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tokf-fork", Namespace: "default"}, &got); err == nil {
			if got.Status.ReadyReplicas >= 1 && len(got.Status.Children) >= 1 {
				forkInfo = &got.Status.Children[0]
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if forkInfo == nil {
		t.Fatal("fork did not become ready within 15s")
	}

	c := newCoreClient(t)
	secret := waitTokenSecret(t, c, forkInfo.Name+"-sandbox-token")

	token := string(secret.Data["token"])
	assertHex64(t, token)
	if ep := string(secret.Data["endpoint"]); ep != forkInfo.Endpoint {
		t.Fatalf("secret endpoint = %q, want %q", ep, forkInfo.Endpoint)
	}

	owner := metav1.GetControllerOf(secret)
	if owner == nil || owner.Kind != "Sandbox" || owner.Name != "tokf-fork" {
		t.Fatalf("secret controller owner = %+v, want Sandbox tokf-fork", owner)
	}

	// The fork's own token gates its sandbox: unauthenticated without, auth
	// passing with. On the mock engine the sandbox has no reachable guest agent,
	// so the authed exec surfaces CodeUnavailable, proving auth passed.
	code, body := execStatus(t, forkInfo.Endpoint, forkInfo.SandboxID, "")
	if code != connect.CodeUnauthenticated {
		t.Fatalf("fork exec without token: code = %v, body = %s, want unauthenticated", code, body)
	}
	code, body = execStatus(t, forkInfo.Endpoint, forkInfo.SandboxID, token)
	if code == connect.CodeUnauthenticated || code != connect.CodeUnavailable {
		t.Fatalf("fork exec with token: code = %v, body = %s, want unavailable (auth passed, mock engine has no reachable guest)", code, body)
	}
}

// TestForkBearerTokenIsFreshlyReissuedNotInheritedFromParent proves the
// per-fork credential reissue property for the platform bearer token
// (fork-correctness row 3): a fork does NOT present its parent's bearer token,
// each fork's token is freshly minted and DISTINCT from the source claim's, so a
// fork cannot authenticate to the sandbox HTTP API as its parent. (Tenant Secret
// values are a different class governed by the default-deny inheritance gate;
// see TestLiveForkOfSecretHolderIsRejectedByDefault.)
func TestForkBearerTokenIsFreshlyReissuedNotInheritedFromParent(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "reissue-node-1", "reissue-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "reissue-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "reissue-claim", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "reissue-pool"}},
		},
	}
	for _, obj := range []client.Object{pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
	})

	waitClaimReady(t, "reissue-claim")
	c := newCoreClient(t)

	// The parent's bearer token.
	parentSecret := waitTokenSecret(t, c, "reissue-claim-sandbox-token")
	parentToken := string(parentSecret.Data["token"])
	assertHex64(t, parentToken)

	forkObj := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "reissue-fork", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "reissue-claim"}},
			Replicas: 1,
		},
	}
	if err := k8sClient.Create(ctx, forkObj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, forkObj) })

	var forkInfo *v1.SandboxChild
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "reissue-fork", Namespace: "default"}, &got); err == nil {
			if got.Status.ReadyReplicas >= 1 && len(got.Status.Children) >= 1 {
				forkInfo = &got.Status.Children[0]
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if forkInfo == nil {
		t.Fatal("fork did not become ready within 15s")
	}

	// The fork's bearer token: freshly minted and DISTINCT from the parent's.
	forkSecret := waitTokenSecret(t, c, forkInfo.Name+"-sandbox-token")
	forkToken := string(forkSecret.Data["token"])
	assertHex64(t, forkToken)

	if forkToken == parentToken {
		t.Fatal("fork inherited the parent's bearer token: a fork could authenticate to the sandbox API as its parent (per-fork credential reissue violated)")
	}
}
