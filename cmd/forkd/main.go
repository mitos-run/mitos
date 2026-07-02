package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"mitos.run/mitos/internal/casgc"
	"mitos.run/mitos/internal/daemon"
	"mitos.run/mitos/internal/dnsproxy"
	"mitos.run/mitos/internal/egressproxy"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/kms"
	"mitos.run/mitos/internal/netconf"
	"mitos.run/mitos/internal/network"
	"mitos.run/mitos/internal/observability"
	"mitos.run/mitos/internal/pki"
	"mitos.run/mitos/internal/sniproxy"
)

func main() {
	var (
		listenAddr           string
		httpAddr             string
		dataDir              string
		firecrackerBin       string
		kernelPath           string
		mockMode             bool
		tlsCert              string
		tlsKey               string
		tlsCA                string
		jailerBin            string
		chrootBase           string
		uidRange             string
		casDir               string
		allowUnverified      bool
		allowIncompatible    bool
		enableNet            bool
		sandboxSubnet        string
		uplink               string
		dnsResolver          string
		enableDNSEgress      bool
		dnsUpstream          string
		enableEgressProxy    bool
		proxySentinel        string
		proxyPort            int
		enableSNIEgress      bool
		sniProxyPort         int
		agentBin             string
		busyboxBin           string
		enableVolumes        bool
		enableEncryption     bool
		kekFile              string
		auditLog             string
		otlpEndpoint         string
		memReserveBytes      int64
		maxSandboxes         int
		maxStreamsPerSandbox int
		maxExecTimeoutSecs   int
		casListen            string
		allowInsecureGRPC    bool
		casGCInterval        time.Duration
		casDiskHigh          float64
		casDiskLow           float64
	)
	// peerToken is read from the environment, NOT a flag: a flag is visible in
	// /proc/<pid>/cmdline, and the token is a credential. The controller already
	// reads the same FORKD_PEER_TOKEN env var, so the two sides match by config.
	peerToken := os.Getenv("FORKD_PEER_TOKEN")

	flag.StringVar(&listenAddr, "listen", ":9090", "gRPC listen address (controller communication)")
	flag.StringVar(&httpAddr, "http", ":9091", "HTTP listen address (metrics + sandbox exec/files API)")
	flag.StringVar(&dataDir, "data-dir", "/var/lib/mitos", "Data directory for snapshots and sandboxes")
	flag.StringVar(&firecrackerBin, "firecracker", "/usr/local/bin/firecracker", "Firecracker binary path")
	flag.StringVar(&kernelPath, "kernel", "/var/lib/mitos/vmlinux", "Guest kernel path")
	flag.BoolVar(&mockMode, "mock", false, "Use mock fork engine (no KVM required)")
	flag.StringVar(&tlsCert, "tls-cert", "", "Path to the forkd server certificate PEM (mTLS)")
	flag.StringVar(&tlsKey, "tls-key", "", "Path to the forkd server key PEM (mTLS)")
	flag.StringVar(&tlsCA, "tls-ca", "", "Path to the control plane CA certificate PEM (mTLS)")
	flag.BoolVar(&allowInsecureGRPC, "allow-insecure-grpc", false, "Opt in to serving the controller-facing gRPC surface UNAUTHENTICATED when no TLS flags are set (local development only). Without this flag forkd FAILS CLOSED and refuses to start without mTLS, because the gRPC surface delivers secrets and drives forks. The shipped DaemonSet always sets TLS, so this never applies in production")
	flag.StringVar(&jailerBin, "jailer", "", "Jailer binary path; every VM is launched through it with a per-VM uid and chroot. Empty disables the jailer (development only)")
	flag.StringVar(&chrootBase, "chroot-base", "/srv/jailer", "Jailer chroot base directory; must share a filesystem with --data-dir")
	flag.StringVar(&uidRange, "uid-range", "64000-64999", "Inclusive uid/gid range for per-VM jailer users, formatted low-high")
	flag.StringVar(&casDir, "cas-dir", "", "Content-addressed store directory for snapshot integrity and transfer. Empty means <data-dir>/cas")
	flag.DurationVar(&casGCInterval, "cas-gc-interval", 5*time.Minute, "How often forkd evicts unpinned CAS chunks when the data-dir filesystem crosses --cas-disk-high-watermark; 0 disables CAS GC. Pinned (active-template) chunks are never evicted (#464)")
	flag.Float64Var(&casDiskHigh, "cas-disk-high-watermark", 0.85, "Data-dir filesystem used fraction at or above which CAS GC evicts; prevents un-GC'd CAS from tripping node DiskPressure (#464)")
	flag.Float64Var(&casDiskLow, "cas-disk-low-watermark", 0.70, "Data-dir filesystem used fraction the CAS GC evicts down toward when triggered")
	flag.BoolVar(&allowUnverified, "allow-unverified-snapshots", false, "Allow forking snapshots that fail or skip integrity verification (development only; refused by default)")
	flag.BoolVar(&allowIncompatible, "allow-incompatible-snapshots", false, "Allow forking snapshots whose recorded environment (Firecracker version, CPU model, or snapshot format) is incompatible with this host (development only; refused by default)")
	flag.BoolVar(&enableNet, "enable-networking", false, "Enable per-sandbox guest networking (tap device, egress nftables, NIC attach). Default false until proven on KVM CI")
	flag.StringVar(&sandboxSubnet, "sandbox-subnet", "10.200.0.0/16", "IPv4 subnet carved into per-sandbox /30 point-to-point links; requires --enable-networking")
	flag.StringVar(&uplink, "uplink", "", "Host egress interface for the optional sandbox-subnet MASQUERADE rule. Empty relies on the node's existing NAT")
	flag.StringVar(&dnsResolver, "dns-resolver", "", "DNS resolver IP guests may reach; adds a DNS allow rule to each fork's egress ruleset. Empty omits the rule. With --enable-dns-egress this is the address the controlled resolver binds and every guest is pointed at; it defaults to 169.254.1.1 when unset")
	flag.BoolVar(&enableDNSEgress, "enable-dns-egress", false, "Enable name-based egress: run a controlled DNS resolver that resolves only allowlisted names and pins each resolved IP into the sandbox's egress set, and point guests at it. Requires --enable-networking. Default false until proven on KVM CI; when off, name-based allow entries stay unenforced as today")
	flag.StringVar(&dnsUpstream, "dns-upstream", "", "Upstream resolver (host:port) the controlled DNS proxy forwards allowed queries to. Empty derives the first nameserver from /etc/resolv.conf, falling back to 1.1.1.1:53")
	flag.BoolVar(&enableEgressProxy, "egress-proxy", false, "Enable the per-sandbox egress proxy: run a host-side HTTP forward proxy that attributes each fork's egress by source IP and enforces upstream policy, and point every fork at it via a fork-stable sentinel address (DNATed to each fork's gateway). Requires --enable-networking. Default false until proven on KVM CI; when off, guest egress is governed by the per-sandbox nftables chain exactly as before")
	flag.StringVar(&proxySentinel, "proxy-sentinel", "169.254.169.2", "Fork-stable sentinel address baked into every fork's proxy endpoint; each fork's nftables DNAT redirects it to that fork's gateway where the per-node proxy listens. Effective only with --egress-proxy")
	flag.IntVar(&proxyPort, "proxy-port", 3128, "TCP port the per-node egress proxy listens on and the DNAT/accept rules target. Effective only with --egress-proxy")
	flag.BoolVar(&enableSNIEgress, "sni-egress", false, "Enable the host-side TLS SNI egress filter: a transparent peek-and-splice proxy that reads each TLS ClientHello's SNI and allows the connection only when the SNI matches this sandbox's domain allowlist (the SAME exact/anchored-wildcard names the controlled DNS resolver enforces), splicing on allow and closing fail-closed on deny. Requires --enable-networking, --enable-dns-egress (the allowlist source), and --egress-proxy (source attribution and the denied-IP floor). Default false until proven on KVM CI; the nftables redirect of guest tcp/443 to this listener is a KVM follow-up")
	flag.IntVar(&sniProxyPort, "sni-proxy-port", 8443, "TCP port the host-side TLS SNI egress filter listens on; the nftables redirect target for guest tcp/443. Effective only with --sni-egress")
	flag.StringVar(&agentBin, "agent-bin", "", "Path to the guest agent binary injected as /init when a template is built from an OCI image. Required for image builds; unused for file-path rootfs templates. For now this binary must be present in the forkd image (a follow-up will go:embed it)")
	flag.StringVar(&busyboxBin, "busybox-bin", "", "Optional path to a static busybox providing /bin/sh, injected when an image ships no shell. Empty means images without a shell cannot run init")
	flag.BoolVar(&enableVolumes, "enable-volumes", false, "Enable per-fork volume drives: the template build bakes a placeholder drive per template volume and each fork prepares its own backing and rebinds the drive. Default false until proven on KVM CI")
	flag.BoolVar(&enableEncryption, "enable-encryption", false, "Encrypt template snapshots at rest: each template is built inside a per-template LUKS2 container (requires cryptsetup) and crypto-shred at delete. Default false (plaintext snapshots on disk, exactly as before). KEY CUSTODY: the per-template encryption key is supplied by the controller over the mTLS gRPC request (CreateTemplate/Fork), held in forkd memory only for the lifetime of an open container, and never written to the node data disk. REQUIRES mTLS: this flag refuses to start unless the gRPC server is configured with --tls-cert/--tls-key/--tls-ca (and the controller runs PKI bootstrap), so the key is never sent over an insecure channel")
	flag.StringVar(&kekFile, "kek-file", "", "Path to the 32-byte AES-256 KEK file (mounted from a Kubernetes Secret) used to UNWRAP the per-template DEK delivered over the mTLS RPC (envelope encryption). REQUIRED with --enable-encryption. The KEK is a secret value: it is never logged. Cloud KMS providers (AWS/GCP/Vault) are a documented follow-up.")
	flag.StringVar(&auditLog, "audit-log", "", "Structured audit log of exec and file operations. A file path, or '-'/'stderr' for stderr. Empty disables auditing. Records command strings, paths, and byte counts only; never file content or secret values")
	flag.StringVar(&otlpEndpoint, "otlp-endpoint", "", "OTLP gRPC endpoint (host:port) for OpenTelemetry trace export. Empty disables tracing (zero cost). Spans carry ids, counts, and timings only; never secret values")
	flag.Int64Var(&memReserveBytes, "memory-reserve-bytes", 2*1024*1024*1024, "Bytes of host memory withheld from the schedulable budget for the OS and forkd itself. GetCapacity reports MemoryTotal = max(0, /proc/meminfo MemTotal - this reserve), the budget the controller bin-packs forks against. Default 2 GiB")
	flag.IntVar(&maxSandboxes, "max-sandboxes", 0, "Per-node host-DoS ceiling (production-blocker #2): the maximum number of live sandboxes this forkd admits. Fork refuses with RESOURCE_EXHAUSTED once the live count reaches this, BEFORE allocating or booting anything (an O(1) admission check off the fork hot path), so a runaway tenant cannot exhaust the node by opening forks. 0 disables the ceiling (the prior behavior). GetCapacity reports it so the controller sees the cap.")
	flag.IntVar(&maxStreamsPerSandbox, "max-streams-per-sandbox", 16, "Per-sandbox ceiling on concurrent OPEN streams (production-blocker #2): streaming exec, run_code, and PTY each hold a dedicated vsock connection plus host goroutines for the command lifetime, so an unbounded number would exhaust host vsock connections and goroutines. A NEW stream opened over this cap is rejected with 429 (the too_many_streams error); existing streams are never killed. The cap is checked at stream OPEN, off the activate path. 0 disables the cap (unbounded, the prior behavior).")
	flag.IntVar(&maxExecTimeoutSecs, "max-exec-timeout-seconds", 86400, "Ceiling (seconds) on a requested exec or run_code timeout (issue #216). A request over the ceiling is REJECTED with the typed timeout_too_large error, never silently reduced, so a requested deadline is always honored or rejected. Default 86400 (24h) clears the SDK exec_background one-day default. 0 disables the ceiling (any timeout honored).")
	flag.StringVar(&casListen, "cas-listen", ":9092", "Listen address for the DEDICATED token-gated TLS CAS listener used for peer template distribution. The CAS surface is served here, on its OWN port, NOT on the sandbox HTTP port (--http): the sandbox exec/files/metrics/healthz API keeps its existing scheme so SDK clients are unaffected. Effective only when CAS distribution is enabled (FORKD_PEER_TOKEN set together with mTLS). The controller derives this port to build each holder's CAS source URL")
	// peerToken (FORKD_PEER_TOKEN env) is the shared bearer token a peer forkd
	// (driven by the controller) must present to pull templates from this node's
	// content-addressed store. It is read from the ENVIRONMENT, not a flag, so it
	// is never exposed in /proc/<pid>/cmdline (the token is a credential and is
	// never logged). When set together with mTLS (--tls-cert/--tls-key/--tls-ca),
	// the token-gated CAS surface is served on the dedicated --cas-listen TLS
	// port and template distribution is enabled. REQUIRES mTLS: the surface is
	// served over TLS only so the token stays confidential; chunks are
	// digest-addressed so integrity is channel-independent, but the token gates
	// enumeration/pull. The controller must be configured with the SAME token
	// (it reads the same FORKD_PEER_TOKEN env var). SIMPLEST defensible model; a
	// per-pull minted token / forkd-peer mTLS identity is a follow-up.
	flag.Parse()

	// Fail fast on egress-proxy without networking (M3): the proxy attributes
	// every connection by per-sandbox guest IP, which only exists when
	// --enable-networking is on. Without it the proxy block is skipped and
	// --egress-proxy would silently no-op, leaving operators to believe egress is
	// proxied when it is not. Refuse rather than degrade silently.
	if enableEgressProxy && !enableNet {
		fmt.Fprintln(os.Stderr, "forkd: --egress-proxy requires --enable-networking: the proxy attributes egress by per-sandbox guest IP, which only exists when networking is on. Enable --enable-networking or drop --egress-proxy.")
		os.Exit(1)
	}

	// The SNI egress filter reuses the DNS resolver's per-sandbox domain
	// allowlist (its allowlist source) and the egress proxy's source attribution
	// plus denied-IP floor, all of which exist only with networking on. Refuse
	// rather than degrade silently to a filter with no allowlist to enforce.
	if enableSNIEgress && (!enableNet || !enableDNSEgress || !enableEgressProxy) {
		fmt.Fprintln(os.Stderr, "forkd: --sni-egress requires --enable-networking, --enable-dns-egress (the domain allowlist source), and --egress-proxy (per-sandbox source attribution and the denied-IP floor). Enable all three or drop --sni-egress.")
		os.Exit(1)
	}

	shutdownTracing, err := observability.Setup(context.Background(), "mitos-forkd", otlpEndpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forkd: tracing setup: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	grpcOpts, err := grpcServerOptions(tlsCert, tlsKey, tlsCA, allowInsecureGRPC)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forkd: %v\n", err)
		os.Exit(1)
	}
	// Fail closed: at-rest encryption delivers the per-template key over the
	// gRPC request, so it must only run over an mTLS channel. The gRPC server is
	// secure only when all three TLS flags are set (see grpcServerOptions); any
	// other state leaves the channel insecure and would leak the key in
	// cleartext. Refuse to serve in that case.
	tlsConfigured := tlsCert != "" && tlsKey != "" && tlsCA != ""
	if err := requireTLSForEncryption(enableEncryption, tlsConfigured); err != nil {
		fmt.Fprintf(os.Stderr, "forkd: %v\n", err)
		os.Exit(1)
	}
	// The otelgrpc server handler receives the controller's propagated trace
	// context so forkd spans join the controller's trace. Harmless when
	// tracing is disabled (global no-op provider).
	grpcOpts = append(grpcOpts, grpc.StatsHandler(observability.GRPCServerStatsHandler()))

	var engine daemon.ForkEngine
	// dnsProxyServer is the node-level controlled resolver, set only when
	// --enable-dns-egress and networking are both on. Nil otherwise.
	var dnsProxyServer *dnsproxy.Server
	// egressProxyServer is the node-level HTTP forward proxy and proxyListenAddr
	// its listen address, set only when --egress-proxy and networking are both
	// on. Nil otherwise.
	var egressProxyServer *egressproxy.Proxy
	var proxyListenAddr string
	// sniProxyServer is the node-level TLS SNI egress filter and sniListenAddr its
	// listen address, set only when --sni-egress (and its prerequisites) are on. It
	// reuses the DNS registry as its domain allowlist and the egress proxy registry
	// for source attribution. Nil otherwise.
	var sniProxyServer *sniproxy.Proxy
	var sniListenAddr string
	// dnsRegistryForSNI and egressRegistryForSNI capture the two registries the SNI
	// filter composes, so it can be built after both proxy blocks below.
	var dnsRegistryForSNI *dnsproxy.Registry
	var egressRegistryForSNI *egressproxy.Registry
	// reqKeyProvider is the request-scoped encryption key provider, set only when
	// --enable-encryption is on. The same instance is wired into the engine and
	// the daemon server so the handlers can hand the controller-delivered key to
	// the engine. Nil otherwise.
	var reqKeyProvider *fork.RequestKeyProvider
	// casServing, when set, enables the token-gated TLS CAS surface for peer
	// template distribution. Set only on the real engine when mTLS and a peer
	// token are both configured. Nil leaves the HTTP server plaintext as before.
	var casServing *daemon.CASServing
	// casGCStore is the real engine's CAS store, captured for the periodic GC
	// (#464). nil in mock mode (no real CAS to evict).
	var casGCStore casgc.Store

	if mockMode {
		fmt.Println("forkd: running in mock mode")
		mock := fork.NewMockEngine()
		if err := mock.CreateTemplate("default", "python:3.12-slim", nil, nil, nil, nil, false); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: mock template: %v\n", err)
			os.Exit(1)
		}
		engine = mock
	} else {
		jailerCfg, err := buildJailerConfig(jailerBin, chrootBase, uidRange, dataDir, os.Geteuid(), sameDevice)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forkd: %v\n", err)
			os.Exit(1)
		}
		if !jailerCfg.Enabled() {
			fmt.Fprintln(os.Stderr, "forkd: jailer DISABLED; Firecracker runs unjailed as forkd's user (threat model section 1); supply --jailer for any non-development deployment")
		} else {
			// The jailer pivot_roots into a per-VM dir under --chroot-base, which
			// inside a pod needs a PRIVATE MOUNT as the chroots' parent (a pod's
			// rootfs is commonly shared, and pivot_root refuses a new root whose
			// parent mount is shared). When --chroot-base is under --data-dir, forkd
			// binds the DATA DIR private so that, additionally, the template files the
			// jailer hard-links into each chroot share that mount and stay CoW (link(2)
			// will not cross a mount boundary; issue #526). Done once, here, in forkd's
			// own mount namespace, BEFORE the engine launches any jailed VM. This is
			// what lets the DaemonSet drop privileged: true for the explicit jailer
			// capability set (CAP_SYS_ADMIN does the mount(2)). A no-op on non-linux
			// (mount_other.go).
			if err := prepareChrootMount(dataDir, jailerCfg.ChrootBaseDir); err != nil {
				fmt.Fprintf(os.Stderr, "forkd: prepare jailer chroot base mount: %v\n", err)
				os.Exit(1)
			}
			// Self-check the CoW layout: probe a real hard link from the data dir into
			// the chroot base. If it crosses a mount boundary the jailer will COPY the
			// template rootfs into every VM chroot (slow, defeats fork CoW, can time
			// the jailer out mid-build). Warn with remediation rather than fail: a
			// degraded-but-up node is better than removing the node's fork capacity,
			// and the operator gets the exact fix at boot (issue #526).
			if cow, detail, err := verifyChrootCoW(dataDir, jailerCfg.ChrootBaseDir); err != nil {
				fmt.Fprintf(os.Stderr, "forkd: could not verify jailer chroot CoW layout: %v\n", err)
			} else if !cow {
				fmt.Fprintf(os.Stderr, "forkd: WARNING: %s\n", detail)
			}
		}
		engineOpts := fork.EngineOpts{
			CASDir:             casDir,
			AllowUnverified:    allowUnverified,
			AllowIncompatible:  allowIncompatible,
			AgentBinPath:       agentBin,
			BusyboxPath:        busyboxBin,
			EnableVolumes:      enableVolumes,
			MaxSandboxes:       int32(maxSandboxes),
			MemoryReserveBytes: memReserveBytes,
		}
		// Template distribution: when mTLS and a peer token are configured, build
		// the HTTP client PullTemplate dials a holder forkd's CAS with. It presents
		// this forkd's own client identity (the same cert pair the gRPC server
		// uses) and trusts the control-plane CA, so the pull rides forkd-to-forkd
		// mTLS; the peer token is the additional gate the holder enforces. The
		// token, not the client, carries the credential.
		if tlsConfigured && peerToken != "" {
			pullClient, perr := pullHTTPClient(tlsCert, tlsKey, tlsCA)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "forkd: build template-pull client: %v\n", perr)
				os.Exit(1)
			}
			engineOpts.PullHTTPClient = pullClient
		}
		if enableVolumes {
			fmt.Println("forkd: per-fork volumes ENABLED")
		}
		if enableEncryption {
			// Envelope key custody: the controller owns the per-template DEK, wraps
			// it with the KMS KEK, and delivers only the WRAPPED DEK on each mTLS
			// RPC. The node neither generates nor persists the plaintext DEK; the
			// RequestKeyProvider unwraps via the local KEK on demand and zeroizes
			// the plaintext after the cryptsetup call. The KEK arrives by PATH
			// (--kek-file), never as a value in argv. Fail closed: refuse to start
			// if encryption is enabled without a KEK, so a wrapped DEK can never
			// arrive without an unwrapper. The same provider instance is wired into
			// both the engine (it reads the DEK via KeyFor) and the daemon server
			// (the handlers stash the wrapped DEK via SetWrappedKey and forget it).
			if kekFile == "" {
				fmt.Fprintln(os.Stderr, "forkd: --enable-encryption requires --kek-file (the KEK that unwraps the per-template DEK); refusing to start so a wrapped DEK can never arrive without an unwrapper")
				os.Exit(1)
			}
			wrapper, kerr := kms.LoadLocalKEKFromFile(kekFile)
			if kerr != nil {
				fmt.Fprintf(os.Stderr, "forkd: load KEK: %v\n", kerr)
				os.Exit(1)
			}
			engineOpts.EnableEncryption = true
			reqKeyProvider = fork.NewRequestKeyProvider(wrapper)
			engineOpts.KeyProvider = reqKeyProvider
			fmt.Printf("forkd: at-rest snapshot encryption ENABLED (envelope: the per-template DEK arrives WRAPPED over the mTLS RPC and is unwrapped by the local KEK %s; the plaintext DEK is never generated or persisted on the node)\n", wrapper.KEKID())
		}
		if enableNet {
			alloc, err := netconf.NewAllocator(sandboxSubnet, "sbtap")
			if err != nil {
				fmt.Fprintf(os.Stderr, "forkd: invalid --sandbox-subnet: %v\n", err)
				os.Exit(1)
			}
			engineOpts.NetManager = network.NewManager(network.Options{
				SubnetCIDR: sandboxSubnet,
				Uplink:     uplink,
				// ProxyEnabled mirrors the node-wide egress proxy flag so teardown
				// removes each tap's per-fork prerouting DNAT (no leak on tap reuse).
				ProxyEnabled: enableEgressProxy,
				// The node is assumed to forward already; the optional uplink
				// MASQUERADE covers SNAT when set. Forwarding is not toggled
				// here to avoid surprising the host's sysctl state.
			})
			engineOpts.NetAllocator = alloc
			// With DNS egress on, default the resolver IP to a node-wide
			// link-local address the proxy binds and every chain allows on 53.
			if enableDNSEgress && dnsResolver == "" {
				dnsResolver = defaultDNSResolverIP
			}
			if dnsResolver != "" {
				ip := net.ParseIP(dnsResolver)
				if ip == nil {
					fmt.Fprintf(os.Stderr, "forkd: invalid --dns-resolver %q\n", dnsResolver)
					os.Exit(1)
				}
				engineOpts.ResolverIP = ip
			}
			fmt.Printf("forkd: per-sandbox networking ENABLED (subnet %s, uplink %q)\n", sandboxSubnet, uplink)

			// Name-based egress: a controlled DNS resolver bound to the resolver
			// IP, registering each fork's name allowlist (by guest IP) and
			// pinning resolved IPs into that sandbox's nft set. Requires the
			// resolver IP (defaulted above) and networking (the registry keys on
			// the per-fork guest IP).
			if enableDNSEgress {
				registry := dnsproxy.NewRegistry()
				engineOpts.DNSRegistry = registry
				engineOpts.EnableDNSEgress = true
				dnsRegistryForSNI = registry
				dnsProxyServer = buildDNSProxy(registry, alloc, dnsResolver, dnsUpstream)
				fmt.Printf("forkd: name-based DNS egress ENABLED (resolver %s, upstream %s)\n", dnsResolver, resolvedUpstream(dnsUpstream))
			}

			// Per-sandbox egress proxy: a host-side HTTP forward proxy that
			// attributes each fork's egress by source IP and enforces upstream
			// policy. Each fork registers its guest IP with the registry and is
			// pointed at the sentinel endpoint; the per-fork DNAT redirects the
			// sentinel to that fork's gateway, where this one process listens.
			if enableEgressProxy {
				sentinelIP := net.ParseIP(proxySentinel)
				if sentinelIP == nil {
					fmt.Fprintf(os.Stderr, "forkd: invalid --proxy-sentinel %q\n", proxySentinel)
					os.Exit(1)
				}
				registry := egressproxy.NewRegistry()
				engineOpts.EgressProxy = registry
				engineOpts.ProxySentinel = sentinelIP
				engineOpts.ProxyPort = proxyPort
				egressRegistryForSNI = registry
				egressProxyServer = buildEgressProxy(registry)
				// The DNAT targets each fork's gateway (the host side of its /30),
				// so the single listener must accept on every gateway address: bind
				// the wildcard on the proxy port. Source IP still attributes each
				// connection to its sandbox.
				proxyListenAddr = net.JoinHostPort("", strconv.Itoa(proxyPort))
				fmt.Printf("forkd: per-sandbox egress proxy ENABLED (sentinel %s, port %d)\n", proxySentinel, proxyPort)
			}

			// Host-side TLS SNI egress filter: a transparent peek-and-splice proxy
			// that enforces the SAME per-sandbox domain allowlist the DNS resolver
			// holds, at TLS-connection time, by reading the ClientHello SNI without
			// terminating TLS. It composes the DNS registry (allowlist source) and the
			// egress proxy registry (source attribution), both built above; the
			// prerequisite flag check ran at startup. The nftables redirect of guest
			// tcp/443 to this listener is a KVM follow-up (see docs/networking.md), so
			// until it lands no traffic reaches this listener.
			if enableSNIEgress {
				sniProxyServer = buildSNIProxy(dnsRegistryForSNI, egressRegistryForSNI)
				sniListenAddr = net.JoinHostPort("", strconv.Itoa(sniProxyPort))
				fmt.Printf("forkd: per-sandbox TLS SNI egress filter ENABLED (port %d); the nftables redirect of guest tcp/443 to it is a KVM follow-up\n", sniProxyPort)
			}
		}
		real, err := fork.NewEngine(dataDir, firecrackerBin, kernelPath, jailerCfg, engineOpts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forkd: failed to initialize: %v\n", err)
			fmt.Fprintf(os.Stderr, "forkd: use --mock for local development without KVM\n")
			os.Exit(1)
		}
		engine = real
		casGCStore = real.CASStore()

		// Raw-forkd multi-tenant gate (security blocker 5): the raw-forkd engine
		// path (this non-husk forkd DaemonSet) runs the daemon privileged and, when
		// the jailer is disabled, runs Firecracker unjailed. Snapshots are node-flat,
		// so a node that mixes tenants on raw-forkd exposes them to one another at the
		// host boundary. Per-fork rootfs CoW now stops the cross-fork rootfs write
		// bleed, but raw-forkd is still NOT a hardened multi-tenant isolation
		// boundary. Surface this loudly at startup so an operator enabling it is never
		// misled into placing untrusted multi-tenant workloads on it; the hardened
		// multi-tenant path is the husk-pod engine. See docs/threat-model.md.
		fmt.Fprintln(os.Stderr, "forkd: WARNING raw-forkd (the non-husk engine path) is NOT for untrusted multi-tenant workloads: the daemon runs privileged, snapshots are node-flat, and (without --jailer) Firecracker runs unjailed. Use the husk-pod engine for untrusted multi-tenant isolation. See docs/threat-model.md.")

		// Enable CAS peer distribution only when mTLS AND a peer token are set:
		// the surface serves digest-addressed bytes over TLS, gated by the shared
		// token. It is served on its OWN listener (--cas-listen), NOT the sandbox
		// HTTP port, so the sandbox API scheme is unchanged. The CAS listener
		// serves HTTPS using the same cert pair as the gRPC server.
		if tlsConfigured && peerToken != "" {
			httpTLS, terr := serverHTTPTLSConfig(tlsCert, tlsKey, tlsCA)
			if terr != nil {
				fmt.Fprintf(os.Stderr, "forkd: build CAS server TLS: %v\n", terr)
				os.Exit(1)
			}
			casServing = &daemon.CASServing{Store: real.CASStore(), Token: peerToken, TLS: httpTLS, Addr: casListen}
		} else if peerToken != "" {
			// A token without mTLS would serve it in cleartext: refuse to enable.
			fmt.Fprintln(os.Stderr, "forkd: FORKD_PEER_TOKEN set without mTLS (--tls-cert/--tls-key/--tls-ca); CAS distribution stays DISABLED so the token is never served in cleartext")
		}
	}

	sandboxAPI := daemon.NewSandboxAPI(dataDir)
	sandboxAPI.SetMaxStreamsPerSandbox(maxStreamsPerSandbox)
	sandboxAPI.SetMaxExecTimeoutSeconds(maxExecTimeoutSecs)
	// Drive the real engine's pause/resume (full memory+fs snapshot and restore)
	// from the sandbox API's pause/resume endpoints (issue #218).
	sandboxAPI.SetEnginePauser(engine)
	auditor, auditCloser, err := daemon.AuditorFromFlag(auditLog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forkd: %v\n", err)
		os.Exit(1)
	}
	if auditCloser != nil {
		defer auditCloser.Close()
	}
	sandboxAPI.SetAuditor(auditor)
	server := daemon.NewServer(engine, sandboxAPI)
	// Wire the request-scoped key provider so the gRPC handlers can hand the
	// controller-delivered encryption key to the engine for the duration of a
	// CreateTemplate/Fork call. Same instance the engine reads from.
	if reqKeyProvider != nil {
		server.SetKeyProvider(reqKeyProvider)
	}

	// Start gRPC server (controller communication)
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forkd: failed to listen on %s: %v\n", listenAddr, err)
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(grpcOpts...)
	daemon.RegisterForkDaemonServer(grpcServer, server)

	// Start HTTP server (metrics + sandbox exec/files API). When CAS
	// distribution is enabled, ServeHTTP also starts the dedicated token-gated
	// TLS CAS listener (--cas-listen) in its own goroutine; the sandbox HTTP API
	// scheme stays unchanged so SDK clients are unaffected.
	go daemon.ServeHTTP(httpAddr, engine, sandboxAPI, casServing)

	// Periodically refresh the metering gauges /metrics serves so they report
	// LIFETIME memory, not the fork-time footprint (fork-correctness Row 5, issue
	// #3). Without this the mitos_memory_unique_bytes gauge is never populated.
	// Bound to a cancelable context cancelled on shutdown below. interval 0 uses
	// the daemon default.
	metricsCtx, metricsCancel := context.WithCancel(context.Background())
	defer metricsCancel()
	go server.SampleMetrics(metricsCtx, 0)

	// Periodic CAS garbage collection (#464): the content-addressed store evicts
	// unpinned (deleted-template) chunks down to a low watermark whenever the
	// data-dir filesystem crosses the high watermark, so orphaned chunks cannot
	// grow unbounded and trip node DiskPressure. Pinned active-template chunks are
	// never evicted. Cancelled on shutdown via metricsCtx.
	if casGCStore != nil && casGCInterval > 0 {
		go casgc.Run(metricsCtx, casGCStore, dataDir, casGCInterval, casDiskHigh, casDiskLow, casgc.DiskUsage,
			func(format string, args ...any) { fmt.Fprintf(os.Stderr, "forkd: "+format+"\n", args...) })
	}

	// Start the controlled DNS resolver (node-level Runnable) when enabled. It
	// binds the resolver IP on port 53 (udp + tcp); a listen failure is fatal
	// because name-based egress would silently not work otherwise.
	if dnsProxyServer != nil {
		dnsAddr := net.JoinHostPort(dnsResolver, "53")
		go func() {
			fmt.Printf("forkd: DNS resolver on %s\n", dnsAddr)
			if err := dnsProxyServer.ListenAndServe(dnsAddr); err != nil {
				fmt.Fprintf(os.Stderr, "forkd: DNS resolver error: %v\n", err)
				os.Exit(1)
			}
		}()
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := dnsProxyServer.Shutdown(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "forkd: DNS resolver shutdown: %v\n", err)
			}
		}()
	}

	// Start the per-sandbox egress proxy (node-level Runnable) when enabled. It
	// binds the proxy port on every interface so each fork's DNATed gateway
	// destination lands here; a listen failure is fatal because egress would
	// silently not work otherwise.
	if egressProxyServer != nil {
		go func() {
			fmt.Printf("forkd: egress proxy on %s\n", proxyListenAddr)
			if err := egressProxyServer.ListenAndServe(proxyListenAddr); err != nil {
				fmt.Fprintf(os.Stderr, "forkd: egress proxy error: %v\n", err)
				os.Exit(1)
			}
		}()
		defer func() {
			if err := egressProxyServer.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "forkd: egress proxy shutdown: %v\n", err)
			}
		}()
	}

	// Start the host-side TLS SNI egress filter (node-level Runnable) when enabled.
	// It binds the wildcard on the SNI proxy port so transparently-redirected guest
	// TLS lands here; a listen failure is fatal because the SNI filter would
	// silently not enforce otherwise.
	if sniProxyServer != nil {
		go func() {
			fmt.Printf("forkd: TLS SNI egress filter on %s\n", sniListenAddr)
			if err := sniProxyServer.ListenAndServe(sniListenAddr); err != nil {
				fmt.Fprintf(os.Stderr, "forkd: SNI egress filter error: %v\n", err)
				os.Exit(1)
			}
		}()
		defer func() {
			if err := sniProxyServer.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "forkd: SNI egress filter shutdown: %v\n", err)
			}
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		fmt.Printf("forkd: gRPC on %s, HTTP on %s\n", listenAddr, httpAddr)
		if err := grpcServer.Serve(lis); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: gRPC error: %v\n", err)
			os.Exit(1)
		}
	}()

	<-stop
	fmt.Println("forkd: shutting down")
	grpcServer.GracefulStop()
}

