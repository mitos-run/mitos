package fork

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type Engine struct {
	mu             sync.RWMutex
	dataDir        string
	firecrackerBin string
	kernelPath     string
	templates      map[string]*Template
	sandboxes      map[string]*Sandbox
}

type Template struct {
	ID           string
	Image        string
	SnapshotDir  string
	MemFile      string
	VMStateFile  string
	CreatedAt    time.Time
	Ready        bool
	SnapshotSize int64
}

type Sandbox struct {
	ID            string
	TemplateID    string
	SnapshotID    string
	KVMFD         int
	Endpoint      string
	Pid           int
	MemoryMap     []byte
	CreatedAt     time.Time
	MemoryUnique  int64
	MemoryShared  int64
}

type ForkResult struct {
	SandboxID    string
	Endpoint     string
	ForkTimeMs   float64
	MemoryUnique int64
	MemoryShared int64
}

func NewEngine(dataDir, firecrackerBin, kernelPath string) (*Engine, error) {
	if err := validateKVM(); err != nil {
		return nil, fmt.Errorf("KVM not available: %w", err)
	}

	return &Engine{
		dataDir:        dataDir,
		firecrackerBin: firecrackerBin,
		kernelPath:     kernelPath,
		templates:      make(map[string]*Template),
		sandboxes:      make(map[string]*Sandbox),
	}, nil
}

func validateKVM() error {
	info, err := os.Stat("/dev/kvm")
	if err != nil {
		return fmt.Errorf("/dev/kvm not found: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("/dev/kvm is not a character device")
	}
	return nil
}

// Fork creates a new sandbox from a snapshot using CoW memory mapping.
// This is the hot path — target is <2ms.
func (e *Engine) Fork(snapshotID, sandboxID string, opts ForkOpts) (*ForkResult, error) {
	start := time.Now()

	e.mu.RLock()
	template, ok := e.findTemplateBySnapshot(snapshotID)
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("snapshot %s not found", snapshotID)
	}

	memFile := filepath.Join(template.SnapshotDir, "mem")
	vmStateFile := filepath.Join(template.SnapshotDir, "vmstate")

	// 1. Memory-map the snapshot file with MAP_PRIVATE (CoW)
	// When the fork writes to a page, the kernel creates a private copy.
	// All forks share the same physical pages until they diverge.
	memFd, err := unix.Open(memFile, unix.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open snapshot memory: %w", err)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(memFd, &stat); err != nil {
		unix.Close(memFd)
		return nil, fmt.Errorf("stat snapshot memory: %w", err)
	}

	memMap, err := unix.Mmap(memFd, 0, int(stat.Size),
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE)
	if err != nil {
		unix.Close(memFd)
		return nil, fmt.Errorf("mmap snapshot: %w", err)
	}
	unix.Close(memFd)

	// 2. Create a new KVM VM with the mapped memory
	kvmFd, err := e.createKVMVM(memMap, vmStateFile)
	if err != nil {
		unix.Munmap(memMap)
		return nil, fmt.Errorf("create KVM VM: %w", err)
	}

	// 3. Set up networking (vsock or TAP)
	endpoint, err := e.setupNetworking(sandboxID, opts)
	if err != nil {
		unix.Munmap(memMap)
		return nil, fmt.Errorf("setup networking: %w", err)
	}

	// 4. Apply environment variables and secrets
	if err := e.injectEnv(sandboxID, opts.Env, opts.Secrets); err != nil {
		unix.Munmap(memMap)
		return nil, fmt.Errorf("inject env: %w", err)
	}

	sandbox := &Sandbox{
		ID:         sandboxID,
		TemplateID: template.ID,
		SnapshotID: snapshotID,
		KVMFD:      kvmFd,
		Endpoint:   endpoint,
		MemoryMap:  memMap,
		CreatedAt:  time.Now(),
	}

	e.mu.Lock()
	e.sandboxes[sandboxID] = sandbox
	e.mu.Unlock()

	// 5. Read memory stats
	unique, shared := e.readMemoryStats(sandbox.Pid)
	sandbox.MemoryUnique = unique
	sandbox.MemoryShared = shared

	elapsed := time.Since(start)

	return &ForkResult{
		SandboxID:    sandboxID,
		Endpoint:     endpoint,
		ForkTimeMs:   float64(elapsed.Microseconds()) / 1000.0,
		MemoryUnique: unique,
		MemoryShared: shared,
	}, nil
}

