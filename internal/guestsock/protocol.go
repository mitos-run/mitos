// Package guestsock is the in-guest self-service protocol (issue #22, API v2
// section 2.2): the small request/response shape the guest agent serves on a
// unix socket inside the VM (MITOS_SOCKET, default /run/mitos.sock) so the
// in-VM workload can self-service without any network egress and without an
// external orchestrator round-trip.
//
// It is kept separate from the linux-only guest agent, like guestenv, so the
// protocol and the pure request handler are unit-testable on any platform; the
// guest agent owns only the unix listener wiring.
//
// The wire framing matches the vsock convention the agent already speaks:
// newline-delimited JSON, one Request per line, one Response per line. The
// socket is reachable only from inside the VM; it carries the sandbox's OWN
// identity and budget, never a secret VALUE.
package guestsock

// DefaultSocketPath is where the guest agent serves the self-service socket
// when MITOS_SOCKET is unset. It lives on the tmpfs the agent mounts at /run.
const DefaultSocketPath = "/run/mitos.sock"

// SocketEnvVar is the env var that advertises the socket path to the in-VM
// workload (and the mitos.guest SDK). The host sets it at claim time.
const SocketEnvVar = "MITOS_SOCKET"

// RequestType discriminates the self-service operations.
type RequestType string

const (
	// TypeInfo reads the sandbox's own identity and budget (read-only). It is
	// the always-available call: it never mutates state and needs no budget.
	TypeInfo RequestType = "info"
	// TypeFork requests a self-initiated fork within budget. The budget
	// enforcement and the actual fork are continuation work (issue #25); the
	// guest agent answers it today with a not-implemented response so the SDK
	// shape and the socket are real and exercised.
	TypeFork RequestType = "fork"
)

// Request is one self-service call. Exactly one of the per-type payloads is set
// for the request Type.
type Request struct {
	Type RequestType  `json:"type"`
	Fork *ForkRequest `json:"fork,omitempty"`
}

// ForkRequest asks for n self-initiated sibling forks. label is an optional
// human tag echoed into lineage. Budget-gated (issue #25, continuation).
type ForkRequest struct {
	N     int    `json:"n"`
	Label string `json:"label,omitempty"`
}

// Response is one self-service reply. OK reports success; on failure Error
// carries a one-line cause (never a secret value). Exactly one typed payload is
// set on success.
type Response struct {
	OK    bool          `json:"ok"`
	Error string        `json:"error,omitempty"`
	Info  *InfoResponse `json:"info,omitempty"`
	Fork  *ForkResponse `json:"fork,omitempty"`
}

// Identity is the sandbox's own identity: the names it was created under. Names
// only, never a secret value. Empty fields mean the host did not deliver them
// (e.g. an ephemeral standalone sandbox).
type Identity struct {
	SandboxID string `json:"sandboxId,omitempty"`
	Claim     string `json:"claim,omitempty"`
	Pool      string `json:"pool,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

// Budget is the capability budget the sandbox carries (issue #25). The guest
// reads the caps from its delivered env; spend accounting is continuation work,
// so Spend is reported as the guest currently observes it (zero until #25 wires
// the ledger through). Numbers, never secrets.
type Budget struct {
	MaxForks       int `json:"maxForks,omitempty"`
	MaxCheckpoints int `json:"maxCheckpoints,omitempty"`
	ForksUsed      int `json:"forksUsed"`
}

// InfoResponse is the read-own-identity payload: who am I, and what is my
// budget. It is assembled from the env the host delivered at claim time.
type InfoResponse struct {
	Identity Identity `json:"identity"`
	Budget   Budget   `json:"budget"`
}

// ForkResponse is the self-fork result: the names of the sibling sandboxes the
// controller materialized. Empty until issue #25 wires budget-gated forks.
type ForkResponse struct {
	Children []string `json:"children,omitempty"`
}
