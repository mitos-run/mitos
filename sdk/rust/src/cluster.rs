//! Kubernetes cluster mode: the [`AgentRun`] surface.
//!
//! This is the operator path. Where direct mode (`SandboxServer`) talks to a
//! standalone or hosted REST server, cluster mode drives the declarative CRDs
//! (`SandboxPool`, `Sandbox`, `Workspace`) in the `mitos.run/v1` API group on a
//! Kubernetes cluster. It mirrors the Python SDK `AgentRun`
//! (`sdk/python/mitos/client.py`) and the TypeScript SDK, for parity.
//!
//! [`AgentRun`] is the entry point: construct it with a namespace and either an
//! in-cluster service account or a kubeconfig, then call `sandbox(image)` for
//! the lazy one-liner (get-or-create a default pool, then create a Sandbox), or
//! the explicit `create` / `get` / `list` / `pool_status` / workspace verbs.

use serde_json::{json, Value};

use crate::error::MitosError;
use crate::k8s::{base64_decode, K8sClient};

/// The CRD API group.
pub const API_GROUP: &str = "mitos.run";
/// The CRD API version.
pub const API_VERSION: &str = "v1";

const DEFAULT_POOL_PREFIX: &str = "mitos-default-";

/// The lifecycle phase a [`ClusterSandbox`] reports, mirroring the Sandbox
/// `status.phase`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SandboxPhase {
    /// The sandbox is being scheduled or activated.
    Pending,
    /// The sandbox is restoring from a snapshot.
    Restoring,
    /// The sandbox is Ready: it has an endpoint and accepts exec / file calls.
    Ready,
    /// The sandbox is terminating.
    Terminating,
    /// The sandbox failed.
    Failed,
}

impl SandboxPhase {
    /// Parses the CRD `status.phase` string, defaulting to `Pending` for an
    /// unknown or absent value.
    fn parse(s: &str) -> Self {
        match s {
            "Restoring" => SandboxPhase::Restoring,
            "Ready" => SandboxPhase::Ready,
            "Terminating" => SandboxPhase::Terminating,
            "Failed" => SandboxPhase::Failed,
            _ => SandboxPhase::Pending,
        }
    }
}

/// The status of a `SandboxPool`, returned by [`AgentRun::pool_status`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PoolStatus {
    /// The pool name.
    pub name: String,
    /// The number of warm, ready-to-fork snapshots.
    pub ready_snapshots: i64,
    /// The desired replica count from the spec.
    pub desired: i64,
    /// Ready snapshots per node, keyed by node name.
    pub node_distribution: Vec<(String, i64)>,
}

/// Derives a deterministic default-pool name for an image. Kept byte-for-byte
/// equivalent to the Python `default_pool_name` and the TypeScript
/// `defaultPoolName`: the image is lowercased, `/` and `:` become `-`, any other
/// run of characters outside `[a-z0-9.-]` collapses to a single `-`, the slug is
/// bounded to the first 40 characters, then leading and trailing `-` and `.` are
/// stripped (truncation must never leave a name ending in `.` or `-`), and the
/// `mitos-default-` prefix is prepended.
pub fn default_pool_name(image: &str) -> String {
    // Lowercase, then "/" and ":" -> "-".
    let lowered: String = image
        .chars()
        .flat_map(|c| c.to_lowercase())
        .map(|c| if c == '/' || c == ':' { '-' } else { c })
        .collect();

    // Collapse every run of characters NOT in [a-z0-9.-] to a single "-".
    let mut collapsed = String::with_capacity(lowered.len());
    let mut in_run = false;
    for c in lowered.chars() {
        let safe = c.is_ascii_lowercase() || c.is_ascii_digit() || c == '.' || c == '-';
        if safe {
            collapsed.push(c);
            in_run = false;
        } else if !in_run {
            collapsed.push('-');
            in_run = true;
        }
    }

    // Bound to the first 40 chars (bytes; the slug is ASCII at this point),
    // THEN strip leading / trailing "-" and ".".
    let bounded: String = collapsed.chars().take(40).collect();
    let slug = bounded.trim_matches(|c| c == '-' || c == '.');

    format!("{DEFAULT_POOL_PREFIX}{slug}")
}

/// The Kubernetes cluster-mode client: the operator-path entry point.
///
/// Construct it with [`AgentRun::in_cluster`] (a mounted service account) or
/// [`AgentRun::from_kubeconfig`] (a kubeconfig file), then drive the CRDs.
#[derive(Clone)]
pub struct AgentRun {
    client: K8sClient,
    namespace: String,
    allow_default_pool: bool,
}

