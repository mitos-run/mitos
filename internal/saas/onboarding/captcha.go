package onboarding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultFriendlyCaptchaBaseURL is the Friendly Captcha v2 siteverify base URL.
const defaultFriendlyCaptchaBaseURL = "https://global.frcapi.com"

// maxCaptchaResponseBytes caps the Friendly Captcha API response body to guard
// against an oversized response from a misconfigured or malicious server.
const maxCaptchaResponseBytes = 1 << 14 // 16 KiB

// captchaTimeout is the maximum time the Friendly Captcha API call may take,
// applied as a child context on top of the caller's context. A short timeout
// ensures a slow external API never holds up the signup handler.
const captchaTimeout = 10 * time.Second

// CaptchaVerifier verifies a captcha solution server-side. Verify returns nil
// when the solution is valid, a non-nil error when it is invalid or verification
// failed. The no-op verifier (used when unconfigured) always returns nil
// (pass-through).
type CaptchaVerifier interface {
	Verify(ctx context.Context, solution string) error
}

// NoopCaptcha passes every solution (self-host / unconfigured).
type NoopCaptcha struct{}

// Verify always returns nil.
func (NoopCaptcha) Verify(context.Context, string) error { return nil }

// friendlyCaptcha calls the Friendly Captcha v2 siteverify API.
type friendlyCaptcha struct {
	apiKey  string
	siteKey string
	baseURL string
	client  *http.Client
}

// NewFriendlyCaptcha returns a CaptchaVerifier that validates solutions via the
// Friendly Captcha v2 API (EU-sovereign). The apiKey is sent only in the
// X-API-Key request header; it is never included in any returned error or log.
// The solution is sent in the request body and is never included in any error.
// If baseURL is empty the production endpoint (defaultFriendlyCaptchaBaseURL) is
// used. If client is nil http.DefaultClient is used.
func NewFriendlyCaptcha(apiKey, siteKey, baseURL string, client *http.Client) CaptchaVerifier {
	if baseURL == "" {
		baseURL = defaultFriendlyCaptchaBaseURL
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &friendlyCaptcha{
		apiKey:  apiKey,
		siteKey: siteKey,
		baseURL: baseURL,
		client:  client,
	}
}

// Verify submits the solution to the Friendly Captcha v2 siteverify endpoint
// and returns nil when the solution is accepted. A non-2xx HTTP status or a
// success:false body returns a non-nil error. The API key and the solution are
// never present in the returned error.
func (f *friendlyCaptcha) Verify(ctx context.Context, solution string) error {
	ctx, cancel := context.WithTimeout(ctx, captchaTimeout)
	defer cancel()

	payload, err := json.Marshal(struct {
		Response string `json:"response"`
		SiteKey  string `json:"sitekey"`
	}{
		Response: solution,
		SiteKey:  f.siteKey,
	})
	if err != nil {
		return fmt.Errorf("captcha: marshal request: %w", err)
	}

	url := f.baseURL + "/api/v2/captcha/siteverify"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("captcha: build request: %w", err)
	}
	req.Header.Set("X-API-Key", f.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		// Distinguish context cancellation / timeout from other network errors.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return fmt.Errorf("ctx: %w", err)
		}
		return errors.New("captcha: request failed")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCaptchaResponseBytes))
	if err != nil {
		return fmt.Errorf("captcha: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("captcha: verification returned status %d", resp.StatusCode)
	}

	var out struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("captcha: malformed response: %w", err)
	}
	if !out.Success {
		return errors.New("captcha: solution invalid")
	}
	return nil
}
