package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Budget is the creator-set capability budget carried by a sandbox (issue #25,
// docs/api/v2-spec.md section 3, design in docs/api/capability-budgets.md). It is
// the declarative source for the attenuated runtime token: a sandbox's token
// carries this budget, and a self-initiated fork's token is strictly narrower
// (budget minus spend, same-or-smaller scopes; see internal/captoken).
//
// These types are the API SHAPE for capability budgets. Per ADR 0007 the budget
// lands on the v2 Sandbox noun, which is not yet the served API; the four
// v1alpha1 kinds remain unchanged. To avoid CRD drift to the live SandboxClaim,
// SandboxFork, SandboxPool, and SandboxTemplate kinds, Budget and BudgetSpend are
// defined here WITHOUT object-root markers and are NOT yet embedded in any served
// spec or status. The runtime-wiring plan (controller materializing budget-gated
// forks, depth-aggregate accounting, status.budgetSpend) is the follow-up tracked
// in docs/api/capability-budgets.md and issues #24/#25.
//
// +kubebuilder:object:generate=true
type Budget struct {
	// MaxForks is the maximum number of self-initiated forks the sandbox may
	// create, counted depth-aggregate across its fork subtree. Zero means no
	// self-initiated forks are permitted.
	// +optional
	MaxForks int64 `json:"maxForks,omitempty"`

	// MaxCheckpoints is the maximum number of self-initiated checkpoints
	// (live-state to workspace-revision) the sandbox may take.
	// +optional
	MaxCheckpoints int64 `json:"maxCheckpoints,omitempty"`

	// MaxCpuSeconds is the maximum cumulative guest CPU-seconds the sandbox and
	// its fork subtree may consume.
	// +optional
	MaxCpuSeconds int64 `json:"maxCpuSeconds,omitempty"`

	// MaxLifetimeExtension is the maximum total lifetime a sandbox may add to its
	// lease via self-initiated ExtendLifetime calls.
	// +optional
	MaxLifetimeExtension *metav1.Duration `json:"maxLifetimeExtension,omitempty"`

	// MaxEgressBytes is the maximum total network egress the sandbox and its fork
	// subtree may emit.
	// +optional
	MaxEgressBytes *resource.Quantity `json:"maxEgressBytes,omitempty"`
}

// BudgetSpend is the amount of each budget dimension already consumed by a
// sandbox and its fork subtree. It is the status counterpart of Budget: the
// controller materializes self-initiated forks as real objects, accounts their
// spend depth-aggregate, and surfaces it here so an orchestrator and the audit
// ledger see the complete picture. Remaining budget delegated to a fork is
// Budget minus BudgetSpend (floored at zero); see internal/captoken.Budget.Remaining.
//
// +kubebuilder:object:generate=true
type BudgetSpend struct {
	// Forks is the number of self-initiated forks created so far (depth-aggregate).
	// +optional
	Forks int64 `json:"forks,omitempty"`

	// Checkpoints is the number of self-initiated checkpoints taken so far.
	// +optional
	Checkpoints int64 `json:"checkpoints,omitempty"`

	// CpuSeconds is the cumulative guest CPU-seconds consumed so far.
	// +optional
	CpuSeconds int64 `json:"cpuSeconds,omitempty"`

	// LifetimeExtension is the total lifetime added via ExtendLifetime so far.
	// +optional
	LifetimeExtension *metav1.Duration `json:"lifetimeExtension,omitempty"`

	// EgressBytes is the total network egress emitted so far.
	// +optional
	EgressBytes *resource.Quantity `json:"egressBytes,omitempty"`
}
