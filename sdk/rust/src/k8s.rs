//! A minimal Kubernetes REST client over `ureq`, used only by cluster mode.
//!
//! This is deliberately thin: it resolves the API server URL, the trust anchor
//! (CA), and the credential (a bearer token or a client certificate / key) from
//! either the in-cluster service account or a kubeconfig file, then exposes
//! typed `get` / `list` / `post` / `delete` / `patch` against the namespaced
//! custom-resource REST paths
//! (`/apis/{group}/{version}/namespaces/{ns}/{plural}[/{name}]`).
//!
//! It does NOT pull in the official Kubernetes client or a second TLS stack: it
//! reuses the rustls that `ureq` already re-exports, trusting the cluster CA so
//! the self-signed API server certificate verifies. The bearer token is held in
//! memory only and is never logged.

use std::io::Cursor;
use std::sync::Arc;
use std::time::Duration;

use ureq::rustls::pki_types::{CertificateDer, PrivateKeyDer};
use ureq::rustls::{ClientConfig, RootCertStore};

use crate::error::MitosError;

const SA_DIR: &str = "/var/run/secrets/kubernetes.io/serviceaccount";
const ENV_K8S_HOST: &str = "KUBERNETES_SERVICE_HOST";
const ENV_K8S_PORT: &str = "KUBERNETES_SERVICE_PORT";
const ENV_KUBECONFIG: &str = "KUBECONFIG";

/// A bearer token or a client certificate / key pair. Held in memory; the token
/// VALUE is never logged.
enum Credential {
    None,
    Token(String),
    /// A PEM-encoded client certificate chain and private key, both as bytes.
    ClientCert(Vec<u8>, Vec<u8>),
}

/// The resolved connection details for the API server.
struct K8sConfig {
    /// The API server base URL, trailing slash trimmed.
    server: String,
    /// The PEM CA bundle to trust, when one is configured. When `None`, the
    /// webpki / OS roots are used (a public-CA-signed API server).
    ca_pem: Option<Vec<u8>>,
    credential: Credential,
    /// Skip TLS verification (kubeconfig `insecure-skip-tls-verify: true`). Off
    /// by default; honored only when the kubeconfig explicitly opts in.
    insecure: bool,
}

/// A thin Kubernetes REST client. Clone is cheap (the agent and token are
/// reference-counted / small).
#[derive(Clone)]
pub(crate) struct K8sClient {
    server: String,
    token: Option<String>,
    agent: ureq::Agent,
}

impl K8sClient {
    /// Builds a client from the in-cluster service account: the API server from
    /// `KUBERNETES_SERVICE_HOST` / `KUBERNETES_SERVICE_PORT`, the CA from the
    /// projected `ca.crt`, and the bearer token from the projected `token`.
    pub(crate) fn in_cluster() -> Result<Self, MitosError> {
        let host = nonempty_env(ENV_K8S_HOST).ok_or_else(|| {
            config_err(
                "not_in_cluster",
                format!("{ENV_K8S_HOST} is not set"),
                "in_cluster=true requires running inside a pod with a mounted service account. Use a kubeconfig outside the cluster.",
            )
        })?;
        let port = nonempty_env(ENV_K8S_PORT).unwrap_or_else(|| "443".to_string());
        let token = std::fs::read_to_string(format!("{SA_DIR}/token")).map_err(|e| {
            config_err(
                "service_account_unreadable",
                format!("reading the projected service account token failed: {e}"),
                "Confirm a service account is mounted and automountServiceAccountToken is not disabled.",
            )
        })?;
        let ca_pem = std::fs::read(format!("{SA_DIR}/ca.crt")).ok();
        let config = K8sConfig {
            server: format!("https://{host}:{port}"),
            ca_pem,
            credential: Credential::Token(token.trim().to_string()),
            insecure: false,
        };
        Self::from_config(config)
    }

    /// Builds a client from a kubeconfig file: explicit `path`, else
    /// `$KUBECONFIG`, else `$HOME/.kube/config`. Resolves the current context's
    /// cluster (server, CA) and user (token or client cert / key).
    pub(crate) fn from_kubeconfig(path: Option<&str>) -> Result<Self, MitosError> {
        let config = load_kubeconfig(path)?;
        Self::from_config(config)
    }

