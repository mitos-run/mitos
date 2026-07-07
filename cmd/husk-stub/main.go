// Command husk-stub is the single-VM husk process: it brings up a DORMANT
// Firecracker VMM at start, then listens on a control Unix socket and ACTIVATES
// the VM in place by loading a snapshot when an activate request arrives. One
// husk-stub process owns exactly one VM.
//
// A husk pod is LONG-LIVED. After a successful activate the process KEEPS the
// VM alive and serving the sandbox; it tears the VM down ONLY on a terminate
// signal (SIGINT/SIGTERM) or context cancel. It does not exit (and does not
// kill the VM) just because an activate completed.
//
// The activate path drives a VMM and FAILS CLOSED: a snapshot-load or
// guest-readiness failure is reported as an error result and the VM is left
// unusable rather than reported as live. All lifecycle logging goes to stderr
// and never includes secrets.
//
// With --activate the binary instead acts as a CONTROL CLIENT: it connects to
// an already-serving stub's --control-socket, sends one ActivateRequest for
// --snapshot-dir, prints the ActivateResult as JSON on stdout, and exits 0 only
// when the result is OK. This is the in-CI driver for the activation-latency
// proof; it spawns no VMM of its own. With --control-addr (plus the TLS flags)
// instead of --control-socket the activate client drives the NETWORK mTLS
// control channel via the controller's ActivateHuskPod, the slice-2 transport.
//
// With --emit-certs DIR the binary issues a fresh internal/pki test CA and the
// two control plane leaves (the husk server leaf with the pki.ServerName SAN,
// the controller client leaf with the pki.ControllerName SAN) plus an
// independent wrong-CA controller leaf for negative mTLS tests, writes them as
// PEM files under DIR, and exits. It spawns no VMM. This keeps the CI cert SANs
// in lockstep with the names the husk control channel pins and authorizes.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/daemon"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/husk"
	"mitos.run/mitos/internal/metering"
	"mitos.run/mitos/internal/pki"
	"mitos.run/mitos/internal/snapcompat"
)

// huskSandboxID is the stable sandbox id the husk-stub registers its single
// activated VM under in the daemon.SandboxAPI. The husk pod owns exactly one VM,
// so one fixed id is sufficient; the controller addresses the in-pod API by
// podIP:port, and the per-sandbox bearer token (not the id) is the auth gate.
const huskSandboxID = "husk"

// huskVMMPingInterval and huskVMMPingFailures tune the dormant/active VMM
// liveness monitor (issue #527). A dead Firecracker is declared after
// huskVMMPingFailures consecutive failed pings spaced huskVMMPingInterval apart
// (about 6s), well inside the controller's readiness tolerance, while the
// consecutive-failure threshold absorbs a transient blip (e.g. a slow API call
// during Activate) without flapping the pod.
const (
	huskVMMPingInterval = 2 * time.Second
	huskVMMPingFailures = 3
)

// huskVMIDPattern is the allowlist the --vm-id flag must satisfy before it is
// joined into the per-activation rootfs CoW clone path (and handed to
// firecracker.StartVM, which applies the same constraint). It forbids path
// separators and traversal sequences, so a per-pod id can never escape the CoW
// directory. It is intentionally identical to firecracker's internal vmIDPattern
// (a DNS-1123-compatible allowlist that a Kubernetes pod name satisfies).
var huskVMIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// kvFlag collects repeatable KEY=VALUE flags into a map. It is used for --env
// and --secret. The String method NEVER renders the values: a --secret flag's
// values must not leak via usage/error output, so String reports counts only.
type kvFlag struct {
	pairs map[string]string
}

func (f *kvFlag) String() string {
	// Keys and values are intentionally omitted: a secret value must never be
	// printed. Report only how many pairs were collected.
	return fmt.Sprintf("(%d pairs)", len(f.pairs))
}

func (f *kvFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		// The error mentions only that the form is wrong, never the value, so a
		// malformed --secret does not echo its payload.
		return fmt.Errorf("expected KEY=VALUE")
	}
	if f.pairs == nil {
		f.pairs = make(map[string]string)
	}
	f.pairs[k] = val
	return nil
}

// orNil returns the collected map, or nil when empty, so an absent flag threads
// through as a nil map rather than an empty one.
func (f *kvFlag) orNil() map[string]string {
	if len(f.pairs) == 0 {
		return nil
	}
	return f.pairs
}

