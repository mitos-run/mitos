package fork

import "testing"

// TestLiveForkOpts verifies the pure ForkOpts assembly helper so a future edit
// that drops LiveFork or the netOpts pass-through fails on the non-KVM path.
func TestLiveForkOpts(t *testing.T) {
	t.Run("with_netOpts", func(t *testing.T) {
		policy := &NetworkOpts{EgressPolicy: "allow", Inbound: "deny"}
		src := &Sandbox{netOpts: policy}
		got := liveForkOpts(src)
		if !got.LiveFork {
			t.Error("LiveFork must be true for a live fork")
		}
		if got.Network != policy {
			t.Errorf("Network must be the source's netOpts pointer: got %v want %v", got.Network, policy)
		}
	})

	t.Run("nil_netOpts", func(t *testing.T) {
		src := &Sandbox{netOpts: nil}
		got := liveForkOpts(src)
		if !got.LiveFork {
			t.Error("LiveFork must be true even when netOpts is nil")
		}
		if got.Network != nil {
			t.Errorf("Network must be nil when source has no netOpts: got %v", got.Network)
		}
	})
}
