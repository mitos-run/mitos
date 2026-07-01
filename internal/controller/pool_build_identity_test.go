package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	v1 "mitos.run/mitos/api/v1"
)

// poolBuildIdentity is the content hash that decides whether a pool's template
// snapshot must be rebuilt. A change to any build-affecting field (image,
// init/buildSteps, workload command/env/ready probe, volumes, resources,
// encryption) must change the identity so the controller rebuilds the snapshot
// (issue #475). Two templates that differ only in a non-build field, or not at
// all, must share an identity so a steady-state reconcile does not churn.
func TestPoolBuildIdentity(t *testing.T) {
	base := func() *v1.PoolTemplateSpec {
		return &v1.PoolTemplateSpec{
			Image: "python:3.12-slim",
			Init:  []string{"pip install flask"},
			Workload: &v1.WorkloadSpec{
				Command: []string{"flask", "run"},
				Env:     []corev1.EnvVar{{Name: "PORT", Value: "8080"}},
				Ready:   &v1.HTTPReadyProbe{Port: 8080, Path: "/healthz"},
			},
		}
	}

	tests := []struct {
		name   string
		mutate func(t *v1.PoolTemplateSpec)
		differ bool
	}{
		{
			name:   "identical template",
			mutate: func(*v1.PoolTemplateSpec) {},
			differ: false,
		},
		{
			name:   "workload command changed",
			mutate: func(t *v1.PoolTemplateSpec) { t.Workload.Command = []string{"gunicorn", "app:app"} },
			differ: true,
		},
		{
			name:   "workload env changed",
			mutate: func(t *v1.PoolTemplateSpec) { t.Workload.Env[0].Value = "9090" },
			differ: true,
		},
		{
			name:   "workload ready probe changed",
			mutate: func(t *v1.PoolTemplateSpec) { t.Workload.Ready.Path = "/ready" },
			differ: true,
		},
		{
			name:   "workload added",
			mutate: func(t *v1.PoolTemplateSpec) { t.Workload = nil },
			differ: true,
		},
		{
			name:   "image changed",
			mutate: func(t *v1.PoolTemplateSpec) { t.Image = "python:3.13-slim" },
			differ: true,
		},
		{
			name:   "init changed",
			mutate: func(t *v1.PoolTemplateSpec) { t.Init = []string{"pip install django"} },
			differ: true,
		},
	}

	want := poolBuildIdentity(base())
	if want == "" {
		t.Fatal("poolBuildIdentity returned empty for a non-nil template")
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutated := base()
			tc.mutate(mutated)
			got := poolBuildIdentity(mutated)
			if tc.differ && got == want {
				t.Errorf("identity did not change after %s; both = %s", tc.name, got)
			}
			if !tc.differ && got != want {
				t.Errorf("identity changed for %s: got %s want %s", tc.name, got, want)
			}
		})
	}
}
