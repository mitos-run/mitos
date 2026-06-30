// Fork-correctness: eth0 reconfiguration on the notify-forked path.
//
// Every fork restores the same snapshot-baked guest IP. The host remaps the
// NIC to a distinct tap and delivers this fork's distinct guest IP + gateway
// in the notify-forked request. This module reconfigures eth0 on receipt so
// each fork gets a unique address and can route return traffic.
//
// Mirrors configureNetwork (notifyforked.go:193-210) and writeResolvConf
// (notifyforked.go:223-232) exactly, including the operation order:
//   1. If mac is non-empty: link down, set MAC, link up; else just link up.
//   2. Flush existing eth0 addresses.
//   3. Add the new address/prefix.
//   4. Replace the default route via the gateway.
//   5. If resolver_ip is non-empty: write /etc/resolv.conf.
//
// All values (IPs, prefix, MAC) are configuration, not secrets, and may be
// logged (Go logs them; we follow suit). Nothing Go treats as secret is logged.
//
// Rationale for raw netlink (sys::netlink) over the `rtnetlink` crate:
//   The rtnetlink crate is async (tokio-based) and cannot be called from this
//   sync function without either spawning a new runtime or blocking on one, both
//   of which are fragile when called from within an existing tokio context. The
//   existing codebase already uses raw libc syscalls in sys/ for all other
//   fork-correctness paths (RNDADDENTROPY, clock_settime), so the same pattern
//   is used here. Raw AF_NETLINK builds identically on gnu and musl (confirmed
//   on box1) and mirrors what Go's internal/guestnet package does.

/// Per-fork network identity delivered by the host in the notify-forked request.
/// All fields are plain addresses or config; none are secret.
pub struct NetworkConfig {
    /// IPv4 address to assign to eth0 (e.g. "10.200.0.6").
    pub guest_ip: String,
    /// IPv4 gateway to install as the default route (e.g. "10.200.0.5").
    pub gateway_ip: String,
    /// IPv4 prefix length (0..=32).
    pub prefix_len: u32,
    /// Ethernet hardware address for eth0, e.g. "02:ab:cd:ef:12:34".
    /// Empty means leave the snapshot-baked MAC untouched.
    pub guest_mac: String,
    /// IPv4 address of the per-node DNS resolver.
    /// Empty means leave /etc/resolv.conf untouched.
    pub resolver_ip: String,
    /// Host:port of the per-fork egress HTTP(S) proxy, e.g. "169.254.169.2:3128".
    /// The guest agent exports it as HTTP_PROXY and HTTPS_PROXY in
    /// /etc/profile.d/mitos-proxy.sh so egress is policy-enforced by the host.
    /// Empty means the egress proxy is disabled; no proxy env file is written.
    /// Config, not secret: the endpoint is a local link address, not credentials.
    pub proxy_endpoint: String,
    /// When true, this is a live fork: flush stale ARP neighbor entries on eth0
    /// after re-addressing so captured upstream sockets die and clients re-dial
    /// through the egress proxy. False leaves existing ARP state in place, which
    /// is correct for a cold fork from a snapshot.
    pub reset_upstreams: bool,
}

/// Reconfigure eth0 with the per-fork address and default route.
///
/// Mirrors configureNetwork in notifyforked.go:193-210 and writeResolvConf
/// at notifyforked.go:223-232. No-op when cfg is None.
///
/// On any netlink failure the error is printed to stderr and the function
/// returns without panicking, leaving the guest without egress (fail closed).
/// This matches the Go behavior: "log + continue" (configureNetwork does not
/// propagate the error out of handleNotifyForked).
///
/// The interface name defaults to "eth0" but can be overridden for testing.
pub fn configure_network(cfg: Option<&NetworkConfig>) {
    configure_network_on(cfg, "eth0", "/etc/resolv.conf");
}

/// Like configure_network but with injectable interface name and resolv.conf
/// path, for unit/integration testing on dummy links.
pub fn configure_network_on(cfg: Option<&NetworkConfig>, iface: &str, resolv_conf_path: &str) {
    let cfg = match cfg {
        None => return,
        Some(c) => c,
    };

    #[cfg(target_os = "linux")]
    {
        apply_linux(cfg, iface, resolv_conf_path);
    }

    #[cfg(not(target_os = "linux"))]
    {
        let _ = (cfg, iface, resolv_conf_path);
        // Non-Linux (macOS CI): no-op, the netlink syscalls do not exist.
    }
}

