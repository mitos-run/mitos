// Package runmanifest parses and validates the mitos.yaml "Run with Mitos"
// manifest (schema v1) and maps it to the mitos primitives: a golden SandboxPool
// to fork from, plus the run, preview, secret, egress, workspace, and auto-update
// (track) intent the provisioner and the auto-update reconciler consume.
//
// This is the foundation slice of the Run with Mitos feature (issue #340, the
// production funnel #440). It does pure parsing, validation, and mapping; it holds
// no cluster, no I/O, and no secret values (secret VALUES are supplied per-fork at
// run time, never in the manifest, never in the golden snapshot).
package runmanifest

// SchemaVersion is the only manifest schema version this package understands.
const SchemaVersion = 1

// Manifest is a parsed mitos.yaml. Every field maps to a step in the click ->
// fork -> serve flow; see the package doc and docs/superpowers/specs for the
// full design.
type Manifest struct {
	// Version is the manifest schema version; only SchemaVersion is accepted.
	// Zero is treated as SchemaVersion for forward-friendly minimal manifests.
	Version int `json:"version,omitempty"`

	// Name is the DNS-label identity of the app (pool name, URL label).
	Name string `json:"name"`
	// Title and Icon are presentation only (the consent screen, the badge).
	Title string `json:"title,omitempty"`
	Icon  string `json:"icon,omitempty"`

	// Source is where the golden template comes from (image or build) and how it
	// tracks upstream for auto-update.
	Source Source `json:"source"`

	// Run is the workload baked into the golden snapshot.
	Run Run `json:"run"`

	// Preview is the interactable surface that becomes the live URL.
	Preview Preview `json:"preview"`

	// Secrets are prompted at click and injected per-fork; never baked into the
	// golden (fork-correctness secret non-inheritance).
	Secrets []Secret `json:"secrets,omitempty"`

	// Egress is the default-deny outbound allowlist for the sandbox.
	Egress Egress `json:"egress,omitempty"`

	// Workspace is the durable, versioned path that survives updates and rebases.
	Workspace *Workspace `json:"workspace,omitempty"`

	// Resources sizes each forked instance.
	Resources Resources `json:"resources,omitempty"`
}

// Source declares the golden's origin and its auto-update tracking.
type Source struct {
	// Image is the OCI image the golden is built from. Mutually exclusive with
	// Build; exactly one must be set.
	Image string `json:"image,omitempty"`
	// Build builds the golden from a repo when no published image exists.
	Build *Build `json:"build,omitempty"`
	// Track watches upstream and re-snapshots the golden on a new release.
	Track *Track `json:"track,omitempty"`
}

// Build builds the golden from source.
type Build struct {
	Repo       string `json:"repo"`
	Dockerfile string `json:"dockerfile,omitempty"`
}

// Track is the auto-update policy.
type Track struct {
	// Watch is the image or repo to watch for new releases.
	Watch string `json:"watch"`
	// Channel is the tag, semver range, or branch to follow.
	Channel string `json:"channel,omitempty"`
	// OnNewRelease is the action on a new release. The conservative default is
	// resnapshot+offer-rebase (re-snapshot the golden, offer instances a rebase).
	OnNewRelease OnNewRelease `json:"on_new_release,omitempty"`
}

// OnNewRelease is the auto-update action.
type OnNewRelease string

const (
	// ResnapshotOfferRebase re-snapshots the golden and offers running instances a
	// one-click rebase (the safe default).
	ResnapshotOfferRebase OnNewRelease = "resnapshot+offer-rebase"
	// ResnapshotAutoRebase re-snapshots and auto-rebases instances (opt-in).
	ResnapshotAutoRebase OnNewRelease = "resnapshot+auto-rebase"
	// ResnapshotOnly re-snapshots the golden but leaves running instances alone.
	ResnapshotOnly OnNewRelease = "resnapshot"
)

// Run is the workload the golden runs.
type Run struct {
	// Command overrides the image entrypoint. Empty inherits the image CMD.
	Command []string `json:"command,omitempty"`
	// Workdir runs the command from this directory.
	Workdir string `json:"workdir,omitempty"`
	// Env are NON-secret environment variables baked into the golden. Secret
	// values come from Secrets, injected per-fork.
	Env map[string]string `json:"env,omitempty"`
	// Ready gates when the golden is snapshotted, so every fork boots serving.
	Ready *Ready `json:"ready,omitempty"`
}

// Ready is the snapshot-after-serving gate.
type Ready struct {
	HTTP    *HTTPReady `json:"http,omitempty"`
	Timeout string     `json:"timeout,omitempty"`
}

// HTTPReady is an HTTP health gate.
type HTTPReady struct {
	Port   int    `json:"port"`
	Path   string `json:"path,omitempty"`
	Expect int    `json:"expect,omitempty"`
}

// Preview is the interactable port and its auth posture.
type Preview struct {
	Port int `json:"port"`
	// Auth is the expose auth posture; "ladder" is the private-by-default default.
	Auth string `json:"auth,omitempty"`
}

// Secret is a secret the clicker supplies. Only its SHAPE lives here; the VALUE
// is collected at run time and injected per-fork.
type Secret struct {
	Name     string `json:"name"`
	Label    string `json:"label,omitempty"`
	Required bool   `json:"required,omitempty"`
	// Generate, when > 0, lets mitos mint a random secret of this byte length if
	// the clicker leaves it blank.
	Generate int `json:"generate,omitempty"`
}

// Egress is the default-deny outbound allowlist.
type Egress struct {
	Allow []string `json:"allow,omitempty"`
}

// Workspace is the durable, versioned path.
type Workspace struct {
	Path    string `json:"path"`
	Persist bool   `json:"persist,omitempty"`
}

// Resources sizes each forked instance.
type Resources struct {
	Pool     string `json:"pool,omitempty"`
	CPU      string `json:"cpu,omitempty"`
	Memory   string `json:"memory,omitempty"`
	Lifetime string `json:"lifetime,omitempty"`
}
