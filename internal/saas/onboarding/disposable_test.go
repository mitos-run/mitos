package onboarding

import (
	"testing"
)

// TestDisposableBlocksKnownDomain checks that a domain on the blocklist is blocked
// and a domain that is not on the list is allowed.
func TestDisposableBlocksKnownDomain(t *testing.T) {
	d := NewDisposable([]string{"mailinator.com"}, nil)
	if !d.Blocked("mailinator.com") {
		t.Fatal("mailinator.com must be blocked")
	}
	if d.Blocked("gmail.com") {
		t.Fatal("gmail.com must not be blocked")
	}
}

// TestDisposableNilReceiverAllows asserts a nil *Disposable never blocks
// (self-host / unconfigured safe default).
func TestDisposableNilReceiverAllows(t *testing.T) {
	var d *Disposable
	if d.Blocked("mailinator.com") {
		t.Fatal("nil *Disposable must return false for every domain")
	}
}

// TestDisposableStaffAllowExempts asserts that a domain on the staff-allow list
// is NOT blocked even when it is also on the blocklist.
func TestDisposableStaffAllowExempts(t *testing.T) {
	d := NewDisposable([]string{"x.com"}, []string{"x.com"})
	if d.Blocked("x.com") {
		t.Fatal("staff-allowed domain must not be blocked")
	}
}

// TestDisposableCaseInsensitive asserts Blocked folds domain case before checking.
func TestDisposableCaseInsensitive(t *testing.T) {
	d := NewDisposable([]string{"Mailinator.COM"}, nil)
	if !d.Blocked("MAILINATOR.com") {
		t.Fatal("Blocked must match case-insensitively")
	}
	if !d.Blocked("mailinator.com") {
		t.Fatal("Blocked must match lowercase form")
	}
}

// TestLoadDisposableEmbeddedContainsMember asserts LoadDisposable reads the
// embedded JSON and that at least mailinator.com is blocked (a known member).
func TestLoadDisposableEmbeddedContainsMember(t *testing.T) {
	d, err := LoadDisposable("")
	if err != nil {
		t.Fatalf("LoadDisposable: %v", err)
	}
	if !d.Blocked("mailinator.com") {
		t.Fatal("embedded list must include mailinator.com as blocked")
	}
	if d.Blocked("example.com") {
		t.Fatal("example.com must not be blocked by the embedded list")
	}
}

// TestLoadDisposableStaffAllowCSVExempts asserts that passing a comma-separated
// staff-allow list prevents those domains from being blocked even when they appear
// in the embedded blocklist.
func TestLoadDisposableStaffAllowCSVExempts(t *testing.T) {
	d, err := LoadDisposable("mailinator.com")
	if err != nil {
		t.Fatalf("LoadDisposable: %v", err)
	}
	if d.Blocked("mailinator.com") {
		t.Fatal("staff-allowed mailinator.com must not be blocked")
	}
}
