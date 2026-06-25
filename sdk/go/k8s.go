package mitos

// Minimal Kubernetes REST client for cluster mode. The Go SDK stays
// dependency-free: direct mode (SandboxServer) is untouched and pulls nothing,
// and cluster mode here is implemented on the Go standard library alone
// (net/http, crypto/tls, crypto/x509, encoding/json, encoding/base64). It does
// NOT pull k8s.io/client-go, which is large and would drag a transitive
// dependency tree into every consumer of this module.
//
// The client speaks the custom-resource REST paths directly:
//
//	/apis/mitos.run/v1/namespaces/{ns}/{plural}            (list, create)
//	/apis/mitos.run/v1/namespaces/{ns}/{plural}/{name}     (get, delete, patch)
//
// and reads core Secrets at /api/v1/namespaces/{ns}/secrets/{name}. Auth is a
// bearer token; TLS trusts the cluster CA. The token VALUE is never logged.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// k8sAPIGroup, k8sAPIVersion are the CRD group/version the cluster client drives.
const (
	k8sAPIGroup   = "mitos.run"
	k8sAPIVersion = "v1"
)

// In-cluster service-account mount paths (the standard Kubernetes locations).
const (
	inClusterTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // path, not a secret value
	inClusterCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// k8sConfig is the resolved connection to the API server: the server URL, the
// bearer token, and an HTTP client whose TLS trusts the cluster CA. The token
// VALUE is held in memory only and is never logged.
type k8sConfig struct {
	server string
	token  string
	http   *http.Client
}

// k8sObject is a Kubernetes custom object as a generic JSON map, mirroring the
// untyped dict the Python and TypeScript SDKs pass around. Helpers below read
// nested spec/status/metadata fields defensively (a missing key yields a zero
// value, never a panic), matching the Python SDK's obj.get(...).
type k8sObject map[string]any

// k8sObjectList is the wire shape of a CRD list response.
type k8sObjectList struct {
	Items []k8sObject `json:"items"`
}

// loadInClusterConfig builds a k8sConfig from the in-cluster service-account
// mount: the API server from KUBERNETES_SERVICE_HOST/PORT, the bearer token
// from the projected token file, and the CA from the mounted ca.crt. It returns
// an actionable AgentRunError when run outside a cluster (the env vars are
// absent) so the failure names the fix rather than surfacing a bare file error.
func loadInClusterConfig() (*k8sConfig, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, &Error{
			Code:        "not_in_cluster",
			Message:     "in-cluster config requested but KUBERNETES_SERVICE_HOST/PORT are not set",
			Cause:       "the in-cluster service-account environment is not present",
			Remediation: "Run inside a Kubernetes pod, or use a kubeconfig with WithKubeconfig(path).",
		}
	}
	tokenBytes, err := os.ReadFile(inClusterTokenPath)
	if err != nil {
		return nil, &Error{
			Code:        "incluster_token_unreadable",
			Message:     "could not read the in-cluster service-account token",
			Cause:       err.Error(),
			Remediation: "Ensure the pod mounts a service-account token at " + inClusterTokenPath + ".",
		}
	}
	caBytes, err := os.ReadFile(inClusterCAPath)
	if err != nil {
		return nil, &Error{
			Code:        "incluster_ca_unreadable",
			Message:     "could not read the in-cluster CA certificate",
			Cause:       err.Error(),
			Remediation: "Ensure the pod mounts the cluster CA at " + inClusterCAPath + ".",
		}
	}
	httpClient, err := httpClientForCA(caBytes)
	if err != nil {
		return nil, err
	}
	server := "https://" + net.JoinHostPort(host, port)
	return &k8sConfig{
		server: server,
		token:  strings.TrimSpace(string(tokenBytes)),
		http:   httpClient,
	}, nil
}

// httpClientForCA builds an *http.Client whose TLS root pool trusts caPEM. When
// caPEM is empty the client uses the system roots (a kubeconfig may carry no
// inline CA). A CA that fails to parse is a typed error so the misconfiguration
// is legible.
func httpClientForCA(caPEM []byte) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, &Error{
				Code:        "ca_parse_failed",
				Message:     "the cluster CA certificate could not be parsed",
				Cause:       "AppendCertsFromPEM rejected the CA PEM",
				Remediation: "Check the CA certificate is valid PEM (the kubeconfig certificate-authority-data or the mounted ca.crt).",
			}
		}
		tlsConfig.RootCAs = pool
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}, nil
}