impl AgentRun {
    /// Builds a client from the in-cluster service account (a mounted pod
    /// service account: the API server from the environment, the CA and token
    /// from the projected files). Default pools are allowed.
    pub fn in_cluster(namespace: impl Into<String>) -> Result<Self, MitosError> {
        Ok(AgentRun {
            client: K8sClient::in_cluster()?,
            namespace: namespace.into(),
            allow_default_pool: true,
        })
    }

    /// Builds a client from a kubeconfig file: explicit `path`, else
    /// `$KUBECONFIG`, else `$HOME/.kube/config`. Resolves the current context's
    /// server, CA, and credential. Default pools are allowed.
    pub fn from_kubeconfig(
        namespace: impl Into<String>,
        path: Option<&str>,
    ) -> Result<Self, MitosError> {
        Ok(AgentRun {
            client: K8sClient::from_kubeconfig(path)?,
            namespace: namespace.into(),
            allow_default_pool: true,
        })
    }

    /// Builds a client pointed at a plain `http://` API server, for the
    /// in-process mock server in the integration tests. Not part of the public
    /// surface.
    #[doc(hidden)]
    pub fn for_testing(server: &str, namespace: impl Into<String>) -> Self {
        AgentRun {
            client: K8sClient::for_testing(server, None),
            namespace: namespace.into(),
            allow_default_pool: true,
        }
    }

    /// Disables the lazy default-pool path on this client. When off,
    /// [`AgentRun::sandbox`] with an image (and no explicit pool) is rejected
    /// rather than creating a pool. Mirrors the Python `allow_default_pool`.
    pub fn allow_default_pool(mut self, allow: bool) -> Self {
        self.allow_default_pool = allow;
        self
    }

    /// The namespace this client targets.
    pub fn namespace(&self) -> &str {
        &self.namespace
    }

    /// The one-liner entry point. Ensures a default pool named
    /// `mitos-default-<image-slug>` exists (creating it with an inline template
    /// if absent and allowed), then creates a `Sandbox` from it. For the
    /// explicit path that never creates anything, use [`AgentRun::create`].
    pub fn sandbox(&self, image: &str) -> Result<ClusterSandbox, MitosError> {
        if !self.allow_default_pool {
            return Err(MitosError::client(
                "no_default_pool",
                "default pools are disabled on this client",
                "the client was built with allow_default_pool(false)",
                "Pass an existing pool to create(), or enable default pools.",
            ));
        }
        let pool = self.ensure_default_pool(image)?;
        self.create(&pool, CreateSandbox::default())
    }

    /// get-or-create the default `SandboxPool` for an image, returning the pool
    /// name. A pre-existing pool is reused (after verifying its inline image
    /// matches, guarding against a slug collision); a missing one is created
    /// with an inline `spec.template.image` and `replicas: 1`.
    fn ensure_default_pool(&self, image: &str) -> Result<String, MitosError> {
        let name = default_pool_name(image);
        match self.client.get(
            API_GROUP,
            API_VERSION,
            &self.namespace,
            "sandboxpools",
            &name,
        ) {
            Ok(existing) => {
                self.verify_pool_image(&existing, &name, image)?;
                return Ok(name);
            }
            Err(e) if e.status == 404 => {} // create below
            Err(e) => return Err(e),
        }

        let pool = json!({
            "apiVersion": format!("{API_GROUP}/{API_VERSION}"),
            "kind": "SandboxPool",
            "metadata": {"name": name, "namespace": self.namespace},
            "spec": {"template": {"image": image}, "replicas": 1},
        });
        self.create_or_reuse(&pool, "sandboxpools")?;
        Ok(name)
    }

    /// Guards default-pool reuse against a slug collision serving the wrong
    /// image: reads the inline `spec.template.image` and compares it. A missing
    /// or mismatched image fails closed rather than silently running the wrong
    /// image. Mirrors the Python `_verify_pool_image`.
    fn verify_pool_image(&self, pool: &Value, name: &str, image: &str) -> Result<(), MitosError> {
        let existing = pool
            .get("spec")
            .and_then(|s| s.get("template"))
            .and_then(|t| t.get("image"))
            .and_then(|v| v.as_str());
        match existing {
            None | Some("") => Err(MitosError::client(
                "pool_image_mismatch",
                format!("default pool {name} has no readable inline template image"),
                format!("pool {name} spec.template.image is absent or unreadable"),
                format!("Pass pool {name:?} explicitly to reuse this pool, or use a distinct image."),
            )),
            Some(found) if found != image => Err(MitosError::client(
                "pool_image_mismatch",
                format!("default pool {name} already exists for a different image"),
                format!("pool {name} runs image {found:?}, not the requested {image:?} (the image slug collides)"),
                format!("Pass pool {name:?} explicitly to reuse this pool, or use a distinct image."),
            )),
            _ => Ok(()),
        }
    }

