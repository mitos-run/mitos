package fork

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TestCreateTemplateRefusesAConcurrentBuildOfTheSameTemplate pins the fix for a
// production outage. A build that pulls and unpacks an image takes minutes, while the
// controller re-reconciles the pool every ~20 seconds and calls CreateTemplate again.
// Both calls then tried to create the same placeholder tap, the second failed with
// `ioctl(TUNSETIFF): Device or resource busy`, and the controller reported the pool
// BuildFailed. A pool whose template is not built does not serve its warm husk pods, so
// every create went Pending for as long as the flapping lasted.
//
// The second call must be told the build is IN PROGRESS, which is a retry-later signal,
// not a failure.
func TestCreateTemplateRefusesAConcurrentBuildOfTheSameTemplate(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var mu sync.Mutex
	first := true

	e := &Engine{
		// The guard is taken before this seam, so blocking here holds the FIRST build.
		// Only the first one blocks: a later call must be free to prove the guard is
		// what refuses it, not this fake.
		kernelStaged: func(string) error {
			mu.Lock()
			hold := first
			first = false
			mu.Unlock()
			if hold {
				close(entered)
				<-release
			}
			return errors.New("stop the build here; the guard is what this test asserts")
		},
	}

	done := make(chan error, 1)
	go func() { done <- e.CreateTemplate("python", "img", nil, nil, nil, nil, CreateTemplateOpts{}) }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first build never started")
	}

	err := e.CreateTemplate("python", "img", nil, nil, nil, nil, CreateTemplateOpts{})
	if !errors.Is(err, ErrTemplateBuildInProgress) {
		t.Fatalf("concurrent build of the same template returned %v, want ErrTemplateBuildInProgress", err)
	}

	// A DIFFERENT template must not be blocked by this one.
	other := make(chan error, 1)
	go func() { other <- e.CreateTemplate("node", "img", nil, nil, nil, nil, CreateTemplateOpts{}) }()
	select {
	case err := <-other:
		if errors.Is(err, ErrTemplateBuildInProgress) {
			t.Error("a different template was refused; the guard must be per template")
		}
	case <-time.After(5 * time.Second):
		t.Error("a different template blocked behind an unrelated build")
	}

	close(release)
	<-done

	// Once the first build returns, the template can be built again.
	if err := e.CreateTemplate("python", "img", nil, nil, nil, nil, CreateTemplateOpts{}); errors.Is(err, ErrTemplateBuildInProgress) {
		t.Error("the guard was not released when the build returned")
	}
}