#[cfg(target_os = "linux")]
fn apply_linux(cfg: &NetworkConfig, iface: &str, resolv_conf_path: &str) {
    use std::net::Ipv4Addr;
    use std::str::FromStr;

    let guest_ip: Ipv4Addr = match Ipv4Addr::from_str(&cfg.guest_ip) {
        Ok(ip) => ip,
        Err(e) => {
            eprintln!("sandbox-agent: net config: invalid guest_ip {:?}: {e}", cfg.guest_ip);
            return;
        }
    };
    let gateway_ip: Ipv4Addr = match Ipv4Addr::from_str(&cfg.gateway_ip) {
        Ok(ip) => ip,
        Err(e) => {
            eprintln!("sandbox-agent: net config: invalid gateway_ip {:?}: {e}", cfg.gateway_ip);
            return;
        }
    };
    if cfg.prefix_len > 32 {
        eprintln!("sandbox-agent: net config: invalid prefix_len {}", cfg.prefix_len);
        return;
    }
    let prefix_len = cfg.prefix_len as u8;

    let mac: Option<[u8; 6]> = if cfg.guest_mac.is_empty() {
        None
    } else {
        match parse_mac(&cfg.guest_mac) {
            Ok(hw) => Some(hw),
            Err(e) => {
                eprintln!("sandbox-agent: net config: invalid guest_mac {:?}: {e}", cfg.guest_mac);
                return;
            }
        }
    };

    if let Err(e) = crate::sys::netlink::configure(iface, mac, guest_ip, gateway_ip, prefix_len) {
        eprintln!("sandbox-agent: net config failed: {e}");
        // Do not return: attempt resolv.conf and proxy env even if netlink failed,
        // matching the Go behavior of writeResolvConf after a configureNetwork error.
    }

    if let Err(e) = write_resolv_conf(resolv_conf_path, &cfg.resolver_ip) {
        eprintln!("sandbox-agent: write resolv.conf: {e}");
    }

    // Write proxy env file when a per-fork egress proxy is configured.
    if !cfg.proxy_endpoint.is_empty() {
        let proxy_path = std::path::Path::new("/etc/profile.d/mitos-proxy.sh");
        if let Err(e) = write_proxy_env(proxy_path, &cfg.proxy_endpoint) {
            eprintln!("sandbox-agent: write proxy env: {e}");
        }
    }

    // Flush stale ARP entries on live forks so captured upstream sockets die
    // and clients re-dial through the per-fork egress proxy. Best-effort: log
    // and continue on error, matching the overall "fail-open for observability"
    // style of this function.
    if cfg.reset_upstreams {
        if let Err(e) = crate::sys::netlink::flush_neighbors(iface) {
            eprintln!("sandbox-agent: flush neighbors on {iface}: {e}");
        }
    }

    let addr_str = format!("{guest_ip}/{prefix_len}");
    println!(
        "sandbox-agent: configured {iface} addr={addr_str} gateway={gateway} resolver={resolver} proxy={proxy} reset_upstreams={reset}",
        gateway = cfg.gateway_ip,
        resolver = cfg.resolver_ip,
        proxy = cfg.proxy_endpoint,
        reset = cfg.reset_upstreams,
    );
}

/// Parse a colon-separated MAC address string into a 6-byte array.
fn parse_mac(mac: &str) -> Result<[u8; 6], String> {
    let parts: Vec<&str> = mac.split(':').collect();
    if parts.len() != 6 {
        return Err(format!("expected 6 colon-separated octets, got {}", parts.len()));
    }
    let mut out = [0u8; 6];
    for (i, part) in parts.iter().enumerate() {
        let byte = u8::from_str_radix(part, 16)
            .map_err(|e| format!("octet {i} {part:?}: {e}"))?;
        if let Some(slot) = out.get_mut(i) {
            *slot = byte;
        }
    }
    Ok(out)
}

/// Write a single `nameserver <ip>` line to path.
/// No-op when resolver_ip is empty. Replaces the file in full (idempotent on
/// re-delivery). Mirrors writeResolvConf in notifyforked.go:223-232.
/// Mode 0o644 is set explicitly (matching Go's os.WriteFile call) so the
/// result is umask-independent.
fn write_resolv_conf(path: &str, resolver_ip: &str) -> std::io::Result<()> {
    use std::fs::OpenOptions;
    use std::io::Write;
    #[cfg(target_os = "linux")]
    use std::os::unix::fs::OpenOptionsExt;

    if resolver_ip.is_empty() {
        return Ok(());
    }
    let content = format!("nameserver {resolver_ip}\n");
    #[cfg(target_os = "linux")]
    {
        let mut f = OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o644)
            .open(path)?;
        f.write_all(content.as_bytes())
    }
    #[cfg(not(target_os = "linux"))]
    {
        std::fs::write(path, content)
    }
}