    /// Builds a client pointed at a plain `http://` server with no custom TLS,
    /// for the in-process mock API server in the integration tests. Not part of
    /// the public surface (gated behind `#[doc(hidden)]` on the caller).
    #[doc(hidden)]
    pub(crate) fn for_testing(server: &str, token: Option<String>) -> Self {
        let agent = ureq::AgentBuilder::new()
            .timeout(Duration::from_secs(30))
            .build();
        K8sClient {
            server: server.trim_end_matches('/').to_string(),
            token,
            agent,
        }
    }

    fn from_config(config: K8sConfig) -> Result<Self, MitosError> {
        let tls = build_tls_config(&config)?;
        let agent = ureq::AgentBuilder::new()
            .timeout(Duration::from_secs(30))
            .tls_config(Arc::new(tls))
            .build();
        let token = match config.credential {
            Credential::Token(t) if !t.is_empty() => Some(t),
            _ => None,
        };
        Ok(K8sClient {
            server: config.server.trim_end_matches('/').to_string(),
            token,
            agent,
        })
    }

    /// The REST path for a namespaced custom-resource collection or item.
    fn crd_path(
        &self,
        group: &str,
        version: &str,
        namespace: &str,
        plural: &str,
        name: Option<&str>,
    ) -> String {
        let base = format!(
            "{}/apis/{}/{}/namespaces/{}/{}",
            self.server, group, version, namespace, plural
        );
        match name {
            Some(n) => format!("{base}/{n}"),
            None => base,
        }
    }

    /// GETs a single namespaced custom object.
    pub(crate) fn get(
        &self,
        group: &str,
        version: &str,
        namespace: &str,
        plural: &str,
        name: &str,
    ) -> Result<serde_json::Value, MitosError> {
        let url = self.crd_path(group, version, namespace, plural, Some(name));
        let req = self.auth(self.agent.get(&url));
        self.send(req, None)
    }

    /// LISTs a namespaced custom-object collection, returning the raw list
    /// object (`{items: [...]}`).
    pub(crate) fn list(
        &self,
        group: &str,
        version: &str,
        namespace: &str,
        plural: &str,
    ) -> Result<serde_json::Value, MitosError> {
        let url = self.crd_path(group, version, namespace, plural, None);
        let req = self.auth(self.agent.get(&url));
        self.send(req, None)
    }

    /// POSTs a new namespaced custom object.
    pub(crate) fn create(
        &self,
        group: &str,
        version: &str,
        namespace: &str,
        plural: &str,
        body: &serde_json::Value,
    ) -> Result<serde_json::Value, MitosError> {
        let url = self.crd_path(group, version, namespace, plural, None);
        let req = self
            .auth(self.agent.post(&url))
            .set("Content-Type", "application/json");
        self.send(req, Some(body))
    }

    /// DELETEs a namespaced custom object.
    pub(crate) fn delete(
        &self,
        group: &str,
        version: &str,
        namespace: &str,
        plural: &str,
        name: &str,
    ) -> Result<serde_json::Value, MitosError> {
        let url = self.crd_path(group, version, namespace, plural, Some(name));
        let req = self.auth(self.agent.delete(&url));
        self.send(req, None)
    }

    /// Reads a namespaced Secret's `.data` map (base64-encoded values), used to
    /// fetch the per-sandbox bearer token Secret. Returns the Secret object.
    pub(crate) fn get_secret(
        &self,
        namespace: &str,
        name: &str,
    ) -> Result<serde_json::Value, MitosError> {
        let url = format!(
            "{}/api/v1/namespaces/{}/secrets/{}",
            self.server, namespace, name
        );
        let req = self.auth(self.agent.get(&url));
        self.send(req, None)
    }

    /// Attaches `Authorization: Bearer <token>` when a token is configured. The
    /// token VALUE is never logged. Client-certificate auth is carried by the
    /// TLS layer instead, so no header is added in that case.
    fn auth(&self, req: ureq::Request) -> ureq::Request {
        match &self.token {
            Some(t) if !t.is_empty() => req.set("Authorization", &format!("Bearer {t}")),
            _ => req,
        }
    }

