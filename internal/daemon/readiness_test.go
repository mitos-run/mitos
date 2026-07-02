package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"mitos.run/mitos/internal/fork"
)

// fakeCap is a minimal capacityProvider so the readiness handler can be tested
// without standing up a real fork engine.
type fakeCap struct{ kvm bool }

func (f fakeCap) GetCapacity() fork.Capacity { return fork.Capacity{KVMAvailable: f.kvm} }

func TestMissingCharDevices(t *testing.T) {
	// /dev/null is a character device on both linux and darwin.
	if got := missingCharDevices([]string{"/dev/null"}); len(got) != 0 {
		t.Errorf("/dev/null should be present as a char device, got missing=%v", got)
	}
	if got := missingCharDevices([]string{"/definitely/not/a/device"}); len(got) != 1 {
		t.Errorf("a nonexistent path should be reported missing, got %v", got)
	}
	// A regular file exists but is not a character device: still "missing".
	reg := t.TempDir() + "/regular"
	if err := os.WriteFile(reg, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := missingCharDevices([]string{reg}); len(got) != 1 {
		t.Errorf("a regular file is not a char device and must be reported missing, got %v", got)
	}
}

func TestReadyzMockEngineAlwaysReady(t *testing.T) {
	// The mock engine has no real devices; readiness must not gate on them.
	old := requiredKVMDevices
	requiredKVMDevices = []string{"/definitely/not/a/device"}
	defer func() { requiredKVMDevices = old }()

	rr := httptest.NewRecorder()
	readinessHandler(fakeCap{kvm: false})(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("mock engine should be ready, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReadyzKVMFailsClosedOnMissingDevice(t *testing.T) {
	old := requiredKVMDevices
	requiredKVMDevices = []string{"/definitely/not/a/device"}
	defer func() { requiredKVMDevices = old }()

	rr := httptest.NewRecorder()
	readinessHandler(fakeCap{kvm: true})(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("a real-KVM node missing a device must be NotReady (503), got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "devices_missing") {
		t.Errorf("body should name the failure code, got %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "/definitely/not/a/device") {
		t.Errorf("body should name the missing device, got %s", rr.Body.String())
	}
}

func TestReadyzKVMReadyWhenDevicesPresent(t *testing.T) {
	old := requiredKVMDevices
	requiredKVMDevices = []string{"/dev/null"} // present char device on the CI/dev host
	defer func() { requiredKVMDevices = old }()

	rr := httptest.NewRecorder()
	readinessHandler(fakeCap{kvm: true})(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("a real-KVM node with all devices present should be ready, got %d: %s", rr.Code, rr.Body.String())
	}
}
