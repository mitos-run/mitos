package fork

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/netconf"
)

const buildJournalDirName = "build-journal"

// buildRecord journals an in-flight template build so a restarted forkd can reap
// the build Firecracker, its jailer chroot, and the placeholder tap if forkd
// died ungracefully mid-build (eviction, OOM, SIGKILL) before the deferred Kill
// ran (#469). Unlike a sandboxRecord, a build is NEVER re-adopted: an interrupted
// build has no resumable state, so a still-running build VM is killed. Carries
// host paths, a pid, and a uid only, no secrets.
type buildRecord struct {
	ID             string    `json:"id"`
	Pid            int       `json:"pid"`
	ChrootDir      string    `json:"chrootDir,omitempty"`
	JailerVMDir    string    `json:"jailerVMDir,omitempty"`
	JailedUID      uint32    `json:"jailedUID,omitempty"`
	FirecrackerBin string    `json:"firecrackerBin"`
	Networked      bool      `json:"networked,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

// buildJournal persists build records under <dataDir>/build-journal, separate
// from the sandbox journal so the two never collide.
type buildJournal struct {
	dir string
}

func newBuildJournal(dataDir string) *buildJournal {
	return &buildJournal{dir: filepath.Join(dataDir, buildJournalDirName)}
}

func (j *buildJournal) recordPath(id string) string {
	return filepath.Join(j.dir, id+".json")
}

// write atomically persists a record (temp file + rename), mirroring the sandbox
// journal so a reader or a crash never observes a partial record.
func (j *buildJournal) write(rec buildRecord) error {
	if err := os.MkdirAll(j.dir, 0o755); err != nil {
		return fmt.Errorf("create build journal dir: %w", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal build record %s: %w", rec.ID, err)
	}
	tmp, err := os.CreateTemp(j.dir, rec.ID+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp build record %s: %w", rec.ID, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temp build record %s: %w", rec.ID, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp build record %s: %w", rec.ID, err)
	}
	if err := os.Rename(tmpName, j.recordPath(rec.ID)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("commit build record %s: %w", rec.ID, err)
	}
	return nil
}

// remove deletes a build record. Removing an absent record is not an error.
func (j *buildJournal) remove(id string) error {
	if err := os.Remove(j.recordPath(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove build record %s: %w", id, err)
	}
	return nil
}

// load reads every build record, skipping any that fail to parse (fail open).
func (j *buildJournal) load() ([]buildRecord, error) {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read build journal dir: %w", err)
	}
	recs := make([]buildRecord, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(j.dir, e.Name()))
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "forkd: skip unreadable build record %s: %v\n", e.Name(), rerr)
			continue
		}
		var rec buildRecord
		if uerr := json.Unmarshal(data, &rec); uerr != nil {
			fmt.Fprintf(os.Stderr, "forkd: skip unparseable build record %s: %v\n", e.Name(), uerr)
			continue
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// journalBuild records an in-flight build (called from the onStarted hook).
func (e *Engine) journalBuild(id string, pid int, js firecracker.JailerState, networked bool) {
	if e.buildJournal == nil {
		return
	}
	rec := buildRecord{
		ID:             id,
		Pid:            pid,
		ChrootDir:      js.ChrootDir,
		JailerVMDir:    js.JailerVMDir,
		JailedUID:      js.JailedUID,
		FirecrackerBin: e.firecrackerBin,
		Networked:      networked,
		CreatedAt:      time.Now(),
	}
	if err := e.buildJournal.write(rec); err != nil {
		fmt.Fprintf(os.Stderr, "forkd: journal build %s: %v\n", id, err)
	}
}

// unjournalBuild drops a build record once the build returns (success or fail);
// the deferred Kill in CreateTemplate has already torn the build VM down.
func (e *Engine) unjournalBuild(id string) {
	if e.buildJournal == nil {
		return
	}
	if err := e.buildJournal.remove(id); err != nil {
		fmt.Fprintf(os.Stderr, "forkd: unjournal build %s: %v\n", id, err)
	}
}

// placeholderNetIdentity is the fixed identity of the host-side placeholder tap
// the template build attaches; shared by the build path and the orphan reaper.
func placeholderNetIdentity(templateID string) netconf.Identity {
	return netconf.Identity{
		TapName:  firecracker.PlaceholderTapNameFor(templateID),
		GuestMAC: placeholderMAC,
		HostIP:   placeholderHostIP,
		GuestIP:  placeholderGuestIP,
	}
}

// reconcileBuilds reaps build-time orphans at startup: a build Firecracker, its
// jailer chroot, and the placeholder tap that leaked because forkd died mid-build
// before the deferred Kill ran (#469). A build is never adopted (no resumable
// state); a still-running build VM is killed, PID-recycle-guarded exactly like
// the sandbox reaper.
func (e *Engine) reconcileBuilds() {
	if e.buildJournal == nil {
		return
	}
	recs, err := e.buildJournal.load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "forkd: skip build reconcile (journal unreadable): %v\n", err)
		return
	}
	if len(recs) == 0 {
		return
	}
	verify := e.verifyPID
	if verify == nil {
		verify = procfsVerifier
	}
	reaped := 0
	for _, rec := range recs {
		// Kill a still-running orphan build VM. The PID-recycle guard: only signal
		// a pid that still resolves to OUR firecracker; a dead or recycled pid is
		// not ours to kill (artifacts are reaped regardless).
		if rec.Pid > 0 && verify(rec.Pid, rec.FirecrackerBin) {
			if proc, perr := os.FindProcess(rec.Pid); perr == nil {
				if kerr := proc.Kill(); kerr != nil && !isProcessGone(kerr) {
					fmt.Fprintf(os.Stderr, "forkd: kill orphan build %s (pid %d): %v\n", rec.ID, rec.Pid, kerr)
				}
			}
		}
		e.reapBuildArtifacts(rec)
		if rerr := e.buildJournal.remove(rec.ID); rerr != nil {
			fmt.Fprintf(os.Stderr, "forkd: remove reaped build record %s: %v\n", rec.ID, rerr)
		}
		reaped++
	}
	fmt.Fprintf(os.Stderr, "forkd: build reconcile complete: %d orphan build(s) reaped\n", reaped)
}

// reapBuildArtifacts removes a leaked build's host artifacts: the jailer chroot
// workspace, the placeholder tap, and the jailer uid. Best-effort and idempotent.
// It never touches <dataDir>/templates/<id>: an interrupted build's snapshot dir
// is incomplete and simply overwritten on the next build, and a completed
// template whose unjournal we narrowly missed must not be deleted.
func (e *Engine) reapBuildArtifacts(rec buildRecord) {
	if rec.JailerVMDir != "" {
		if err := os.RemoveAll(rec.JailerVMDir); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: reap build jailer dir %s for %s: %v\n", rec.JailerVMDir, rec.ID, err)
		}
	}
	if rec.Networked && e.netMgr != nil && e.networkEnabled() {
		if err := e.netMgr.Teardown(context.Background(), placeholderNetIdentity(rec.ID)); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: reap build placeholder tap for %s: %v\n", rec.ID, err)
		}
	}
	if rec.JailedUID != 0 && e.jailer.Allocator != nil {
		e.jailer.Allocator.Release(rec.JailedUID)
	}
	fmt.Fprintf(os.Stderr, "forkd: reaped orphan build %s (pid %d, jailerDir %q)\n", rec.ID, rec.Pid, rec.JailerVMDir)
}
