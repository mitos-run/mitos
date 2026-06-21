package firecracker

type VMConfig struct {
	ID             string
	FirecrackerBin string
	WorkDir        string
	SocketPath     string
	KernelPath     string
	RootfsPath     string
	VcpuCount      int
	MemSizeMib     int
	BootArgs       string
	// Jailer configures launching through the jailer binary. The zero
	// value keeps the direct-exec behavior.
	Jailer JailerConfig
	// ChrootFiles lists host files (kernel, rootfs, snapshot mem and
	// vmstate) that prepareChroot hard-links into the VM's chroot before
	// launch. Ignored when the jailer is disabled.
	ChrootFiles []string
	// VolumeDrives are placeholder block devices baked into the snapshot at
	// build time, one per template volume. Firecracker bakes its device model at
	// snapshot time and cannot add a drive on restore, so the device must exist
	// in the snapshot; each fork then rebinds the drive's backing to its own
	// file via PatchDrive after restore. The drives are added (in slice order)
	// BEFORE InstanceStart and are NOT mounted in the guest at build time. The
	// guest device order follows AddDrive order: rootfs is vda, these follow as
	// vdb, vdc, ... in this slice's order. Empty keeps the prior drive-less
	// behavior (only the rootfs drive).
	VolumeDrives []VolumeDrive
	// Network, when set, attaches a NIC to the VM. It is used two ways:
	// for a FRESH boot (template creation, non-restore claims) the NIC is
	// bound to a live tap before InstanceStart; the engine fork path instead
	// passes the identity to LoadSnapshotWithOverrides so each fork remaps
	// the snapshot's baked placeholder NIC to its own tap. The zero value
	// keeps the prior NIC-less behavior. The fields are safe to log.
	Network *NetworkIdentity
	// EntropyDevice, when true, attaches a virtio-rng device before
	// InstanceStart so the snapshot bakes a CONTINUOUS host entropy source
	// into every restored fork. This complements the one-shot NotifyForked
	// CRNG reseed (fork-correctness row 1): the reseed credits fresh entropy
	// at the instant of the fork, the virtio-rng device keeps feeding the
	// guest CRNG afterwards. Firecracker bakes its device model at snapshot
	// time and cannot add a device on restore, so the device must exist at
	// build time; once baked, every fork restores it without any per-fork
	// API call. DefaultVMConfig enables it. The field carries no secrets.
	EntropyDevice bool
	// HugePages selects the guest-memory page granularity baked into the
	// template snapshot (issue #167). "" is the Firecracker default (4 KiB base
	// pages); "2M" backs guest memory with 2 MiB hugetlbfs pages so each
	// snapshot-restore fault moves 2 MiB instead of 4 KiB. It is set at the
	// template build (the snapshot records the backing), so every fork restores
	// with the same page size without a per-fork API call. The field is config
	// (no secrets), safe to log.
	HugePages string
}

// VolumeDrive is one placeholder block device a template build attaches before
// snapshot. DriveID is the volume name (forks rebind this exact id), PathOnHost
// is the template seed backing baked into the snapshot, and ReadOnly carries
// the volume's read-only flag. All fields are config (no secrets), safe to log.
type VolumeDrive struct {
	DriveID    string
	PathOnHost string
	ReadOnly   bool
}

// NetworkIdentity is the per-VM network binding StartVM applies: which guest
// NIC (IfaceID, GuestMAC) maps to which host tap (HostDevName). It mirrors the
// fields of netconf.Identity that Firecracker needs, kept here so the
// firecracker package does not import netconf. All fields are safe to log.
type NetworkIdentity struct {
	IfaceID     string
	GuestMAC    string
	HostDevName string
}

func DefaultVMConfig() VMConfig {
	return VMConfig{
		FirecrackerBin: "/usr/local/bin/firecracker",
		VcpuCount:      1,
		MemSizeMib:     512,
		BootArgs:       "console=ttyS0 reboot=k panic=1 pci=off",
		// On by default: every template snapshot bakes a virtio-rng device so
		// each fork inherits a continuous host entropy source (fork-correctness
		// row 1). See VMConfig.EntropyDevice.
		EntropyDevice: true,
	}
}

type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

