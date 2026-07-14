package controlplane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
)

// seedTokenSecret creates the controller-owned token Secret an adopt reads.
func seedTokenSecret(t *testing.T, c client.Client, ns, name, token string) {
	t.Helper()
	if err := c.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{"token": []byte(token)},
	}); err != nil {
		t.Fatalf("seed token secret: %v", err)
	}
}

// recordingWarmer records keepalive invocations per sandbox and can fail
// selected names, so tests pin WHEN the buffer warms and what an eviction
// looks like without a live guest.
type recordingWarmer struct {
	mu    sync.Mutex
	calls map[string]int
	fail  map[string]bool
}

func newRecordingWarmer() *recordingWarmer {
	return &recordingWarmer{calls: map[string]int{}, fail: map[string]bool{}}
}

func (w *recordingWarmer) warm(_ context.Context, e bufferedSandbox) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls[e.name]++
	if w.fail[e.name] {
		return errors.New("keepalive cell failed")
	}
	return nil
}

func (w *recordingWarmer) count(name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[name]
}

// TestCheckoutKeepaliveWarmsStaleEntries is the #903 finding-1 mitigation
// contract: a buffered entry whose last warm is older than the keepalive
// interval gets ONE inert run_code per reconcile pass, a fresh entry gets
// none, and a warmed entry is not re-warmed until the interval elapses again.
// The measured basis (2026-07-14 prod discriminator on #903): an idle guest's
// first run_code decays to 231-350 ms while a 60 s run_code keepalive holds it
// at 76-110 ms; a cheap exec touch does NOT help, so the keepalive must be a
// run_code cell.
func TestCheckoutKeepaliveWarmsStaleEntries(t *testing.T) {
	cp := checkoutCP(t)
	b := cp.checkout
	w := newRecordingWarmer()
	b.warm = w.warm

	stale := seedBuffered(t, cp.c, "sb-stale")
	stale.lastWarm = time.Now().Add(-2 * checkoutKeepAliveInterval)
	b.add(stale)
	fresh := seedBuffered(t, cp.c, "sb-fresh")
	fresh.lastWarm = time.Now()
	b.add(fresh)

	b.reconcilePool(context.Background(), "python")
	if got := w.count("sb-stale"); got != 1 {
		t.Fatalf("stale entry warmed %d times, want exactly 1", got)
	}
	if got := w.count("sb-fresh"); got != 0 {
		t.Fatalf("fresh entry warmed %d times, want 0", got)
	}

	// A second immediate pass must not re-warm: the successful keepalive
	// refreshed lastWarm.
	b.reconcilePool(context.Background(), "python")
	if got := w.count("sb-stale"); got != 1 {
		t.Fatalf("stale entry re-warmed within the interval (calls=%d); lastWarm was not refreshed", got)
	}
}

// TestCheckoutKeepaliveEvictsOnFailure: a buffered sandbox that cannot run the
// inert cell must NEVER be handed to a tenant. The failed entry leaves the
// cache and its CR is deleted (the refill loop replaces it); this also
// de-amplifies the finding-2 stalls, because a wedged buffered sandbox is
// detected at keepalive time instead of at a customer's first exec.
func TestCheckoutKeepaliveEvictsOnFailure(t *testing.T) {
	cp := checkoutCP(t)
	b := cp.checkout
	w := newRecordingWarmer()
	w.fail["sb-wedged"] = true
	b.warm = w.warm

	wedged := seedBuffered(t, cp.c, "sb-wedged")
	wedged.lastWarm = time.Now().Add(-2 * checkoutKeepAliveInterval)
	b.add(wedged)

	b.reconcilePool(context.Background(), "python")

	if _, ok := b.pop("python"); ok {
		t.Fatal("a buffered sandbox that failed its keepalive stayed claimable; it must be evicted")
	}
	var cur v1.Sandbox
	err := cp.c.Get(context.Background(), client.ObjectKey{Namespace: "mitos", Name: "sb-wedged"}, &cur)
	if err == nil {
		t.Fatal("the wedged buffered CR still exists; eviction must delete it so the refill loop replaces it")
	}
}

// TestCheckoutAdoptedEntriesWarmOnFirstPass: an adopted entry (gateway restart
// or the other replica's refill) has unknown warmth, so it must be warmed on
// the pass that adopts it rather than trusted for a full interval.
func TestCheckoutAdoptedEntriesWarmOnFirstPass(t *testing.T) {
	cp := checkoutCP(t)
	b := cp.checkout
	w := newRecordingWarmer()
	b.warm = w.warm

	// Seed the CR plus its token secret, but do NOT pre-add a cache entry:
	// reconcilePool must adopt it, then warm it in the same pass.
	e := seedBuffered(t, cp.c, "sb-adopted")
	seedTokenSecret(t, cp.c, "mitos", "sb-adopted"+tokenSecretSuffix, e.token)

	b.reconcilePool(context.Background(), "python")
	if got := w.count("sb-adopted"); got != 1 {
		t.Fatalf("adopted entry warmed %d times on the adopting pass, want 1 (unknown warmth must not be trusted)", got)
	}
}

// noopWarm satisfies the warm seam for tests that exercise the buffer's other
// behavior and must not dial their fake endpoints.
func noopWarm(context.Context, bufferedSandbox) error { return nil }

// TestCheckoutKeepaliveEvictionNeverDeletesAClaimedSandbox pins the race the
// review found: a keepalive is in flight (bounded at 15 s) while a tenant
// checkout pops and claims the same entry. A failed warm that then evicts by
// bare name would DELETE the tenant's live sandbox. The eviction must instead
// notice the claim (the entry left the cache, and the live CR no longer
// carries the buffered label) and leave the CR alone.
func TestCheckoutKeepaliveEvictionNeverDeletesAClaimedSandbox(t *testing.T) {
	cp := checkoutCP(t)
	b := cp.checkout

	e := seedBuffered(t, cp.c, "sb-raced")
	e.lastWarm = time.Now().Add(-2 * checkoutKeepAliveInterval)
	b.add(e)

	// The warm seam simulates the race deterministically: while the keepalive
	// is "in flight", the tenant checkout wins the entry (pop + the org stamp,
	// exactly what claim does), then the warm comes back failed.
	b.warm = func(ctx context.Context, we bufferedSandbox) error {
		popped, ok := b.pop("python")
		if !ok || popped.name != "sb-raced" {
			t.Fatalf("test setup: expected to pop sb-raced, got %v ok=%v", popped.name, ok)
		}
		if !b.stampOrg(ctx, "mitos", orgA, popped) {
			t.Fatalf("test setup: org stamp failed")
		}
		return errors.New("keepalive lost the race and failed")
	}

	b.reconcilePool(context.Background(), "python")

	var cur v1.Sandbox
	if err := cp.c.Get(context.Background(), client.ObjectKey{Namespace: "mitos", Name: "sb-raced"}, &cur); err != nil {
		t.Fatalf("the claimed sandbox was DELETED by the losing keepalive: %v", err)
	}
	if _, buffered := cur.Labels[BufferedLabelKey]; buffered {
		t.Fatalf("test setup: the CR should be claimed (no buffered label), labels=%v", cur.Labels)
	}
}
