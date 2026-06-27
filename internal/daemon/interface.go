package daemon

import (
	"context"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/metering"
	"mitos.run/mitos/internal/volume"
)

// ForkEngine is the interface both the real Firecracker engine
// and the mock engine implement.
type ForkEngine interface {
	Fork(snapshotID, sandboxID string, opts fork.ForkOpts) (*fork.ForkResult, error)
	ForkRunning(sourceSandboxID, newSandboxID string, pauseSource bool) (*fork.ForkResult, error)
	Terminate(sandboxID string) error
	// Pause snapshots a sandbox's full state (memory + filesystem) and pauses the
	// VM so a later Resume restores it (issue #218). Resume restores a paused
	// sandbox to RUNNING. Both are idempotent and back the sandbox HTTP API's
	// pause/resume endpoints; on the mock engine they track the held state only.
	Pause(sandboxID string) error
	Resume(sandboxID string) error
	GetCapacity() fork.Capacity
	// Metering returns the full CoW-aware metering report (per-sandbox and
	// per-template memory plus disk) for the operator/billing endpoint. Unlike
	// GetCapacity it is NOT on the fork hot path and may stat backing files.
	Metering() metering.Report
	ListSandboxes() []fork.SandboxRecord
	// ListVolumes enumerates per-sandbox volume backing dirs on disk, keyed by
	// sandbox id, so a backing dir whose sandbox is gone is still reported. It
	// is NOT on the fork hot path. The mock engine reports an in-memory set.
	ListVolumes() []fork.VolumeRecord
	// ReclaimVolume removes one per-sandbox volume backing dir (and its sandbox
	// dir). It is the volume-orphan counterpart to Terminate; the controller GC
	// calls it for a backing dir whose claim object is gone.
	ReclaimVolume(sandboxID string) error
	// CreateTemplate builds a template snapshot. volumes are the template's
	// declared volumes; the engine bakes one placeholder drive per volume into
	// the snapshot. Nil leaves the template drive-less (only the rootfs).
	CreateTemplate(id string, image string, initCommands []string, volumes []volume.Spec, workload *firecracker.WorkloadSpec) error
	// PullTemplate fetches a template's snapshot from a peer forkd's CAS over
	// the peer's token-gated TLS surface, materializes it, verifies it, and
	// records the digest. token is a credential and must never be logged.
	PullTemplate(ctx context.Context, templateID, manifestDigest, sourceURL, token string) error
}
