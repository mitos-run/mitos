package controlplane

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
)

// TestSandboxSummarySurfacesRestarts pins that the hosted status response tells a
// caller when its VM was destroyed and replaced (mitos-run/mitos#870).
//
// A husk pod loss under DrainPolicy Kill re-activates the sandbox from the pool
// template: the guest is a brand new VM and all in-guest state is gone. The Ready
// condition flips only transiently, so without `restarts` in the payload a client
// polling GET /v1/sandboxes/<id> sees an unbroken healthy sandbox and keeps issuing
// calls against state that no longer exists.
func TestSandboxSummarySurfacesRestarts(t *testing.T) {
	fresh := &v1.Sandbox{}
	fresh.Name = "sb-1"
	fresh.Status.Phase = v1.SandboxReady
	if got, ok := sandboxSummary(fresh)["restarts"]; !ok || got != int32(0) {
		t.Errorf("a never-restarted sandbox must report restarts 0; got %v (present=%v)", got, ok)
	}

	// The controller stamps Restarts and LastRestartTime together on every re-pend.
	restarted := &v1.Sandbox{}
	restarted.Name = "sb-2"
	restarted.Status.Phase = v1.SandboxReady
	restarted.Status.Restarts = 2
	when := metav1.NewTime(time.Unix(1783600000, 0).UTC())
	restarted.Status.LastRestartTime = &when
	sum := sandboxSummary(restarted)
	if got := sum["restarts"]; got != int32(2) {
		t.Errorf("restarts = %v, want 2: the caller must be able to detect its guest was replaced", got)
	}
	got, ok := sum["lastRestartTime"]
	if !ok {
		t.Fatal("a restarted sandbox must report lastRestartTime so a caller can tell WHEN its guest was replaced")
	}
	if got != when.UTC().Format(time.RFC3339) {
		t.Errorf("lastRestartTime = %v, want %v", got, when.UTC().Format(time.RFC3339))
	}
}