    /// Creates a namespaced custom object, tolerating a 409 from a concurrent
    /// creator (the object is reused untouched). Mirrors `_create_or_reuse`.
    fn create_or_reuse(&self, body: &Value, plural: &str) -> Result<(), MitosError> {
        match self
            .client
            .create(API_GROUP, API_VERSION, &self.namespace, plural, body)
        {
            Ok(_) => Ok(()),
            Err(e) if e.status == 409 => Ok(()),
            Err(e) => Err(e),
        }
    }

    /// Creates a sandbox from a pool. `opts` carries the optional name, env,
    /// secrets, ttl, and workspace binding. Mirrors the Python `create`.
    pub fn create(&self, pool: &str, opts: CreateSandbox) -> Result<ClusterSandbox, MitosError> {
        let name = opts.name.unwrap_or_else(random_sandbox_name);

        let mut spec = json!({"source": {"poolRef": {"name": pool}}});
        if let Some(replicas) = opts.replicas {
            spec["replicas"] = json!(replicas);
        }
        if !opts.env.is_empty() {
            spec["env"] = Value::Array(
                opts.env
                    .iter()
                    .map(|(k, v)| json!({"name": k, "value": v}))
                    .collect(),
            );
        }
        if !opts.secrets.is_empty() {
            spec["secrets"] = Value::Array(
                opts.secrets
                    .iter()
                    .map(|(env_var, (secret_name, secret_key))| {
                        json!({
                            "name": env_var,
                            "secretRef": {"name": secret_name, "key": secret_key},
                            "envVar": env_var,
                        })
                    })
                    .collect(),
            );
        }
        if let Some(ttl) = &opts.ttl {
            spec["lifetime"] = json!({"ttl": ttl});
        }
        if let Some(ws) = &opts.workspace {
            spec["workspaceRef"] = json!({"name": ws});
        }

        let body = json!({
            "apiVersion": format!("{API_GROUP}/{API_VERSION}"),
            "kind": "Sandbox",
            "metadata": {"name": name, "namespace": self.namespace},
            "spec": spec,
        });
        self.client
            .create(API_GROUP, API_VERSION, &self.namespace, "sandboxes", &body)?;

        Ok(ClusterSandbox {
            name,
            namespace: self.namespace.clone(),
            pool: pool.to_string(),
            endpoint: None,
            phase: SandboxPhase::Pending,
            client: self.client.clone(),
        })
    }

    /// Reconnects to an existing sandbox by name, returning a live handle. Alias
    /// for [`AgentRun::get`], named for the reconnect use case.
    pub fn from_name(&self, name: &str) -> Result<ClusterSandbox, MitosError> {
        self.get(name)
    }

    /// Gets an existing sandbox by name. Reads the pool from
    /// `spec.source.poolRef.name` and the phase / endpoint from status; a Ready
    /// sandbox also loads its per-sandbox token.
    pub fn get(&self, name: &str) -> Result<ClusterSandbox, MitosError> {
        let obj = self
            .client
            .get(API_GROUP, API_VERSION, &self.namespace, "sandboxes", name)?;
        Ok(self.sandbox_from_object(name, &obj))
    }

    /// Lists sandboxes, optionally filtered by pool. Reads each one's pool from
    /// `spec.source.poolRef.name`.
    pub fn list(&self, pool: Option<&str>) -> Result<Vec<ClusterSandbox>, MitosError> {
        let list = self
            .client
            .list(API_GROUP, API_VERSION, &self.namespace, "sandboxes")?;
        let items = list
            .get("items")
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();
        let mut out = Vec::new();
        for obj in items {
            let obj_pool = pool_ref(&obj);
            if let Some(want) = pool {
                if obj_pool != want {
                    continue;
                }
            }
            let name = obj
                .get("metadata")
                .and_then(|m| m.get("name"))
                .and_then(|v| v.as_str())
                .unwrap_or_default()
                .to_string();
            out.push(self.sandbox_from_object(&name, &obj));
        }
        Ok(out)
    }

    /// Builds a [`ClusterSandbox`] handle from a fetched object, loading the
    /// token when the object is Ready.
    fn sandbox_from_object(&self, name: &str, obj: &Value) -> ClusterSandbox {
        let phase = SandboxPhase::parse(
            obj.get("status")
                .and_then(|s| s.get("phase"))
                .and_then(|v| v.as_str())
                .unwrap_or("Pending"),
        );
        let endpoint = obj
            .get("status")
            .and_then(|s| s.get("endpoint"))
            .and_then(|v| v.as_str())
            .map(|s| s.to_string());
        let mut sb = ClusterSandbox {
            name: name.to_string(),
            namespace: self.namespace.clone(),
            pool: pool_ref(obj),
            endpoint,
            phase,
            client: self.client.clone(),
        };
        if phase == SandboxPhase::Ready {
            // Best-effort: a missing token Secret leaves the sandbox tokenless.
            let _ = sb.load_token();
        }
        sb
    }

