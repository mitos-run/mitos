package fork

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"mitos.run/mitos/internal/firecracker"
)

// TestReconcileBuildsReapsDeadOrphan proves a build whose pid is dead/recycled is
// reaped at startup: its jailer chroot is removed and its journal record dropped,
// without signalling the (not-ours) pid (#469).
func TestReconcileBuildsReapsDeadOrphan(t *testing.T) {
	dd := t.TempDir()
	jdir := filepath.Join(dd, "jailer", "tmpl1")
	if err := os.MkdirAll(jdir, 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Engine{
		dataDir:        dd,
		firecrackerBin: "firecracker",
		buildJournal:   newBuildJournal(dd),
		// pid is NOT our firecracker (dead or recycled): reap artifacts, do not kill.
		verifyPID: func(int, string) bool { return false },
	}
	if err := e.buildJournal.write(buildRecord{
		ID: "tmpl1", Pid: 2147480000, JailerVMDir: jdir,
		FirecrackerBin: "firecracker", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	e.reconcileBuilds()

	if _, err := os.Stat(jdir); !os.IsNotExist(err) {
		t.Fatalf("orphan build jailer dir not reaped: %v", err)
	}
	recs, _ := e.buildJournal.load()
	if len(recs) != 0 {
		t.Fatalf("build record not removed after reap: %d left", len(recs))
	}
}

// TestReconcileBuildsKillsLiveOrphan proves a build whose pid IS still our
// firecracker is killed (an interrupted build is never adopted). A real child
// process stands in for the leaked build VM, with verifyPID stubbed true.
func TestReconcileBuildsKillsLiveOrphan(t *testing.T) {
	dd := t.TempDir()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	pid := cmd.Process.Pid

	e := &Engine{
		dataDir:        dd,
		firecrackerBin: "firecracker",
		buildJournal:   newBuildJournal(dd),
		verifyPID:      func(int, string) bool { return true }, // "still our build FC"
	}
	if err := e.buildJournal.write(buildRecord{
		ID: "tmpl1", Pid: pid, FirecrackerBin: "firecracker", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	e.reconcileBuilds()

	// The stand-in process must have been killed.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done: // exited (killed)
	case <-time.After(5 * time.Second):
		t.Fatal("reconcileBuilds did not kill the live orphan build process")
	}
}

// TestJournalBuildRoundTrip proves the onStarted hook's payload is persisted and
// then dropped: journalBuild writes a record carrying the build's pid + jailer
// artifacts, and unjournalBuild removes it (the per-build window, #469).
func TestJournalBuildRoundTrip(t *testing.T) {
	dd := t.TempDir()
	e := &Engine{dataDir: dd, firecrackerBin: "firecracker", buildJournal: newBuildJournal(dd)}

	e.journalBuild("tmpl1", 4321, firecracker.JailerState{ChrootDir: "/c", JailerVMDir: "/j", JailedUID: 64007}, true)

	recs, err := e.buildJournal.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 build record, got %d", len(recs))
	}
	r := recs[0]
	if r.ID != "tmpl1" || r.Pid != 4321 || r.JailerVMDir != "/j" || r.JailedUID != 64007 || !r.Networked || r.FirecrackerBin != "firecracker" {
		t.Fatalf("record fields wrong: %+v", r)
	}

	e.unjournalBuild("tmpl1")
	recs, _ = e.buildJournal.load()
	if len(recs) != 0 {
		t.Fatalf("record not dropped after unjournal: %d left", len(recs))
	}
}
