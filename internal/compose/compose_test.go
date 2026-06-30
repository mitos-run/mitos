package compose

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingBackend is the mock compose backend used to prove the contract,
// routing, validation, and collect orchestration WITHOUT a real in-guest
// docker compose (that backend is the gated follow-up, issue #490).
type recordingBackend struct {
	available bool

	execCalls []ServiceExecRequest
	stopCalls []ServiceStopRequest
	dlFile    []string // "service:path"
	dlDir     []string // "service:path"
	isDir     []string // "service:path"

	execResult ExecResult
	execErr    error
	isDirVal   bool
}

func (b *recordingBackend) Available() bool { return b.available }

func (b *recordingBackend) ServiceExec(_ context.Context, req ServiceExecRequest) (ExecResult, error) {
	b.execCalls = append(b.execCalls, req)
	if b.execErr != nil {
		return ExecResult{}, b.execErr
	}
	return b.execResult, nil
}

func (b *recordingBackend) ServiceDownloadFile(_ context.Context, service, path string) ([]byte, error) {
	b.dlFile = append(b.dlFile, service+":"+path)
	return []byte("file-bytes"), nil
}

func (b *recordingBackend) ServiceDownloadDir(_ context.Context, service, path string, _ []string) ([]byte, error) {
	b.dlDir = append(b.dlDir, service+":"+path)
	return []byte("tar-bytes"), nil
}

func (b *recordingBackend) ServiceIsDir(_ context.Context, service, path string) (bool, error) {
	b.isDir = append(b.isDir, service+":"+path)
	return b.isDirVal, nil
}

func (b *recordingBackend) StopService(_ context.Context, req ServiceStopRequest) error {
	b.stopCalls = append(b.stopCalls, req)
	return nil
}

func newAvailableProvider() (*Provider, *recordingBackend) {
	b := &recordingBackend{available: true, execResult: ExecResult{ExitCode: 0, Stdout: []byte("ok")}}
	return NewProvider(b), b
}

func TestCapabilitiesReflectBackendAvailability(t *testing.T) {
	p, _ := newAvailableProvider()
	if !p.Capabilities().DockerCompose {
		t.Fatal("available backend must advertise docker_compose=true")
	}

	pu := NewProvider(UnavailableBackend{})
	if pu.Capabilities().DockerCompose {
		t.Fatal("unavailable backend must advertise docker_compose=false (honest capability)")
	}

	if NewProvider(nil).Capabilities().DockerCompose {
		t.Fatal("nil backend must advertise docker_compose=false")
	}
}

func TestServiceExecRoutesToBackend(t *testing.T) {
	p, b := newAvailableProvider()
	res, err := p.ServiceExec(context.Background(), ServiceExecRequest{
		Service: "db",
		Command: []string{"psql", "-c", "select 1"},
		WorkDir: "/var/lib",
	})
	if err != nil {
		t.Fatalf("ServiceExec: %v", err)
	}
	if string(res.Stdout) != "ok" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	if len(b.execCalls) != 1 || b.execCalls[0].Service != "db" {
		t.Fatalf("backend not called once with service db: %+v", b.execCalls)
	}
}

func TestServiceExecRejectsBadServiceNameBeforeBackend(t *testing.T) {
	bad := []string{"", "../etc", "svc/../x", "a b", "/abs", ".hidden", strings.Repeat("a", 100)}
	for _, name := range bad {
		p, b := newAvailableProvider()
		_, err := p.ServiceExec(context.Background(), ServiceExecRequest{Service: name, Command: []string{"echo"}})
		if err == nil {
			t.Fatalf("service %q: expected validation error", name)
		}
		if len(b.execCalls) != 0 {
			t.Fatalf("service %q: backend must not be called on invalid input", name)
		}
	}
}

func TestServiceExecRejectsEmptyCommand(t *testing.T) {
	p, b := newAvailableProvider()
	_, err := p.ServiceExec(context.Background(), ServiceExecRequest{Service: "db", Command: nil})
	if err == nil {
		t.Fatal("expected error on empty command")
	}
	if len(b.execCalls) != 0 {
		t.Fatal("backend must not be called on empty command")
	}
}