// requireTLSForEncryption is the fail-closed guard for at-rest encryption: the
// controller delivers the per-template key over the gRPC request, so the
// channel must be mTLS. It returns a fatal error when encryption is enabled but
// the gRPC server is not TLS-configured (the --tls-cert/--tls-key/--tls-ca
// flags that drive the mTLS server are absent), and nil otherwise (encryption
// off, or encryption on with TLS configured). The error carries actionable
// remediation: configure the TLS flags and enable PKI bootstrap.
func requireTLSForEncryption(enableEnc, tlsConfigured bool) error {
	if enableEnc && !tlsConfigured {
		return fmt.Errorf("--enable-encryption requires mTLS: the controller delivers the encryption key over the gRPC request, which must not travel over an insecure channel; set --tls-cert, --tls-key, and --tls-ca (and run the controller with PKI bootstrap) or disable encryption")
	}
	return nil
}

// serverHTTPTLSConfig builds the TLS config the HTTP server uses for the
// token-gated CAS surface. It reuses forkd's own mTLS cert pair and requires a
// verified client certificate (a peer forkd or the controller), so CAS pulls
// ride forkd-to-forkd mTLS and the peer token is the additional gate. A bad
// pull token is rejected by the middleware; a peer without a CA-signed cert is
// rejected at the TLS handshake.
func serverHTTPTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error) {
	certPEM, keyPEM, caPEM, err := readTLSFiles(certPath, keyPath, caPath)
	if err != nil {
		return nil, err
	}
	return pki.ServerTLSConfig(certPEM, keyPEM, caPEM)
}

