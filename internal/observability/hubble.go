// Package observability holds the platform-neutral, unit-testable logic behind
// the Layer 1 (Cilium Hubble flow logs) and Layer 2 (OpenCost cost attribution)
// integrations of issue #164. The LIVE integrations (a real Cilium/Hubble relay
// emitting flows, a real OpenCost cluster reporting allocation) are
// cluster-gated and documented in docs/observability.md; the resolution and
// reconcile logic here is pure Go exercised by unit tests so the per-sandbox
// attribution rules are verified without a cluster.
package observability

// These label keys are the per-sandbox identity the husk pod (and any pod-backed
// sandbox) carries; they mirror the constants the controller sets on husk pods
// (internal/controller/huskpod.go). They are duplicated here rather than
// imported to keep this package free of the controller's k8s dependencies, since
// it parses external Hubble/OpenCost records, not k8s objects.
const (
	claimLabelKey = "mitos.run/claim"
	poolLabelKey  = "mitos.run/pool"
)

// SandboxIdentity is the mitos claim a network flow or cost record belongs to:
// the namespace plus the claim and pool names. Pool may be empty for a poolless
// direct sandbox; Claim and Namespace are always set when ok is true.
type SandboxIdentity struct {
	Namespace string
	Claim     string
	Pool      string
}

// HubbleEndpoint is the source or destination endpoint of a Hubble flow: its pod
// namespace, pod name, and the pod's labels. It is the subset of the Hubble
// flow.Endpoint message the resolver reads; a real consumer decodes the Hubble
// gRPC/JSON flow into this shape.
type HubbleEndpoint struct {
	Namespace string            `json:"namespace"`
	PodName   string            `json:"pod_name"`
	Labels    map[string]string `json:"labels"`
}

// HubbleFlow is the subset of a Cilium Hubble flow record the per-sandbox egress
// resolver needs: the verdict (FORWARDED, DROPPED, ...), the source and
// destination endpoints, and the L4 destination port. A real consumer maps the
// Hubble flow protobuf onto this.
type HubbleFlow struct {
	Verdict     string         `json:"verdict"`
	Source      HubbleEndpoint `json:"source"`
	Destination HubbleEndpoint `json:"destination"`
	L4Protocol  string         `json:"l4_protocol"`
	DestPort    uint32         `json:"destination_port"`
}

// ResolveSandbox maps a Hubble endpoint onto the mitos claim it belongs to via
// the mitos.run/claim label the pod carries. The bool is false when the endpoint
// is not a mitos sandbox (no claim label, or nil labels), so a non-sandbox flow
// is never attributed to a fabricated claim. The pool label is best-effort
// (empty for a poolless sandbox).
func ResolveSandbox(ep HubbleEndpoint) (SandboxIdentity, bool) {
	if ep.Labels == nil {
		return SandboxIdentity{}, false
	}
	claim := ep.Labels[claimLabelKey]
	if claim == "" {
		return SandboxIdentity{}, false
	}
	return SandboxIdentity{
		Namespace: ep.Namespace,
		Claim:     claim,
		Pool:      ep.Labels[poolLabelKey],
	}, true
}

// EgressEvent is a per-sandbox egress flow resolved from a Hubble record: which
// sandbox it came from, whether the egress was denied by policy, and the L4
// destination. It is what an operator queries to see a sandbox's egress and, in
// particular, its policy drops (the issue's denied-egress visibility goal).
type EgressEvent struct {
	Sandbox    SandboxIdentity
	Denied     bool
	L4Protocol string
	DestPort   uint32
}

// ClassifyEgress turns a Hubble flow into a per-sandbox EgressEvent. The bool is
// false when the flow's source is not a mitos sandbox (it is not a sandbox
// egress). A DROPPED (or DROPPED-class) verdict classifies as Denied, which is
// how a policy-blocked egress surfaces tied to its claim; FORWARDED is allowed.
func ClassifyEgress(f HubbleFlow) (EgressEvent, bool) {
	id, ok := ResolveSandbox(f.Source)
	if !ok {
		return EgressEvent{}, false
	}
	return EgressEvent{
		Sandbox:    id,
		Denied:     isDeniedVerdict(f.Verdict),
		L4Protocol: f.L4Protocol,
		DestPort:   f.DestPort,
	}, true
}

// isDeniedVerdict reports whether a Hubble verdict means the flow was blocked by
// policy. Hubble uses DROPPED for a policy drop; ERROR and an explicit DENIED
// are treated as denied too. Anything else (FORWARDED, AUDIT, REDIRECTED, ...)
// is not a denial.
func isDeniedVerdict(v string) bool {
	switch v {
	case "DROPPED", "DENIED", "ERROR":
		return true
	default:
		return false
	}
}