    /// Creates an empty durable `Workspace`.
    pub fn create_workspace(&self, name: &str) -> Result<Workspace, MitosError> {
        let body = json!({
            "apiVersion": format!("{API_GROUP}/{API_VERSION}"),
            "kind": "Workspace",
            "metadata": {"name": name, "namespace": self.namespace},
            "spec": {},
        });
        self.client
            .create(API_GROUP, API_VERSION, &self.namespace, "workspaces", &body)?;
        Ok(self.workspace(name))
    }

    /// A lazy handle to a workspace; it does not touch the cluster until a verb
    /// is called.
    pub fn workspace(&self, name: &str) -> Workspace {
        Workspace {
            name: name.to_string(),
            namespace: self.namespace.clone(),
            client: self.client.clone(),
            serve_wait_interval: DEFAULT_SERVE_WAIT_INTERVAL,
        }
    }

    /// Reconnects to an existing workspace, raising if it is absent.
    pub fn get_workspace(&self, name: &str) -> Result<Workspace, MitosError> {
        let ws = self.workspace(name);
        ws.get()?;
        Ok(ws)
    }

    /// Lists the workspaces in the client's namespace.
    pub fn list_workspaces(&self) -> Result<Vec<Workspace>, MitosError> {
        let list = self
            .client
            .list(API_GROUP, API_VERSION, &self.namespace, "workspaces")?;
        let items = list
            .get("items")
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();
        Ok(items
            .iter()
            .filter_map(|o| {
                o.get("metadata")
                    .and_then(|m| m.get("name"))
                    .and_then(|v| v.as_str())
                    .map(|n| self.workspace(n))
            })
            .collect())
    }

    /// Gets the status of a `SandboxPool`: ready snapshots, desired replicas, and
    /// the per-node distribution.
    pub fn pool_status(&self, name: &str) -> Result<PoolStatus, MitosError> {
        let obj = self.client.get(
            API_GROUP,
            API_VERSION,
            &self.namespace,
            "sandboxpools",
            name,
        )?;
        let status = obj.get("status").cloned().unwrap_or(Value::Null);
        let spec = obj.get("spec").cloned().unwrap_or(Value::Null);
        let ready = status
            .get("readySnapshots")
            .and_then(|v| v.as_i64())
            .unwrap_or(0);
        let desired = spec.get("replicas").and_then(|v| v.as_i64()).unwrap_or(0);
        let node_distribution = status
            .get("nodeDistribution")
            .and_then(|v| v.as_object())
            .map(|m| {
                m.iter()
                    .map(|(k, v)| (k.clone(), v.as_i64().unwrap_or(0)))
                    .collect()
            })
            .unwrap_or_default();
        Ok(PoolStatus {
            name: name.to_string(),
            ready_snapshots: ready,
            desired,
            node_distribution,
        })
    }
}

/// Optional fields for [`AgentRun::create`]. Built with the fluent setters; an
/// unset field is omitted from the Sandbox spec.
#[derive(Default, Clone)]
pub struct CreateSandbox {
    name: Option<String>,
    replicas: Option<i64>,
    env: Vec<(String, String)>,
    secrets: Vec<(String, (String, String))>,
    ttl: Option<String>,
    workspace: Option<String>,
}

impl CreateSandbox {
    /// Starts an empty option set (a generated name, no env / secrets / ttl).
    pub fn new() -> Self {
        Self::default()
    }

    /// Sets an explicit sandbox name. Generated when unset.
    pub fn name(mut self, name: impl Into<String>) -> Self {
        self.name = Some(name.into());
        self
    }

    /// Sets `spec.replicas` (the number of sandboxes to claim from the pool).
    pub fn replicas(mut self, n: i64) -> Self {
        self.replicas = Some(n);
        self
    }

    /// Injects an environment variable.
    pub fn env(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.env.push((key.into(), value.into()));
        self
    }

    /// Injects an environment variable sourced from a Secret key. `env_var` is
    /// the variable name; `secret_name` / `secret_key` locate the Secret value.
    pub fn secret(
        mut self,
        env_var: impl Into<String>,
        secret_name: impl Into<String>,
        secret_key: impl Into<String>,
    ) -> Self {
        self.secrets
            .push((env_var.into(), (secret_name.into(), secret_key.into())));
        self
    }

    /// Sets the maximum lifetime (`spec.lifetime.ttl`), for example `"30m"`.
    pub fn ttl(mut self, ttl: impl Into<String>) -> Self {
        self.ttl = Some(ttl.into());
        self
    }