// pullHTTPClient builds the HTTP client PullTemplate dials a holder forkd's CAS
// with. It presents forkd's own client identity and trusts the control-plane
// CA. The pinned ServerName (pki.ServerName) means the holder's serving cert
// must carry that SAN; per-node SAN pinning is a follow-up tracked with the
// per-pull token work.
func pullHTTPClient(certPath, keyPath, caPath string) (*http.Client, error) {
	certPEM, keyPEM, caPEM, err := readTLSFiles(certPath, keyPath, caPath)
	if err != nil {
		return nil, err
	}
	clientTLS, err := pki.ClientTLSConfig(certPEM, keyPEM, caPEM)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: clientTLS},
		Timeout:   30 * time.Minute,
	}, nil
}

// readTLSFiles reads the three mTLS PEM files. It does not log their contents.
func readTLSFiles(certPath, keyPath, caPath string) (certPEM, keyPEM, caPEM []byte, err error) {
	if certPEM, err = os.ReadFile(certPath); err != nil {
		return nil, nil, nil, fmt.Errorf("read --tls-cert: %w", err)
	}
	if keyPEM, err = os.ReadFile(keyPath); err != nil {
		return nil, nil, nil, fmt.Errorf("read --tls-key: %w", err)
	}
	if caPEM, err = os.ReadFile(caPath); err != nil {
		return nil, nil, nil, fmt.Errorf("read --tls-ca: %w", err)
	}
	return certPEM, keyPEM, caPEM, nil
}

