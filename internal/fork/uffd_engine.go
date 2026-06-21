package fork

import (
	"fmt"
	"path/filepath"
	"time"

	"mitos.run/mitos/internal/cas"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/vsock"
)

// This file is the engine-side glue for the userfaultfd memory backend (issue
// #167). The platform handler lives in uffd_linux.go / uffd_other.go; the pure
// region arithmetic in uffd.go. Here the engine decides when to restore through
// UFFD and drives the per-fork handshake.

// isEmptyHot reports whether a manifest hot-page set carries nothing to preload.
// A nil pointer or an offset-less set both mean "no prefetch", which keeps the
// file-backed restore path on the table for non-hugepage templates.
func isEmptyHot(h *cas.HotPageSet) bool {
	return h == nil || h.PageSizeBytes <= 0 || len(h.Offsets) == 0
}

// templateMemBacking reads a template's recorded manifest (the same one the
// integrity gate verified) and returns its captured hot-page set and its
// guest-memory page backing ("" for 4 KiB, "2M" for hugepages). Both are zero
// when the template has no recorded digest/manifest. The restore path uses them
// to decide whether the snapshot MUST be restored through the UFFD backend
// (hugepages) and whether there is a hot-page set to preload (prefetch).
func (e *Engine) templateMemBacking(snapshotID string) (*cas.HotPageSet, string) {
	d, err := readDigestFile(e.dataDir, snapshotID)
	if err != nil {
		return nil, ""
	}
	m, err := e.casStore.GetManifest(d)
	if err != nil {
		return nil, ""
	}
	return m.HotPages, m.HugePages
}

// loadSnapshotUFFD restores a snapshot through the userfaultfd backend and
// returns a handler already serving the VM's guest memory. It binds the per-fork
// UFFD socket, starts the handshake receiver, points Firecracker at the socket on
// /snapshot/load (paused), waits for the handshake, PRELOADS the hot-page set (if
// any) before the caller resumes, and starts the Serve loop for the pages not
// preloaded. On any error it closes the handler and returns. The caller resumes
// the VM and stores the handler on the Sandbox so Terminate can Close it.
func (e *Engine) loadSnapshotUFFD(client *firecracker.Client, sandboxDir, memFile, vmStateFile string, overrides []firecracker.NetworkOverride, hot *cas.HotPageSet, capture bool) (*uffdHandler, error) {
	sockPath := filepath.Join(sandboxDir, "uffd.sock")
	h, err := newUFFDHandler(sockPath, memFile, capture)
	if err != nil {
		return nil, fmt.Errorf("uffd backend: %w", err)
	}

	// Firecracker connects to the socket during /snapshot/load, so the receiver
	// must be accepting before the load call. Both the load PUT and the handshake
	// run concurrently.
	//
	// CRITICAL ordering: Firecracker FAULTS guest memory DURING the load itself
	// (restoring device state dereferences guest RAM), and blocks in the kernel
	// (handle_userfault) until the handler services those faults. So Serve MUST be
	// running before the load can complete. The sequence is: start the handshake
	// receiver and the load PUT; once the handshake delivers the uffd, start Serve
	// so the load's faults are handled; only then does the PUT return.
	recvErr := make(chan error, 1)
	go func() { recvErr <- h.receive() }()

	putErr := make(chan error, 1)
	go func() { putErr <- client.LoadSnapshotUFFD(vmStateFile, sockPath, overrides) }()

	// Firecracker sends the uffd early in the load, before it faults; wait for it,
	// then start serving so the load's device-restore faults are handled.
	if err := <-recvErr; err != nil {
		_ = h.Close()
		<-putErr // let the load unwind so the FC process is reaped by the caller
		return nil, fmt.Errorf("uffd handshake: %w", err)
	}
	go func() { _ = h.Serve() }()

	if err := <-putErr; err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("load snapshot (uffd): %w", err)
	}

	// Now the snapshot is loaded (paused) with Serve handling lazy faults. Preload
	// the hot-page set before the caller resumes, paying the lazy-fault tail up
	// front. A page Serve already filled is tolerated (UFFDIO_COPY EEXIST).
	if !isEmptyHot(hot) {
		if _, err := h.Preload(*hot); err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("uffd preload hot pages: %w", err)
		}
	}
	return h, nil
}

// FaultsServed returns the number of page faults the userfaultfd handler has
// serviced for a live sandbox (issue #167), or 0 for a file-backed restore or an
// unknown sandbox. The prefetch benchmark reads it after first-exec to compare
// the lazy-fault count with prefetch on vs off.
func (e *Engine) FaultsServed(sandboxID string) int64 {
	e.mu.Lock()
	sb, ok := e.sandboxes[sandboxID]
	e.mu.Unlock()
	if !ok || sb.uffd == nil {
		return 0
	}
	return sb.uffd.FaultCount()
}