/// Write shell export lines for HTTP_PROXY and HTTPS_PROXY to `path`, pointing
/// at `http://<endpoint>`, plus NO_PROXY/no_proxy covering loopback and the cloud
/// metadata address/link-local (defense in depth; the host denylist is the real
/// control). The file is written with mode 0o644 (umask-independent
/// on Linux via OpenOptionsExt; plain write on other platforms).
///
/// An EMPTY `endpoint` is a no-op: nothing is written and no error is returned,
/// so non-proxy forks leave the file untouched.
///
/// This function performs only file I/O (no syscalls, no Linux-specific calls)
/// and is testable on any OS. No secret material is written: `endpoint` is a
/// plain host:port address, not a credential.
pub(super) fn write_proxy_env(path: &std::path::Path, endpoint: &str) -> std::io::Result<()> {
    use std::io::Write;
    #[cfg(target_os = "linux")]
    use std::os::unix::fs::OpenOptionsExt;

    if endpoint.is_empty() {
        return Ok(());
    }
    let url = format!("http://{endpoint}");
    // NO_PROXY (defense in depth): cooperative guest clients bypass the proxy for
    // loopback (so they never proxy localhost calls) and the cloud metadata
    // address / link-local range (so an honest client never even routes IMDS
    // through the proxy). This is ADVISORY only: the authoritative control is the
    // host-side destination denylist in internal/egressproxy, which refuses these
    // regardless of whether the client honors NO_PROXY.
    let no_proxy = "127.0.0.1,::1,localhost,169.254.169.254,169.254.0.0/16";
    let content = format!(
        "export HTTP_PROXY={url}\nexport HTTPS_PROXY={url}\nexport NO_PROXY={no_proxy}\nexport no_proxy={no_proxy}\n"
    );
    #[cfg(target_os = "linux")]
    {
        // Create parent directory if needed (best-effort; file write may fail
        // independently if the dir truly cannot be created).
        if let Some(parent) = path.parent() {
            let _ = std::fs::create_dir_all(parent);
        }
        let mut f = std::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o644)
            .open(path)?;
        f.write_all(content.as_bytes())
    }
    #[cfg(not(target_os = "linux"))]
    {
        // Non-Linux (macOS CI): plain write, no mode control.
        std::fs::write(path, content)
    }
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

#[cfg(test)]
#[allow(
    clippy::expect_used,
    clippy::unwrap_used,
    clippy::panic,
    clippy::indexing_slicing
)]
mod tests {
    use super::*;

    // -----------------------------------------------------------------------
    // Unit tests: run on all platforms (no syscalls).
    // -----------------------------------------------------------------------

    // TDD RED: this test must fail until NetworkConfig gains proxy_endpoint +
    // reset_upstreams and write_proxy_env is implemented.
    #[test]
    fn writes_proxy_env_file_on_reset() {
        let dir = tempfile::tempdir().unwrap();
        let env_path = dir.path().join("mitos-proxy.sh");
        let cfg = NetworkConfig {
            guest_ip: "10.0.0.6".into(),
            gateway_ip: "10.0.0.5".into(),
            prefix_len: 30,
            guest_mac: String::new(),
            resolver_ip: String::new(),
            proxy_endpoint: "169.254.169.2:3128".into(),
            reset_upstreams: true,
        };
        write_proxy_env(std::path::Path::new(env_path.to_str().unwrap()), &cfg.proxy_endpoint).unwrap();
        let body = std::fs::read_to_string(&env_path).unwrap();
        assert!(body.contains("HTTP_PROXY=http://169.254.169.2:3128"));
        assert!(body.contains("HTTPS_PROXY=http://169.254.169.2:3128"));
        // NO_PROXY (and lowercase no_proxy) must cover loopback and the cloud
        // metadata address/link-local so cooperative clients bypass the proxy
        // for them (defense in depth; the host denylist is the real control).
        assert!(body.contains("NO_PROXY="));
        assert!(body.contains("no_proxy="));
        for needle in ["127.0.0.1", "::1", "localhost", "169.254.169.254", "169.254.0.0/16"] {
            assert!(body.contains(needle), "NO_PROXY missing {needle}: {body}");
        }
        // no secrets, just the endpoint
        assert!(!body.contains("Authorization"));
    }

    #[test]
    fn none_config_is_noop() {
        // Must not panic or error when cfg is None.
        configure_network(None);
    }