// loadSecretFile reads KEY=VALUE lines (one per line, blank and #-comment lines
// skipped) into m, creating it if nil. Secret values are never logged: parse
// errors mention the line number only, never the line content.
func loadSecretFile(path string, m map[string]string) (map[string]string, error) {
	file, err := os.Open(path) //nolint:gosec // operator-supplied secret file path
	if err != nil {
		return m, fmt.Errorf("open secret file: %w", err)
	}
	defer file.Close()

	if m == nil {
		m = make(map[string]string)
	}
	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		k, v, ok := strings.Cut(text, "=")
		if !ok || k == "" {
			return m, fmt.Errorf("secret file line %d: expected KEY=VALUE", line)
		}
		m[k] = v
	}
	if err := scanner.Err(); err != nil {
		return m, fmt.Errorf("read secret file: %w", err)
	}
	return m, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "husk-stub: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		firecrackerBin  = flag.String("firecracker", "/usr/local/bin/firecracker", "path to the firecracker binary")
		kernel          = flag.String("kernel", "", "path to the guest kernel image")
		workdir         = flag.String("workdir", "", "per-VM working directory (firecracker cmd.Dir; vsock UDS is bound relative to it)")
		controlSocket   = flag.String("control-socket", "", "path to the control Unix socket to listen on for activate requests")
		controlListen   = flag.String("control-listen", "", "TCP address (host:port) to serve the mTLS NETWORK control on for activate requests; the husk pod uses this. Requires --tls-cert/--tls-key/--tls-ca; refuses to serve without them")
		sandboxListen   = flag.String("sandbox-listen", ":9091", "TCP address (host:port) to serve the in-pod sandbox HTTP API (exec/files) on after activation; gated by the per-sandbox bearer token delivered over the control channel. The claim's Status.Endpoint is podIP:this port")
		tlsCert         = flag.String("tls-cert", "", "path to the husk server certificate PEM (mTLS); the forkd server leaf identity")
		tlsKey          = flag.String("tls-key", "", "path to the husk server key PEM (mTLS)")
		tlsCA           = flag.String("tls-ca", "", "path to the control plane CA certificate PEM (mTLS); used to verify the controller client certificate")
		vcpus           = flag.Int("vcpus", 1, "guest vCPU count")
		memMiB          = flag.Int("mem-mib", 512, "guest memory in MiB")
		activate        = flag.Bool("activate", false, "act as a control CLIENT: connect to --control-socket (or --control-addr over mTLS), send one activate request for --snapshot-dir, print the result, and exit (spawns no VMM)")
		forkSnapshot    = flag.Bool("fork-snapshot", false, "act as a control CLIENT: connect to --control-addr over mTLS, send one fork-snapshot op for --fork-id writing into the source pod's forks dir, print the result, and exit (spawns no VMM)")
		forkID          = flag.String("fork-id", "", "fork-snapshot client mode: the node-local fork id (the forks dir leaf the source stub writes the snapshot under)")
		controlAddr     = flag.String("control-addr", "", "activate client mode: TCP address (host:port) of a husk pod's mTLS NETWORK control to activate over; uses ActivateHuskPod and requires --tls-cert/--tls-key/--tls-ca. Mutually exclusive with --control-socket")
		snapshotDir     = flag.String("snapshot-dir", "", "activate client mode: the template snapshot directory (expects snapshot/{mem,vmstate} layout) to activate")
		secretFile      = flag.String("secret-file", "", "activate client mode: path to a KEY=VALUE secret file (one per line) delivered to the guest; values are never logged")
		tokenFile       = flag.String("token-file", "", "activate client mode: path to a file holding the per-sandbox bearer token delivered over the control channel; the stub gates the in-pod sandbox API on it. The value is a secret and is never logged")
		emitCerts       = flag.String("emit-certs", "", "issue an internal/pki test CA and control plane leaves into this directory, then exit (spawns no VMM): ca.crt, server.{crt,key} (pki.ServerName SAN), controller.{crt,key} (pki.ControllerName SAN), wrong-ca.crt + wrong-controller.{crt,key} (a different CA, for negative mTLS tests)")
		manifest        = flag.String("manifest", "", "path to the recorded CAS manifest for the template snapshot. When set, the stub re-verifies the snapshot (digest integrity + snapcompat) against it BEFORE loading, fail-closed (the husk mirror of forkd's verify-on-load gate, issues #9 and #32). The manifest is a content address, not a secret")
		allowUnverified = flag.Bool("allow-unverified-snapshots", false, "DEVELOPMENT ONLY: skip the activate-time snapshot integrity + compatibility verification, mirroring forkd's --allow-unverified-snapshots. Default false keeps verify enforced; a missing manifest/digest or a failed check then refuses the activate (fail-closed)")
		expectedDigest  = flag.String("expected-digest", "", "activate client mode: the template's recorded CAS manifest digest, threaded into the ActivateRequest so the serving stub verifies the snapshot against it (a content address, not a secret)")
		rootfsCoWDir    = flag.String("rootfs-cow-dir", "", "directory on the SAME node filesystem as the template rootfs where this activation's copy-on-write rootfs clone is written (reflink where supported, full copy otherwise). Empty keeps the prior behavior of writing the shared template rootfs in place. A content address, not a secret")
		templateRootfs  = flag.String("template-rootfs", "", "host path of the template rootfs.ext4 to clone per activation. Empty (with --rootfs-cow-dir) disables the per-activation clone")
		vmID            = flag.String("vm-id", huskSandboxID, "the per-pod VM id. It scopes this pod's per-activation rootfs CoW clone path (<rootfs-cow-dir>/<vm-id>/rootfs.ext4), so two husk pods sharing the node CoW hostPath never collide on, overwrite, or delete each other's clone. The controller passes the pod name (downward API metadata.name); empty falls back to the legacy fixed id. A node-local identifier, not a secret")
		forksDir        = flag.String("forks-dir", "", "directory the node forks dir is mounted at, where a fork-snapshot op writes <forks-dir>/<fork-id>/{mem,vmstate}. When set, the serving stub confines fork-snapshot and remove-fork-snapshot writes to within it (fail-closed: a request naming a path outside it is refused). Empty leaves the prior behavior (the request's snapshot dir is used as-is). A node-local path, not a secret")
		casDir          = flag.String("cas-dir", "", "directory the node content-addressed store is mounted at (read-write). When set, the dehydrate-workspace control op captures the active VM's /workspace into it and returns the manifest digest, and the hydrate-workspace op restores a manifest back into the VM. Empty disables the workspace ops (they fail closed). A node-local path, not a secret; workspace content is never logged")
		dnsUpstream     = flag.String("dns-upstream", "", "Comma-separated real DNS resolver list (host:port) the per-pod egress proxy forwards allowlisted queries to, tried in failover order (recommended: 1.1.1.1:53,8.8.8.8:53). Empty disables name-based egress (IP-only allowlist mode).")
		enableEgress    = flag.Bool("enable-egress-filter", true, "Program the in-pod nftables egress filter (default-deny + metadata block) for the activated VM. Requires NET_ADMIN in the pod netns. Default true (the husk isolation guarantee).")
		multiVM         = flag.Bool("multi-vm", false, "EXPERIMENTAL, default false: opt into the multi-VM-per-pod execution mode (#764), running many same-tenant CoW forks inside ONE husk pod instead of one pod per VM. Increment 1 wires only a default-off scaffold; false (the default, and the only value the controller emits) keeps the single-VM behavior byte-for-byte. Not a secret.")
		liveCowFork     = flag.Bool("live-cow-fork", false, "EXPERIMENTAL, default false: opt a CO-LOCATED fork child onto the live copy-on-write path (share the PARENT's resident guest memory via the patched Firecracker memfd + userfaultfd write-protect handler) instead of restoring from the disk fork snapshot (milestone m4b). SEPARATE from --multi-vm so it can be deployed off and canaried independently. Off (the default, and the only value the controller emits unless the operator opts in) keeps the co-located fork on the disk snapshot restore byte-for-byte. No-op off Linux (userfaultfd write-protect is Linux-only), failing closed to the disk restore. Not a secret.")
	)
	var envFlag, secretFlag kvFlag
	flag.Var(&envFlag, "env", "activate client mode: repeatable KEY=VALUE guest env var")
	flag.Var(&secretFlag, "secret", "activate client mode: repeatable KEY=VALUE guest secret; values are never logged")
	flag.Parse()

	if *emitCerts != "" {
		return emitTestCerts(*emitCerts)
	}

	if *activate {
		secrets := secretFlag.orNil()
		if *secretFile != "" {
			var err error
			secrets, err = loadSecretFile(*secretFile, secrets)
			if err != nil {
				return err
			}
		}
		// The per-sandbox bearer token is read from a FILE, not a flag, so the
		// secret never appears in the process argv. Its value is never logged.
		token := ""
		if *tokenFile != "" {
			raw, err := os.ReadFile(*tokenFile) //nolint:gosec // operator-supplied token file path
			if err != nil {
				return fmt.Errorf("read token file: %w", err)
			}
			token = strings.TrimSpace(string(raw))
		}
		if *controlAddr != "" {
			if *controlSocket != "" {
				return fmt.Errorf("--control-addr and --control-socket are mutually exclusive")
			}
			return runNetworkActivateClient(*controlAddr, *snapshotDir, *expectedDigest, *tlsCert, *tlsKey, *tlsCA, envFlag.orNil(), secrets, token)
		}
		return runActivateClient(*controlSocket, *snapshotDir, *expectedDigest, envFlag.orNil(), secrets, token)
	}

	if *forkSnapshot {
		if *controlAddr == "" {
			return fmt.Errorf("--fork-snapshot requires --control-addr")
		}
		if *forkID == "" {
			return fmt.Errorf("--fork-snapshot requires --fork-id")
		}
		if *tlsCert == "" || *tlsKey == "" || *tlsCA == "" {
			return fmt.Errorf("--fork-snapshot requires --tls-cert, --tls-key, and --tls-ca")
		}
		certPEM, err := os.ReadFile(*tlsCert)
		if err != nil {
			return fmt.Errorf("read --tls-cert: %w", err)
		}
		keyPEM, err := os.ReadFile(*tlsKey)
		if err != nil {
			return fmt.Errorf("read --tls-key: %w", err)
		}
		caPEM, err := os.ReadFile(*tlsCA)
		if err != nil {
			return fmt.Errorf("read --tls-ca: %w", err)
		}
		tlsConf, err := pki.ClientTLSConfig(certPEM, keyPEM, caPEM)
		if err != nil {
			return fmt.Errorf("build controller client TLS config: %w", err)
		}
		res, err := controller.ForkSnapshotOnHusk(context.Background(), *controlAddr, tlsConf, husk.ForkSnapshotRequest{
			ForkID:      *forkID,
			SnapshotDir: filepath.Join("/var/lib/mitos/forks", *forkID),
		})
		if err != nil {
			return fmt.Errorf("fork-snapshot over network control: %w", err)
		}
		enc := json.NewEncoder(os.Stdout)
		if err := enc.Encode(res); err != nil {
			return fmt.Errorf("encode fork-snapshot result: %w", err)
		}
		if !res.OK {
			return fmt.Errorf("fork-snapshot failed: %s", res.Error)
		}
		return nil
	}

	if *workdir == "" {
		return fmt.Errorf("--workdir is required")
	}
	if *controlSocket == "" && *controlListen == "" {
		return fmt.Errorf("one of --control-socket or --control-listen is required")
	}

	// Fail closed: the network control channel activates a VM with tenant
	// secrets, so it MUST be mTLS-authenticated. Refuse to serve --control-listen
	// unless all three TLS flags are set, mirroring forkd's encryption guard. An
	// unauthenticated activate channel that delivers secrets is unacceptable.
	tlsConfigured := *tlsCert != "" && *tlsKey != "" && *tlsCA != ""
	if *controlListen != "" && !tlsConfigured {
		return fmt.Errorf("--control-listen requires --tls-cert, --tls-key, and --tls-ca: refusing to serve an unauthenticated network control channel that delivers secrets")
	}

	var controlTLS *tls.Config
	if *controlListen != "" {
		var err error
		controlTLS, err = buildControlTLS(*tlsCert, *tlsKey, *tlsCA)
		if err != nil {
			return err
		}
	}

	if err := os.MkdirAll(*workdir, 0o755); err != nil {
		return fmt.Errorf("create workdir: %w", err)
	}

	// The VM id is PER-POD: it scopes this pod's per-activation rootfs CoW clone
	// path (<rootfs-cow-dir>/<id>/rootfs.ext4) so two husk pods that share the
	// node's CoW hostPath never write, overwrite, or delete the same clone file.
	// The controller passes the pod name via the downward API; an empty flag
	// keeps the legacy fixed id (single-pod-per-node assumption). The id is joined
	// into a filesystem path here and in firecracker.StartVM, so validate it at
	// this trust boundary up front (the same DNS-1123-compatible allowlist the
	// firecracker client enforces) rather than relying on call ordering.
	vmConfigID := *vmID
	if vmConfigID == "" {
		vmConfigID = huskSandboxID
	}
	if !huskVMIDPattern.MatchString(vmConfigID) {
		return fmt.Errorf("--vm-id %q is invalid: must match %s", vmConfigID, huskVMIDPattern.String())
	}

	cfg := firecracker.VMConfig{
		ID:             vmConfigID,
		FirecrackerBin: *firecrackerBin,
		WorkDir:        *workdir,
		KernelPath:     *kernel,
		SocketPath:     filepath.Join(*workdir, "firecracker.sock"),
		VcpuCount:      *vcpus,
		MemSizeMib:     *memMiB,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The in-pod sandbox HTTP API (exec/files), reusing the SAME daemon.SandboxAPI
	// forkd serves, PLUS an unauthenticated GET /v1/metering route so the usage
	// collector can meter this pod's one VM (issue #613). The metering route is
	// served from DORMANT (before activate) so a scrape during the warm window is a
	// clean empty report, mirroring forkd's operational mux; the exec/files routes
	// fail closed (401) until a sandbox+token is registered at activate. The token
	// is a secret and is never logged. The vsock UDS dir is the per-VM workdir (the
	// agent UDS lives there); EnableUnixFallback is deliberately NOT set, matching
	// forkd.
	sandboxAPI := daemon.NewSandboxAPI(*workdir)
	// Single-sandbox mode: a husk pod serves exactly ONE VM, registered locally
	// under huskSandboxID, but the SDK addresses this in-pod API with the claim's
	// status.sandboxID (the husk pod name), which never equals huskSandboxID. In
	// single-sandbox mode the per-sandbox bearer token is the auth gate: every
	// request is validated against the one registered token regardless of its
	// "sandbox" id and routed to the single VM, fixing the cluster-e2e 401 while
	// keeping forkd's multi-sandbox per-id gate untouched (forkd never sets this).
	// A wrong/absent token is still rejected and an untokened activate stays
	// fail-closed.
	sandboxAPI.SetSingleSandbox(huskSandboxID)

	// The metering source reads the stub's per-VM report. stub is assigned by
	// husk.New below, AFTER the sandbox server goroutine is already serving
	// /v1/metering, so the handoff must be an atomic publication: a plain
	// captured pointer would be a data race between the server goroutine's read
	// and main's later assignment. Until publication the source reports empty
	// (dormant pod, no VM yet).
	var stub *husk.Stub
	var publishedStub atomic.Pointer[husk.Stub]
	meteringSource := func() metering.Report {
		if s := publishedStub.Load(); s != nil {
			return s.Metering()
		}
		return metering.Report{}
	}
	onActivated, meteringSrv, err := startSandboxServer(ctx, sandboxAPI, *sandboxListen, meteringSource)
	if err != nil {
		return err
	}
	defer func() { _ = meteringSrv.Close() }()

	// Snapshot verify gate (fail-closed): detect this node's environment so the
	// stub can run snapcompat.Check on the recorded manifest, and pass the mounted
	// manifest path. With verify enforced (default) a snapshot tampered on the node
	// disk or incompatible with this node is refused before it is loaded, the same
	// gate forkd's Fork path applies (issues #9 and #32). --allow-unverified-
	// snapshots is the development escape hatch; the snapshot dir / manifest path
	// and the digest are content addresses, never secrets.
	var detectedEnv snapcompat.Environment
	if !*allowUnverified {
		var derr error
		detectedEnv, derr = snapcompat.DetectEnvironment(*firecrackerBin, snapcompat.ExecRunner, snapcompat.ProcCPUInfoReader)
		if derr != nil {
			return fmt.Errorf("detect host environment for snapshot compatibility check: %w (set --allow-unverified-snapshots to skip verification in development)", derr)
		}
	}

	stub = husk.New(cfg, husk.Options{
		OnActivated:     onActivated,
		ManifestPath:    *manifest,
		Env:             detectedEnv,
		AllowUnverified: *allowUnverified,
		// When the controller passes the snapshot dir + expected digest at
		// startup, the dormant pod verifies the snapshot during Prepare (pre-paid)
		// so the claim's Activate is just the load + handshake, not the re-hash.
		PrepareSnapshotDir:    *snapshotDir,
		PrepareExpectedDigest: *expectedDigest,
		// Per-activation rootfs CoW: clone the template rootfs to a per-pod file
		// on a writable co-located volume at Prepare and rebind the rootfs drive
		// to it at Activate, so concurrent activations of one template never
		// share or corrupt a single rootfs. Both empty keeps the prior in-place
		// shared-rootfs behavior.
		RootfsTemplatePath: *templateRootfs,
		RootfsCoWDir:       *rootfsCoWDir,
		// The node forks dir this pod mounts: the serving stub confines
		// fork-snapshot / remove-fork-snapshot writes to within it (fail-closed).
		ForksDir: *forksDir,
		// The node CAS this pod mounts read-write: the dehydrate-workspace op
		// persists a captured /workspace here and the hydrate-workspace op restores
		// from it. Empty disables the workspace ops (fail-closed).
		CASDir: *casDir,
		// EXPERIMENTAL multi-VM-per-pod mode (#764), default off: false keeps the
		// single-VM behavior byte-for-byte. When on, the stub multiplexes the
		// lifecycle over its per-VM instance map and derives each non-default VM's
		// OWN workdir + Firecracker/vsock socket nested under this base --workdir
		// (deriveVMConfig), so several Firecracker processes coexist in one pod
		// without colliding on the single fixed socket the single-VM path uses. No
		// production caller sets this yet; the controller is not wired to it.
		MultiVM: *multiVM,
		// LiveCowFork opts the co-located fork child onto the live copy-on-write path
		// (parent memory share + write-protect handler) instead of the disk snapshot
		// restore (milestone m4b). Default off; SEPARATE from --multi-vm so it
		// canaries independently. Off is byte-for-byte the disk co-location.
		LiveCowFork: *liveCowFork,
	})
	// Publish the constructed stub to the already-serving metering source.
	publishedStub.Store(stub)

	// In-pod egress filter wiring (the husk isolation guarantee). When enabled,
	// the stub programs the VM's tap + default-deny nftables egress chain in THIS
	// pod's network namespace at activate via an exec-based runner. nft and the
	// ip link commands need NET_ADMIN, which the husk pod has scoped to its own
	// netns. The combined stdout+stderr is folded into the error so a failed nft
	// apply carries actionable remediation text; no secret is on these argvs.
	if *enableEgress {
		stub.SetNetRunner(func(ctx context.Context, argv []string, stdin string) error {
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			if stdin != "" {
				cmd.Stdin = strings.NewReader(stdin)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("run %v: %w: %s", argv, err, string(out))
			}
			return nil
		})
		stub.SetDNSUpstream(*dnsUpstream)
		// Enable IPv4 forwarding so the kernel routes the guest /30 between the tap
		// and the pod uplink; the activate-time masquerade then SNATs it out.
		//
		// BEST EFFORT: in Kubernetes the app container JOINS the pod (pause)
		// network namespace rather than creating it, so the runtime leaves
		// /proc/sys/net read-only even with NET_ADMIN; the write then fails. In the
		// husk pod a short-lived privileged init container already set ip_forward=1
		// in the shared netns, so the read below sees "1" and this is a no-op. We
		// log and continue (never fail activation): with forwarding off the guest
		// cannot route out, so egress stays fully denied (fail closed) while the VM
		// and exec (vsock) still work. The non-k8s sandbox-server path, which owns
		// its netns, writes it here directly.
		stub.SetForwardingEnabler(func() error {
			const p = "/proc/sys/net/ipv4/ip_forward"
			if cur, rerr := os.ReadFile(p); rerr == nil && strings.TrimSpace(string(cur)) == "1" {
				return nil // already enabled (kubelet pod sysctl or host default)
			}
			if werr := os.WriteFile(p, []byte("1\n"), 0o644); werr != nil {
				fmt.Fprintf(os.Stderr, "husk-stub: could not enable ip_forward (%v); egress allowlists need net.ipv4.ip_forward=1 in the pod netns (node sysctl). Egress stays fully denied until then.\n", werr)
			}
			return nil
		})
	}

	fmt.Fprintln(os.Stderr, "husk-stub: preparing dormant VMM")
	if err := stub.Prepare(ctx); err != nil {
		return fmt.Errorf("prepare dormant VMM: %w", err)
	}
	fmt.Fprintf(os.Stderr, "husk-stub: dormant, state=%s\n", stub.State())

	// A husk pod is LONG-LIVED: it holds its active VM until the pod is
	// terminated. Tear the VMM down (reaping the firecracker process) ONLY on
	// shutdown, i.e. when Serve returns after a signal (SIGINT/SIGTERM) or ctx
	// cancel. We do NOT close right after a successful activate; the activated
	// VM must outlive activate so it can serve the sandbox.
	defer func() {
		if err := stub.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "husk-stub: close: %v\n", err)
		}
	}()

	// Liveness monitor: watch the dormant/active Firecracker VMM and, if it dies
	// (for example a snapshot load failed and left it defunct), exit non-zero so
	// the kubelet restarts the pod. Without this a dead VMM lingers behind a still
	// Ready pod (husk-stub PID 1 keeps the control listener open), the pool counts
	// it a warm slot, and every claim that lands on it fails connection-refused to
	// the dead firecracker socket (issue #527). Serve runs under monCtx so a
	// detected death also unblocks it; run() then returns monErr (exit 1) while the
	// deferred Close still reaps the VMM, and the kubelet (RestartPolicy Always)
	// restarts the container into a fresh Prepare that self-heals once the snapshot
	// is present.
	monCtx, monCancel := context.WithCancel(ctx)
	defer monCancel()
	var monErr error
	var monOnce sync.Once
	go func() {
		if err := stub.MonitorVMM(monCtx, huskVMMPingInterval, huskVMMPingFailures); err != nil {
			monOnce.Do(func() { monErr = err })
			fmt.Fprintf(os.Stderr, "husk-stub: %v; exiting so the kubelet restarts this pod\n", err)
			monCancel()
		}
	}()

	// The husk pod uses the mTLS NETWORK control: when --control-listen is set
	// (with TLS, enforced above), serve ServeTLS authorized to the controller
	// identity. The unix --control-socket path remains for the in-CI driver and
	// local use. Both Serve variants block, holding the active VM and returning
	// only on a signal (SIGINT/SIGTERM) or ctx cancel; the deferred Close then
	// kills the VM. The VM is alive and usable for the whole serving lifetime.
	if *controlListen != "" {
		ln, err := net.Listen("tcp", *controlListen)
		if err != nil {
			return fmt.Errorf("listen on control address %s: %w", *controlListen, err)
		}
		fmt.Fprintf(os.Stderr, "husk-stub: serving mTLS network control %s\n", *controlListen)
		if err := husk.ServeTLS(monCtx, ln, stub, controlTLS, husk.AuthorizeControllerIdentity); err != nil {
			return fmt.Errorf("serve network control: %w", err)
		}
		if monErr != nil {
			return monErr
		}
		fmt.Fprintf(os.Stderr, "husk-stub: shutting down, state=%s\n", stub.State())
		return nil
	}

	// Fresh control socket; a stale file from a prior run would block bind.
	_ = os.Remove(*controlSocket)
	ln, err := net.Listen("unix", *controlSocket)
	if err != nil {
		return fmt.Errorf("listen on control socket %s: %w", *controlSocket, err)
	}
	fmt.Fprintf(os.Stderr, "husk-stub: serving control socket %s\n", *controlSocket)

	if err := stub.Serve(monCtx, ln); err != nil {
		return fmt.Errorf("serve control socket: %w", err)
	}
	if monErr != nil {
		return monErr
	}
	fmt.Fprintf(os.Stderr, "husk-stub: shutting down, state=%s\n", stub.State())
	return nil
}