// grpcServerOptions builds transport security for the controller-facing
// gRPC listener. All three TLS flags set means mTLS with controller
// identity enforcement; a partial set is a configuration error.
//
// FAIL CLOSED: the gRPC control surface delivers secrets and drives forks, so
// with no TLS flags it REFUSES to start (returns an error naming the missing
// flags and the opt-in) UNLESS allowInsecure (--allow-insecure-grpc) is
// explicitly set, which keeps the legacy insecure-with-loud-warning behavior for
// local development only. The shipped DaemonSet always sets TLS, so production is
// unaffected; this only stops a silent-insecure misconfig.
func grpcServerOptions(certPath, keyPath, caPath string, allowInsecure bool) ([]grpc.ServerOption, error) {
	set := 0
	for _, p := range []string{certPath, keyPath, caPath} {
		if p != "" {
			set++
		}
	}
	switch set {
	case 0:
		if !allowInsecure {
			return nil, fmt.Errorf("refusing to serve gRPC without mTLS: the control surface delivers secrets and drives forks, so it fails closed by default; set --tls-cert, --tls-key, and --tls-ca (and run the controller with PKI bootstrap), or pass --allow-insecure-grpc to opt in to an UNAUTHENTICATED server for local development only")
		}
		fmt.Fprintln(os.Stderr, "forkd: gRPC is UNAUTHENTICATED (--allow-insecure-grpc set); supply --tls-cert/--tls-key/--tls-ca for any non-development deployment (threat model section 3)")
		return nil, nil
	case 3:
		// fall through to TLS setup below
	default:
		return nil, fmt.Errorf("--tls-cert, --tls-key, and --tls-ca must be set together")
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read --tls-cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read --tls-key: %w", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read --tls-ca: %w", err)
	}
	cfg, err := pki.ServerTLSConfig(certPEM, keyPEM, caPEM)
	if err != nil {
		return nil, fmt.Errorf("build server TLS config: %w", err)
	}
	return []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(cfg)),
		grpc.UnaryInterceptor(daemon.RequireControllerIdentity),
		grpc.StreamInterceptor(daemon.RequireControllerIdentityStream),
	}, nil
}

