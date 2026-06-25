package daemon

// connect_audit.go restores per-op AUDIT on the Connect sandbox.v1.Sandbox
// runtime path (PR A, issue #358). The legacy /v1 JSON handlers recorded an
// AuditEvent per op via api.auditor.Record; the Connect handler previously
// mounted only the auth interceptor, so exec/files/run_code over Connect were
// NOT audited. This interceptor records one AuditEvent per RPC AFTER it
// completes, for both unary and streaming RPCs.
//
// SECRET HYGIENE: the recorded event carries ONLY the op string, the sandbox
// id, and OK. It NEVER reads or records the command, argv, env, file path, file
// content, stdin/stdout, or any token. Detail and Bytes are deliberately left
// unset on this path. Auditing must never fail an RPC.

import (
	"context"
	"strings"

	"connectrpc.com/connect"

	"mitos.run/mitos/internal/sandboxrpc"
)

// connectAuditInterceptor records one AuditEvent per Connect runtime RPC after
// it completes. The auditor is read lazily through getAuditor at record time so
// SetAuditor controls it exactly as it controls the legacy /v1 path (NopAuditor
// = off).
type connectAuditInterceptor struct {
	getAuditor func() Auditor
}

// newConnectAuditInterceptor builds the audit interceptor. getAuditor returns
// the live Auditor (so SetAuditor still governs auditing); it must not be nil.
func newConnectAuditInterceptor(getAuditor func() Auditor) connect.Interceptor {
	return &connectAuditInterceptor{getAuditor: getAuditor}
}

// auditOpForProcedure maps a Connect procedure ("/sandbox.v1.Sandbox/<Method>")
// to a stable op string for the audit record. Only sandbox.v1.Sandbox runtime
// methods reach this handler. Unknown methods fall back to the bare method name
// lowercased so a new RPC is still audited under a predictable op.
func auditOpForProcedure(procedure string) string {
	method := procedure
	if i := strings.LastIndex(procedure, "/"); i >= 0 {
		method = procedure[i+1:]
	}
	switch method {
	case "Exec", "ExecStream":
		return "exec"
	case "RunCode", "RunCodeStream":
		return "run_code"
	case "ReadFile":
		return "read_file"
	case "WriteFile":
		return "write_file"
	case "List":
		return "list_dir"
	case "Stat":
		return "stat"
	case "Mkdir":
		return "mkdir"
	case "Remove":
		return "remove"
	case "Processes":
		return "processes"
	case "Vitals":
		return "vitals"
	case "Signal":
		return "signal"
	case "PortForward":
		return "port_forward"
	default:
		return strings.ToLower(method)
	}
}

// record emits one AuditEvent carrying ONLY the op, the authenticated sandbox
// id, and OK. The sandbox id comes from the auth interceptor (which runs
// outermost and injects it into ctx); when absent (tokenless standalone) it is
// left empty. A nil err means the RPC succeeded (OK = true). Auditing never
// touches the request payload, so no command, argv, env, path, content, or
// token can reach the record.
func (c *connectAuditInterceptor) record(ctx context.Context, procedure string, err error) {
	aud := c.getAuditor()
	if aud == nil {
		return
	}
	id, _ := sandboxrpc.SandboxIDFromContext(ctx)
	aud.Record(AuditEvent{
		SandboxID: id,
		Op:        auditOpForProcedure(procedure),
		OK:        err == nil,
	})
}

// WrapUnary records an AuditEvent after the unary handler returns, so OK
// reflects the handler's outcome.
func (c *connectAuditInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		resp, err := next(ctx, req)
		c.record(ctx, req.Spec().Procedure, err)
		return resp, err
	}
}

// WrapStreamingClient is a no-op: this interceptor runs on the handler side and
// never wraps client streams.
func (c *connectAuditInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler records ONE AuditEvent after the whole stream completes
// (not per frame), so OK reflects the entire stream's outcome. It covers the
// bidi Exec/RunCode, the server-streaming ExecStream/RunCodeStream/ReadFile/
// Vitals/Watch, and the client-streaming WriteFile.
func (c *connectAuditInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		err := next(ctx, conn)
		c.record(ctx, conn.Spec().Procedure, err)
		return err
	}
}