    /// Sends the request and maps the outcome to a parsed body or a typed error.
    /// The bearer token is redacted from any error body before it surfaces.
    fn send(
        &self,
        req: ureq::Request,
        body: Option<&serde_json::Value>,
    ) -> Result<serde_json::Value, MitosError> {
        let result = match body {
            Some(b) => req.send_json(b.clone()),
            None => req.call(),
        };
        match result {
            Ok(resp) => parse_body(resp),
            Err(ureq::Error::Status(status, resp)) => {
                let text = resp.into_string().unwrap_or_default();
                Err(k8s_error(status, &text, self.token.as_deref()))
            }
            Err(ureq::Error::Transport(t)) => Err(MitosError::client(
                "transport_error",
                "the Kubernetes API request failed to reach the API server",
                redact(&t.to_string(), self.token.as_deref()),
                "Check the API server is reachable, the CA is correct, and the credential is valid.",
            )),
        }
    }
}

/// Reads a 2xx response body into JSON, returning `Value::Null` for an empty
/// body (a bare 200/204 such as DELETE with no payload).
fn parse_body(resp: ureq::Response) -> Result<serde_json::Value, MitosError> {
    let text = resp.into_string().map_err(|e| {
        MitosError::client(
            "response_read_error",
            "failed to read the Kubernetes API response body",
            e.to_string(),
            "Retry the request; if it persists, inspect the API server.",
        )
    })?;
    if text.trim().is_empty() {
        return Ok(serde_json::Value::Null);
    }
    serde_json::from_str(&text).map_err(|e| {
        MitosError::client(
            "decode_error",
            "failed to decode the Kubernetes API response",
            e.to_string(),
            "The API server returned a body that does not match the expected schema.",
        )
    })
}

/// Maps a non-2xx Kubernetes API response to a typed [`MitosError`], preferring
/// the Status object's `reason` and `message`. The token is redacted first.
pub(crate) fn k8s_error(status: u16, raw_body: &str, token: Option<&str>) -> MitosError {
    let body = redact(raw_body, token);
    let mut code = status_code(status).to_string();
    let mut cause = if body.trim().is_empty() {
        format!("HTTP {status}")
    } else {
        body.trim().to_string()
    };
    if let Ok(parsed) = serde_json::from_str::<serde_json::Value>(&body) {
        // A Kubernetes Status object: {kind:"Status", reason, message}.
        if let Some(reason) = parsed.get("reason").and_then(|v| v.as_str()) {
            if !reason.is_empty() {
                code = to_snake(reason);
            }
        }
        if let Some(message) = parsed.get("message").and_then(|v| v.as_str()) {
            if !message.is_empty() {
                cause = redact(message, token);
            }
        }
    }
    MitosError {
        code,
        message: format!("Kubernetes API request failed: HTTP {status}"),
        cause,
        remediation: status_remediation(status).to_string(),
        status,
    }
}

fn status_code(status: u16) -> &'static str {
    match status {
        400 => "bad_request",
        401 => "unauthorized",
        403 => "forbidden",
        404 => "not_found",
        409 => "conflict",
        422 => "invalid",
        429 => "rate_limited",
        500 => "internal_error",
        503 => "unavailable",
        s if s >= 500 => "server_error",
        _ => "request_failed",
    }
}

fn status_remediation(status: u16) -> &'static str {
    match status {
        401 | 403 => "Check the credential authorizes this namespace and resource (RBAC).",
        404 => "Confirm the object exists in this namespace and the CRDs are installed.",
        409 => "The object already exists or was modified concurrently.",
        s if s >= 500 => {
            "Retry the request; if it persists, inspect the API server and controller."
        }
        _ => "Inspect the request against the Kubernetes API contract.",
    }
}

/// Lowercases the first letter and inserts an underscore before each subsequent
/// uppercase letter, mapping a Kubernetes Status `reason` (for example
/// `AlreadyExists`, `NotFound`) to a stable snake_case error code.
fn to_snake(reason: &str) -> String {
    let mut out = String::with_capacity(reason.len() + 4);
    for (i, ch) in reason.chars().enumerate() {
        if ch.is_ascii_uppercase() {
            if i != 0 {
                out.push('_');
            }
            out.push(ch.to_ascii_lowercase());
        } else {
            out.push(ch);
        }
    }
    out
}