// defaultDNSResolverIP is the node-wide resolver address used when DNS egress
// is enabled and --dns-resolver is not set. It is an IPv4 link-local address:
// the host binds the controlled resolver here and every sandbox chain allows
// udp/tcp 53 to it, so a single address serves every per-/30 sandbox (the
// proxy attributes each query by the source guest IP, not by the resolver IP).
// The operator must ensure this address is reachable from the sandbox subnet
// (for example bound on the host so the per-sandbox gateway routes to it).
const defaultDNSResolverIP = "169.254.1.1"

// dnsProxyTTLFloor is the minimum lifetime of a pinned (ip . port) element. A
// very short upstream TTL is raised to this floor so a pin does not expire
// before the guest opens its connection.
const dnsProxyTTLFloor = 30 * time.Second

// buildDNSProxy constructs the controlled resolver: it pins resolved IPs into
// each sandbox's nft set via an exec-based nft runner, attributes queries to a
// tap through the allocator's guest-IP lookup, and forwards allowed queries to
// the resolved upstream.
func buildDNSProxy(registry *dnsproxy.Registry, alloc *netconf.Allocator, resolverIP, upstream string) *dnsproxy.Server {
	pinner := dnsproxy.NewNftPinner(func(argv []string) error {
		if len(argv) == 0 {
			return fmt.Errorf("empty nft command")
		}
		cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // fixed nft argv built from validated addresses/ports
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", argv[0], err, string(out))
		}
		return nil
	})
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// An explicit --dns-upstream may be a comma-separated list (failover order);
	// an empty value derives a single upstream from /etc/resolv.conf.
	upstreams := dnsproxy.ParseUpstreams(upstream)
	if len(upstreams) == 0 {
		upstreams = []string{resolvedUpstream("")}
	}
	return dnsproxy.NewServer(registry, pinner, upstreams, dnsProxyTTLFloor, alloc.TapForGuestIP, logger)
}

