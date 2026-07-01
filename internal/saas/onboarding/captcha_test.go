package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testAPIKey = "test-api-key-secret"
const testSiteKey = "test-site-key"

// newFriendlyCaptchaServer starts an httptest.Server that responds to the
// Friendly Captcha v2 siteverify path. It records the last request received.
func newFriendlyCaptchaServer(success bool, status int) (*httptest.Server, *captchaRequest) {
	last := &captchaRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.apiKey = r.Header.Get("X-API-Key")
		last.contentType = r.Header.Get("Content-Type")
		last.path = r.URL.Path
		var body struct {
			Response string `json:"response"`
			SiteKey  string `json:"sitekey"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		last.solution = body.Response
		last.siteKey = body.SiteKey

		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if success {
			_, _ = w.Write([]byte(`{"success":true}`))
		} else {
			_, _ = w.Write([]byte(`{"success":false}`))
		}
	}))
	return srv, last
}

// captchaRequest captures fields from the last call to the fake server.
type captchaRequest struct {
	apiKey      string
	contentType string
	path        string
	solution    string
	siteKey     string
}

// TestFriendlyCaptchaValidSolution asserts that a server returning success:true
// yields a nil error, and that the request carried the X-API-Key header and the
// solution + sitekey in the body.
func TestFriendlyCaptchaValidSolution(t *testing.T) {
	srv, last := newFriendlyCaptchaServer(true, http.StatusOK)
	defer srv.Close()

	v := NewFriendlyCaptcha(testAPIKey, testSiteKey, srv.URL, srv.Client())
	if err := v.Verify(context.Background(), "valid-solution"); err != nil {
		t.Fatalf("Verify returned non-nil error for valid solution: %v", err)
	}

	if last.apiKey != testAPIKey {
		t.Errorf("X-API-Key header = %q, want %q", last.apiKey, testAPIKey)
	}
	if last.solution != "valid-solution" {
		t.Errorf("request body solution = %q, want %q", last.solution, "valid-solution")
	}
	if last.siteKey != testSiteKey {
		t.Errorf("request body sitekey = %q, want %q", last.siteKey, testSiteKey)
	}
}

// TestFriendlyCaptchaInvalidSolution asserts that a server returning
// success:false yields a non-nil error.
func TestFriendlyCaptchaInvalidSolution(t *testing.T) {
	srv, _ := newFriendlyCaptchaServer(false, http.StatusOK)
	defer srv.Close()

	v := NewFriendlyCaptcha(testAPIKey, testSiteKey, srv.URL, srv.Client())
	err := v.Verify(context.Background(), "bad-solution")
	if err == nil {
		t.Fatal("Verify returned nil error for invalid solution (success:false)")
	}
	// A definitive rejection must be ErrCaptchaInvalid so the signup handler fails
	// CLOSED on it (as opposed to a transient error, which fails open).
	if !errors.Is(err, ErrCaptchaInvalid) {
		t.Fatalf("invalid-solution error = %v, want ErrCaptchaInvalid", err)
	}
}

// TestFriendlyCaptchaNon2xxStatus asserts that a non-2xx API response yields a
// non-nil error that is NOT the definitive-rejection sentinel (so the handler
// fails OPEN on a provider fault rather than dropping the signup).
func TestFriendlyCaptchaNon2xxStatus(t *testing.T) {
	srv, _ := newFriendlyCaptchaServer(false, http.StatusUnauthorized)
	defer srv.Close()

	v := NewFriendlyCaptcha(testAPIKey, testSiteKey, srv.URL, srv.Client())
	err := v.Verify(context.Background(), "any-solution")
	if err == nil {
		t.Fatal("Verify returned nil error for non-2xx API response")
	}
	if errors.Is(err, ErrCaptchaInvalid) {
		t.Fatal("a non-2xx provider fault must NOT be ErrCaptchaInvalid (it should fail open)")
	}
}

// TestFriendlyCaptchaSecretsNotInError asserts that neither the API key nor the
// submitted solution ever appears in any error returned by Verify, across all
// failure scenarios.
func TestFriendlyCaptchaSecretsNotInError(t *testing.T) {
	const solution = "some-solution"
	cases := []struct {
		name    string
		success bool
		status  int
	}{
		{"invalid solution", false, http.StatusOK},
		{"non-2xx", false, http.StatusForbidden},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newFriendlyCaptchaServer(tc.success, tc.status)
			defer srv.Close()

			v := NewFriendlyCaptcha(testAPIKey, testSiteKey, srv.URL, srv.Client())
			err := v.Verify(context.Background(), solution)
			if err == nil {
				t.Fatal("Verify returned nil error; expected a failure")
			}
			if strings.Contains(err.Error(), testAPIKey) {
				t.Errorf("error message contains the API key (secret leak): %v", err)
			}
			if strings.Contains(err.Error(), solution) {
				t.Errorf("error message contains the submitted solution (leak): %v", err)
			}
		})
	}
}

// TestNoopCaptchaAlwaysPasses asserts that NoopCaptcha.Verify always returns nil.
func TestNoopCaptchaAlwaysPasses(t *testing.T) {
	var v CaptchaVerifier = NoopCaptcha{}
	for _, solution := range []string{"", "bad", "ok", "any-value"} {
		if err := v.Verify(context.Background(), solution); err != nil {
			t.Errorf("NoopCaptcha.Verify(%q) = %v, want nil", solution, err)
		}
	}
}