// huskMeteringHandler serves this husk pod's CoW-aware metering report as JSON on
// GET /v1/metering for the usage collector (issue #613). Like forkd's identical
// endpoint it is pod-scoped operational data (the same access class as /metrics
// and /healthz), NOT per-sandbox traffic, so it is mounted OUTSIDE the
// per-sandbox bearer middleware: a sandbox token grants no extra access here and
// none is required. src returns the empty report before activation and the
// single-VM report after; the report never carries a secret value.
func huskMeteringHandler(src func() metering.Report) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		report := src()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "husk-stub: encode metering report: %v\n", err)
		}
	})
}

// newSandboxMux builds the in-pod HTTP mux: the unauthenticated GET /v1/metering
// route in front of the token-gated /v1 sandbox API (exec/files). Registering the
// specific "GET /v1/metering" pattern before the "/" catch-all makes it take
// precedence, so the metering scrape is never routed through the per-sandbox
// bearer middleware, exactly as forkd's server.go mounts its identical endpoint
// ahead of the /v1/ sandbox handler. Every other path (including the Connect
// runtime paths the SDK uses) falls through to api.Handler() unchanged.
func newSandboxMux(api *daemon.SandboxAPI, meteringSrc func() metering.Report) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/metering", huskMeteringHandler(meteringSrc))
	mux.Handle("/", api.Handler())
	return mux
}