type MachineConfig struct {
	VcpuCount  int `json:"vcpu_count"`
	MemSizeMib int `json:"mem_size_mib"`
	// HugePages selects the backing page size for guest memory (issue #167).
	// Empty means the Firecracker default (4 KiB base pages) and is omitted from
	// the request so the wire form is byte-identical to the pre-field behavior;
	// "2M" backs guest memory with 2 MiB hugetlbfs pages so each snapshot-restore
	// fault moves 2 MiB instead of 4 KiB, cutting the lazy-fault count ~512x.
	HugePages string `json:"huge_pages,omitempty"`
}

type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsReadOnly   bool   `json:"is_read_only"`
	IsRootDevice bool   `json:"is_root_device"`
}

// DrivePatch is the PATCH /drives/{drive_id} request body. Firecracker accepts
// a path_on_host update on an existing drive to rebind its backing file. This
// is how each fork of one shared snapshot points its baked placeholder volume
// drive at the fork's OWN backing: the snapshot bakes the block device by
// drive_id, and every fork PATCHes that drive_id to its prepared backing after
// the snapshot is loaded and resumed (before the guest mounts it). The field
// names match the Firecracker API exactly; the values (drive id and host path)
// carry no secrets and are safe to log.
type DrivePatch struct {
	DriveID    string `json:"drive_id"`
	PathOnHost string `json:"path_on_host"`
}

type Vsock struct {
	GuestCID int    `json:"guest_cid"`
	UdsPath  string `json:"uds_path"`
}

// Entropy is the PUT /entropy request body. It attaches a virtio-rng device
// backed by the host RNG, giving the guest a continuous entropy source. The
// rate_limiter is optional; omitting it (a nil pointer with omitempty) yields a
// bare {} body, which Firecracker accepts as an unthrottled device. The device
// is baked into the snapshot at build time, so every fork restores it without a
// per-fork API call. No secrets: the device draws from the host RNG and the
// request carries only an optional rate-limit shape, never any entropy bytes.
type Entropy struct {
	RateLimiter *RateLimiter `json:"rate_limiter,omitempty"`
}

// RateLimiter is the optional token-bucket shape Firecracker accepts on the
// entropy (and other) devices. It is here for completeness; the engine attaches
// the entropy device unthrottled (a nil RateLimiter), so this is currently only
// exercised by callers that want to bound guest RNG draw. No secrets.
type RateLimiter struct {
	Bandwidth *TokenBucket `json:"bandwidth,omitempty"`
	Ops       *TokenBucket `json:"ops,omitempty"`
}

// TokenBucket is one Firecracker token-bucket spec (size + refill time). No
// secrets.
type TokenBucket struct {
	Size         int64 `json:"size"`
	RefillTimeMs int64 `json:"refill_time"`
	OneTimeBurst int64 `json:"one_time_burst,omitempty"`
}

type Action struct {
	ActionType string `json:"action_type"`
}

type VMState struct {
	State string `json:"state"`
}

type SnapshotCreate struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type SnapshotLoad struct {
	SnapshotPath        string `json:"snapshot_path"`
	MemFilePath         string `json:"mem_file_path"`
	EnableDiffSnapshots bool   `json:"enable_diff_snapshots"`
	ResumeVM            bool   `json:"resume_vm"`
	// NetworkOverrides remaps each snapshot network interface to a fresh
	// host tap at load time. Firecracker (>= v1.12, pinned CI is v1.15)
	// accepts this optional array on PUT /snapshot/load so a single shared
	// snapshot can be forked many times with each fork bound to its OWN tap.
	// Omitted (nil) restores the device against its baked host_dev_name,
	// preserving the prior behavior for snapshots taken without a NIC.
	NetworkOverrides []NetworkOverride `json:"network_overrides,omitempty"`
}

// NetworkOverride remaps one snapshot network interface (identified by its
// IfaceID) to a different host tap (HostDevName) when the snapshot is loaded.
// It is the network analog of the relative vsock uds_path: it lets every fork
// of one snapshot bind a distinct, freshly created tap. All fields are safe to
// log (interface ids and tap names carry no secrets).
type NetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

// NetworkInterface is the PUT /network-interfaces/{iface_id} request body. It
// binds a guest NIC (IfaceID, GuestMAC) to a host tap device (HostDevName).
// The JSON field names match the Firecracker API exactly. All fields are safe
// to log: ids, MACs, and tap names carry no secrets.
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac,omitempty"`
	HostDevName string `json:"host_dev_name"`
}