// k8sClient drives the CRD and Secret REST endpoints for one namespace's worth
// of cluster-mode calls. It is the Go analogue of the Python CustomObjectsApi +
// CoreV1Api pair and the TypeScript K8sApi seam.
type k8sClient struct {
	cfg *k8sConfig
}

// newK8sClient builds a client over a resolved config.
func newK8sClient(cfg *k8sConfig) *k8sClient {
	return &k8sClient{cfg: cfg}
}

// crdPath builds the REST path for a namespaced custom resource. When name is
// empty it is the collection path (list, create); otherwise the item path (get,
// delete, patch).
func crdPath(namespace, plural, name string) string {
	base := fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s", k8sAPIGroup, k8sAPIVersion, namespace, plural)
	if name == "" {
		return base
	}
	return base + "/" + name
}

// secretPath builds the core Secret item path.
func secretPath(namespace, name string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", namespace, name)
}

// getObject GETs a single custom object. A 404 surfaces as an *Error with
// Status 404 so callers (the lazy default-pool path, reconnect-by-name) can tell
// absent from a real failure.
func (c *k8sClient) getObject(ctx context.Context, namespace, plural, name string) (k8sObject, error) {
	var obj k8sObject
	if err := c.do(ctx, http.MethodGet, crdPath(namespace, plural, name), "", nil, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// listObjects GETs the collection of custom objects.
func (c *k8sClient) listObjects(ctx context.Context, namespace, plural string) (*k8sObjectList, error) {
	var list k8sObjectList
	if err := c.do(ctx, http.MethodGet, crdPath(namespace, plural, ""), "", nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// createObject POSTs a custom object to the collection.
func (c *k8sClient) createObject(ctx context.Context, namespace, plural string, body k8sObject) (k8sObject, error) {
	var out k8sObject
	if err := c.do(ctx, http.MethodPost, crdPath(namespace, plural, ""), "application/json", body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// deleteObject DELETEs a single custom object.
func (c *k8sClient) deleteObject(ctx context.Context, namespace, plural, name string) error {
	return c.do(ctx, http.MethodDelete, crdPath(namespace, plural, name), "", nil, nil)
}

// readSecret reads a core Secret and returns its data decoded to UTF-8 strings
// keyed by the Secret key. A 404 surfaces as an *Error with Status 404 so a
// caller can tolerate a missing token Secret. Secret VALUES are held in memory
// only and are NEVER logged.
func (c *k8sClient) readSecret(ctx context.Context, namespace, name string) (map[string]string, error) {
	var obj struct {
		Data map[string]string `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, secretPath(namespace, name), "", nil, &obj); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(obj.Data))
	for k, v := range obj.Data {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			// A non-base64 Secret value is unexpected; skip it rather than
			// surfacing the raw value (which could be a secret) in an error.
			continue
		}
		out[k] = string(decoded)
	}
	return out, nil
}

// do issues a Kubernetes API request with the bearer token and decodes a 2xx
// JSON body into out (when non-nil). A non-2xx response is parsed into a typed
// *Error carrying the Kubernetes Status reason/code; the bearer token is never
// placed in an error. contentType, when set, is sent for a body-carrying request
// (application/json for create, application/merge-patch+json for patch).
func (c *k8sClient) do(ctx context.Context, method, path, contentType string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.server+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.token)
	}

	resp, err := c.cfg.http.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseK8sError(resp.StatusCode, respBody)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// k8sStatus is the Kubernetes Status object returned on an API error.
type k8sStatus struct {
	Message string `json:"message"`
	Reason  string `json:"reason"`
	Code    int    `json:"code"`
}

// parseK8sError turns a non-2xx Kubernetes response into a typed *Error. The
// HTTP status is preserved on Error.Status so callers can branch on 404 (absent)
// versus 409 (already exists). The token never appears in a Status body, so no
// redaction is needed here.
func parseK8sError(status int, body []byte) error {
	var st k8sStatus
	msg := strings.TrimSpace(string(body))
	if err := json.Unmarshal(body, &st); err == nil && (st.Message != "" || st.Reason != "") {
		message := st.Message
		if message == "" {
			message = st.Reason
		}
		return &Error{
			Code:        "k8s_" + k8sErrorCode(status, st.Reason),
			Message:     message,
			Cause:       fmt.Sprintf("kubernetes API returned %d %s", status, st.Reason),
			Remediation: k8sRemediation(status),
			Status:      status,
		}
	}
	return &Error{
		Code:    "k8s_http_error",
		Message: fmt.Sprintf("kubernetes API returned HTTP %d", status),
		Cause:   msg,
		Status:  status,
	}
}

// k8sErrorCode maps an HTTP status to a short, stable code suffix.
func k8sErrorCode(status int, reason string) string {
	switch status {
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusUnauthorized:
		return "unauthorized"
	}
	if reason != "" {
		return strings.ToLower(reason)
	}
	return "error"
}

// k8sRemediation returns an actionable hint for common API errors.
func k8sRemediation(status int) string {
	switch status {
	case http.StatusNotFound:
		return "Check the object name and namespace; create it first if it does not exist."
	case http.StatusForbidden, http.StatusUnauthorized:
		return "Check the service-account RBAC grants access to mitos.run resources in this namespace."
	}
	return ""
}

// statusOf returns the HTTP status carried by an *Error, or 0 when err is not an
// *Error. It lets the cluster logic test for 404/409 without string matching.
func statusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// kubeconfig is the minimal subset of a kubeconfig file the SDK parses. Only the
// current-context's cluster (server + CA) and user (token, or client cert/key)
// are read; the rest of the file is ignored. See loadKubeconfig for the
// supported subset.
type kubeconfig struct {
	CurrentContext string `json:"current-context" yaml:"current-context"`
	Clusters       []struct {
		Name    string `json:"name" yaml:"name"`
		Cluster struct {
			Server                   string `json:"server" yaml:"server"`
			CertificateAuthorityData string `json:"certificate-authority-data" yaml:"certificate-authority-data"`
			CertificateAuthority     string `json:"certificate-authority" yaml:"certificate-authority"`
			InsecureSkipTLSVerify    bool   `json:"insecure-skip-tls-verify" yaml:"insecure-skip-tls-verify"`
		} `json:"cluster" yaml:"cluster"`
	} `json:"clusters" yaml:"clusters"`
	Contexts []struct {
		Name    string `json:"name" yaml:"name"`
		Context struct {
			Cluster   string `json:"cluster" yaml:"cluster"`
			User      string `json:"user" yaml:"user"`
			Namespace string `json:"namespace" yaml:"namespace"`
		} `json:"context" yaml:"context"`
	} `json:"contexts" yaml:"contexts"`
	Users []struct {
		Name string `json:"name" yaml:"name"`
		User struct {
			Token                 string `json:"token" yaml:"token"`
			ClientCertificateData string `json:"client-certificate-data" yaml:"client-certificate-data"`
			ClientKeyData         string `json:"client-key-data" yaml:"client-key-data"`
		} `json:"user" yaml:"user"`
	} `json:"users" yaml:"users"`
}

// loadKubeconfig parses a kubeconfig file and resolves the current context into
// a k8sConfig. It supports the common subset:
//
//   - server URL and inline certificate-authority-data (base64 PEM), or a
//     certificate-authority file path, or system roots when neither is set;
//   - user auth by bearer token, or by inline client-certificate-data +
//     client-key-data (base64 PEM) for mutual-TLS clusters (kind, minikube).
//
// It does NOT support exec credential plugins, auth-provider blocks, or external
// command tokens. The file is parsed as YAML via a minimal embedded decoder
// (yamlToJSON) so the SDK keeps its zero third-party dependencies. The path
// defaults to $KUBECONFIG, then $HOME/.kube/config, when empty.
func loadKubeconfig(path string) (*k8sConfig, error) {
	if path == "" {
		path = os.Getenv("KUBECONFIG")
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			path = filepath.Join(home, ".kube", "config")
		}
	}
	if path == "" {
		return nil, &Error{
			Code:        "kubeconfig_not_found",
			Message:     "no kubeconfig path and no $HOME to derive ~/.kube/config",
			Cause:       "neither WithKubeconfig nor $KUBECONFIG nor $HOME is set",
			Remediation: "Pass WithKubeconfig(path), set $KUBECONFIG, or use WithInCluster() inside a pod.",
		}
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path supplied by the caller / their KUBECONFIG
	if err != nil {
		return nil, &Error{
			Code:        "kubeconfig_unreadable",
			Message:     "could not read the kubeconfig file",
			Cause:       err.Error(),
			Remediation: "Check the kubeconfig path is correct and readable: " + path,
		}
	}
	jsonBytes, err := yamlToJSON(raw)
	if err != nil {
		return nil, &Error{
			Code:        "kubeconfig_parse_failed",
			Message:     "could not parse the kubeconfig file",
			Cause:       err.Error(),
			Remediation: "The SDK parses a common kubeconfig subset (no exec/auth-provider plugins); check the file or run inside the cluster with WithInCluster().",
		}
	}
	var kc kubeconfig
	if err := json.Unmarshal(jsonBytes, &kc); err != nil {
		return nil, &Error{
			Code:        "kubeconfig_parse_failed",
			Message:     "could not decode the kubeconfig structure",
			Cause:       err.Error(),
			Remediation: "Check the kubeconfig has clusters, contexts, and users entries.",
		}
	}
	return kc.resolve(filepath.Dir(path))
}

// resolve turns a parsed kubeconfig into a connection for its current context.
// baseDir is the kubeconfig's directory, used to resolve a relative
// certificate-authority file path.
func (kc *kubeconfig) resolve(baseDir string) (*k8sConfig, error) {
	if kc.CurrentContext == "" {
		return nil, &Error{
			Code:        "kubeconfig_no_context",
			Message:     "the kubeconfig has no current-context",
			Cause:       "current-context is empty",
			Remediation: "Set a current context with kubectl config use-context <name>.",
		}
	}
	var clusterName, userName string
	for _, ctx := range kc.Contexts {
		if ctx.Name == kc.CurrentContext {
			clusterName = ctx.Context.Cluster
			userName = ctx.Context.User
			break
		}
	}
	if clusterName == "" {
		return nil, &Error{
			Code:        "kubeconfig_context_missing",
			Message:     "the current-context is not defined in the kubeconfig",
			Cause:       "no contexts entry matches current-context " + kc.CurrentContext,
			Remediation: "Check the kubeconfig contexts list includes " + kc.CurrentContext + ".",
		}
	}

	var server string
	var caPEM []byte
	for _, cl := range kc.Clusters {
		if cl.Name != clusterName {
			continue
		}
		server = cl.Cluster.Server
		switch {
		case cl.Cluster.CertificateAuthorityData != "":
			decoded, err := base64.StdEncoding.DecodeString(cl.Cluster.CertificateAuthorityData)
			if err != nil {
				return nil, &Error{
					Code:        "kubeconfig_ca_invalid",
					Message:     "certificate-authority-data is not valid base64",
					Cause:       err.Error(),
					Remediation: "Regenerate the kubeconfig or check the certificate-authority-data field.",
				}
			}
			caPEM = decoded
		case cl.Cluster.CertificateAuthority != "":
			p := cl.Cluster.CertificateAuthority
			if !filepath.IsAbs(p) {
				p = filepath.Join(baseDir, p)
			}
			b, err := os.ReadFile(p) //nolint:gosec // path from the user's own kubeconfig
			if err != nil {
				return nil, &Error{
					Code:        "kubeconfig_ca_unreadable",
					Message:     "could not read the certificate-authority file",
					Cause:       err.Error(),
					Remediation: "Check the certificate-authority path in the kubeconfig: " + p,
				}
			}
			caPEM = b
		}
		break
	}
	if server == "" {
		return nil, &Error{
			Code:        "kubeconfig_no_server",
			Message:     "the current cluster has no server URL",
			Cause:       "cluster " + clusterName + " has an empty server field",
			Remediation: "Check the kubeconfig clusters entry for " + clusterName + ".",
		}
	}

	httpClient, err := httpClientForCA(caPEM)
	if err != nil {
		return nil, err
	}

	var token string
	var clientCertPEM, clientKeyPEM []byte
	for _, u := range kc.Users {
		if u.Name != userName {
			continue
		}
		token = u.User.Token
		if u.User.ClientCertificateData != "" && u.User.ClientKeyData != "" {
			cert, errC := base64.StdEncoding.DecodeString(u.User.ClientCertificateData)
			key, errK := base64.StdEncoding.DecodeString(u.User.ClientKeyData)
			if errC == nil && errK == nil {
				clientCertPEM = cert
				clientKeyPEM = key
			}
		}
		break
	}

	if len(clientCertPEM) > 0 && len(clientKeyPEM) > 0 {
		cert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
		if err != nil {
			return nil, &Error{
				Code:        "kubeconfig_client_cert_invalid",
				Message:     "the kubeconfig client certificate or key is invalid",
				Cause:       err.Error(),
				Remediation: "Check client-certificate-data and client-key-data in the kubeconfig.",
			}
		}
		if tr, ok := httpClient.Transport.(*http.Transport); ok {
			tr.TLSClientConfig.Certificates = []tls.Certificate{cert}
		}
	}

	return &k8sConfig{server: strings.TrimRight(server, "/"), token: token, http: httpClient}, nil
}
