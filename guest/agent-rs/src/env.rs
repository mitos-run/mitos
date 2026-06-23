//! Guest environment merge: mirrors the Go guestenv.Merge precedence exactly.
//!
//! Precedence, lowest to highest: base < configured < request.
//! This matches internal/guestenv/merge.go line by line.
//!
//! Secret values are handled by callers; this module never logs any value.
//! Only key counts are observable from outside.

use std::collections::HashMap;
use tokio::sync::RwLock;

/// Configured environment state stored by the guest agent.
///
/// Populated by the Configure RPC (host-trusted channel) at claim time.
/// Secrets and plain env vars share the same map so the merge is uniform;
/// the caller (Configure handler) is responsible for not logging values.
///
/// The inner map is protected by a `tokio::sync::RwLock` so the type can be
/// shared as `Arc<ConfiguredEnv>` across async tasks without external locking.
/// `apply` takes a write lock; reads (`len`, `is_empty`, `snapshot`) take a
/// read lock.
#[derive(Debug, Default)]
pub struct ConfiguredEnv {
    /// The combined configured env plus secrets. Stored together so the merge
    /// treats them identically to plain vars. Key count may be logged; values
    /// must not be logged.
    vars: RwLock<HashMap<String, String>>,
}

impl ConfiguredEnv {
    /// Create a new, empty ConfiguredEnv.
    pub fn new() -> Self {
        Self::default()
    }

    /// Merge additional env and secret entries into the configured state.
    ///
    /// Later duplicates win (additive merge, matching handleConfigure in Go).
    /// Values are never logged; only the total key count after merge is safe to
    /// observe.
    ///
    /// Takes a write lock internally; `&self` allows sharing via `Arc<ConfiguredEnv>`.
    pub async fn apply(&self, env: HashMap<String, String>, secrets: HashMap<String, String>) {
        let mut guard = self.vars.write().await;
        for (k, v) in env {
            guard.insert(k, v);
        }
        for (k, v) in secrets {
            guard.insert(k, v);
        }
    }

    /// Return the number of configured keys. Safe to log.
    pub async fn len(&self) -> usize {
        self.vars.read().await.len()
    }

    /// Returns true when no configured vars are set.
    pub async fn is_empty(&self) -> bool {
        self.vars.read().await.is_empty()
    }

    /// Return a point-in-time snapshot of the configured key-value pairs.
    ///
    /// Returns owned `HashMap<String, String>` because the read lock cannot be
    /// held across `await` points or returned as a borrow.
    ///
    /// Values are secret; callers must not log them.
    pub async fn snapshot(&self) -> HashMap<String, String> {
        self.vars.read().await.clone()
    }
}