// netEgressDialer opens upstream sockets through the host's net.Dialer so the
// host process owns every upstream connection: a forked guest never inherits an
// already-open upstream. A bounded dial timeout stops a slow upstream from
// pinning a host goroutine indefinitely.
type netEgressDialer struct {
	d net.Dialer
}

func (n netEgressDialer) Dial(ctx context.Context, hostport string) (net.Conn, error) {
	return n.d.DialContext(ctx, "tcp", hostport)
}

// redactingEgressLogger records egress events with sandbox ID, host:port, and
// byte counts ONLY: never headers, bodies, paths, query strings, or auth
// values. It satisfies egressproxy.Logger.
type redactingEgressLogger struct {
	log *slog.Logger
}

func (r redactingEgressLogger) Egress(sandboxID, hostport string, bytesUp, bytesDown int64) {
	r.log.Info("egress", "sandbox", sandboxID, "hostport", hostport, "bytes_up", bytesUp, "bytes_down", bytesDown)
}

// Deny records a destination refused by the proxy's hard denylist (IMDS/SSRF
// floor). Like Egress it logs ONLY the sandbox ID and host:port: never headers,
// paths, query strings, or auth values.
func (r redactingEgressLogger) Deny(sandboxID, hostport string) {
	r.log.Info("egress_denied", "sandbox", sandboxID, "hostport", hostport)
}