/// Replaces every occurrence of a non-empty token with `[REDACTED]`.
fn redact(text: &str, token: Option<&str>) -> String {
    match token {
        Some(t) if !t.is_empty() => text.replace(t, "[REDACTED]"),
        _ => text.to_string(),
    }
}

/// Builds a rustls `ClientConfig` trusting the cluster CA (when present),
/// optionally presenting a client certificate. Mirrors ureq's own use of the
/// ring provider so no process-global default provider need be installed.
fn build_tls_config(config: &K8sConfig) -> Result<ClientConfig, MitosError> {
    let builder =
        ClientConfig::builder_with_provider(ureq::rustls::crypto::ring::default_provider().into())
            .with_safe_default_protocol_versions()
            .map_err(|e| {
                config_err(
                    "tls_init_error",
                    format!("building the TLS configuration failed: {e}"),
                    "This is an internal error; please report it.",
                )
            })?;

    if config.insecure {
        // insecure-skip-tls-verify: true was set explicitly in the kubeconfig.
        // Client-certificate auth is not layered on in this mode (a rare
        // combination); a bearer token still rides through the header.
        let cfg = builder
            .dangerous()
            .with_custom_certificate_verifier(Arc::new(NoVerify))
            .with_no_client_auth();
        return Ok(cfg);
    }

    let mut roots = RootCertStore::empty();
    if let Some(ca_pem) = &config.ca_pem {
        let mut cursor = Cursor::new(ca_pem.as_slice());
        for cert in rustls_pemfile::certs(&mut cursor) {
            let cert = cert.map_err(|e| {
                config_err(
                    "ca_parse_error",
                    format!("parsing the cluster CA certificate failed: {e}"),
                    "Confirm the CA is a valid PEM certificate bundle.",
                )
            })?;
            roots.add(cert).map_err(|e| {
                config_err(
                    "ca_parse_error",
                    format!("adding the cluster CA certificate failed: {e}"),
                    "Confirm the CA is a valid certificate.",
                )
            })?;
        }
    } else {
        // No cluster CA: trust the bundled webpki roots (a public-CA-signed API
        // server). ureq re-exports webpki-roots through its TLS feature.
        roots.extend(webpki_roots_certs());
    }

    let cfg = builder.with_root_certificates(roots);
    apply_client_auth(config, cfg)
}

/// Loads the bundled webpki trust roots. ureq compiles webpki-roots in for its
/// default TLS, and we reuse it so no separate roots crate is added.
fn webpki_roots_certs() -> Vec<ureq::rustls::pki_types::TrustAnchor<'static>> {
    webpki_roots::TLS_SERVER_ROOTS.to_vec()
}

/// Applies a client certificate to a config built WITH root certificates (the
/// verifying path), or finishes with no client auth.
fn apply_client_auth(
    config: &K8sConfig,
    builder: ureq::rustls::ConfigBuilder<ClientConfig, ureq::rustls::client::WantsClientCert>,
) -> Result<ClientConfig, MitosError> {
    match &config.credential {
        Credential::ClientCert(cert_pem, key_pem) => {
            let (certs, key) = parse_client_cert(cert_pem, key_pem)?;
            builder.with_client_auth_cert(certs, key).map_err(|e| {
                config_err(
                    "client_cert_error",
                    format!("loading the client certificate failed: {e}"),
                    "Confirm the kubeconfig client-certificate-data and client-key-data are a valid pair.",
                )
            })
        }
        _ => Ok(builder.with_no_client_auth()),
    }
}

