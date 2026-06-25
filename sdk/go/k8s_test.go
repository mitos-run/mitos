package mitos

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// A self-signed CA PEM is not needed here: we only assert the kubeconfig parser
// resolves the current context's server and bearer token. TLS construction with
// system roots (no inline CA) is exercised by the absence of CA data.
func TestLoadKubeconfigBearerToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	// A minimal but realistic kubeconfig with two contexts; current-context
	// selects the second so the parser must pick by name, not position.
	kube := `apiVersion: v1
kind: Config
current-context: prod
clusters:
- name: dev
  cluster:
    server: https://dev.example:6443
- name: prod
  cluster:
    server: https://prod.example:6443
    insecure-skip-tls-verify: true
contexts:
- name: dev
  context:
    cluster: dev
    user: dev-user
- name: prod
  context:
    cluster: prod
    user: prod-user
    namespace: agents
users:
- name: dev-user
  user:
    token: dev-token
- name: prod-user
  user:
    token: prod-token
`
	if err := os.WriteFile(path, []byte(kube), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadKubeconfig(path)
	if err != nil {
		t.Fatalf("loadKubeconfig: %v", err)
	}
	if cfg.server != "https://prod.example:6443" {
		t.Errorf("server = %q, want https://prod.example:6443", cfg.server)
	}
	if cfg.token != "prod-token" {
		t.Errorf("token = %q, want prod-token", cfg.token)
	}
}

func TestLoadKubeconfigInlineCA(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	// A syntactically valid (if not chain-verifiable) CA PEM, base64-encoded as
	// certificate-authority-data, to exercise the CA-pool construction branch.
	caPEM := testCAPEM
	caB64 := base64.StdEncoding.EncodeToString([]byte(caPEM))
	kube := `apiVersion: v1
kind: Config
current-context: c
clusters:
- name: cl
  cluster:
    server: https://cl.example:6443
    certificate-authority-data: ` + caB64 + `
contexts:
- name: c
  context:
    cluster: cl
    user: u
users:
- name: u
  user:
    token: tok
`
	if err := os.WriteFile(path, []byte(kube), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadKubeconfig(path)
	if err != nil {
		t.Fatalf("loadKubeconfig with inline CA: %v", err)
	}
	if cfg.token != "tok" {
		t.Errorf("token = %q, want tok", cfg.token)
	}
}

func TestYAMLToJSONNested(t *testing.T) {
	src := `a: 1
b:
  c: hello
  d:
  - x
  - y
e: true
`
	out, err := yamlToJSON([]byte(src))
	if err != nil {
		t.Fatalf("yamlToJSON: %v", err)
	}
	got := string(out)
	// The exact key order is not guaranteed; assert the meaningful substrings.
	for _, want := range []string{`"a":1`, `"c":"hello"`, `"d":["x","y"]`, `"e":true`} {
		if !contains(got, want) {
			t.Errorf("yamlToJSON output %q missing %q", got, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// testCAPEM is a real, well-formed self-signed CA certificate (PEM) used only
// to exercise the x509 CA-pool construction branch. It is not trusted by
// anything and carries no private key.
const testCAPEM = `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdDgQiBCBeyQUFMRPjNqlfsVlf
WM7QdjuJD7nKCSqf+l2xeu1IpDAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----`
