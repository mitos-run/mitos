package fork

// Engine-level plumbing test for the warm_kernel flag: Engine.CreateTemplate
// must thread the flag to the Firecracker-backed template build (the
// runTemplateBuild seam), where it triggers the pre-snapshot kernel warmup.

import (
	"testing"

	"mitos.run/mitos/internal/firecracker"
)

func TestCreateTemplate_ThreadsWarmKernelToBuild(t *testing.T) {
	e := newTestEngine(t)

	got := make(map[string]bool)
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string, _ *firecracker.WorkloadSpec, warmKernel bool) error {
		got[id] = warmKernel
		writeRebuiltTemplate(t, e.dataDir, id)
		return nil
	}

	if err := e.CreateTemplate("warm", "/fake/rootfs.ext4", nil, nil, nil, nil, CreateTemplateOpts{WarmKernel: true}); err != nil {
		t.Fatalf("CreateTemplate(warm): %v", err)
	}
	if err := e.CreateTemplate("cold", "/fake/rootfs.ext4", nil, nil, nil, nil, CreateTemplateOpts{}); err != nil {
		t.Fatalf("CreateTemplate(cold): %v", err)
	}

	if v, ok := got["warm"]; !ok || !v {
		t.Errorf("expected warmKernel=true to reach the template build for %q, got %v (called=%v)", "warm", v, ok)
	}
	if v, ok := got["cold"]; !ok || v {
		t.Errorf("expected warmKernel=false to reach the template build for %q, got %v (called=%v)", "cold", v, ok)
	}
}