/// Parses a PEM client certificate chain and a PEM private key into rustls types.
fn parse_client_cert(
    cert_pem: &[u8],
    key_pem: &[u8],
) -> Result<(Vec<CertificateDer<'static>>, PrivateKeyDer<'static>), MitosError> {
    let mut cert_cursor = Cursor::new(cert_pem);
    let certs: Vec<CertificateDer<'static>> = rustls_pemfile::certs(&mut cert_cursor)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| {
            config_err(
                "client_cert_error",
                format!("parsing the client certificate failed: {e}"),
                "Confirm client-certificate-data is a valid PEM certificate.",
            )
        })?;
    if certs.is_empty() {
        return Err(config_err(
            "client_cert_error",
            "the client certificate PEM contained no certificates".to_string(),
            "Confirm client-certificate-data is a valid PEM certificate.",
        ));
    }
    let mut key_cursor = Cursor::new(key_pem);
    let key = rustls_pemfile::private_key(&mut key_cursor)
        .map_err(|e| {
            config_err(
                "client_cert_error",
                format!("parsing the client key failed: {e}"),
                "Confirm client-key-data is a valid PEM private key.",
            )
        })?
        .ok_or_else(|| {
            config_err(
                "client_cert_error",
                "the client key PEM contained no private key".to_string(),
                "Confirm client-key-data is a valid PEM private key.",
            )
        })?;
    Ok((certs, key))
}

/// A certificate verifier that accepts any server certificate, used only when a
/// kubeconfig sets `insecure-skip-tls-verify: true`. Off by default.
#[derive(Debug)]
struct NoVerify;