    /// Binds the sandbox to a durable `Workspace` by name.
    pub fn workspace(mut self, name: impl Into<String>) -> Self {
        self.workspace = Some(name.into());
        self
    }
}

/// A cluster-mode sandbox handle, returned by the [`AgentRun`] verbs. It carries
/// the resolved pool, phase, endpoint, and (when Ready) the per-sandbox bearer
/// token held in memory. Mirrors the Python `Sandbox` cluster handle.
#[derive(Clone)]
pub struct ClusterSandbox {
    /// The sandbox name (its CRD object name).
    pub name: String,
    /// The namespace the sandbox lives in.
    pub namespace: String,
    /// The pool the sandbox was claimed from.
    pub pool: String,
    endpoint: Option<String>,
    phase: SandboxPhase,
    client: K8sClient,
}

impl ClusterSandbox {
    /// The sandbox lifecycle phase as last read from the cluster.
    pub fn phase(&self) -> SandboxPhase {
        self.phase
    }

    /// The sandbox API endpoint, when the cluster has reported one.
    pub fn endpoint(&self) -> Option<&str> {
        self.endpoint.as_deref()
    }

    /// Re-reads the sandbox from the cluster, refreshing the phase and endpoint
    /// (and loading the token when it becomes Ready).
    pub fn refresh(&mut self) -> Result<(), MitosError> {
        let obj = self.client.get(
            API_GROUP,
            API_VERSION,
            &self.namespace,
            "sandboxes",
            &self.name,
        )?;
        self.phase = SandboxPhase::parse(
            obj.get("status")
                .and_then(|s| s.get("phase"))
                .and_then(|v| v.as_str())
                .unwrap_or("Pending"),
        );
        self.endpoint = obj
            .get("status")
            .and_then(|s| s.get("endpoint"))
            .and_then(|v| v.as_str())
            .map(|s| s.to_string());
        if self.phase == SandboxPhase::Ready {
            let _ = self.load_token();
        }
        Ok(())
    }

    /// Reads the per-sandbox bearer token from the `<name>-sandbox-token`
    /// Secret, holding it in memory. A missing Secret is tolerated (the sandbox
    /// stays tokenless). The token VALUE is never logged. Returns the token, if
    /// any, so callers can use it directly (it is never returned in Debug).
    fn load_token(&mut self) -> Result<Option<String>, MitosError> {
        let secret_name = format!("{}-sandbox-token", self.name);
        let secret = match self.client.get_secret(&self.namespace, &secret_name) {
            Ok(s) => s,
            Err(e) if e.status == 404 => return Ok(None),
            Err(e) => return Err(e),
        };
        let token = secret
            .get("data")
            .and_then(|d| d.get("token"))
            .and_then(|v| v.as_str())
            .and_then(base64_decode)
            .and_then(|bytes| String::from_utf8(bytes).ok());
        Ok(token)
    }

    /// Terminates the sandbox, returning the bound workspace name (the new
    /// revision is then discoverable) or `None` when the sandbox is unbound.
    /// Mirrors the Python `terminate` minus the outputs / checkpoint patch.
    pub fn terminate(&mut self) -> Result<Option<String>, MitosError> {
        let obj = self.client.get(
            API_GROUP,
            API_VERSION,
            &self.namespace,
            "sandboxes",
            &self.name,
        )?;
        let ws_ref = obj
            .get("spec")
            .and_then(|s| s.get("workspaceRef"))
            .and_then(|w| w.get("name"))
            .and_then(|v| v.as_str())
            .map(|s| s.to_string());
        self.client.delete(
            API_GROUP,
            API_VERSION,
            &self.namespace,
            "sandboxes",
            &self.name,
        )?;
        self.phase = SandboxPhase::Terminating;
        Ok(ws_ref)
    }
}

impl std::fmt::Debug for ClusterSandbox {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // The per-sandbox token is never carried in Debug.
        f.debug_struct("ClusterSandbox")
            .field("name", &self.name)
            .field("namespace", &self.namespace)
            .field("pool", &self.pool)
            .field("phase", &self.phase)
            .field("endpoint", &self.endpoint)
            .finish()
    }
}

/// Options for [`Workspace::serve`]. Build with the fluent setters; unset fields
/// use documented defaults.
#[derive(Default, Clone)]
pub struct ServeOptions {
    /// The SandboxPool to claim a sandbox from. Required.
    pool: Option<String>,
    /// The guest TCP port to expose. Defaults to 8080. Must be 1-65535.
    port: Option<u16>,
    /// The access sharing tier: "private", "link", "org", "authenticated", or
    /// "public". Defaults to "private".
    sharing: Option<String>,
    /// An explicit single DNS label for the subdomain. When absent the created
    /// sandbox name is used. Lowercased before validation.
    label: Option<String>,
    /// The base expose domain, for example "mitos.app". When absent the
    /// `MITOS_EXPOSE_DOMAIN` environment variable is used.
    expose_domain: Option<String>,
}

