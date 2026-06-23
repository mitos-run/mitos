package mitos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// httpClient is the minimal subset of *http.Client the transport needs. It is an
// interface so callers can inject a custom client via WithHTTPClient and tests
// can stub it.
type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// transport carries the resolved base URL and bearer token and issues the
// sandbox-server REST calls. The token VALUE is never logged and is redacted
// from any error body before it surfaces in an *Error.
type transport struct {
	baseURL string
	token   string
	http    httpClient
}

// errEnvelope is the server error wire shape: {error:{code, message, cause,
// remediation}}. Mirrors internal/apierr.Encode.
type errEnvelope struct {
	Error struct {
		Code        string `json:"code"`
		Message     string `json:"message"`
		Cause       string `json:"cause"`
		Remediation string `json:"remediation"`
	} `json:"error"`
}

// do issues a request with the given method, path, optional JSON body, and
// optional extra headers, and decodes a 2xx JSON response into out (when out is
// non-nil). A non-2xx response is parsed into an *Error from the envelope; a body
// that is not the envelope still yields an *Error carrying the raw (redacted)
// body as the cause. The bearer token rides on the Authorization header when set
// and is never placed in an error.
func (t *transport) do(ctx context.Context, method, path string, body any, extra map[string]string, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return t.parseError(resp.StatusCode, respBody)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// parseError turns a non-2xx response into an *Error. It prefers the structured
// envelope; when the body is not the envelope it falls back to the raw body as
// the cause and a generic code. The bearer token is redacted from every field so
// a server that reflects it never leaks it into the error.
func (t *transport) parseError(status int, body []byte) error {
	var env errEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Code != "" {
		return &Error{
			Code:        env.Error.Code,
			Message:     t.redact(env.Error.Message),
			Cause:       t.redact(env.Error.Cause),
			Remediation: t.redact(env.Error.Remediation),
			Status:      status,
		}
	}
	return &Error{
		Code:    "http_error",
		Message: fmt.Sprintf("server returned HTTP %d", status),
		Cause:   t.redact(strings.TrimSpace(string(body))),
		Status:  status,
	}
}

// redact removes the configured bearer token from a string so it never surfaces
// in an error. When no token is configured the string is returned unchanged.
func (t *transport) redact(s string) string {
	if t.token == "" || s == "" {
		return s
	}
	return strings.ReplaceAll(s, t.token, "[REDACTED]")
}
