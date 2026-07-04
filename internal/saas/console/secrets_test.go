package console

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// --- Secrets (org-scoped, write-only) ---

// TestSecretCreateReturnsMetadataNotValue asserts the load-bearing custody rule:
// creating a secret returns only non-secret metadata (name, provider, mode,
// version, fingerprint) and NEVER echoes the value back. The raw value must not
// appear anywhere in the response body.
func TestSecretCreateReturnsMetadataNotValue(t *testing.T) {
	f := newFixture(t)
	const value = "sk-ant-super-secret-value"
	w := f.req(t, "POST", "/console/secrets", `{"name":"ANTHROPIC_API_KEY","value":"`+value+`"}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), value) {
		t.Fatalf("response body leaked the secret value: %s", w.Body.String())
	}
	var got SecretView
	decode(t, w, &got)
	if got.Name != "ANTHROPIC_API_KEY" {
		t.Errorf("name = %q, want ANTHROPIC_API_KEY", got.Name)
	}
	if got.OrgID != f.aliceOrg {
		t.Errorf("org_id = %q, want %q", got.OrgID, f.aliceOrg)
	}
	if got.Provider != "kube" {
		t.Errorf("provider = %q, want kube (default)", got.Provider)
	}
	if got.Version != 1 {
		t.Errorf("version = %d, want 1 on first create", got.Version)
	}
	if got.Fingerprint == "" || strings.Contains(got.Fingerprint, value) {
		t.Errorf("fingerprint = %q, want a non-sensitive digest", got.Fingerprint)
	}
}

// TestSecretRotateIncrementsVersionAndChangesFingerprint asserts a re-create
// (rotate) of the same name bumps the version and that the fingerprint tracks
// the value: same value → same fingerprint, different value → different one.
func TestSecretRotateIncrementsVersionAndChangesFingerprint(t *testing.T) {
	f := newFixture(t)
	w1 := f.req(t, "POST", "/console/secrets", `{"name":"TOKEN","value":"v1"}`, f.aliceAcct, f.aliceOrg)
	var s1 SecretView
	decode(t, w1, &s1)
	w2 := f.req(t, "POST", "/console/secrets", `{"name":"TOKEN","value":"v2"}`, f.aliceAcct, f.aliceOrg)
	var s2 SecretView
	decode(t, w2, &s2)
	if s2.Version != 2 {
		t.Errorf("rotate version = %d, want 2", s2.Version)
	}
	if s1.Fingerprint == s2.Fingerprint {
		t.Errorf("fingerprint did not change on value change: %q", s1.Fingerprint)
	}
	w3 := f.req(t, "POST", "/console/secrets", `{"name":"TOKEN","value":"v2"}`, f.aliceAcct, f.aliceOrg)
	var s3 SecretView
	decode(t, w3, &s3)
	if s3.Fingerprint != s2.Fingerprint {
		t.Errorf("same value produced different fingerprints: %q vs %q", s2.Fingerprint, s3.Fingerprint)
	}
}

// TestSecretsListReturnsOnlyCallerOrg asserts cross-org isolation: a secret
// created by alice is never visible in bob's list.
func TestSecretsListReturnsOnlyCallerOrg(t *testing.T) {
	f := newFixture(t)
	f.req(t, "POST", "/console/secrets", `{"name":"ALICE_ONLY","value":"x"}`, f.aliceAcct, f.aliceOrg)

	wb := f.req(t, "GET", "/console/secrets", "", f.bobAcct, f.bobOrg)
	if wb.Code != http.StatusOK {
		t.Fatalf("bob list status = %d body=%s", wb.Code, wb.Body.String())
	}
	var resp struct {
		OrgID   string       `json:"org_id"`
		Secrets []SecretView `json:"secrets"`
	}
	decode(t, wb, &resp)
	if len(resp.Secrets) != 0 {
		t.Fatalf("bob saw %d secrets, want 0 (cross-org leak)", len(resp.Secrets))
	}

	wa := f.req(t, "GET", "/console/secrets", "", f.aliceAcct, f.aliceOrg)
	var aResp struct {
		Secrets []SecretView `json:"secrets"`
	}
	decode(t, wa, &aResp)
	if len(aResp.Secrets) != 1 || aResp.Secrets[0].Name != "ALICE_ONLY" {
		t.Fatalf("alice list = %+v, want exactly ALICE_ONLY", aResp.Secrets)
	}
}

// TestSecretDeleteCrossOrgIsNotFound asserts bob cannot delete alice's secret
// even knowing its name: the cross-org delete is reported as not-found and the
// secret survives.
func TestSecretDeleteCrossOrgIsNotFound(t *testing.T) {
	f := newFixture(t)
	f.req(t, "POST", "/console/secrets", `{"name":"SHARED_NAME","value":"x"}`, f.aliceAcct, f.aliceOrg)

	wd := f.req(t, "DELETE", "/console/secrets/SHARED_NAME", "", f.bobAcct, f.bobOrg)
	if wd.Code != http.StatusNotFound {
		t.Fatalf("bob delete status = %d, want 404", wd.Code)
	}
	// Alice's secret survives bob's attempt.
	wa := f.req(t, "GET", "/console/secrets", "", f.aliceAcct, f.aliceOrg)
	var aResp struct {
		Secrets []SecretView `json:"secrets"`
	}
	decode(t, wa, &aResp)
	if len(aResp.Secrets) != 1 {
		t.Fatalf("alice secret was deleted cross-org: %+v", aResp.Secrets)
	}
}

// TestSecretMutationAudited asserts every secret mutation emits a non-secret
// audit event (the value never appears in the audit detail).
func TestSecretMutationAudited(t *testing.T) {
	f := newFixture(t)
	const value = "top-secret"
	f.req(t, "POST", "/console/secrets", `{"name":"K","value":"`+value+`"}`, f.aliceAcct, f.aliceOrg)
	f.req(t, "DELETE", "/console/secrets/K", "", f.aliceAcct, f.aliceOrg)

	events, err := f.audit.List(context.Background(), f.aliceOrg, 0)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	var actions []string
	for _, ev := range events {
		actions = append(actions, ev.Action)
		if strings.Contains(ev.Detail, value) {
			t.Fatalf("audit detail leaked secret value: %q", ev.Detail)
		}
	}
	if !contains(actions, "secret.create") || !contains(actions, "secret.delete") {
		t.Fatalf("audit actions = %v, want secret.create and secret.delete", actions)
	}
	for _, ev := range events {
		if ev.TargetType != "secret" || ev.TargetName != "K" {
			t.Errorf("event %s: TargetType/TargetName = %q/%q, want secret/K", ev.Action, ev.TargetType, ev.TargetName)
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