// buildEgressProxy constructs the per-node HTTP forward proxy: it attributes
// each connection to a sandbox by source IP via the registry, dials every
// upstream host-side through a bounded-timeout net.Dialer, and logs only the
// sandbox ID, host:port, and byte counts (no headers, paths, or secrets).
func buildEgressProxy(registry *egressproxy.Registry) *egressproxy.Proxy {
	dialer := netEgressDialer{d: net.Dialer{Timeout: 30 * time.Second}}
	logger := redactingEgressLogger{log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	return egressproxy.NewProxy(registry, dialer, logger)
}

// sniEgressLogger records TLS SNI egress decisions. The SNI server name is NOT a
// secret (it travels in cleartext in the ClientHello on the wire, exactly like
// the DNS query name), so it may be logged; no bytes after the ClientHello are
// ever inspected or logged. It satisfies sniproxy.Logger.
type sniEgressLogger struct {
	log *slog.Logger
}

func (l sniEgressLogger) Allow(sandboxID, serverName string, port int, bytesUp, bytesDown int64) {
	l.log.Info("sni_egress", "sandbox", sandboxID, "sni", serverName, "port", port, "bytes_up", bytesUp, "bytes_down", bytesDown)
}

func (l sniEgressLogger) Deny(sandboxID, serverName string, port int, reason string) {
	l.log.Info("sni_egress_denied", "sandbox", sandboxID, "sni", serverName, "port", port, "reason", reason)
}

// buildSNIProxy constructs the host-side TLS SNI egress filter. It enforces the
// SAME per-sandbox domain allowlist the DNS resolver holds (allowReg, via
// sniproxy.RegistryAllowlist, reusing its exact/anchored-wildcard matcher),
// attributes each connection to a sandbox by source IP (attribReg, the egress
// proxy registry), dials the original destination host-side through the same
// bounded-timeout net.Dialer as the egress proxy, and logs only the sandbox ID,
// SNI, port, and byte counts.
func buildSNIProxy(allowReg *dnsproxy.Registry, attribReg *egressproxy.Registry) *sniproxy.Proxy {
	dialer := netEgressDialer{d: net.Dialer{Timeout: 30 * time.Second}}
	logger := sniEgressLogger{log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	return sniproxy.NewProxy(attribReg, sniproxy.RegistryAllowlist{Registry: allowReg}, dialer, logger)
}

// resolvedUpstream returns the upstream resolver address for the proxy. An
// explicit --dns-upstream wins; otherwise the first nameserver from
// /etc/resolv.conf is used, falling back to 1.1.1.1:53. A nameserver without a
// port gets :53 appended.
func resolvedUpstream(upstream string) string {
	if upstream != "" {
		return upstream
	}
	if ns := firstResolvConfNameserver("/etc/resolv.conf"); ns != "" {
		return ns
	}
	return "1.1.1.1:53"
}

// firstResolvConfNameserver returns the first `nameserver <ip>` from path as a
// host:port (appending :53 when no port is present), or "" when none is found
// or the file cannot be read.
func firstResolvConfNameserver(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			ns := fields[1]
			if _, _, err := net.SplitHostPort(ns); err != nil {
				ns = net.JoinHostPort(ns, "53")
			}
			return ns
		}
	}
	return ""
}
