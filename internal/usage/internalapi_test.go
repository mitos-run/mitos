package usage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// seedStore returns a MemUsageStore with one record per org.
func seedStore(t *testing.T) (*MemUsageStore, time.Time) {
	t.Helper()
	store := NewMemUsageStore()
	now := time.Date(2026, 6, 26, 0, 0, 0, 0, time.UTC)
	if err := store.UpsertRecord(context.Background(), UsageRecord{OrgID: "alice", SandboxID: "a-sb", Window: now, VCPUSeconds: 100, EgressBytes: 1 << 30}); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if err := store.UpsertRecord(context.Background(), UsageRecord{OrgID: "bob", SandboxID: "b-sb", Window: now, VCPUSeconds: 999}); err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	return store, now
}

// TestInternalUsageHandlerRoundTrip asserts the internal handler serves the
// header org's records when the bearer token matches.
func TestInternalUsageHandlerRoundTrip(t *testing.T) {
	store, _ := seedStore(t)
	h := NewInternalUsageHandler(store, DefaultPriceList(), "s3cr3t")

	r := httptest.NewRequest("GET", "/internal/usage", nil)
	r.Header.Set("Authorization", "Bearer s3cr3t")
	r.Header.Set(InternalOrgHeader, "alice")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

// TestInternalUsageHandlerBadTokenRefused asserts a wrong/missing bearer token
// is refused before any usage is served.
func TestInternalUsageHandlerBadTokenRefused(t *testing.T) {
	store, _ := seedStore(t)
	h := NewInternalUsageHandler(store, DefaultPriceList(), "s3cr3t")

	for _, auth := range []string{"", "Bearer wrong", "s3cr3t"} {
		r := httptest.NewRequest("GET", "/internal/usage", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		r.Header.Set(InternalOrgHeader, "alice")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusOK {
			t.Fatalf("auth=%q served usage; must be refused", auth)
		}
	}
}

// TestInternalUsageHandlerEmptyTokenFailsClosed asserts a handler built with an
// empty token refuses every request rather than serving usage unauthenticated.
func TestInternalUsageHandlerEmptyTokenFailsClosed(t *testing.T) {
	store, _ := seedStore(t)
	h := NewInternalUsageHandler(store, DefaultPriceList(), "")
	r := httptest.NewRequest("GET", "/internal/usage", nil)
	r.Header.Set(InternalOrgHeader, "alice")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Fatalf("empty-token handler served usage; must fail closed")
	}
}

// TestHTTPStoreReadsOnlyItsOrg drives the HTTPStore against the internal handler
// end-to-end and asserts each org reads ONLY its own records, never the other's.
func TestHTTPStoreReadsOnlyItsOrg(t *testing.T) {
	store, _ := seedStore(t)
	h := NewInternalUsageHandler(store, DefaultPriceList(), "s3cr3t")
	srv := httptest.NewServer(h)
	defer srv.Close()

	client := NewHTTPStore(srv.URL, "s3cr3t", srv.Client())

	alice, err := client.ListRecords(context.Background(), "alice", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("alice ListRecords: %v", err)
	}
	if len(alice) != 1 || alice[0].SandboxID != "a-sb" {
		t.Fatalf("alice records = %+v, want exactly a-sb", alice)
	}
	for _, rec := range alice {
		if rec.OrgID != "alice" {
			t.Fatalf("cross-org record in alice read: %+v", rec)
		}
	}

	bob, err := client.ListRecords(context.Background(), "bob", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("bob ListRecords: %v", err)
	}
	if len(bob) != 1 || bob[0].SandboxID != "b-sb" {
		t.Fatalf("bob records = %+v, want exactly b-sb", bob)
	}
}

// TestHTTPStoreEmptyOrgIsEmpty asserts an org with no usage reads as an empty,
// non-nil slice (never another org's records).
func TestHTTPStoreEmptyOrgIsEmpty(t *testing.T) {
	store, _ := seedStore(t)
	h := NewInternalUsageHandler(store, DefaultPriceList(), "s3cr3t")
	srv := httptest.NewServer(h)
	defer srv.Close()

	client := NewHTTPStore(srv.URL, "s3cr3t", srv.Client())
	recs, err := client.ListRecords(context.Background(), "carol", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("carol ListRecords: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("carol records = %+v, want empty", recs)
	}
}

// TestHTTPStoreBadTokenErrors asserts the client surfaces an error (never a
// silent empty success) when the controller refuses the token.
func TestHTTPStoreBadTokenErrors(t *testing.T) {
	store, _ := seedStore(t)
	h := NewInternalUsageHandler(store, DefaultPriceList(), "s3cr3t")
	srv := httptest.NewServer(h)
	defer srv.Close()

	client := NewHTTPStore(srv.URL, "wrong", srv.Client())
	if _, err := client.ListRecords(context.Background(), "alice", time.Time{}, time.Time{}); err == nil {
		t.Fatalf("bad-token ListRecords returned nil error; want an error")
	}
}

// TestHTTPStoreUpsertIsReadOnly asserts the console-side store rejects writes.
func TestHTTPStoreUpsertIsReadOnly(t *testing.T) {
	client := NewHTTPStore("http://unused", "t", http.DefaultClient)
	if err := client.UpsertRecord(context.Background(), UsageRecord{OrgID: "alice"}); err == nil {
		t.Fatalf("UpsertRecord returned nil; want ErrReadOnly")
	}
}

// TestHTTPStoreImplementsUsageStore is a compile-time seam assertion.
func TestHTTPStoreImplementsUsageStore(t *testing.T) {
	var _ UsageStore = (*HTTPStore)(nil)
}
