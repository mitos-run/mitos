// Package templatebuild holds the pure, host-side logic for the declarative
// template builder (issue #220): the content-addressed cache key chained over
// the base image and each build step, the skip decision that reuses an
// unchanged prefix, and the typed build error. None of this needs KVM: it is
// deterministic over the SandboxTemplate's declared build recipe, so it is
// unit-tested on darwin. The actual layer materialization and reuse on a real
// build is engine and KVM gated; this package decides WHICH steps a real build
// may skip, the engine performs the skip.
package templatebuild

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"mitos.run/mitos/api/v1alpha1"
)

// Cache reports whether a step output for a given cache key is already
// materialized. The real implementation is backed by the node CAS store; the
// pure tests use an in-memory set. Lookup is the only operation the skip
// decision needs.
type Cache interface {
	Has(key string) bool
}

// stepFingerprint is the canonical, stable serialization of a single build step
// used as cache-key input. It names every field that affects the step's output,
// so a change to any of them changes the fingerprint and therefore the key. The
// leading type tag keeps a run "x" distinct from an env step that happens to
// stringify to "x".
func stepFingerprint(s v1alpha1.BuildStep) string {
	switch s.Type {
	case v1alpha1.BuildStepRun:
		return "run\x00" + s.Run
	case v1alpha1.BuildStepEnv:
		return "env\x00" + s.EnvName + "\x00" + s.EnvValue
	case v1alpha1.BuildStepWorkdir:
		return "workdir\x00" + s.Workdir
	case v1alpha1.BuildStepCopy:
		// Copy contributes its declared source and destination. The content of
		// the source files is folded in by the engine at build time (it hashes
		// the staged bytes and combines them with this key); the pure key chains
		// over the declared paths so a path change always invalidates.
		return "copy\x00" + s.Source + "\x00" + s.Dest
	default:
		return string(s.Type) + "\x00"
	}
}

// CacheKeys returns one content-addressed cache key per step, chained: the key
// at index i is sha256(key[i-1] || fingerprint(step[i])), seeded by the base
// image. So key[i] depends on the base image and every step from 0 through i.
// Changing step N changes key[N] and, through the chain, every key after it,
// but leaves key[0..N-1] untouched. That is the property that lets a real build
// reuse the unchanged prefix and rebuild only from the first changed step.
func CacheKeys(baseImage string, steps []v1alpha1.BuildStep) []string {
	keys := make([]string, len(steps))
	// Seed the chain with the base image so two recipes with identical steps on
	// different base images never collide.
	prev := sha256.Sum256([]byte("base\x00" + baseImage))
	for i := range steps {
		h := sha256.New()
		h.Write(prev[:])
		h.Write([]byte{0})
		h.Write([]byte(stepFingerprint(steps[i])))
		sum := h.Sum(nil)
		keys[i] = hex.EncodeToString(sum)
		copy(prev[:], sum)
	}
	return keys
}

// StepPlan is the decision for one build step: its cache key and whether the
// build may skip it (Cached) because its output is already materialized AND
// every step before it was also cached. Once a step misses, Cached is false for
// it and for every later step, because their inputs (the rebuilt earlier step)
// changed.
type StepPlan struct {
	Index  int
	Key    string
	Step   v1alpha1.BuildStep
	Cached bool
}

// Plan computes the skip decision for an ordered build. A step is Cached only
// when its key is present in the cache AND no earlier step missed: the first
// miss forces every later step to rebuild, because a rebuilt earlier step
// invalidates the inputs of everything downstream even if a later key happens
// to be present (a stale or partial entry). This is the headline E2B-style
// fast-cached-build behavior, made deterministic and testable on the host.
func Plan(baseImage string, steps []v1alpha1.BuildStep, cache Cache) []StepPlan {
	keys := CacheKeys(baseImage, steps)
	plan := make([]StepPlan, len(steps))
	missed := false
	for i := range steps {
		cached := false
		if !missed && cache != nil && cache.Has(keys[i]) {
			cached = true
		}
		if !cached {
			missed = true
		}
		plan[i] = StepPlan{Index: i, Key: keys[i], Step: steps[i], Cached: cached}
	}
	return plan
}

// FirstRebuildIndex returns the index of the first step a build must rebuild
// (the first non-cached step), or len(plan) if every step is cached (a full
// cache hit, nothing to rebuild). It is the boundary between the reused prefix
// and the rebuilt tail.
func FirstRebuildIndex(plan []StepPlan) int {
	for i := range plan {
		if !plan[i].Cached {
			return i
		}
	}
	return len(plan)
}

// String renders a plan as a human-readable build summary, one line per step,
// marking each CACHED or BUILD. The CLI prints this so a caller can see the
// cache hit before the real build runs.
func Summary(plan []StepPlan) string {
	out := ""
	for _, p := range plan {
		mark := "BUILD "
		if p.Cached {
			mark = "CACHED"
		}
		out += fmt.Sprintf("[%s] step %d %s %s\n", mark, p.Index, p.Step.Type, stepLabel(p.Step))
	}
	return out
}

func stepLabel(s v1alpha1.BuildStep) string {
	switch s.Type {
	case v1alpha1.BuildStepRun:
		return s.Run
	case v1alpha1.BuildStepEnv:
		return s.EnvName + "=" + s.EnvValue
	case v1alpha1.BuildStepWorkdir:
		return s.Workdir
	case v1alpha1.BuildStepCopy:
		return s.Source + " -> " + s.Dest
	default:
		return ""
	}
}