impl ureq::rustls::client::danger::ServerCertVerifier for NoVerify {
    fn verify_server_cert(
        &self,
        _end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ureq::rustls::pki_types::ServerName<'_>,
        _ocsp: &[u8],
        _now: ureq::rustls::pki_types::UnixTime,
    ) -> Result<ureq::rustls::client::danger::ServerCertVerified, ureq::rustls::Error> {
        Ok(ureq::rustls::client::danger::ServerCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        _message: &[u8],
        _cert: &CertificateDer<'_>,
        _dss: &ureq::rustls::DigitallySignedStruct,
    ) -> Result<ureq::rustls::client::danger::HandshakeSignatureValid, ureq::rustls::Error> {
        Ok(ureq::rustls::client::danger::HandshakeSignatureValid::assertion())
    }

    fn verify_tls13_signature(
        &self,
        _message: &[u8],
        _cert: &CertificateDer<'_>,
        _dss: &ureq::rustls::DigitallySignedStruct,
    ) -> Result<ureq::rustls::client::danger::HandshakeSignatureValid, ureq::rustls::Error> {
        Ok(ureq::rustls::client::danger::HandshakeSignatureValid::assertion())
    }

    fn supported_verify_schemes(&self) -> Vec<ureq::rustls::SignatureScheme> {
        ureq::rustls::crypto::ring::default_provider()
            .signature_verification_algorithms
            .supported_schemes()
    }
}

/// Loads and resolves a kubeconfig into the connection details.
fn load_kubeconfig(path: Option<&str>) -> Result<K8sConfig, MitosError> {
    let resolved = match path {
        Some(p) if !p.is_empty() => std::path::PathBuf::from(p),
        _ => match nonempty_env(ENV_KUBECONFIG) {
            // KUBECONFIG may be a list; the first entry wins (no merge here).
            Some(list) => {
                let first = list.split(':').next().unwrap_or("").to_string();
                std::path::PathBuf::from(first)
            }
            None => {
                let home = std::env::var("HOME").ok().filter(|h| !h.is_empty()).ok_or_else(|| {
                    config_err(
                        "kubeconfig_not_found",
                        "no kubeconfig path, $KUBECONFIG, or $HOME is set".to_string(),
                        "Pass a kubeconfig path, set KUBECONFIG, or run with a $HOME containing .kube/config.",
                    )
                })?;
                std::path::PathBuf::from(home).join(".kube").join("config")
            }
        },
    };
    let raw = std::fs::read_to_string(&resolved).map_err(|e| {
        config_err(
            "kubeconfig_unreadable",
            format!(
                "reading the kubeconfig at {} failed: {e}",
                resolved.display()
            ),
            "Confirm the kubeconfig path exists and is readable.",
        )
    })?;
    let doc: serde_yml::Value = serde_yml::from_str(&raw).map_err(|e| {
        config_err(
            "kubeconfig_parse_error",
            format!("parsing the kubeconfig YAML failed: {e}"),
            "Confirm the kubeconfig is valid YAML.",
        )
    })?;
    parse_kubeconfig(&doc, resolved.parent())
}

/// Resolves the current context's cluster and user from a parsed kubeconfig.
fn parse_kubeconfig(
    doc: &serde_yml::Value,
    base_dir: Option<&std::path::Path>,
) -> Result<K8sConfig, MitosError> {
    let current = doc
        .get("current-context")
        .and_then(|v| v.as_str())
        .ok_or_else(|| {
            config_err(
                "kubeconfig_no_context",
                "the kubeconfig has no current-context".to_string(),
                "Set a current-context with `kubectl config use-context`.",
            )
        })?;
    let context = find_named(doc, "contexts", current)
        .and_then(|c| c.get("context"))
        .ok_or_else(|| {
            config_err(
                "kubeconfig_no_context",
                format!("context {current:?} is not defined in the kubeconfig"),
                "Confirm the current-context names a defined context.",
            )
        })?;
    let cluster_name = context
        .get("cluster")
        .and_then(|v| v.as_str())
        .unwrap_or_default();
    let user_name = context
        .get("user")
        .and_then(|v| v.as_str())
        .unwrap_or_default();

    let cluster = find_named(doc, "clusters", cluster_name)
        .and_then(|c| c.get("cluster"))
        .ok_or_else(|| {
            config_err(
                "kubeconfig_no_cluster",
                format!("cluster {cluster_name:?} is not defined in the kubeconfig"),
                "Confirm the context references a defined cluster.",
            )
        })?;

    let server = cluster
        .get("server")
        .and_then(|v| v.as_str())
        .ok_or_else(|| {
            config_err(
                "kubeconfig_no_server",
                format!("cluster {cluster_name:?} has no server URL"),
                "Confirm the cluster entry has a server field.",
            )
        })?
        .trim_end_matches('/')
        .to_string();

    let insecure = cluster
        .get("insecure-skip-tls-verify")
        .and_then(|v| v.as_bool())
        .unwrap_or(false);

    let ca_pem = read_pem_field(
        cluster,
        "certificate-authority-data",
        "certificate-authority",
        base_dir,
    )?;

    let credential = resolve_user_credential(doc, user_name, base_dir)?;

    Ok(K8sConfig {
        server,
        ca_pem,
        credential,
        insecure,
    })
}

/// Resolves the user credential: a bearer token (inline or from a token file),
/// else a client certificate / key pair, else none.
fn resolve_user_credential(
    doc: &serde_yml::Value,
    user_name: &str,
    base_dir: Option<&std::path::Path>,
) -> Result<Credential, MitosError> {
    let user = match find_named(doc, "users", user_name).and_then(|u| u.get("user")) {
        Some(u) => u,
        None => return Ok(Credential::None),
    };

    if let Some(token) = user.get("token").and_then(|v| v.as_str()) {
        if !token.is_empty() {
            return Ok(Credential::Token(token.to_string()));
        }
    }
    if let Some(token_file) = user.get("tokenFile").and_then(|v| v.as_str()) {
        if let Ok(token) = std::fs::read_to_string(resolve_path(token_file, base_dir)) {
            return Ok(Credential::Token(token.trim().to_string()));
        }
    }

    let cert_pem = read_pem_field(
        user,
        "client-certificate-data",
        "client-certificate",
        base_dir,
    )?;
    let key_pem = read_pem_field(user, "client-key-data", "client-key", base_dir)?;
    if let (Some(cert), Some(key)) = (cert_pem, key_pem) {
        return Ok(Credential::ClientCert(cert, key));
    }

    Ok(Credential::None)
}

/// Reads a PEM value from a kubeconfig entry: the base64 `*-data` field if
/// present, else the file path field resolved relative to the kubeconfig dir.
fn read_pem_field(
    obj: &serde_yml::Value,
    data_key: &str,
    file_key: &str,
    base_dir: Option<&std::path::Path>,
) -> Result<Option<Vec<u8>>, MitosError> {
    if let Some(b64) = obj.get(data_key).and_then(|v| v.as_str()) {
        if !b64.is_empty() {
            let decoded = base64_decode(b64).ok_or_else(|| {
                config_err(
                    "kubeconfig_parse_error",
                    format!("{data_key} is not valid base64"),
                    "Confirm the kubeconfig base64 fields are intact.",
                )
            })?;
            return Ok(Some(decoded));
        }
    }
    if let Some(path) = obj.get(file_key).and_then(|v| v.as_str()) {
        if !path.is_empty() {
            let resolved = resolve_path(path, base_dir);
            let bytes = std::fs::read(&resolved).map_err(|e| {
                config_err(
                    "kubeconfig_file_unreadable",
                    format!(
                        "reading {} from the kubeconfig failed: {e}",
                        resolved.display()
                    ),
                    "Confirm the kubeconfig file references exist and are readable.",
                )
            })?;
            return Ok(Some(bytes));
        }
    }
    Ok(None)
}

/// Resolves a possibly-relative path against the kubeconfig's directory.
fn resolve_path(path: &str, base_dir: Option<&std::path::Path>) -> std::path::PathBuf {
    let p = std::path::Path::new(path);
    if p.is_absolute() {
        return p.to_path_buf();
    }
    match base_dir {
        Some(dir) => dir.join(p),
        None => p.to_path_buf(),
    }
}

/// Finds the entry named `name` in a kubeconfig top-level list
/// (`clusters` / `contexts` / `users`), each element being `{name, <kind>}`.
fn find_named<'a>(
    doc: &'a serde_yml::Value,
    list_key: &str,
    name: &str,
) -> Option<&'a serde_yml::Value> {
    doc.get(list_key)?
        .as_sequence()?
        .iter()
        .find(|e| e.get("name").and_then(|v| v.as_str()) == Some(name))
}