// startSandboxServer starts the in-pod sandbox HTTP server (metering + the
// token-gated exec/files API) on listenAddr NOW, during the dormant window, and
// returns the husk Stub OnActivated hook plus the running server. Serving from
// dormant means a metering scrape during the warm window is a clean empty report,
// not a connection refusal, while the exec/files routes fail closed (401) until a
// sandbox+token is registered at activate.
//
// FAIL CLOSED: if the listener cannot bind it returns an error so the pod exits
// and the kubelet restarts it (the pod never goes Ready), rather than advertising
// an endpoint that is not served. The returned OnActivated hook registers the
// activated VM (by its host vsock UDS path) and its per-sandbox bearer token; it
// returns an error (so Activate reports OK=false) when the VM cannot be
// registered, because a VM whose sandbox API is not reachable is not usable by a
// tenant. The bearer token is a SECRET: it is registered with the API but NEVER
// logged here.
func startSandboxServer(ctx context.Context, api *daemon.SandboxAPI, listenAddr string, meteringSrc func() metering.Report) (func(vsockPath, token string) error, *http.Server, error) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on sandbox address %s: %w", listenAddr, err)
	}
	// ReadHeaderTimeout bounds header parsing and IdleTimeout reaps keep-alive
	// connections between requests. Read and Write timeouts are deliberately NOT
	// set: this mux serves long-lived streaming exec and file transfers, and a
	// blanket deadline would sever them mid-stream (forkd's :9091 server makes
	// the same call).
	srv := &http.Server{
		Handler:           newSandboxMux(api, meteringSrc),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "husk-stub: serving sandbox API (metering unauthenticated, exec/files token-gated) %s\n", listenAddr)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "husk-stub: sandbox API server: %v\n", err)
		}
	}()
	// Shut the sandbox API down on stub shutdown (signal / ctx cancel), mirroring
	// how the control server returns and the VM is torn down.
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	onActivated := func(vsockPath, token string) error {
		// Register the activated VM and its bearer token. RegisterToken with an
		// empty token is a no-op: the API then fails closed (401) because
		// EnableUnixFallback/AllowTokenless are NOT set, so an activate that
		// delivered no token yields an unreachable-but-safe sandbox rather than an
		// open one.
		if err := api.RegisterSandbox(huskSandboxID, vsockPath); err != nil {
			return fmt.Errorf("register activated sandbox with in-pod API: %w", err)
		}
		api.RegisterStreamPath(huskSandboxID, vsockPath)
		api.RegisterToken(huskSandboxID, token)
		return nil
	}
	return onActivated, srv, nil
}

