package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "mitos.run/mitos/api/v1"
)

func TestForkdWorkloadMapping(t *testing.T) {
	if forkdWorkload(nil) != nil {
		t.Fatal("nil workload must map to nil")
	}
	if forkdWorkload(&v1.WorkloadSpec{}) != nil {
		t.Fatal("a workload with no command must map to nil")
	}
	w := forkdWorkload(&v1.WorkloadSpec{
		Command: []string{"node", "server.js"},
		Env:     []corev1.EnvVar{{Name: "PORT", Value: "8080"}},
		Ready:   &v1.HTTPReadyProbe{Port: 8080, Path: "/healthz", Expect: 200, TimeoutSeconds: 60},
	})
	if w == nil || len(w.GetCommand()) != 2 || w.GetCommand()[0] != "node" {
		t.Fatalf("command = %v", w.GetCommand())
	}
	if w.GetEnv()["PORT"] != "8080" {
		t.Fatalf("env = %v", w.GetEnv())
	}
	r := w.GetReady()
	if r == nil || r.GetPort() != 8080 || r.GetPath() != "/healthz" || r.GetExpect() != 200 || r.GetTimeoutSeconds() != 60 {
		t.Fatalf("ready = %+v", r)
	}
}

func TestForkdResourcesMapping(t *testing.T) {
	if forkdResources(v1.SandboxResources{}) != nil {
		t.Fatal("empty resources must map to nil (keep the build VM default)")
	}
	r := forkdResources(v1.SandboxResources{
		CPU:    resource.MustParse("2"),
		Memory: resource.MustParse("2Gi"),
	})
	if r == nil || r.GetVcpus() != 2 {
		t.Fatalf("vcpus = %v", r.GetVcpus())
	}
	if r.GetMemoryMb() != 2048 {
		t.Fatalf("memory_mb = %d, want 2048", r.GetMemoryMb())
	}
}