/// Decodes standard base64 (the encoding kubeconfig and Secret data use)
/// without adding a base64 crate.
pub(crate) fn base64_decode(input: &str) -> Option<Vec<u8>> {
    const INVALID: u8 = 0xFF;
    let mut table = [INVALID; 256];
    let alphabet = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    for (i, &c) in alphabet.iter().enumerate() {
        table[c as usize] = i as u8;
    }
    let mut out = Vec::with_capacity(input.len() / 4 * 3);
    let mut buf = 0u32;
    let mut bits = 0u32;
    for &byte in input.as_bytes() {
        if byte == b'=' || byte.is_ascii_whitespace() {
            continue;
        }
        let val = table[byte as usize];
        if val == INVALID {
            return None;
        }
        buf = (buf << 6) | val as u32;
        bits += 6;
        if bits >= 8 {
            bits -= 8;
            out.push((buf >> bits) as u8);
        }
    }
    Some(out)
}

/// Reads an environment variable, treating empty as unset.
fn nonempty_env(key: &str) -> Option<String> {
    std::env::var(key).ok().filter(|v| !v.is_empty())
}

/// Builds a typed configuration error (no HTTP round trip happened).
fn config_err(code: &str, cause: String, remediation: &str) -> MitosError {
    MitosError::client(
        code,
        "Kubernetes client configuration failed",
        cause,
        remediation,
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base64_decode_round_trips_known_vectors() {
        assert_eq!(base64_decode("aGVsbG8=").unwrap(), b"hello");
        assert_eq!(base64_decode("").unwrap(), b"");
        assert_eq!(base64_decode("Zm9vYmFy").unwrap(), b"foobar");
        // Whitespace (as Secret data sometimes wraps) is ignored.
        assert_eq!(base64_decode("aGVs\nbG8=").unwrap(), b"hello");
        assert!(base64_decode("not valid !!!").is_none());
    }

    #[test]
    fn to_snake_maps_status_reasons() {
        assert_eq!(to_snake("NotFound"), "not_found");
        assert_eq!(to_snake("AlreadyExists"), "already_exists");
        assert_eq!(to_snake("Conflict"), "conflict");
        assert_eq!(to_snake("Forbidden"), "forbidden");
    }

    #[test]
    fn k8s_error_prefers_status_reason_and_redacts_token() {
        let body = r#"{"kind":"Status","reason":"NotFound","message":"sandboxes.mitos.run \"x\" not found"}"#;
        let err = k8s_error(404, body, Some("secret-token"));
        assert_eq!(err.code, "not_found");
        assert_eq!(err.status, 404);
        assert!(err.cause.contains("not found"));

        let leaked = k8s_error(401, "token secret-token denied", Some("secret-token"));
        assert!(!leaked.cause.contains("secret-token"));
        assert!(leaked.cause.contains("[REDACTED]"));
    }
}
