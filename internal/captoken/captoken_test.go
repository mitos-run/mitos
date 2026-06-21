package captoken

import (
	"math"
	"testing"
)

// fullBudget is a permissive root budget used as a starting point for the
// attenuation tests.
func fullBudget() Budget {
	return Budget{
		MaxForks:             100,
		MaxCheckpoints:       100,
		MaxCpuSeconds:        100000,
		MaxLifetimeExtension: 100000,
		MaxEgressBytes:       1 << 40,
	}
}

func allScopes() []Scope {
	return []Scope{ScopeExec, ScopeFiles, ScopeFork, ScopeCheckpoint, ScopeExtend, ScopeNetwork}
}

// newRootForTest mints a signed root token over the test signer.
func newRootForTest(t *testing.T, s *Signer, b Budget, scopes []Scope) Token {
	t.Helper()
	tok, err := Mint(s, "sb-root", b, scopes)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return tok
}

func testSigner(t testing.TB) *Signer {
	t.Helper()
	s, err := NewSigner([]byte("test-hmac-key-32-bytes-long-aaaaa"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

// TestAttenuateNeverWidensBudget is the LOAD-BEARING property: for any parent
// remaining budget, spend, and requested budget, the child token's budget is
// element-wise <= the parent's remaining budget. Attenuation can only narrow.
func TestAttenuateNeverWidensBudget(t *testing.T) {
	s := testSigner(t)
	cases := []struct {
		name      string
		parentRem Budget
		requested Budget
	}{
		{
			name:      "request wider than parent on every field",
			parentRem: Budget{MaxForks: 5, MaxCheckpoints: 3, MaxCpuSeconds: 1000, MaxLifetimeExtension: 60, MaxEgressBytes: 1024},
			requested: Budget{MaxForks: 99, MaxCheckpoints: 99, MaxCpuSeconds: 999999, MaxLifetimeExtension: 99999, MaxEgressBytes: 1 << 30},
		},
		{
			name:      "request narrower than parent on every field",
			parentRem: Budget{MaxForks: 50, MaxCheckpoints: 50, MaxCpuSeconds: 5000, MaxLifetimeExtension: 5000, MaxEgressBytes: 1 << 20},
			requested: Budget{MaxForks: 1, MaxCheckpoints: 2, MaxCpuSeconds: 3, MaxLifetimeExtension: 4, MaxEgressBytes: 5},
		},
		{
			name:      "mixed: some fields wider, some narrower",
			parentRem: Budget{MaxForks: 10, MaxCheckpoints: 2, MaxCpuSeconds: 100, MaxLifetimeExtension: 7, MaxEgressBytes: 4096},
			requested: Budget{MaxForks: 100, MaxCheckpoints: 1, MaxCpuSeconds: 9999, MaxLifetimeExtension: 3, MaxEgressBytes: 8192},
		},
		{
			name:      "zero parent budget pins child to zero",
			parentRem: Budget{},
			requested: fullBudget(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := newRootForTest(t, s, tc.parentRem, allScopes())
			child, err := Attenuate(s, parent, tc.requested, allScopes())
			if err != nil {
				t.Fatalf("Attenuate: %v", err)
			}
			cb := child.Budget()
			if cb.MaxForks > tc.parentRem.MaxForks {
				t.Errorf("MaxForks widened: child %d > parent %d", cb.MaxForks, tc.parentRem.MaxForks)
			}
			if cb.MaxCheckpoints > tc.parentRem.MaxCheckpoints {
				t.Errorf("MaxCheckpoints widened: child %d > parent %d", cb.MaxCheckpoints, tc.parentRem.MaxCheckpoints)
			}
			if cb.MaxCpuSeconds > tc.parentRem.MaxCpuSeconds {
				t.Errorf("MaxCpuSeconds widened: child %d > parent %d", cb.MaxCpuSeconds, tc.parentRem.MaxCpuSeconds)
			}
			if cb.MaxLifetimeExtension > tc.parentRem.MaxLifetimeExtension {
				t.Errorf("MaxLifetimeExtension widened: child %d > parent %d", cb.MaxLifetimeExtension, tc.parentRem.MaxLifetimeExtension)
			}
			if cb.MaxEgressBytes > tc.parentRem.MaxEgressBytes {
				t.Errorf("MaxEgressBytes widened: child %d > parent %d", cb.MaxEgressBytes, tc.parentRem.MaxEgressBytes)
			}
		})
	}
}

// TestAttenuateNeverWidensScopes asserts a child can only ever hold a subset of
// the parent's scopes; a requested scope the parent lacks is dropped.
func TestAttenuateNeverWidensScopes(t *testing.T) {
	s := testSigner(t)
	parent := newRootForTest(t, s, fullBudget(), []Scope{ScopeExec, ScopeFiles})
	// Request a superset; the child must not gain ScopeFork or ScopeNetwork.
	child, err := Attenuate(s, parent, fullBudget(), allScopes())
	if err != nil {
		t.Fatalf("Attenuate: %v", err)
	}
	for _, sc := range child.Scopes() {
		if !parent.HasScope(sc) {
			t.Errorf("child gained scope %q not held by parent", sc)
		}
	}
	if child.HasScope(ScopeFork) {
		t.Error("child must not hold ScopeFork: parent did not have it")
	}
	if child.HasScope(ScopeNetwork) {
		t.Error("child must not hold ScopeNetwork: parent did not have it")
	}
}

// TestForgedWiderChildFailsVerification asserts that hand-crafting a child whose
// budget or scopes exceed the parent, and re-signing it, still fails Verify
// because the signature is bound to a chain the verifier walks: a child must be
// no wider than its parent at every link.
func TestForgedWiderChildFailsVerification(t *testing.T) {
	s := testSigner(t)
	root := newRootForTest(t, s, Budget{MaxForks: 2, MaxCheckpoints: 2, MaxCpuSeconds: 10, MaxLifetimeExtension: 10, MaxEgressBytes: 10}, []Scope{ScopeExec})
	child, err := Attenuate(s, root, Budget{MaxForks: 1, MaxCheckpoints: 1, MaxCpuSeconds: 5, MaxLifetimeExtension: 5, MaxEgressBytes: 5}, []Scope{ScopeExec})
	if err != nil {
		t.Fatalf("Attenuate: %v", err)
	}

	// Forge a wider budget field and a wider scope on the final link, then
	// re-sign with the same signer (an attacker who knows the HMAC key still
	// cannot widen, because Verify enforces the never-widen invariant between the
	// embedded parent and child claims).
	forged := Token{
		links: append([]claims(nil), child.links...),
		tags:  append([][]byte(nil), child.tags...),
	}
	last := len(forged.links) - 1
	forged.links[last].Budget.MaxForks = 99
	forged.links[last].Scopes = []Scope{ScopeExec, ScopeFork, ScopeNetwork}
	resigned, err := s.reseal(forged)
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	if err := Verify(s, resigned); err == nil {
		t.Fatal("Verify accepted a forged child that widened budget and scopes")
	}
}

// TestTamperedSignatureFailsVerification asserts that flipping the budget without
// resealing breaks the HMAC.
func TestTamperedSignatureFailsVerification(t *testing.T) {
	s := testSigner(t)
	root := newRootForTest(t, s, fullBudget(), allScopes())
	tampered := Token{
		links: append([]claims(nil), root.links...),
		tags:  append([][]byte(nil), root.tags...),
	}
	// Mutate the budget WITHOUT resealing: the recorded tag no longer matches.
	tampered.links[0].Budget.MaxForks = root.Budget().MaxForks + 1
	if err := Verify(s, tampered); err == nil {
		t.Fatal("Verify accepted a token whose budget was mutated after signing")
	}
}

// TestWrongKeyFailsVerification asserts a token signed by one key does not verify
// under another.
func TestWrongKeyFailsVerification(t *testing.T) {
	s := testSigner(t)
	other, err := NewSigner([]byte("a-totally-different-key-32-bytes!"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	root := newRootForTest(t, s, fullBudget(), allScopes())
	if err := Verify(other, root); err == nil {
		t.Fatal("Verify accepted a token signed by a different key")
	}
}

// TestDepthNAttenuationMonotonicallyNarrows builds a chain of N attenuations,
// each requesting MORE than it should get, and asserts every budget field and
// the scope set is monotonically non-increasing down the chain, and every link
// verifies.
func TestDepthNAttenuationMonotonicallyNarrows(t *testing.T) {
	s := testSigner(t)
	cur := newRootForTest(t, s, fullBudget(), allScopes())
	if err := Verify(s, cur); err != nil {
		t.Fatalf("root does not verify: %v", err)
	}
	prev := cur
	for depth := 1; depth <= 20; depth++ {
		// Always request the full (widest) budget and all scopes; attenuation
		// must clamp to the parent every time.
		next, err := Attenuate(s, prev, fullBudget(), allScopes())
		if err != nil {
			t.Fatalf("depth %d Attenuate: %v", depth, err)
		}
		if err := Verify(s, next); err != nil {
			t.Fatalf("depth %d does not verify: %v", depth, err)
		}
		assertNoWiden(t, depth, prev.Budget(), next.Budget())
		if len(next.Scopes()) > len(prev.Scopes()) {
			t.Errorf("depth %d: scope set grew from %d to %d", depth, len(prev.Scopes()), len(next.Scopes()))
		}
		for _, sc := range next.Scopes() {
			if !prev.HasScope(sc) {
				t.Errorf("depth %d: gained scope %q", depth, sc)
			}
		}
		prev = next
	}
}

func assertNoWiden(t *testing.T, depth int, parent, child Budget) {
	t.Helper()
	if child.MaxForks > parent.MaxForks {
		t.Errorf("depth %d MaxForks widened %d -> %d", depth, parent.MaxForks, child.MaxForks)
	}
	if child.MaxCheckpoints > parent.MaxCheckpoints {
		t.Errorf("depth %d MaxCheckpoints widened %d -> %d", depth, parent.MaxCheckpoints, child.MaxCheckpoints)
	}
	if child.MaxCpuSeconds > parent.MaxCpuSeconds {
		t.Errorf("depth %d MaxCpuSeconds widened %d -> %d", depth, parent.MaxCpuSeconds, child.MaxCpuSeconds)
	}
	if child.MaxLifetimeExtension > parent.MaxLifetimeExtension {
		t.Errorf("depth %d MaxLifetimeExtension widened %d -> %d", depth, parent.MaxLifetimeExtension, child.MaxLifetimeExtension)
	}
	if child.MaxEgressBytes > parent.MaxEgressBytes {
		t.Errorf("depth %d MaxEgressBytes widened %d -> %d", depth, parent.MaxEgressBytes, child.MaxEgressBytes)
	}
}

// TestSpendNarrowsRemaining asserts the budget-minus-spend semantics: a parent
// that has spent some of its budget hands a child a remaining budget reduced by
// that spend, and never below zero.
func TestSpendNarrowsRemaining(t *testing.T) {
	s := testSigner(t)
	parent := newRootForTest(t, s, Budget{MaxForks: 5, MaxCheckpoints: 4, MaxCpuSeconds: 1000, MaxLifetimeExtension: 60, MaxEgressBytes: 8192}, allScopes())
	spend := BudgetSpend{Forks: 2, Checkpoints: 1, CpuSeconds: 400, LifetimeExtension: 10, EgressBytes: 8000}
	remaining := parent.Budget().Remaining(spend)
	if remaining.MaxForks != 3 {
		t.Errorf("remaining forks = %d, want 3", remaining.MaxForks)
	}
	if remaining.MaxCheckpoints != 3 {
		t.Errorf("remaining checkpoints = %d, want 3", remaining.MaxCheckpoints)
	}
	if remaining.MaxCpuSeconds != 600 {
		t.Errorf("remaining cpu = %d, want 600", remaining.MaxCpuSeconds)
	}
	if remaining.MaxEgressBytes != 192 {
		t.Errorf("remaining egress = %d, want 192", remaining.MaxEgressBytes)
	}

	// Overspend never produces a negative remaining.
	over := BudgetSpend{Forks: 99, Checkpoints: 99, CpuSeconds: 99999, LifetimeExtension: 99999, EgressBytes: 1 << 40}
	rem2 := parent.Budget().Remaining(over)
	if rem2.MaxForks < 0 || rem2.MaxCheckpoints < 0 || rem2.MaxCpuSeconds < 0 || rem2.MaxLifetimeExtension < 0 || rem2.MaxEgressBytes < 0 {
		t.Errorf("remaining went negative on overspend: %+v", rem2)
	}
}

// TestSerializeParseRoundTrip asserts a token survives serialize/parse and still
// verifies, and that a parsed-then-forged token fails.
func TestSerializeParseRoundTrip(t *testing.T) {
	s := testSigner(t)
	root := newRootForTest(t, s, fullBudget(), allScopes())
	child, err := Attenuate(s, root, Budget{MaxForks: 2}, []Scope{ScopeExec, ScopeFork})
	if err != nil {
		t.Fatalf("Attenuate: %v", err)
	}
	wire, err := Serialize(child)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	got, err := Parse(wire)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Verify(s, got); err != nil {
		t.Fatalf("round-tripped token does not verify: %v", err)
	}
	if got.Budget().MaxForks != child.Budget().MaxForks {
		t.Errorf("round-trip changed MaxForks: %d vs %d", got.Budget().MaxForks, child.Budget().MaxForks)
	}
	if got.SandboxID() != "sb-root" {
		t.Errorf("round-trip changed sandbox id: %q", got.SandboxID())
	}
}

// FuzzAttenuateNeverWidens is the property/fuzz test: across random parent
// budgets, scope masks, and requested budgets, the child token is never wider
// than the parent on ANY dimension, and always verifies.
func FuzzAttenuateNeverWidens(f *testing.F) {
	s := testSigner(f)
	f.Add(int64(5), int64(3), int64(1000), int64(60), int64(1024), uint8(0b111111),
		int64(99), int64(99), int64(99), int64(99), int64(99), uint8(0b111111))
	f.Fuzz(func(t *testing.T,
		pf, pc, pcpu, plt, peg int64, pmask uint8,
		rf, rc, rcpu, rlt, reg int64, rmask uint8,
	) {
		parentBudget := Budget{
			MaxForks:             clampNonNeg(pf),
			MaxCheckpoints:       clampNonNeg(pc),
			MaxCpuSeconds:        clampNonNeg(pcpu),
			MaxLifetimeExtension: clampNonNeg(plt),
			MaxEgressBytes:       clampNonNeg(peg),
		}
		requested := Budget{
			MaxForks:             clampNonNeg(rf),
			MaxCheckpoints:       clampNonNeg(rc),
			MaxCpuSeconds:        clampNonNeg(rcpu),
			MaxLifetimeExtension: clampNonNeg(rlt),
			MaxEgressBytes:       clampNonNeg(reg),
		}
		parentScopes := scopesFromMask(pmask)
		requestedScopes := scopesFromMask(rmask)

		parent, err := Mint(s, "sb-fuzz", parentBudget, parentScopes)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		child, err := Attenuate(s, parent, requested, requestedScopes)
		if err != nil {
			t.Fatalf("Attenuate: %v", err)
		}
		if err := Verify(s, child); err != nil {
			t.Fatalf("child does not verify: %v", err)
		}
		assertNoWiden(t, 1, parent.Budget(), child.Budget())
		for _, sc := range child.Scopes() {
			if !parent.HasScope(sc) {
				t.Errorf("child gained scope %q parent lacked", sc)
			}
		}
	})
}

func clampNonNeg(v int64) int64 {
	if v < 0 {
		v = -v
	}
	if v == math.MinInt64 {
		return math.MaxInt64
	}
	return v
}

func scopesFromMask(mask uint8) []Scope {
	all := []Scope{ScopeExec, ScopeFiles, ScopeFork, ScopeCheckpoint, ScopeExtend, ScopeNetwork}
	var out []Scope
	for i, sc := range all {
		if mask&(1<<uint(i)) != 0 {
			out = append(out, sc)
		}
	}
	return out
}