    #[test]
    fn none_config_on_is_noop() {
        configure_network_on(None, "eth0", "/etc/resolv.conf");
    }

    #[test]
    fn parse_mac_valid() {
        let hw = parse_mac("02:ab:cd:ef:12:34").unwrap();
        assert_eq!(hw, [0x02, 0xAB, 0xCD, 0xEF, 0x12, 0x34]);
    }

    #[test]
    fn parse_mac_invalid_length() {
        assert!(parse_mac("02:ab:cd").is_err());
    }

    #[test]
    fn parse_mac_invalid_hex() {
        assert!(parse_mac("02:ab:cd:zz:12:34").is_err());
    }

    #[test]
    fn write_resolv_conf_empty_resolver_is_noop() {
        // Should not write anything when resolver_ip is empty.
        // Use a path that does not exist; if we tried to write it we'd get an error.
        let result = write_resolv_conf("/nonexistent/resolv.conf", "");
        assert!(result.is_ok(), "empty resolver must be a no-op, not an error");
    }

    #[test]
    fn write_resolv_conf_writes_nameserver_line() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("resolv.conf");
        let path_str = path.to_str().unwrap();

        write_resolv_conf(path_str, "10.200.0.1").unwrap();

        let content = std::fs::read_to_string(&path).unwrap();
        assert_eq!(content, "nameserver 10.200.0.1\n");
    }

    #[test]
    fn write_resolv_conf_is_idempotent() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("resolv.conf");
        let path_str = path.to_str().unwrap();

        // Write twice; second write must replace the file, not append.
        write_resolv_conf(path_str, "10.200.0.1").unwrap();
        write_resolv_conf(path_str, "10.200.0.2").unwrap();

        let content = std::fs::read_to_string(&path).unwrap();
        assert_eq!(content, "nameserver 10.200.0.2\n", "second write must replace, not append");
    }

    // -----------------------------------------------------------------------
    // Linux integration tests: apply configure_network_on against a dummy link
    // inside an isolated network namespace. Each test forks a child, calls
    // unshare(CLONE_NEWNET), creates a dummy link, runs the test body, and
    // exits. This makes the tests parallel-safe: routing changes are confined
    // to the child's ephemeral namespace.
    // -----------------------------------------------------------------------

    // The linux integration test module uses unsafe for fork/unshare/waitpid.
    // sys/ is the usual home for unsafe, but these are test helpers that cannot
    // live there without cross-module visibility gymnastics. The allow is scoped
    // strictly to this test module.
    #[cfg(target_os = "linux")]
    #[allow(unsafe_code)]
    mod linux {
        use super::*;
        use std::process::Command;

        const CLONE_NEWNET: libc::c_int = 0x4000_0000;

        // Fork, unshare network namespace in child, run test body, assert child exits 0.
        // SAFETY: fork()+unshare() pattern is the same as in sys::netlink tests;
        // see the SAFETY comment there for the rationale.
        fn in_netns<F: FnOnce() + std::panic::UnwindSafe>(test_name: &str, f: F) {
            // Skip without failing when not running as root: unshare(CLONE_NEWNET)
            // requires CAP_SYS_ADMIN. A non-root cargo test run counts as pass.
            // SAFETY: geteuid() is always safe to call; it has no side effects.
            if unsafe { libc::geteuid() } != 0 {
                eprintln!("skipping {test_name}: requires root/CAP_SYS_ADMIN");
                return;
            }
            // SAFETY: fork() duplicates the calling process; the child is single-threaded
            // after fork and calls only _exit, not Rust runtime teardown.
            let pid = unsafe { libc::fork() };
            if pid < 0 {
                panic!("{test_name}: fork failed: {}", std::io::Error::last_os_error());
            }
            if pid == 0 {
                // SAFETY: valid CLONE_NEWNET flag; no pointer args.
                let r = unsafe { libc::unshare(CLONE_NEWNET) };
                if r != 0 {
                    eprintln!("{test_name}: unshare failed: {}", std::io::Error::last_os_error());
                    // SAFETY: _exit is always safe to call.
                    unsafe { libc::_exit(1) };
                }
                let ok = std::panic::catch_unwind(f).is_ok();
                // SAFETY: see above.
                unsafe { libc::_exit(if ok { 0 } else { 1 }) };
            }
            let mut status: libc::c_int = 0;
            // SAFETY: pid > 0 (child pid returned by fork); status is valid i32.
            unsafe { libc::waitpid(pid, &mut status, 0) };
            let ok = libc::WIFEXITED(status) && libc::WEXITSTATUS(status) == 0;
            assert!(ok, "{test_name}: child process failed (status={status})");
        }

        fn with_dummy_link<F: FnOnce(&str)>(link: &str, f: F) {
            let out = Command::new("ip")
                .args(["link", "add", link, "type", "dummy"])
                .output()
                .expect("ip link add");
            assert!(out.status.success(), "ip link add {link} failed: {}", String::from_utf8_lossy(&out.stderr));
            f(link);
            let _ = Command::new("ip").args(["link", "delete", link]).output();
        }

        #[test]
        fn configure_network_on_applies_address_and_route() {
            in_netns("configure_network_on_applies_address_and_route", || {
                with_dummy_link("test-fk-net", |iface| {
                    let dir = tempfile::tempdir().unwrap();
                    let resolv = dir.path().join("resolv.conf");
                    let resolv_str = resolv.to_str().unwrap();

                    let cfg = NetworkConfig {
                        guest_ip: "10.88.0.6".to_string(),
                        gateway_ip: "10.88.0.5".to_string(),
                        prefix_len: 30,
                        guest_mac: "".to_string(),
                        resolver_ip: "10.88.0.1".to_string(),
                        proxy_endpoint: "".to_string(),
                        reset_upstreams: false,
                    };
                    configure_network_on(Some(&cfg), iface, resolv_str);

                    let addr = Command::new("ip")
                        .args(["addr", "show", "dev", iface])
                        .output()
                        .unwrap();
                    let addr_str = String::from_utf8_lossy(&addr.stdout);
                    assert!(addr_str.contains("10.88.0.6/30"), "address must be set: {addr_str}");

                    let content = std::fs::read_to_string(resolv_str).unwrap();
                    assert_eq!(content, "nameserver 10.88.0.1\n");
                });
            });
        }

        #[test]
        fn configure_network_on_is_idempotent_on_re_fork() {
            in_netns("configure_network_on_is_idempotent_on_re_fork", || {
                with_dummy_link("test-fk-idem", |iface| {
                    let dir = tempfile::tempdir().unwrap();
                    let resolv = dir.path().join("resolv.conf");
                    let resolv_str = resolv.to_str().unwrap();

                    let cfg1 = NetworkConfig {
                        guest_ip: "10.89.0.6".to_string(),
                        gateway_ip: "10.89.0.5".to_string(),
                        prefix_len: 30,
                        guest_mac: "".to_string(),
                        resolver_ip: "".to_string(),
                        proxy_endpoint: "".to_string(),
                        reset_upstreams: false,
                    };
                    let cfg2 = NetworkConfig {
                        guest_ip: "10.89.1.6".to_string(),
                        gateway_ip: "10.89.1.5".to_string(),
                        prefix_len: 30,
                        guest_mac: "".to_string(),
                        resolver_ip: "".to_string(),
                        proxy_endpoint: "".to_string(),
                        reset_upstreams: false,
                    };
                    configure_network_on(Some(&cfg1), iface, resolv_str);
                    configure_network_on(Some(&cfg2), iface, resolv_str);

                    let addr = Command::new("ip").args(["addr", "show", "dev", iface]).output().unwrap();
                    let addr_str = String::from_utf8_lossy(&addr.stdout);
                    assert!(addr_str.contains("10.89.1.6/30"), "re-fork address must be set: {addr_str}");
                    assert!(!addr_str.contains("10.89.0.6"), "old address must be flushed: {addr_str}");
                });
            });
        }

        #[test]
        fn configure_network_on_sets_mac() {
            in_netns("configure_network_on_sets_mac", || {
                with_dummy_link("test-fk-mac", |iface| {
                    let dir = tempfile::tempdir().unwrap();
                    let resolv_str = dir.path().join("resolv.conf");
                    let cfg = NetworkConfig {
                        guest_ip: "10.90.0.6".to_string(),
                        gateway_ip: "10.90.0.5".to_string(),
                        prefix_len: 30,
                        guest_mac: "02:aa:bb:cc:dd:ee".to_string(),
                        resolver_ip: "".to_string(),
                        proxy_endpoint: "".to_string(),
                        reset_upstreams: false,
                    };
                    configure_network_on(Some(&cfg), iface, resolv_str.to_str().unwrap());

                    let link_out = Command::new("ip").args(["link", "show", "dev", iface]).output().unwrap();
                    let link_str = String::from_utf8_lossy(&link_out.stdout);
                    assert!(link_str.contains("02:aa:bb:cc:dd:ee"), "MAC must be set: {link_str}");
                });
            });
        }
    }
}
