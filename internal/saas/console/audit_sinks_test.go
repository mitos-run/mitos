package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestSinksOrgScopedCRUD verifies that sinks are scoped to the org: a sink
// created for orgA is invisible to orgB, and delete by a different org returns
// not found.
func TestSinksOrgScopedCRUD(t *testing.T) {
	reg := NewMemSinkRegistry()
	c := New(Deps{Sinks: reg})

	// POST a sink for orgA.
	post := httptest.NewRequest(
		"POST", "/console/audit/sinks",
		strings.NewReader(`{"type":"webhook","endpoint":"https://siem.example/hook"}`),
	).WithContext(WithCaller(context.Background(), "acct", "orgA"))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, post)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create sink status %d, body=%s", rr.Code, rr.Body.String())
	}

	// orgB sees no sinks.
	get := httptest.NewRequest("GET", "/console/audit/sinks", nil).
		WithContext(WithCaller(context.Background(), "acct", "orgB"))
	rr2 := httptest.NewRecorder()
	c.ServeHTTP(rr2, get)
	if want := `"sinks":[]`; !strings.Contains(rr2.Body.String(), want) {
		t.Fatalf("orgB should see no sinks, got %s", rr2.Body.String())
	}
}

// TestSinksListReturnsOwnOrg verifies that GET /console/audit/sinks returns the
// caller org's sinks, not another org's.
func TestSinksListReturnsOwnOrg(t *testing.T) {
	reg := NewMemSinkRegistry()

	// Add a sink for orgA via the registry directly.
	if _, err := reg.Add(context.Background(), "orgA", "webhook", "https://a.example/"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Add a sink for orgB.
	if _, err := reg.Add(context.Background(), "orgB", "webhook", "https://b.example/"); err != nil {
		t.Fatalf("Add orgB: %v", err)
	}

	c := New(Deps{Sinks: reg})

	// orgA should see exactly its own sink.
	get := httptest.NewRequest("GET", "/console/audit/sinks", nil).
		WithContext(WithCaller(context.Background(), "acct-a", "orgA"))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, get)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status %d, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "https://a.example/") {
		t.Errorf("orgA list should contain its endpoint, got %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "https://b.example/") {
		t.Errorf("orgA list must not contain orgB endpoint, got %s", rr.Body.String())
	}
}

// TestSinksDeleteCrossOrgReturnsNotFound verifies that deleting a sink owned by
// a different org returns 404.
func TestSinksDeleteCrossOrgReturnsNotFound(t *testing.T) {
	reg := NewMemSinkRegistry()
	cfg, err := reg.Add(context.Background(), "orgA", "webhook", "https://a.example/")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	c := New(Deps{Sinks: reg})

	// orgB tries to delete orgA's sink.
	del := httptest.NewRequest("DELETE", "/console/audit/sinks/"+cfg.ID, nil).
		WithContext(WithCaller(context.Background(), "acct-b", "orgB"))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, del)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-org delete status %d, want 404; body=%s", rr.Code, rr.Body.String())
	}

	// orgA's sink must still be there.
	sinks := reg.List(context.Background(), "orgA")
	if len(sinks) != 1 {
		t.Errorf("orgA should still have 1 sink after cross-org delete attempt, got %d", len(sinks))
	}
}