/// Merge a base environment (os::vars() / KEY=VALUE strings) with configured
/// (claim-time env + secrets) and per-request variables into a final env list.
///
/// Precedence, lowest to highest: base < configured < request. Later duplicates
/// win. This mirrors internal/guestenv/merge.go exactly:
///   - Base entries without '=' are passed through verbatim (not dropped).
///   - Configured entries override base.
///   - Request entries override configured and base.
///
/// The returned list is KEY=VALUE strings. Consumers that need (key, value)
/// pairs should call `split_once('=')` on each entry; a naive `split('=')`
/// truncates values that themselves contain '=' (such as base64 tokens or URLs).
///
/// Secret values (in configured or request) are never logged here. Callers
/// are responsible for not logging the output vector.
pub fn merge(
    base: &[String],
    configured_snapshot: &HashMap<String, String>,
    request: &HashMap<String, String>,
) -> Vec<String> {
    // Capacity hint: at most base.len() + configured.len() + request.len() entries.
    let cap = base.len() + configured_snapshot.len() + request.len();
    let mut merged: HashMap<String, String> = HashMap::with_capacity(cap);
    // order tracks insertion order for deterministic output, mirroring the Go
    // implementation which preserves the first-seen ordering of keys.
    let mut order: Vec<String> = Vec::with_capacity(cap);
    let mut verbatim: Vec<String> = Vec::new();

    let mut set = |k: String, v: String| {
        if !merged.contains_key(&k) {
            order.push(k.clone());
        }
        merged.insert(k, v);
    };

    // 1. Base: lowest precedence. Entries without '=' are verbatim pass-through.
    for kv in base {
        match kv.split_once('=') {
            Some((k, v)) => set(k.to_string(), v.to_string()),
            None => verbatim.push(kv.clone()),
        }
    }

    // 2. Configured: overrides base.
    for (k, v) in configured_snapshot {
        set(k.to_string(), v.to_string());
    }

    // 3. Request: highest precedence, overrides configured and base.
    for (k, v) in request {
        set(k.to_string(), v.to_string());
    }

    let mut out = Vec::with_capacity(verbatim.len() + order.len());
    out.extend(verbatim);
    for k in &order {
        if let Some(v) = merged.get(k) {
            out.push(format!("{k}={v}"));
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    // Helper: parse KEY=VALUE vec into a map for assertion without order dependence.
    fn to_map(env: &[String]) -> HashMap<String, String> {
        env.iter()
            .filter_map(|kv| kv.split_once('=').map(|(k, v)| (k.to_string(), v.to_string())))
            .collect()
    }

    #[tokio::test]
    async fn base_only_passes_through() {
        let base = vec!["FOO=1".to_string(), "BAR=2".to_string()];
        let configured = ConfiguredEnv::new();
        let request = HashMap::new();
        let out = merge(&base, &configured.snapshot().await, &request);
        let m = to_map(&out);
        assert_eq!(m["FOO"], "1");
        assert_eq!(m["BAR"], "2");
    }

    #[tokio::test]
    async fn configured_overrides_base() {
        let base = vec!["FOO=base".to_string()];
        let configured = ConfiguredEnv::new();
        configured
            .apply(
                [("FOO".to_string(), "configured".to_string())]
                    .into_iter()
                    .collect(),
                HashMap::new(),
            )
            .await;
        let request = HashMap::new();
        let out = merge(&base, &configured.snapshot().await, &request);
        let m = to_map(&out);
        assert_eq!(m["FOO"], "configured");
    }

    #[tokio::test]
    async fn request_overrides_configured_and_base() {
        let base = vec!["FOO=base".to_string()];
        let configured = ConfiguredEnv::new();
        configured
            .apply(
                [("FOO".to_string(), "configured".to_string())]
                    .into_iter()
                    .collect(),
                HashMap::new(),
            )
            .await;
        let request: HashMap<String, String> =
            [("FOO".to_string(), "request".to_string())].into_iter().collect();
        let out = merge(&base, &configured.snapshot().await, &request);
        let m = to_map(&out);
        assert_eq!(m["FOO"], "request");
    }

    #[tokio::test]
    async fn all_three_layers_merge_correctly() {
        // base: A=1 B=2
        // configured: B=cfg C=3
        // request: C=req D=4
        // expected: A=1 B=cfg C=req D=4
        let base = vec!["A=1".to_string(), "B=2".to_string()];
        let configured = ConfiguredEnv::new();
        configured
            .apply(
                [
                    ("B".to_string(), "cfg".to_string()),
                    ("C".to_string(), "3".to_string()),
                ]
                .into_iter()
                .collect(),
                HashMap::new(),
            )
            .await;
        let request: HashMap<String, String> = [
            ("C".to_string(), "req".to_string()),
            ("D".to_string(), "4".to_string()),
        ]
        .into_iter()
        .collect();
        let out = merge(&base, &configured.snapshot().await, &request);
        let m = to_map(&out);
        assert_eq!(m["A"], "1");
        assert_eq!(m["B"], "cfg");
        assert_eq!(m["C"], "req");
        assert_eq!(m["D"], "4");
        assert_eq!(m.len(), 4);
    }

    #[tokio::test]
    async fn base_entry_without_equals_passes_through_verbatim() {
        // Go guestenv.Merge passes through entries without '=' verbatim.
        let base = vec!["NOEQUALS".to_string(), "KEY=val".to_string()];
        let configured = ConfiguredEnv::new();
        let request = HashMap::new();
        let out = merge(&base, &configured.snapshot().await, &request);
        // The verbatim entry is in the output.
        assert!(out.contains(&"NOEQUALS".to_string()));
        let m = to_map(&out);
        assert_eq!(m["KEY"], "val");
    }

    #[tokio::test]
    async fn secrets_in_configured_override_base() {
        // Secrets go into configured.vars via apply(); they must override base.
        let base = vec!["SECRET_KEY=base_val".to_string()];
        let configured = ConfiguredEnv::new();
        configured
            .apply(
                HashMap::new(),
                [("SECRET_KEY".to_string(), "secret_val".to_string())]
                    .into_iter()
                    .collect(),
            )
            .await;
        let request = HashMap::new();
        let out = merge(&base, &configured.snapshot().await, &request);
        let m = to_map(&out);
        // The secret value overrides the base value.
        assert_eq!(m["SECRET_KEY"], "secret_val");
    }

    #[tokio::test]
    async fn configured_env_len_is_combined_count() {
        let configured = ConfiguredEnv::new();
        configured
            .apply(
                [("A".to_string(), "1".to_string())].into_iter().collect(),
                [("B".to_string(), "s".to_string())].into_iter().collect(),
            )
            .await;
        // One env var + one secret = 2 total entries.
        assert_eq!(configured.len().await, 2);
    }

    #[tokio::test]
    async fn empty_everything_returns_empty() {
        let out = merge(&[], &ConfiguredEnv::new().snapshot().await, &HashMap::new());
        assert!(out.is_empty());
    }

    #[tokio::test]
    async fn value_containing_equals_preserved_via_split_once() {
        // Values with embedded '=' (e.g. base64 tokens) must survive the merge.
        // merge() stores key and value separately, so the reconstituted KEY=VALUE
        // entry will contain the full original value including any '=' in it.
        let base = vec!["TOKEN=abc==".to_string()];
        let configured = ConfiguredEnv::new();
        let request = HashMap::new();
        let out = merge(&base, &configured.snapshot().await, &request);
        // Use split_once (not split) to recover the pair correctly.
        let entry = out.iter().find(|s| s.starts_with("TOKEN=")).unwrap();
        let (key, val) = entry.split_once('=').unwrap();
        assert_eq!(key, "TOKEN");
        assert_eq!(val, "abc==");
    }

    #[tokio::test]
    async fn concurrent_apply_and_snapshot_do_not_race() {
        use std::sync::Arc;
        let configured = Arc::new(ConfiguredEnv::new());
        let c1 = Arc::clone(&configured);
        let c2 = Arc::clone(&configured);
        let w1 = tokio::spawn(async move {
            c1.apply(
                [("X".to_string(), "1".to_string())].into_iter().collect(),
                HashMap::new(),
            )
            .await;
        });
        let w2 = tokio::spawn(async move {
            c2.apply(
                [("Y".to_string(), "2".to_string())].into_iter().collect(),
                HashMap::new(),
            )
            .await;
        });
        w1.await.unwrap();
        w2.await.unwrap();
        let snap = configured.snapshot().await;
        // Both writes must be visible; order of concurrent writes is unspecified.
        assert_eq!(snap.len(), 2);
        assert_eq!(snap["X"], "1");
        assert_eq!(snap["Y"], "2");
    }
}