impl ServeOptions {
    /// Starts a new option set with all fields at their defaults.
    pub fn new() -> Self {
        Self::default()
    }

    /// Sets the SandboxPool to claim from. Required.
    pub fn pool(mut self, pool: impl Into<String>) -> Self {
        self.pool = Some(pool.into());
        self
    }

    /// Sets the guest TCP port to expose. Defaults to 8080.
    pub fn port(mut self, port: u16) -> Self {
        self.port = Some(port);
        self
    }

    /// Sets the access sharing tier. Defaults to "private".
    pub fn sharing(mut self, sharing: impl Into<String>) -> Self {
        self.sharing = Some(sharing.into());
        self
    }

    /// Sets an explicit subdomain label. When absent the sandbox name is used.
    pub fn label(mut self, label: impl Into<String>) -> Self {
        self.label = Some(label.into());
        self
    }

    /// Sets the base expose domain. When absent `MITOS_EXPOSE_DOMAIN` is used.
    pub fn expose_domain(mut self, domain: impl Into<String>) -> Self {
        self.expose_domain = Some(domain.into());
        self
    }
}

/// The handle returned by [`Workspace::serve`]. Carries the public HTTPS URL
/// (the primary deliverable for issue #312, slice 5b) and the identity of the
/// backing sandbox.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ServedWorkspace {
    /// The public HTTPS URL: `https://<label>.<expose_domain>/`.
    pub url: String,
    /// The name of the Sandbox CRD that backs this serve session.
    pub sandbox_name: String,
    /// The single DNS label used in the URL subdomain.
    pub label: String,
    /// The effective access sharing tier.
    pub sharing: String,
}

/// Labels that are reserved by the Mitos control plane and may not be used by
/// tenants. Mirrors `internal/preview/route.go reservedLabels` and the Go SDK.
const RESERVED_EXPOSE_LABELS: &[&str] = &[
    "www", "app", "api", "console", "gateway", "admin", "auth", "login", "account", "mail",
    "static", "assets", "cdn", "status",
];

/// Validates a single DNS label and an expose domain, then builds and returns
/// the HTTPS URL. The label is lowercased before validation. Mirrors
/// `buildExposeURL` in the Go SDK (`sdk/go/serve.go`). The SDK must not import
/// `internal/`; this function is the SDK-local equivalent.
fn build_expose_url(label: &str, expose_domain: &str) -> Result<String, MitosError> {
    if expose_domain.is_empty() {
        return Err(MitosError::client(
            "missing_expose_domain",
            "expose domain is required",
            "no expose domain was provided and MITOS_EXPOSE_DOMAIN is not set",
            "Pass ServeOptions::expose_domain(domain) or set the MITOS_EXPOSE_DOMAIN environment variable.",
        ));
    }
    if label.is_empty() {
        return Err(MitosError::client(
            "invalid_expose_label",
            "expose label is required",
            "label is empty",
            "Pass ServeOptions::label(name) or use a sandbox name that is a valid single DNS label.",
        ));
    }
    if label.len() > 63 {
        return Err(MitosError::client(
            "invalid_expose_label",
            format!("expose label {:?} exceeds 63 characters", label),
            format!("label length {} > 63", label.len()),
            "Use a shorter label (at most 63 characters).",
        ));
    }
    // Must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ (already lowercased by caller).
    let valid = {
        let bytes = label.as_bytes();
        let first_ok = bytes[0].is_ascii_alphanumeric();
        let last_ok = bytes[bytes.len() - 1].is_ascii_alphanumeric();
        let middle_ok = bytes[1..bytes.len().saturating_sub(1)]
            .iter()
            .all(|&b| b.is_ascii_alphanumeric() || b == b'-');
        first_ok && last_ok && middle_ok
    };
    if !valid {
        return Err(MitosError::client(
            "invalid_expose_label",
            format!("expose label {:?} is not a valid single DNS label", label),
            "label must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$",
            "Use only lowercase letters, digits, and hyphens; do not start or end with a hyphen.",
        ));
    }
    if RESERVED_EXPOSE_LABELS.contains(&label) {
        return Err(MitosError::client(
            "reserved_expose_label",
            format!(
                "expose label {:?} is reserved and may not be used by tenants",
                label
            ),
            format!("label {:?} is in the reserved set", label),
            "Choose a different label that is not a well-known control-plane name.",
        ));
    }
    Ok(format!("https://{label}.{expose_domain}/"))
}

/// The default polling interval used while waiting for a serve sandbox to reach
/// Ready. Held per [`Workspace`] instance so parallel tests can each shrink it
/// without sharing mutable global state.
const DEFAULT_SERVE_WAIT_INTERVAL: std::time::Duration = std::time::Duration::from_millis(500);