// ForkRunning checkpoints a running sandbox and creates a new fork from it.
func (e *Engine) ForkRunning(sourceSandboxID, newSandboxID string, pauseSource bool) (*ForkResult, error) {
	e.mu.RLock()
	source, ok := e.sandboxes[sourceSandboxID]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found", sourceSandboxID)
	}

	if pauseSource {
		if err := e.pauseVM(source); err != nil {
			return nil, fmt.Errorf("pause source: %w", err)
		}
		defer e.resumeVM(source)
	}

	// Checkpoint the running sandbox to a temporary snapshot
	tmpSnapshotDir, err := e.checkpointVM(source)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	defer os.RemoveAll(tmpSnapshotDir)

	// Fork from the checkpoint
	return e.Fork(sourceSandboxID+"-checkpoint", newSandboxID, ForkOpts{})
}

// Terminate kills a sandbox and releases its resources.
func (e *Engine) Terminate(sandboxID string) error {
	e.mu.Lock()
	sandbox, ok := e.sandboxes[sandboxID]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("sandbox %s not found", sandboxID)
	}
	delete(e.sandboxes, sandboxID)
	e.mu.Unlock()

	if sandbox.MemoryMap != nil {
		unix.Munmap(sandbox.MemoryMap)
	}

	return e.destroyKVMVM(sandbox)
}

// GetCapacity returns the current node capacity.
func (e *Engine) GetCapacity() Capacity {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var totalUnique, totalShared int64
	for _, s := range e.sandboxes {
		totalUnique += s.MemoryUnique
		totalShared += s.MemoryShared
	}

	templateIDs := make([]string, 0, len(e.templates))
	for id := range e.templates {
		templateIDs = append(templateIDs, id)
	}

	return Capacity{
		ActiveSandboxes: int32(len(e.sandboxes)),
		MemoryUsed:      totalUnique + totalShared,
		MemoryShared:    totalShared,
		TemplateIDs:     templateIDs,
		KVMAvailable:    true,
	}
}

type Capacity struct {
	ActiveSandboxes int32
	MaxSandboxes    int32
	MemoryTotal     int64
	MemoryUsed      int64
	MemoryShared    int64
	TemplateIDs     []string
	SnapshotIDs     []string
	KVMAvailable    bool
}

type ForkOpts struct {
	Env     map[string]string
	Secrets map[string]string
	Network *NetworkOpts
}

type NetworkOpts struct {
	EgressPolicy string
	AllowList    []string
}

func (e *Engine) findTemplateBySnapshot(snapshotID string) (*Template, bool) {
	for _, t := range e.templates {
		if t.ID == snapshotID || filepath.Base(t.SnapshotDir) == snapshotID {
			return t, true
		}
	}
	return nil, false
}

// readMemoryStats reads /proc/<pid>/smaps_rollup to determine unique vs shared pages.
func (e *Engine) readMemoryStats(pid int) (unique, shared int64) {
	path := fmt.Sprintf("/proc/%d/smaps_rollup", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	// Parse Private_Clean + Private_Dirty = unique
	// Parse Shared_Clean + Shared_Dirty = shared
	_ = data
	return 0, 0 // TODO: parse smaps_rollup
}

// Stubs for platform-specific KVM operations.
// These wrap the Firecracker snapshot restore API.

func (e *Engine) createKVMVM(memMap []byte, vmStateFile string) (int, error) {
	// TODO: Restore Firecracker VM from snapshot
	// 1. Open /dev/kvm
	// 2. KVM_CREATE_VM
	// 3. Set up memory regions from memMap
	// 4. Restore vCPU state from vmStateFile
	// 5. Return KVM fd
	return 0, fmt.Errorf("not implemented")
}

func (e *Engine) destroyKVMVM(sandbox *Sandbox) error {
	// TODO: Kill Firecracker process, close KVM fd
	return nil
}

func (e *Engine) setupNetworking(sandboxID string, opts ForkOpts) (string, error) {
	// TODO: Create vsock or TAP device for the sandbox
	return fmt.Sprintf("vsock://%s:8080", sandboxID), nil
}

func (e *Engine) injectEnv(sandboxID string, env, secrets map[string]string) error {
	// TODO: Write env vars into the sandbox via guest agent
	return nil
}

func (e *Engine) pauseVM(sandbox *Sandbox) error {
	// TODO: Pause via Firecracker API
	return nil
}

func (e *Engine) resumeVM(sandbox *Sandbox) error {
	// TODO: Resume via Firecracker API
	return nil
}

func (e *Engine) checkpointVM(sandbox *Sandbox) (string, error) {
	// TODO: Snapshot via Firecracker API
	return "", fmt.Errorf("not implemented")
}
