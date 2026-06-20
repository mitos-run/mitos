package husk

import (
	"archive/tar"
	"bytes"
	"context"
	"testing"

	"mitos.run/mitos/internal/cas"
	"mitos.run/mitos/internal/workspace"
)

// fakeWorkspaceAgent is an in-memory workspace.VsockTransport: TarDir returns a
// scripted tar of the guest /workspace, UntarDir records the tar it was handed.
// It lets the stub workspace ops be exercised with no real VM or vsock.
type fakeWorkspaceAgent struct {
	tar       []byte
	tarErr    error
	untarPath string
	untarTar  []byte
	untarErr  error
}

func (f *fakeWorkspaceAgent) TarDir(string) ([]byte, error) {
	return f.tar, f.tarErr
}

func (f *fakeWorkspaceAgent) UntarDir(path string, tarBytes []byte) error {
	f.untarPath = path
	f.untarTar = tarBytes
	return f.untarErr
}

// tarOf builds an in-memory tar with the given name -> content members, the
// shape the guest agent's TarDir returns over vsock.
func tarOf(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range members {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

// newWorkspaceStub returns an active stub wired with a temp node CAS and the
// given fake workspace agent (instead of a real vsock connect), so the
// dehydrate/hydrate ops run the real tar -> CAS round trip without a VM.
func newWorkspaceStub(t *testing.T, agent workspace.VsockTransport) (*Stub, *cas.Store) {
	t.Helper()
	store, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	s := &Stub{
		state:        StateActive,
		vm:           &fakeVMM{},
		casStore:     store,
		wsTransport:  func(string) (workspace.VsockTransport, error) { return agent, nil },
		vsockRelPath: "v.sock",
	}
	return s, store
}

func TestDehydrateWorkspaceTarToCASRoundTrip(t *testing.T) {
	agent := &fakeWorkspaceAgent{tar: tarOf(t, map[string]string{
		"main.go":  "package main",
		"README":   "hello",
		".netrc":   "machine secret password hunter2",
		".ssh/key": "PRIVATE",
	})}
	s, store := newWorkspaceStub(t, agent)

	res, err := s.DehydrateWorkspace(context.Background(), DehydrateWorkspaceRequest{
		ExcludePaths: []string{"/workspace/.netrc", "/workspace/.ssh"},
	})
	if err != nil {
		t.Fatalf("DehydrateWorkspace: %v", err)
	}
	if !res.OK {
		t.Fatalf("DehydrateWorkspace not OK: %+v", res)
	}
	d := cas.Digest(res.ManifestDigest)
	if err := d.Validate(); err != nil {
		t.Fatalf("manifest digest invalid: %v", err)
	}
	m, err := store.GetManifest(d)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	// Secrets excluded: the captured manifest must carry content only.
	names := map[string]bool{}
	for _, fe := range m.Files {
		names[fe.Name] = true
	}
	if !names["main.go"] || !names["README"] {
		t.Fatalf("expected content files in manifest, got %v", names)
	}
	if names[".netrc"] || names[".ssh/key"] {
		t.Fatalf("secret paths must be excluded from the captured revision, got %v", names)
	}
}

func TestDehydrateWorkspaceComputesDiffAgainstParent(t *testing.T) {
	// Seed a parent revision in the node CAS by dehydrating one tree.
	parentAgent := &fakeWorkspaceAgent{tar: tarOf(t, map[string]string{
		"main.go": "package main",
		"old.txt": "gone",
	})}
	s, store := newWorkspaceStub(t, parentAgent)
	pres, err := s.DehydrateWorkspace(context.Background(), DehydrateWorkspaceRequest{})
	if err != nil || !pres.OK {
		t.Fatalf("seed parent dehydrate: err=%v res=%+v", err, pres)
	}
	if pres.Diff != nil {
		t.Fatalf("no parent requested means no diff; got %+v", pres.Diff)
	}

	// Now dehydrate a CHILD tree that adds new.txt, modifies main.go, and removes
	// old.txt, asking for the diff against the parent manifest. The diff must be
	// computed from the two manifests in the node CAS, not the chunk bytes.
	childAgent := &fakeWorkspaceAgent{tar: tarOf(t, map[string]string{
		"main.go": "package main // changed",
		"new.txt": "fresh",
	})}
	s.wsTransport = func(string) (workspace.VsockTransport, error) { return childAgent, nil }
	_ = store

	cres, err := s.DehydrateWorkspace(context.Background(), DehydrateWorkspaceRequest{
		ParentManifestDigest: pres.ManifestDigest,
	})
	if err != nil {
		t.Fatalf("child DehydrateWorkspace: %v", err)
	}
	if !cres.OK {
		t.Fatalf("child DehydrateWorkspace not OK: %+v", cres)
	}
	if cres.Diff == nil {
		t.Fatalf("expected a diff when ParentManifestDigest is set, got nil")
	}
	d := cres.Diff
	if len(d.Added) != 1 || d.Added[0] != "new.txt" {
		t.Fatalf("diff added = %v, want [new.txt]", d.Added)
	}
	if len(d.Removed) != 1 || d.Removed[0] != "old.txt" {
		t.Fatalf("diff removed = %v, want [old.txt]", d.Removed)
	}
	if len(d.Modified) != 1 || d.Modified[0] != "main.go" {
		t.Fatalf("diff modified = %v, want [main.go]", d.Modified)
	}
}

func TestDehydrateWorkspaceDiffFailsClosedOnInvalidParent(t *testing.T) {
	agent := &fakeWorkspaceAgent{tar: tarOf(t, map[string]string{"main.go": "package main"})}
	s, _ := newWorkspaceStub(t, agent)
	res, err := s.DehydrateWorkspace(context.Background(), DehydrateWorkspaceRequest{
		ParentManifestDigest: "not-a-digest",
	})
	if err == nil || res.OK {
		t.Fatalf("dehydrate with an invalid parent digest must fail closed: err=%v res=%+v", err, res)
	}
}

func TestHydrateWorkspaceReadsCASIntoGuest(t *testing.T) {
	// First dehydrate to get a manifest in the node CAS.
	src := &fakeWorkspaceAgent{tar: tarOf(t, map[string]string{"main.go": "package main"})}
	s, store := newWorkspaceStub(t, src)
	dres, err := s.DehydrateWorkspace(context.Background(), DehydrateWorkspaceRequest{})
	if err != nil || !dres.OK {
		t.Fatalf("seed dehydrate: err=%v res=%+v", err, dres)
	}

	// Now hydrate the same manifest into a fresh agent and assert it was untarred
	// into /workspace.
	dst := &fakeWorkspaceAgent{}
	s2 := &Stub{
		state:        StateActive,
		vm:           &fakeVMM{},
		casStore:     store,
		wsTransport:  func(string) (workspace.VsockTransport, error) { return dst, nil },
		vsockRelPath: "v.sock",
	}
	hres, err := s2.HydrateWorkspace(context.Background(), HydrateWorkspaceRequest{ManifestDigest: dres.ManifestDigest})
	if err != nil {
		t.Fatalf("HydrateWorkspace: %v", err)
	}
	if !hres.OK {
		t.Fatalf("HydrateWorkspace not OK: %+v", hres)
	}
	if dst.untarPath != workspace.WorkspacePath {
		t.Fatalf("expected untar into %s, got %s", workspace.WorkspacePath, dst.untarPath)
	}
	if len(dst.untarTar) == 0 {
		t.Fatalf("expected a non-empty tar delivered to the guest")
	}
}

func TestDehydrateWorkspaceRequiresActiveState(t *testing.T) {
	store, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	s := &Stub{state: StateDormant, vm: &fakeVMM{}, casStore: store}
	res, err := s.DehydrateWorkspace(context.Background(), DehydrateWorkspaceRequest{})
	if err == nil || res.OK {
		t.Fatalf("dehydrate must refuse a non-active stub: err=%v res=%+v", err, res)
	}
}

func TestDehydrateWorkspaceFailClosedWithoutCAS(t *testing.T) {
	s := &Stub{
		state:       StateActive,
		vm:          &fakeVMM{},
		wsTransport: func(string) (workspace.VsockTransport, error) { return &fakeWorkspaceAgent{}, nil },
	}
	res, err := s.DehydrateWorkspace(context.Background(), DehydrateWorkspaceRequest{})
	if err == nil || res.OK {
		t.Fatalf("dehydrate without a node CAS must fail closed: err=%v res=%+v", err, res)
	}
}

func TestHydrateWorkspaceFailClosedOnInvalidDigest(t *testing.T) {
	store, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	s := &Stub{
		state:       StateActive,
		vm:          &fakeVMM{},
		casStore:    store,
		wsTransport: func(string) (workspace.VsockTransport, error) { return &fakeWorkspaceAgent{}, nil },
	}
	res, err := s.HydrateWorkspace(context.Background(), HydrateWorkspaceRequest{ManifestDigest: "not-a-digest"})
	if err == nil || res.OK {
		t.Fatalf("hydrate with an invalid digest must fail closed: err=%v res=%+v", err, res)
	}
}