func TestServiceExecRejectsBadWorkDir(t *testing.T) {
	p, b := newAvailableProvider()
	_, err := p.ServiceExec(context.Background(), ServiceExecRequest{
		Service: "db", Command: []string{"echo"}, WorkDir: "rel/../path",
	})
	if err == nil {
		t.Fatal("expected error on traversal workdir")
	}
	if len(b.execCalls) != 0 {
		t.Fatal("backend must not be called on invalid workdir")
	}
}

func TestDownloadFileRoutesAndValidatesPath(t *testing.T) {
	p, b := newAvailableProvider()
	got, err := p.ServiceDownloadFile(context.Background(), "db", "/data/dump.sql")
	if err != nil {
		t.Fatalf("download file: %v", err)
	}
	if string(got) != "file-bytes" {
		t.Fatalf("bytes = %q", got)
	}
	if len(b.dlFile) != 1 || b.dlFile[0] != "db:/data/dump.sql" {
		t.Fatalf("backend dlFile = %+v", b.dlFile)
	}

	for _, bad := range []string{"", "relative/path", "/data/../../etc/passwd", "/a/../b"} {
		p2, b2 := newAvailableProvider()
		if _, err := p2.ServiceDownloadFile(context.Background(), "db", bad); err == nil {
			t.Fatalf("path %q: expected validation error", bad)
		} else if len(b2.dlFile) != 0 {
			t.Fatalf("path %q: backend must not be called", bad)
		}
	}
}

func TestDownloadDirRoutesWithExclusions(t *testing.T) {
	p, b := newAvailableProvider()
	got, err := p.ServiceDownloadDir(context.Background(), "db", "/var/lib/data", []string{"*.tmp"})
	if err != nil {
		t.Fatalf("download dir: %v", err)
	}
	if string(got) != "tar-bytes" {
		t.Fatalf("bytes = %q", got)
	}
	if len(b.dlDir) != 1 || b.dlDir[0] != "db:/var/lib/data" {
		t.Fatalf("backend dlDir = %+v", b.dlDir)
	}

	p2, b2 := newAvailableProvider()
	if _, err := p2.ServiceDownloadDir(context.Background(), "db", "/a/../b", nil); err == nil {
		t.Fatal("expected traversal rejection")
	} else if len(b2.dlDir) != 0 {
		t.Fatal("backend must not be called on traversal")
	}
}

func TestIsDirRoutesAndValidates(t *testing.T) {
	p, b := newAvailableProvider()
	b.isDirVal = true
	ok, err := p.ServiceIsDir(context.Background(), "db", "/data")
	if err != nil || !ok {
		t.Fatalf("isdir = %v, %v", ok, err)
	}
	if len(b.isDir) != 1 {
		t.Fatalf("backend isDir = %+v", b.isDir)
	}

	p2, b2 := newAvailableProvider()
	if _, err := p2.ServiceIsDir(context.Background(), "bad/name", "/data"); err == nil {
		t.Fatal("expected service-name rejection")
	} else if len(b2.isDir) != 0 {
		t.Fatal("backend must not be called on invalid service")
	}
}

