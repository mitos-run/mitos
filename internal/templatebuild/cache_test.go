package templatebuild

import (
	"testing"

	v1 "mitos.run/mitos/api/v1"
)

func runStep(cmd string) v1.BuildStep {
	return v1.BuildStep{Type: v1.BuildStepRun, Run: cmd}
}

// TestCacheKeysChainOverBaseImageAndSteps asserts each step's cache key folds in
// the base image, the step itself, and the previous step's key, so the key at
// position N depends on everything from the base image through step N.
func TestCacheKeysChainOverBaseImageAndSteps(t *testing.T) {
	steps := []v1.BuildStep{
		runStep("apt-get update"),
		runStep("pip install -r requirements.txt"),
		runStep("python -m compileall ."),
	}
	keys := CacheKeys("python:3.12", steps)
	if len(keys) != len(steps) {
		t.Fatalf("got %d keys, want %d", len(keys), len(steps))
	}
	seen := map[string]bool{}
	for i, k := range keys {
		if k == "" {
			t.Fatalf("key %d is empty", i)
		}
		if seen[k] {
			t.Fatalf("key %d duplicates an earlier key: keys must be unique along a distinct chain", i)
		}
		seen[k] = true
	}
}

// TestChangingBaseImageInvalidatesAllKeys asserts a different base image yields a
// completely different chain from the first step on.
func TestChangingBaseImageInvalidatesAllKeys(t *testing.T) {
	steps := []v1.BuildStep{runStep("echo a"), runStep("echo b")}
	a := CacheKeys("python:3.12", steps)
	b := CacheKeys("python:3.13", steps)
	for i := range a {
		if a[i] == b[i] {
			t.Errorf("key %d unchanged across base images %q vs %q", i, a[i], b[i])
		}
	}
}

// TestChangingStepNInvalidatesNToEndOnly is the headline cache property: changing
// the middle step invalidates that step and every step after it, but leaves the
// keys of the steps before it untouched, so an unchanged prefix is reused.
func TestChangingStepNInvalidatesNToEndOnly(t *testing.T) {
	const base = "node:24"
	before := []v1.BuildStep{
		runStep("npm ci"),
		runStep("npm run build"),
		runStep("npm prune --production"),
	}
	after := []v1.BuildStep{
		runStep("npm ci"),
		runStep("npm run build --verbose"), // step 1 changed
		runStep("npm prune --production"),
	}
	a := CacheKeys(base, before)
	b := CacheKeys(base, after)

	if a[0] != b[0] {
		t.Errorf("step 0 key changed but step 0 is unchanged: %q vs %q", a[0], b[0])
	}
	if a[1] == b[1] {
		t.Error("step 1 key unchanged but step 1 changed")
	}
	if a[2] == b[2] {
		t.Error("step 2 key unchanged but a prior step changed (chaining broken)")
	}
}

// TestPlanSkipsCachedPrefix asserts the skip decision: given the keys already
// present in the cache, Plan marks the unchanged prefix as cached (skip) and the
// first changed step plus everything after as rebuild.
func TestPlanSkipsCachedPrefix(t *testing.T) {
	const base = "python:3.12"
	steps := []v1.BuildStep{
		runStep("pip install flask"),
		runStep("pip install gunicorn"),
		runStep("pytest"),
	}
	keys := CacheKeys(base, steps)

	// The cache holds the first two steps' keys (a prior build got that far) but
	// not the third.
	cache := newMemCache(keys[0], keys[1])

	plan := Plan(base, steps, cache)
	if len(plan) != 3 {
		t.Fatalf("plan has %d entries, want 3", len(plan))
	}
	if !plan[0].Cached || !plan[1].Cached {
		t.Errorf("steps 0 and 1 should be cached, got %v %v", plan[0].Cached, plan[1].Cached)
	}
	if plan[2].Cached {
		t.Error("step 2 should rebuild (its key is not cached)")
	}
}

// TestPlanRebuildsTailAfterFirstMiss asserts that once a step misses the cache,
// every later step rebuilds even if its key happened to be cached, because its
// inputs (the rebuilt earlier step) changed.
func TestPlanRebuildsTailAfterFirstMiss(t *testing.T) {
	const base = "alpine"
	steps := []v1.BuildStep{runStep("a"), runStep("b"), runStep("c")}
	keys := CacheKeys(base, steps)

	// Cache holds step 0 and step 2 but NOT step 1. Step 1 misses, so step 2 must
	// rebuild regardless of its cached key.
	cache := newMemCache(keys[0], keys[2])

	plan := Plan(base, steps, cache)
	if !plan[0].Cached {
		t.Error("step 0 should be cached")
	}
	if plan[1].Cached {
		t.Error("step 1 should rebuild (miss)")
	}
	if plan[2].Cached {
		t.Error("step 2 should rebuild because a prior step rebuilt, even though its key is cached")
	}
}

// memCache is a simple in-memory Cache for the pure skip tests.
type memCache map[string]bool

func newMemCache(keys ...string) memCache {
	m := memCache{}
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func (m memCache) Has(key string) bool { return m[key] }
