package saas

import (
	"errors"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// metricsFixture builds a gateway fixture with a registered GatewayMetrics so a
// test can assert the counters the #617 SaaS alerts fire on.
func metricsFixture(t *testing.T, quota QuotaEnforcer) (gatewayFixture, *GatewayMetrics) {
	t.Helper()
	f := newGatewayFixture(t, quota)
	m := NewGatewayMetrics()
	m.MustRegister(prometheus.NewRegistry())
	WithGatewayMetrics(m)(f.gw)
	return f, m
}

// TestGatewayMetricsCountsSuccess asserts a forwarded 200 lands in the 2xx
// request class and no auth denial is recorded.
func TestGatewayMetricsCountsSuccess(t *testing.T) {
	f, m := metricsFixture(t, nil)
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", f.rawA, `{"pool":"default"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("2xx")); got != 1 {
		t.Errorf("requests{2xx} = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(m.authDenials); got != 0 {
		t.Errorf("auth denial series = %v, want 0", got)
	}
}

// TestGatewayMetricsCountsMissingKey asserts a request with no bearer key is a
// 4xx request AND an auth denial with reason missing_key.
func TestGatewayMetricsCountsMissingKey(t *testing.T) {
	f, m := metricsFixture(t, nil)
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", "", "{}")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("4xx")); got != 1 {
		t.Errorf("requests{4xx} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.authDenials.WithLabelValues("missing_key")); got != 1 {
		t.Errorf("authDenials{missing_key} = %v, want 1", got)
	}
}

// TestGatewayMetricsCountsInvalidKey asserts an unknown key is counted as an
// unauthorized auth denial.
func TestGatewayMetricsCountsInvalidKey(t *testing.T) {
	f, m := metricsFixture(t, nil)
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", "mitos_live_not_a_real_key", "{}")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := testutil.ToFloat64(m.authDenials.WithLabelValues("unauthorized")); got != 1 {
		t.Errorf("authDenials{unauthorized} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("4xx")); got != 1 {
		t.Errorf("requests{4xx} = %v, want 1", got)
	}
}

// TestGatewayMetricsCountsControlPlaneFailure asserts a control-plane error is
// counted in the 5xx class (the series the GatewayErrorRateHigh alert rates).
func TestGatewayMetricsCountsControlPlaneFailure(t *testing.T) {
	f, m := metricsFixture(t, nil)
	f.cp.err = errors.New("control plane down")
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", f.rawA, "{}")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("5xx")); got != 1 {
		t.Errorf("requests{5xx} = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(m.authDenials); got != 0 {
		t.Errorf("auth denial series = %v, want 0 (a control-plane failure is not an auth denial)", got)
	}
}

// TestGatewayMetricsQuotaDenialIsNotAuthDenial asserts a quota denial counts as
// a request (4xx) but NOT as an auth denial: the GatewayAuthDenialSpike alert
// must not fire because an org hit its cap.
func TestGatewayMetricsQuotaDenialIsNotAuthDenial(t *testing.T) {
	f, m := metricsFixture(t, denyQuota{})
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", f.rawA, "{}")
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("status = %d, want a 4xx quota denial", rec.Code)
	}
	if got := testutil.CollectAndCount(m.authDenials); got != 0 {
		t.Errorf("auth denial series = %v, want 0 for a quota denial", got)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("4xx")); got != 1 {
		t.Errorf("requests{4xx} = %v, want 1", got)
	}
}

// TestGatewayWithoutMetricsIsSafe asserts a gateway with no metrics wired (the
// default, every existing caller) serves without panicking.
func TestGatewayWithoutMetricsIsSafe(t *testing.T) {
	f := newGatewayFixture(t, nil)
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", f.rawA, "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestCodeClassBoundsGarbageStatus asserts an out-of-range status collapses to
// the single "other" label (a misbehaving upstream can never mint unbounded
// code_class series), while in-range statuses bucket by class.
func TestCodeClassBoundsGarbageStatus(t *testing.T) {
	cases := map[int]string{
		101: "1xx", 200: "2xx", 302: "3xx", 404: "4xx", 599: "5xx",
		0: "other", 42: "other", 600: "other", 7777: "other", -1: "other",
	}
	for status, want := range cases {
		if got := codeClass(status); got != want {
			t.Errorf("codeClass(%d) = %q, want %q", status, got, want)
		}
	}
	m := NewGatewayMetrics()
	m.MustRegister(prometheus.NewRegistry())
	m.observeStatus(7777)
	if got := testutil.ToFloat64(m.requests.WithLabelValues("other")); got != 1 {
		t.Errorf("requests{other} = %v, want 1", got)
	}
}
