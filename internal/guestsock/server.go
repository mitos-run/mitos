package guestsock

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
)

// MaxMessageBytes caps a single framed self-service message. The self-service
// payloads are tiny (identity + budget), so this is a generous safety bound
// against a malformed line, not a functional limit.
const MaxMessageBytes = 1 << 20

// EnvLookup reads a guest env var by name, returning the value and whether it
// was set. It is the seam the handler reads identity and budget through, so the
// pure handler is testable without the guest's package-level env map.
type EnvLookup func(key string) (string, bool)

// Forker materializes n self-initiated sibling forks (issue #25) and returns
// their names. The guest agent injects the real implementation; until #25 it
// injects a stub that reports the not-implemented escalation path. Kept as a
// seam so the socket and SDK are real and exercised today.
type Forker func(req ForkRequest) (ForkResponse, error)

// Handler answers self-service requests. It reads the sandbox's own identity
// and budget through Env and delegates a fork to Fork. Both are seams so the
// handler is unit-testable on any platform; the guest agent wires them to its
// configured env and (later) the controller-backed fork.
type Handler struct {
	Env  EnvLookup
	Fork Forker
}

// Handle answers one request. TypeInfo is always available and read-only;
// TypeFork is delegated to Fork (budget-gated, continuation). An unknown type
// fails with a one-line error carrying no secret.
func (h Handler) Handle(req Request) Response {
	switch req.Type {
	case TypeInfo:
		return Response{OK: true, Info: h.info()}
	case TypeFork:
		fr := ForkRequest{}
		if req.Fork != nil {
			fr = *req.Fork
		}
		if h.Fork == nil {
			return Response{OK: false, Error: "self-fork is not enabled on this sandbox; ask your orchestrator to create siblings, or wait for capability-budgeted forks (issue #25)"}
		}
		res, err := h.Fork(fr)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Fork: &res}
	default:
		return Response{OK: false, Error: fmt.Sprintf("unknown self-service request type: %q", req.Type)}
	}
}

// info assembles the read-own-identity payload from the delivered env. Names and
// numbers only, never a secret value. A missing env var yields an empty field.
func (h Handler) info() *InfoResponse {
	get := func(key string) string {
		if h.Env == nil {
			return ""
		}
		v, _ := h.Env(key)
		return v
	}
	getInt := func(key string) int {
		n, _ := strconv.Atoi(get(key))
		return n
	}
	return &InfoResponse{
		Identity: Identity{
			SandboxID: get("MITOS_SANDBOX_ID"),
			Claim:     get("MITOS_CLAIM_NAME"),
			Pool:      get("MITOS_POOL_NAME"),
			Workspace: get("MITOS_WORKSPACE_NAME"),
		},
		Budget: Budget{
			MaxForks:       getInt("MITOS_BUDGET_MAX_FORKS"),
			MaxCheckpoints: getInt("MITOS_BUDGET_MAX_CHECKPOINTS"),
			ForksUsed:      getInt("MITOS_BUDGET_FORKS_USED"),
		},
	}
}

// Serve accepts connections on lis and answers self-service requests until lis
// is closed. Each connection speaks newline-delimited JSON: one Request per
// line, one Response per line, so a client can pipeline calls. A malformed line
// yields an error Response and the connection continues. Serve is the cross
// platform core; the guest agent provides lis (a unix listener at MITOS_SOCKET).
func (h Handler) Serve(lis net.Listener) error {
	for {
		conn, err := lis.Accept()
		if err != nil {
			return err
		}
		go h.serveConn(conn)
	}
}

func (h Handler) serveConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), MaxMessageBytes)
	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			writeResponse(conn, Response{OK: false, Error: fmt.Sprintf("invalid request: %v", err)})
			continue
		}
		writeResponse(conn, h.Handle(req))
	}
}

func writeResponse(w io.Writer, resp Response) {
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	b = append(b, '\n')
	_, _ = w.Write(b)
}