// buildControlTLS reads the husk server certificate, key, and the control plane
// CA, and builds the mTLS server config that requires and verifies the
// controller client certificate (pki.ServerTLSConfig). Secret material (the
// private key bytes) is never logged: errors name the flag and the underlying
// read/parse error only.
func buildControlTLS(certPath, keyPath, caPath string) (*tls.Config, error) {
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
		return nil, fmt.Errorf("build husk control TLS config: %w", err)
	}
	return cfg, nil
}

// runActivateClient connects to an already-serving stub's control socket, sends
// one ActivateRequest for snapshotDir, prints the ActivateResult as JSON on
// stdout, and returns an error (non-zero exit) when the result is not OK. It
// owns no VMM; it only drives the control protocol so CI can measure the
// activation latency and gate on a successful in-place activation. The
// snapshotDir carries no secrets, so it is safe to echo in the result.
//
// env and secrets are delivered to the guest by the stub after the restore
// handshake. Their VALUES are never logged here: only the result (OK, latency,
// vsock path, and any error, none of which carries a secret) is printed.
func runActivateClient(controlSocket, snapshotDir, expectedDigest string, env, secrets map[string]string, token string) error {
	if controlSocket == "" {
		return fmt.Errorf("--activate requires --control-socket")
	}
	if snapshotDir == "" {
		return fmt.Errorf("--activate requires --snapshot-dir")
	}

	conn, err := net.Dial("unix", controlSocket)
	if err != nil {
		return fmt.Errorf("dial control socket %s: %w", controlSocket, err)
	}
	defer conn.Close()

	if err := husk.WriteRequest(conn, husk.ActivateRequest{
		SnapshotDir:    snapshotDir,
		ExpectedDigest: expectedDigest,
		Env:            env,
		Secrets:        secrets,
		Token:          token,
	}); err != nil {
		return fmt.Errorf("send activate request: %w", err)
	}

	res, err := husk.ReadResult(conn)
	if err != nil {
		return fmt.Errorf("read activate result: %w", err)
	}

	// Emit the full result as JSON so CI can parse LatencyMs and VsockPath.
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(res); err != nil {
		return fmt.Errorf("encode activate result: %w", err)
	}

	if !res.OK {
		return fmt.Errorf("activate failed: %s", res.Error)
	}
	return nil
}

