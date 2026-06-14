package workspace

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// S3Credentials holds the access-key id and secret-access-key resolved from the
// referenced Secret. SecretAccessKey is a secret VALUE: it is held only in
// memory for the duration of a request, used solely to derive the SigV4 signing
// key, and is NEVER logged, never placed in an error, and never written to a
// host path. AccessKeyID is sent on the wire as part of the SigV4 credential
// scope (standard for S3) and is not treated as a high-sensitivity secret, but
// it is still not logged by this client.
type S3Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// S3HTTPClient is a minimal, dependency-free S3-compatible object client that
// signs requests with AWS Signature Version 4. It implements ObjectClient over a
// real bucket (AWS S3, MinIO, Ceph RGW, etc) so the S3Store backend works in
// production without pulling in a large SDK. Only the three operations the
// content-addressed store needs are implemented: PUT, GET, and HEAD of a single
// object by key.
//
// Credential discipline: the secret-access-key never appears in a log line, an
// error, or a header value sent in cleartext; it is used only as the seed for
// the SigV4 HMAC signing-key chain.
type S3HTTPClient struct {
	httpClient *http.Client
	endpoint   string // base endpoint, e.g. https://s3.us-east-1.amazonaws.com or http://minio:9000
	bucket     string
	region     string
	creds      S3Credentials
	// pathStyle addresses objects as <endpoint>/<bucket>/<key> rather than the
	// virtual-host <bucket>.<endpoint>/<key> form. Path style is the safe default
	// for S3-compatible stores (MinIO, Ceph) that do not do virtual-host routing.
	pathStyle bool
}

// S3HTTPConfig configures an S3HTTPClient.
type S3HTTPConfig struct {
	Endpoint  string
	Bucket    string
	Region    string
	Creds     S3Credentials
	PathStyle bool
	// HTTPClient is optional; a default with a sane timeout is used when nil.
	HTTPClient *http.Client
}

// NewS3HTTPClient builds an S3-compatible client from cfg. Region defaults to
// us-east-1, endpoint to the AWS regional endpoint when unset.
func NewS3HTTPClient(cfg S3HTTPConfig) (*S3HTTPClient, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("S3 bucket is required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", region)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &S3HTTPClient{
		httpClient: hc,
		endpoint:   endpoint,
		bucket:     cfg.Bucket,
		region:     region,
		creds:      cfg.Creds,
		pathStyle:  cfg.PathStyle,
	}, nil
}

// objectURL builds the request URL for a key.
func (c *S3HTTPClient) objectURL(key string) (string, string, error) {
	key = strings.TrimLeft(key, "/")
	var raw string
	if c.pathStyle {
		raw = fmt.Sprintf("%s/%s/%s", c.endpoint, c.bucket, key)
	} else {
		// virtual-host: <bucket>.<host>/<key>
		u, err := url.Parse(c.endpoint)
		if err != nil {
			return "", "", fmt.Errorf("parse endpoint: %w", err)
		}
		u.Host = c.bucket + "." + u.Host
		u.Path = "/" + key
		raw = u.String()
	}
	return raw, key, nil
}

// PutObject uploads bytes to a key.
func (c *S3HTTPClient) PutObject(ctx context.Context, key string, r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read object body: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPut, key, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // draining below
	if resp.StatusCode/100 != 2 {
		return c.statusError("put", key, resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain for connection reuse
	return nil
}

// GetObject fetches a key. A 404 maps to errObjectNotFound.
func (c *S3HTTPClient) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close() //nolint:errcheck // discarding
		return nil, errObjectNotFound
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close() //nolint:errcheck // draining in statusError
		return nil, c.statusError("get", key, resp)
	}
	return resp.Body, nil
}

// HeadObject reports whether a key exists.
func (c *S3HTTPClient) HeadObject(ctx context.Context, key string) (bool, error) {
	resp, err := c.do(ctx, http.MethodHead, key, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close() //nolint:errcheck // HEAD has no body
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode/100 != 2 {
		return false, c.statusError("head", key, resp)
	}
	return true, nil
}

// statusError builds an error from a non-2xx response without leaking
// credentials. The response body of an S3 error is XML describing the failure
// (no credentials), so a bounded prefix is safe to include for remediation.
func (c *S3HTTPClient) statusError(op, key string, resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("s3 %s object %q: status %d: %s", op, key, resp.StatusCode, strings.TrimSpace(string(snippet)))
}

// do builds, signs (SigV4), and sends a request.
func (c *S3HTTPClient) do(ctx context.Context, method, key string, body []byte) (*http.Response, error) {
	rawURL, _, err := c.objectURL(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if err := c.signV4(req, body); err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3 request: %w", err)
	}
	return resp, nil
}

// signV4 signs req in place with AWS Signature Version 4 for the s3 service.
// The secret-access-key is used only to derive the signing key and never appears
// in a header value or log line.
func (c *S3HTTPClient) signV4(req *http.Request, body []byte) error {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := hex.EncodeToString(sha256sum(body))
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if req.Header.Get("Host") == "" {
		req.Header.Set("Host", req.URL.Host)
	}

	// Canonical request.
	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		req.URL.Host, payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	// String to sign.
	scope := strings.Join([]string{dateStamp, c.region, "s3", "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sha256sum([]byte(canonicalRequest))),
	}, "\n")

	// Signing key chain.
	kDate := hmacSHA256([]byte("AWS4"+c.creds.SecretAccessKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(c.region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.creds.AccessKeyID, scope, signedHeaders, signature)
	req.Header.Set("Authorization", auth)
	return nil
}

func sha256sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

var _ ObjectClient = (*S3HTTPClient)(nil)