// TestDispatchDeliversToEnabledSink verifies that DispatchingRecorder delivers
// audit events to enabled sinks for the matching org, using WaitForDispatch to
// make the best-effort goroutine deterministic.
func TestDispatchDeliversToEnabledSink(t *testing.T) {
	reg := NewMemSinkRegistry()
	cfg, err := reg.Add(context.Background(), "orgA", "webhook", "https://x")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	var mu sync.Mutex
	delivered := 0
	fake := sinkFunc(func(_ context.Context, c SinkConfig, _ AuditEvent) error {
		if c.ID == cfg.ID {
			mu.Lock()
			delivered++
			mu.Unlock()
		}
		return nil
	})

	rec := NewDispatchingRecorder(NewMemAuditLog(), reg, fake)
	if err := rec.Record(context.Background(), AuditEvent{OrgID: "orgA", Action: "key.create"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// WaitForDispatch blocks until the best-effort dispatch goroutine drains.
	rec.WaitForDispatch()

	mu.Lock()
	got := delivered
	mu.Unlock()
	if got != 1 {
		t.Fatalf("delivered = %d, want 1", got)
	}
}

// TestDispatchSinkFailureDoesNotFailRecord verifies that a delivery failure
// never propagates back to the Record caller.
func TestDispatchSinkFailureDoesNotFailRecord(t *testing.T) {
	reg := NewMemSinkRegistry()
	if _, err := reg.Add(context.Background(), "orgA", "webhook", "https://x"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	alwaysFail := sinkFunc(func(_ context.Context, _ SinkConfig, _ AuditEvent) error {
		return &testSinkError{"simulated delivery failure"}
	})

	rec := NewDispatchingRecorder(NewMemAuditLog(), reg, alwaysFail)
	err := rec.Record(context.Background(), AuditEvent{OrgID: "orgA", Action: "key.create"})
	rec.WaitForDispatch()
	if err != nil {
		t.Fatalf("Record returned error despite sink failure: %v", err)
	}
}

// TestDispatchDoesNotDeliverToDisabledSink verifies that a sink with
// Enabled=false is skipped during dispatch.
func TestDispatchDoesNotDeliverToDisabledSink(t *testing.T) {
	reg := NewMemSinkRegistry()
	cfg, err := reg.Add(context.Background(), "orgA", "webhook", "https://x")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Disable the sink by deleting and re-adding is not the pattern; instead
	// confirm that the registry's Add returns enabled=true, and test the
	// DispatchingRecorder skips disabled sinks via a tampered list.
	//
	// We use a custom SinkRegistry that returns the sink as disabled.
	disabled := cfg
	disabled.Enabled = false
	stubReg := &stubSinkRegistry{sinks: map[string][]SinkConfig{"orgA": {disabled}}}

	var mu sync.Mutex
	delivered := 0
	fake := sinkFunc(func(_ context.Context, _ SinkConfig, _ AuditEvent) error {
		mu.Lock()
		delivered++
		mu.Unlock()
		return nil
	})

	rec := NewDispatchingRecorder(NewMemAuditLog(), stubReg, fake)
	if err := rec.Record(context.Background(), AuditEvent{OrgID: "orgA", Action: "key.create"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rec.WaitForDispatch()

	mu.Lock()
	got := delivered
	mu.Unlock()
	if got != 0 {
		t.Fatalf("delivered to disabled sink: got %d, want 0", got)
	}
}

// TestEveryEndpointRefusesMissingOrgContextSinks extends the auth-gate table
// with the three sink endpoints.
func TestEveryEndpointRefusesMissingOrgContextSinks(t *testing.T) {
	c := New(Deps{})
	endpoints := []struct{ method, target string }{
		{"GET", "/console/audit/sinks"},
		{"POST", "/console/audit/sinks"},
		{"DELETE", "/console/audit/sinks/some-id"},
	}
	for _, ep := range endpoints {
		r := httptest.NewRequest(ep.method, ep.target, nil)
		w := httptest.NewRecorder()
		c.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without org context = %d, want 401", ep.method, ep.target, w.Code)
		}
	}
}

// TestSinksCreateValidation verifies that POST /console/audit/sinks rejects
// unknown types and non-https endpoints with 400, and accepts a valid request
// with 201.
func TestSinksCreateValidation(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
	}{
		{
			name:     "unknown type is rejected",
			body:     `{"type":"syslog","endpoint":"https://siem.example/hook"}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "http endpoint is rejected",
			body:     `{"type":"webhook","endpoint":"http://internal/hook"}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "javascript scheme is rejected",
			body:     `{"type":"webhook","endpoint":"javascript:alert(1)"}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "empty endpoint is rejected",
			body:     `{"type":"webhook","endpoint":""}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "valid webhook https endpoint is accepted",
			body:     `{"type":"webhook","endpoint":"https://siem.example/hook"}`,
			wantCode: http.StatusCreated,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(Deps{})
			req := httptest.NewRequest(
				"POST", "/console/audit/sinks",
				strings.NewReader(tc.body),
			).WithContext(WithCaller(context.Background(), "acct", "orgA"))
			rr := httptest.NewRecorder()
			c.ServeHTTP(rr, req)
			if rr.Code != tc.wantCode {
				t.Errorf("body=%s: got status %d, want %d; response=%s",
					tc.body, rr.Code, tc.wantCode, rr.Body.String())
			}
		})
	}
}

// testSinkError is a test-only error type for sink delivery failures.
type testSinkError struct{ msg string }

func (e *testSinkError) Error() string { return e.msg }

// stubSinkRegistry is a minimal SinkRegistry used in tests to control which
// sinks are returned by List, without going through the real Add/Delete flow.
type stubSinkRegistry struct {
	sinks map[string][]SinkConfig
}

func (s *stubSinkRegistry) List(_ context.Context, orgID string) []SinkConfig {
	return s.sinks[orgID]
}

func (s *stubSinkRegistry) Add(_ context.Context, orgID, sinkType, endpoint string) (SinkConfig, error) {
	cfg := SinkConfig{ID: "stub-id", OrgID: orgID, Type: sinkType, Endpoint: endpoint, Enabled: true}
	s.sinks[orgID] = append(s.sinks[orgID], cfg)
	return cfg, nil
}

func (s *stubSinkRegistry) Delete(_ context.Context, orgID, id string) error {
	sinks := s.sinks[orgID]
	for i, sc := range sinks {
		if sc.ID == id {
			s.sinks[orgID] = append(sinks[:i], sinks[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}