/// A durable, forkable workspace handle. Lazy: it touches the cluster only when
/// a verb is called. Mirrors the Python `Workspace`.
#[derive(Clone)]
pub struct Workspace {
    /// The workspace name.
    pub name: String,
    /// The namespace the workspace lives in.
    pub namespace: String,
    client: K8sClient,
    /// The interval [`Workspace::serve`] sleeps between Ready polls. Per-instance
    /// (not a shared global) so parallel tests cannot race on it.
    serve_wait_interval: std::time::Duration,
}

impl Workspace {
    /// Reads the workspace object, mapping a 404 to a typed
    /// `workspace_not_found` error.
    fn get(&self) -> Result<Value, MitosError> {
        match self.client.get(
            API_GROUP,
            API_VERSION,
            &self.namespace,
            "workspaces",
            &self.name,
        ) {
            Ok(v) => Ok(v),
            Err(e) if e.status == 404 => Err(MitosError::client(
                "workspace_not_found",
                format!("workspace {} not found", self.name),
                e.cause,
                "Create it with AgentRun::create_workspace(name) first.",
            )),
            Err(e) => Err(e),
        }
    }

    /// The workspace head revision name (`status.head`), empty when unset.
    pub fn head(&self) -> Result<String, MitosError> {
        Ok(self
            .get()?
            .get("status")
            .and_then(|s| s.get("head"))
            .and_then(|v| v.as_str())
            .unwrap_or_default()
            .to_string())
    }

    /// Whether the workspace head is resumable (`status.resumable`).
    pub fn resumable(&self) -> Result<bool, MitosError> {
        Ok(self
            .get()?
            .get("status")
            .and_then(|s| s.get("resumable"))
            .and_then(|v| v.as_bool())
            .unwrap_or(false))
    }

    /// Overrides the Ready-poll interval for [`Workspace::serve`]. Not part of the
    /// public surface: the integration tests use it to drop the interval to zero
    /// so they do not sleep, with no shared mutable global between parallel tests.
    #[doc(hidden)]
    pub fn with_serve_wait_interval(mut self, interval: std::time::Duration) -> Self {
        self.serve_wait_interval = interval;
        self
    }

    /// Creates a workspace-bound Sandbox with `spec.expose` set, waits until it
    /// reaches Ready, then returns a [`ServedWorkspace`] carrying the public HTTPS
    /// URL (`https://<label>.<expose_domain>/`).
    ///
    /// The pool is required; all other options have documented defaults. Token
    /// minting is a follow-up: the per-sandbox bearer token is intentionally not
    /// set here; the proxy enforces the sharing tier independently.
    ///
    /// # Errors
    ///
    /// - `missing_serve_pool`: no pool was provided.
    /// - `invalid_serve_port`: port is 0.
    /// - `missing_expose_domain`: no domain was provided and `MITOS_EXPOSE_DOMAIN`
    ///   is not set.
    /// - `invalid_expose_label` / `reserved_expose_label`: the label fails
    ///   DNS validation or is in the reserved set.
    /// - `sandbox_failed`: the sandbox reached the Failed phase before Ready.
    /// - Any transport or Kubernetes API error from the underlying client.
    pub fn serve(&self, opts: ServeOptions) -> Result<ServedWorkspace, MitosError> {
        let pool = opts.pool.ok_or_else(|| {
            MitosError::client(
                "missing_serve_pool",
                "Workspace::serve needs a pool",
                "ServeOptions::pool was not provided",
                "Call ServeOptions::new().pool(name) to select the SandboxPool to claim from.",
            )
        })?;

        let port = opts.port.unwrap_or(8080);
        if port == 0 {
            return Err(MitosError::client(
                "invalid_serve_port",
                "serve port must be 1-65535",
                "port 0 is not a valid TCP port",
                "Pass ServeOptions::port(n) with a port in the range 1-65535.",
            ));
        }

        let sharing = opts.sharing.unwrap_or_else(|| "private".to_string());

        // Resolve the expose domain: option first, then env var.
        let expose_domain = opts
            .expose_domain
            .filter(|d| !d.is_empty())
            .or_else(|| {
                std::env::var("MITOS_EXPOSE_DOMAIN")
                    .ok()
                    .filter(|d| !d.is_empty())
            })
            .unwrap_or_default();

        // Generate a sandbox name now so it can serve as the default label.
        let sandbox_name = random_sandbox_name();

        // Determine the effective label: explicit option, else sandbox name.
        // Lowercase before validation to match Go SDK behavior.
        let label = opts
            .label
            .map(|l| l.to_lowercase())
            .unwrap_or_else(|| sandbox_name.clone());

        // Validate and build the URL before touching the cluster so a bad label
        // fails fast without leaving a partially configured sandbox.
        let url = build_expose_url(&label, &expose_domain)?;

        // POST the Sandbox CRD with spec.expose in the same body as
        // spec.source.poolRef and spec.workspaceRef. The JSON keys match the
        // api/v1 SandboxExpose shape: port (number), label (string), sharing
        // (string). Optional policy fields are omitted here.
        let body = json!({
            "apiVersion": format!("{API_GROUP}/{API_VERSION}"),
            "kind": "Sandbox",
            "metadata": {"name": sandbox_name, "namespace": self.namespace},
            "spec": {
                "source": {"poolRef": {"name": pool}},
                "workspaceRef": {"name": self.name},
                "expose": {
                    "port": port,
                    "label": label,
                    "sharing": sharing,
                },
            },
        });
        self.client
            .create(API_GROUP, API_VERSION, &self.namespace, "sandboxes", &body)?;

        // Poll until Ready or context-equivalent (loop bounded by the caller
        // dropping the reference; a real timeout should be set by the caller).
        self.wait_sandbox_ready(&sandbox_name)?;

        Ok(ServedWorkspace {
            url,
            sandbox_name,
            label,
            sharing,
        })
    }

