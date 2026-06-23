//! Guest environment merge: mirrors the Go guestenv.Merge precedence exactly.
//!
//! Precedence, lowest to highest: base < configured < request.
//! This matches internal/guestenv/merge.go line by line.
//!
//! Secret values are handled by callers; this module never logs any value.
//! Only key counts are observable from outside.

use std::collections::HashMap;

/// Configured environment state stored by the guest agent.
///
/// Populated by the Configure RPC (host-trusted channel) at claim time.
/// Secrets and plain env vars share the same map so the merge is uniform;
/// the caller (Configure handler) is responsible for not logging values.
#[derive(Debug, Default, Clone)]
pub struct ConfiguredEnv {
    /// The combined configured env plus secrets. Stored together so the merge
    /// treats them identically to plain vars. Key count may be logged; values
    /// must not be logged.
    vars: HashMap<String, String>,
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
    pub fn apply(&mut self, env: HashMap<String, String>, secrets: HashMap<String, String>) {
        for (k, v) in env {
            self.vars.insert(k, v);
        }
        for (k, v) in secrets {
            self.vars.insert(k, v);
        }
    }

    /// Return the number of configured keys. Safe to log.
    pub fn len(&self) -> usize {
        self.vars.len()
    }

    /// Returns true when no configured vars are set.
    pub fn is_empty(&self) -> bool {
        self.vars.is_empty()
    }

    /// Iterate over the configured key-value pairs.
    ///
    /// Values are secret; callers must not log them.
    pub fn iter(&self) -> impl Iterator<Item = (&str, &str)> {
        self.vars.iter().map(|(k, v)| (k.as_str(), v.as_str()))
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
/// The returned list is KEY=VALUE strings suitable for passing to Command::envs
/// after splitting on '='.
///
/// Secret values (in configured or request) are never logged here. Callers
/// are responsible for not logging the output vector.
pub fn merge(
    base: &[String],
    configured: &ConfiguredEnv,
    request: &HashMap<String, String>,
) -> Vec<String> {
    // Capacity hint: at most base.len() + configured.len() + request.len() entries.
    let cap = base.len() + configured.len() + request.len();
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
    for (k, v) in configured.iter() {
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

    #[test]
    fn base_only_passes_through() {
        let base = vec!["FOO=1".to_string(), "BAR=2".to_string()];
        let configured = ConfiguredEnv::new();
        let request = HashMap::new();
        let out = merge(&base, &configured, &request);
        let m = to_map(&out);
        assert_eq!(m["FOO"], "1");
        assert_eq!(m["BAR"], "2");
    }

    #[test]
    fn configured_overrides_base() {
        let base = vec!["FOO=base".to_string()];
        let mut configured = ConfiguredEnv::new();
        configured.apply(
            [("FOO".to_string(), "configured".to_string())]
                .into_iter()
                .collect(),
            HashMap::new(),
        );
        let request = HashMap::new();
        let out = merge(&base, &configured, &request);
        let m = to_map(&out);
        assert_eq!(m["FOO"], "configured");
    }

    #[test]
    fn request_overrides_configured_and_base() {
        let base = vec!["FOO=base".to_string()];
        let mut configured = ConfiguredEnv::new();
        configured.apply(
            [("FOO".to_string(), "configured".to_string())]
                .into_iter()
                .collect(),
            HashMap::new(),
        );
        let request: HashMap<String, String> =
            [("FOO".to_string(), "request".to_string())].into_iter().collect();
        let out = merge(&base, &configured, &request);
        let m = to_map(&out);
        assert_eq!(m["FOO"], "request");
    }

    #[test]
    fn all_three_layers_merge_correctly() {
        // base: A=1 B=2
        // configured: B=cfg C=3
        // request: C=req D=4
        // expected: A=1 B=cfg C=req D=4
        let base = vec!["A=1".to_string(), "B=2".to_string()];
        let mut configured = ConfiguredEnv::new();
        configured.apply(
            [
                ("B".to_string(), "cfg".to_string()),
                ("C".to_string(), "3".to_string()),
            ]
            .into_iter()
            .collect(),
            HashMap::new(),
        );
        let request: HashMap<String, String> = [
            ("C".to_string(), "req".to_string()),
            ("D".to_string(), "4".to_string()),
        ]
        .into_iter()
        .collect();
        let out = merge(&base, &configured, &request);
        let m = to_map(&out);
        assert_eq!(m["A"], "1");
        assert_eq!(m["B"], "cfg");
        assert_eq!(m["C"], "req");
        assert_eq!(m["D"], "4");
        assert_eq!(m.len(), 4);
    }

    #[test]
    fn base_entry_without_equals_passes_through_verbatim() {
        // Go guestenv.Merge passes through entries without '=' verbatim.
        let base = vec!["NOEQUALS".to_string(), "KEY=val".to_string()];
        let configured = ConfiguredEnv::new();
        let request = HashMap::new();
        let out = merge(&base, &configured, &request);
        // The verbatim entry is in the output.
        assert!(out.contains(&"NOEQUALS".to_string()));
        let m = to_map(&out);
        assert_eq!(m["KEY"], "val");
    }

    #[test]
    fn secrets_in_configured_override_base() {
        // Secrets go into configured.vars via apply(); they must override base.
        let base = vec!["SECRET_KEY=base_val".to_string()];
        let mut configured = ConfiguredEnv::new();
        configured.apply(
            HashMap::new(),
            [("SECRET_KEY".to_string(), "secret_val".to_string())]
                .into_iter()
                .collect(),
        );
        let request = HashMap::new();
        let out = merge(&base, &configured, &request);
        let m = to_map(&out);
        // The secret value overrides the base value.
        assert_eq!(m["SECRET_KEY"], "secret_val");
    }

    #[test]
    fn configured_env_len_is_combined_count() {
        let mut configured = ConfiguredEnv::new();
        configured.apply(
            [("A".to_string(), "1".to_string())].into_iter().collect(),
            [("B".to_string(), "s".to_string())].into_iter().collect(),
        );
        // One env var + one secret = 2 total entries.
        assert_eq!(configured.len(), 2);
    }

    #[test]
    fn empty_everything_returns_empty() {
        let out = merge(&[], &ConfiguredEnv::new(), &HashMap::new());
        assert!(out.is_empty());
    }
}