// runNetworkActivateClient drives the NETWORK mTLS control channel: it builds
// the controller client TLS config (pki.ClientTLSConfig, which pins the husk
// server identity and presents the controller client certificate), then calls
// the controller's ActivateHuskPod against addr. This is the in-CI driver for
// the slice-2 network-activation proof; it exercises the exact reconcile path
// the claim controller uses (ActivateHuskPod), not a bespoke client. It owns no
// VMM.
//
// The TLS flags are REQUIRED here: ActivateHuskPod refuses a nil tlsConf so the
// activation secrets are never sent over an unauthenticated channel. Secret and
// env VALUES are never logged: only the result (OK, latency, vsock path, and
// any error, none of which carries a secret) is printed.
func runNetworkActivateClient(addr, snapshotDir, expectedDigest, certPath, keyPath, caPath string, env, secrets map[string]string, token string) error {
	if snapshotDir == "" {
		return fmt.Errorf("--activate requires --snapshot-dir")
	}
	if certPath == "" || keyPath == "" || caPath == "" {
		return fmt.Errorf("--control-addr requires --tls-cert, --tls-key, and --tls-ca: refusing to send activation secrets over an unauthenticated channel")
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("read --tls-cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read --tls-key: %w", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return fmt.Errorf("read --tls-ca: %w", err)
	}
	tlsConf, err := pki.ClientTLSConfig(certPEM, keyPEM, caPEM)
	if err != nil {
		return fmt.Errorf("build controller client TLS config: %w", err)
	}

	res, err := controller.ActivateHuskPod(context.Background(), addr, tlsConf, husk.ActivateRequest{
		SnapshotDir:    snapshotDir,
		ExpectedDigest: expectedDigest,
		Env:            env,
		Secrets:        secrets,
		Token:          token,
	})
	if err != nil {
		return fmt.Errorf("activate over network control: %w", err)
	}

	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(res); err != nil {
		return fmt.Errorf("encode activate result: %w", err)
	}
	if !res.OK {
		return fmt.Errorf("activate failed: %s", res.Error)
	}
	return nil
}

// emitTestCerts issues a fresh internal/pki test CA and the control plane leaves
// into dir so the CI can stand up the mTLS network control channel with SANs
// that match exactly what the husk server pins and authorizes. Reusing
// internal/pki (not openssl) guarantees the husk server leaf carries
// pki.ServerName and the controller client leaf carries pki.ControllerName, the
// two names ServerTLSConfig/ClientTLSConfig and AuthorizeControllerIdentity
// depend on. It also emits an INDEPENDENT second CA and a controller leaf signed
// by it (wrong-ca.crt + wrong-controller.{crt,key}) so the negative mTLS test
// can prove a wrong-CA client is rejected. No secret material beyond these test
// keys is in scope; this command is for CI/test only.
func emitTestCerts(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}

	ca, err := pki.NewCA("husk-stub-test")
	if err != nil {
		return fmt.Errorf("create test CA: %w", err)
	}
	server, err := ca.Issue(pki.ServerName)
	if err != nil {
		return fmt.Errorf("issue server leaf: %w", err)
	}
	clientLeaf, err := ca.Issue(pki.ControllerName)
	if err != nil {
		return fmt.Errorf("issue controller leaf: %w", err)
	}

	// An INDEPENDENT CA and a controller leaf signed by it: the negative test
	// presents this leaf, whose SAN is correct but whose chain does not verify
	// against the husk server's trusted CA, so the mTLS handshake must reject it.
	wrongCA, err := pki.NewCA("husk-stub-test-wrong")
	if err != nil {
		return fmt.Errorf("create wrong test CA: %w", err)
	}
	wrongClient, err := wrongCA.Issue(pki.ControllerName)
	if err != nil {
		return fmt.Errorf("issue wrong-CA controller leaf: %w", err)
	}

	files := map[string][]byte{
		"ca.crt":               ca.CertPEM(),
		"server.crt":           server.CertPEM,
		"server.key":           server.KeyPEM,
		"controller.crt":       clientLeaf.CertPEM,
		"controller.key":       clientLeaf.KeyPEM,
		"wrong-ca.crt":         wrongCA.CertPEM(),
		"wrong-controller.crt": wrongClient.CertPEM,
		"wrong-controller.key": wrongClient.KeyPEM,
	}
	for name, pemBytes := range files {
		// Key material is 0o600; certificates and the CA cert are world-readable.
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".key") {
			mode = 0o600
		}
		if err := os.WriteFile(filepath.Join(dir, name), pemBytes, mode); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	fmt.Fprintf(os.Stderr, "husk-stub: wrote %d test cert files to %s\n", len(files), dir)
	return nil
}