    /// Polls the Sandbox until it reaches `Ready` or `Failed`. Returns
    /// immediately on `Ready`; returns a `sandbox_failed` error on `Failed`.
    /// Loops indefinitely on transient or unknown phases, sleeping
    /// `SERVE_WAIT_INTERVAL` between polls.
    fn wait_sandbox_ready(&self, sandbox_name: &str) -> Result<(), MitosError> {
        loop {
            let obj = self.client.get(
                API_GROUP,
                API_VERSION,
                &self.namespace,
                "sandboxes",
                sandbox_name,
            )?;
            let phase = SandboxPhase::parse(
                obj.get("status")
                    .and_then(|s| s.get("phase"))
                    .and_then(|v| v.as_str())
                    .unwrap_or("Pending"),
            );
            match phase {
                SandboxPhase::Ready => return Ok(()),
                SandboxPhase::Failed => {
                    return Err(MitosError::client(
                        "sandbox_failed",
                        format!("sandbox {sandbox_name} reached Failed phase"),
                        "the controller reported a Failed phase before Ready",
                        format!(
                            "Check the Sandbox status for more detail (kubectl describe sandbox {sandbox_name})."
                        ),
                    ))
                }
                _ => std::thread::sleep(self.serve_wait_interval),
            }
        }
    }
}

impl std::fmt::Debug for Workspace {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Workspace")
            .field("name", &self.name)
            .field("namespace", &self.namespace)
            .finish()
    }
}

/// Reads `spec.source.poolRef.name` from a Sandbox object, empty when absent.
fn pool_ref(obj: &Value) -> String {
    obj.get("spec")
        .and_then(|s| s.get("source"))
        .and_then(|s| s.get("poolRef"))
        .and_then(|p| p.get("name"))
        .and_then(|v| v.as_str())
        .unwrap_or_default()
        .to_string()
}

/// Generates a `sandbox-<hex>` name (8 hex chars), matching the Python
/// `sandbox-<uuid4 hex[:8]>` convention.
fn random_sandbox_name() -> String {
    let mut buf = [0u8; 4];
    if getrandom::getrandom(&mut buf).is_err() {
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0)
            .to_le_bytes();
        buf.copy_from_slice(&nanos[..4]);
    }
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut s = String::from("sandbox-");
    for &b in &buf {
        s.push(HEX[(b >> 4) as usize] as char);
        s.push(HEX[(b & 0x0f) as usize] as char);
    }
    s
}

#[cfg(test)]
mod tests {
    use super::default_pool_name;

    #[test]
    fn default_pool_name_matches_python_vectors() {
        assert_eq!(
            default_pool_name("python:3.12"),
            "mitos-default-python-3.12"
        );
        assert_eq!(
            default_pool_name("ghcr.io/Acme/Foo-Bar:latest"),
            "mitos-default-ghcr.io-acme-foo-bar-latest"
        );
        assert_eq!(default_pool_name("busybox"), "mitos-default-busybox");
        assert_eq!(
            default_pool_name("UPPER/Case:TAG"),
            "mitos-default-upper-case-tag"
        );
        assert_eq!(
            default_pool_name(&("a".repeat(60) + ":x")),
            format!("mitos-default-{}", "a".repeat(40))
        );
        assert_eq!(
            default_pool_name("registry.io/x@sha256:abc"),
            "mitos-default-registry.io-x-sha256-abc"
        );
        assert_eq!(default_pool_name("node_18"), "mitos-default-node-18");
    }
}