// hugePagesToBytes maps a snapshot's recorded page backing ("" or "2M") to the
// page size in bytes the hot-page set must use: 2 MiB for a hugepage-backed
// snapshot, else 4 KiB. The capture page size MUST match the snapshot's actual
// backing (not the engine's own config), because Preload's UFFDIO_COPY length is
// the page size and a hugepage region rejects a 4 KiB copy.
func hugePagesToBytes(hugePages string) int64 {
	if hugePages == "2M" {
		return 2 << 20
	}
	return 4096
}

// CaptureTemplateHotPages restores the template once through the UFFD backend in
// capture mode, drives it to first-exec to fault in the claim->first-exec working
// set, reduces the serviced faults to a hot-page set (capped when cap > 0), and
// stamps it onto the template's snapshot manifest so subsequent forks preload it
// (issue #167). It runs OFF the tenant claim path. cap <= 0 keeps every distinct
// faulted page. It returns the captured set.
func (e *Engine) CaptureTemplateHotPages(template string, cap int) (cas.HotPageSet, error) {
	captureID := "hotpage-capture-" + template
	_ = e.Terminate(captureID) // clear any stale capture fork
	res, err := e.Fork(template, captureID, ForkOpts{CaptureHotPages: true})
	if err != nil {
		return cas.HotPageSet{}, fmt.Errorf("capture fork: %w", err)
	}
	defer func() { _ = e.Terminate(captureID) }()

	// Drive the working set to first-exec, the same shape the claim path measures,
	// so the captured pages cover what a fresh fork touches. Best-effort: a connect
	// or exec failure still yields the resume-time working set already faulted in.
	if client, cerr := connectVsockRetry(res.VsockPath); cerr == nil {
		_, _ = client.Exec("/bin/true", "/", nil, 10)
		client.Close()
	}
	// Let the post-resume working set settle so the trace reflects steady state.
	time.Sleep(300 * time.Millisecond)

	e.mu.Lock()
	sb, ok := e.sandboxes[captureID]
	e.mu.Unlock()
	if !ok || sb.uffd == nil {
		return cas.HotPageSet{}, fmt.Errorf("capture fork %s has no uffd handler (capture requires the UFFD backend)", captureID)
	}
	// The capture page size must match the SNAPSHOT's backing, not the engine's
	// config: a hugepage snapshot's pages are 2 MiB and Preload copies whole pages.
	_, snapHugePages := e.templateMemBacking(template)
	trace := sb.uffd.CaptureTrace()
	set := SelectHotPages(trace, HotPageSelection{
		PageSizeBytes: hugePagesToBytes(snapHugePages),
		File:          "mem",
		Cap:           cap,
	})
	if err := e.restampHotPages(template, &set); err != nil {
		return set, fmt.Errorf("stamp hot-page set onto %s manifest: %w", template, err)
	}
	return set, nil
}

// restampHotPages re-content-addresses a template's snapshot with hot set stamped
// onto its manifest, updating the recorded digest and verified marker so later
// forks see (and preload) it. The non-hot metadata is carried verbatim from the
// existing manifest so only the hot-page set changes.
func (e *Engine) restampHotPages(template string, hot *cas.HotPageSet) error {
	d, err := readDigestFile(e.dataDir, template)
	if err != nil {
		return fmt.Errorf("read recorded digest: %w", err)
	}
	m, err := e.casStore.GetManifest(d)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	meta := cas.Metadata{
		SnapshotFormatVersion: m.SnapshotFormatVersion,
		VMMVersion:            m.VMMVersion,
		CPUModel:              m.CPUModel,
		KernelVersion:         m.KernelVersion,
		ConfigHash:            m.ConfigHash,
		CreatedUnix:           m.CreatedUnix,
		HotPages:              hot,
		// Carry the page backing through so re-stamping the hot set does not drop
		// the snapshot's self-describing hugepage marker (issue #167).
		HugePages: m.HugePages,
	}
	newD, err := recordTemplateDigest(e.casStore, e.dataDir, template, meta)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.templateDigests[template] = newD
	e.mu.Unlock()
	return nil
}

// connectVsockRetry dials the guest agent, retrying briefly while the freshly
// resumed guest finishes coming up. It mirrors the bench connect helper so the
// capture path waits the same way a real claim would.
func connectVsockRetry(vsockPath string) (*vsock.Client, error) {
	var lastErr error
	for i := 0; i < 50; i++ {
		c, err := vsock.Connect(vsockPath, vsock.AgentPort)
		if err == nil {
			return c, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, lastErr
}