func TestStopServiceRoutesAndValidates(t *testing.T) {
	p, b := newAvailableProvider()
	if err := p.StopService(context.Background(), ServiceStopRequest{Service: "mock"}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(b.stopCalls) != 1 || b.stopCalls[0].Service != "mock" {
		t.Fatalf("backend stopCalls = %+v", b.stopCalls)
	}

	p2, b2 := newAvailableProvider()
	if err := p2.StopService(context.Background(), ServiceStopRequest{Service: "../x"}); err == nil {
		t.Fatal("expected validation error")
	} else if len(b2.stopCalls) != 0 {
		t.Fatal("backend must not be called on invalid service")
	}
}

func TestCollectOrchestratesAllHooksAndContinuesOnFailure(t *testing.T) {
	b := &recordingBackend{available: true, execResult: ExecResult{ExitCode: 0, Stdout: []byte("dump")}}
	p := NewProvider(b)

	hooks := []CollectHook{
		{Name: "pgdump", Service: "db", Command: []string{"pg_dump", "app"}},
		{Name: "bad-service", Service: "../evil", Command: []string{"cat"}},
		{Name: "requests", Service: "mock", Command: []string{"cat", "/log/requests.json"}},
	}
	results := p.Collect(context.Background(), hooks)
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	// Hook 0 ran on the backend and succeeded.
	if results[0].Name != "pgdump" || results[0].Err != nil || string(results[0].Output) != "dump" {
		t.Fatalf("hook 0 unexpected: %+v", results[0])
	}
	// Hook 1 had an invalid service name; validation error, backend not hit.
	if results[1].Err == nil {
		t.Fatal("hook 1 must carry a validation error")
	}
	// Hook 2 still ran despite hook 1 failing (collect-on-teardown gathers all).
	if results[2].Name != "requests" || results[2].Err != nil {
		t.Fatalf("hook 2 unexpected: %+v", results[2])
	}
	// Only the two valid hooks reached the backend.
	if len(b.execCalls) != 2 {
		t.Fatalf("backend exec calls = %d, want 2 (invalid hook skipped)", len(b.execCalls))
	}
}

func TestCollectRecordsBackendErrorPerHook(t *testing.T) {
	sentinel := errors.New("boom")
	b := &recordingBackend{available: true, execErr: sentinel}
	p := NewProvider(b)
	results := p.Collect(context.Background(), []CollectHook{{Name: "x", Service: "db", Command: []string{"echo"}}})
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("expected per-hook backend error, got %+v", results)
	}
	if !errors.Is(results[0].Err, sentinel) {
		t.Fatalf("error must wrap backend error, got %v", results[0].Err)
	}
}

func TestUnavailableBackendFailsClosed(t *testing.T) {
	p := NewProvider(UnavailableBackend{})

	if _, err := p.ServiceExec(context.Background(), ServiceExecRequest{Service: "db", Command: []string{"echo"}}); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("ServiceExec err = %v, want ErrBackendUnavailable", err)
	}
	if _, err := p.ServiceDownloadFile(context.Background(), "db", "/data/x"); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("DownloadFile err = %v", err)
	}
	if _, err := p.ServiceDownloadDir(context.Background(), "db", "/data", nil); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("DownloadDir err = %v", err)
	}
	if _, err := p.ServiceIsDir(context.Background(), "db", "/data"); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("IsDir err = %v", err)
	}
	if err := p.StopService(context.Background(), ServiceStopRequest{Service: "db"}); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("StopService err = %v", err)
	}

	// Collect over an unavailable backend records the fail-closed error per hook,
	// it does not panic or silently drop hooks.
	results := p.Collect(context.Background(), []CollectHook{{Name: "x", Service: "db", Command: []string{"echo"}}})
	if len(results) != 1 || !errors.Is(results[0].Err, ErrBackendUnavailable) {
		t.Fatalf("Collect over unavailable backend = %+v", results)
	}
}

func TestUnavailableValidationStillRunsFirst(t *testing.T) {
	// Even fail-closed, an invalid request is a validation error (input is the
	// problem), not the backend-unavailable error.
	p := NewProvider(UnavailableBackend{})
	_, err := p.ServiceExec(context.Background(), ServiceExecRequest{Service: "../x", Command: []string{"echo"}})
	if err == nil || errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("invalid input must fail validation before backend dispatch, got %v", err)
	}
}

func TestErrBackendUnavailableCarriesRemediation(t *testing.T) {
	msg := ErrBackendUnavailable.Error()
	if !strings.Contains(strings.ToLower(msg), "docker compose") {
		t.Fatalf("error must name the missing backend: %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "enable") {
		t.Fatalf("error must carry actionable remediation: %q", msg)
	}
}
