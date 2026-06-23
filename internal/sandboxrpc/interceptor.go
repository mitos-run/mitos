package sandboxrpc

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"

	"connectrpc.com/connect"
)

// sandboxIDHeader is the request header the client sets to identify which
// sandbox a Connect RPC targets. It is used by BearerInterceptor to look up
// the registered token for that sandbox. Both unary and streaming RPCs send it
// as a request header alongside "Authorization: Bearer <token>" because
// interceptors receive headers before the first message body, making the header
// the only place an interceptor can read the sandbox identity without peeking
// the message stream. In the standalone/single-sandbox case the caller's lookup
// function may ignore this field entirely and return its single token.
const sandboxIDHeader = "X-Sandbox-Id"

// sandboxIDContextKey is the unexported context key type that carries the
// authenticated sandbox id from the bearerInterceptor to the Service resolver.
// Using an unexported type prevents collisions with keys from other packages.
type sandboxIDContextKey struct{}

// sandboxIDIntoContext returns a new context carrying sandboxID. It is called
// by bearerInterceptor after a successful auth check so the Service resolver
// can read the authenticated identity without re-reading the header.
func sandboxIDIntoContext(ctx context.Context, sandboxID string) context.Context {
	return context.WithValue(ctx, sandboxIDContextKey{}, sandboxID)
}

// SandboxIDFromContext returns the authenticated sandbox id that the
// bearerInterceptor stashed in ctx, and true when it is present. It is used by
// the sandbox resolver passed to WithSandboxResolver when mounting the Connect
// handler on :9091.
func SandboxIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(sandboxIDContextKey{}).(string)
	return v, ok && v != ""
}

// bearerInterceptor enforces per-sandbox bearer tokens on every Connect RPC,
// both unary and streaming. It is the Connect-transport mirror of the HTTP
// requireBearer middleware in internal/daemon/sandbox_api.go and reproduces
// the same security posture: constant-time comparison, fail-closed on missing
// or malformed Authorization header, fail-closed on an unknown sandbox, and a
// token mismatch always returns CodeUnauthenticated without echoing the value.
type bearerInterceptor struct {
	// lookup returns the expected token for a sandbox id. ok=false means the
	// sandbox is not registered and the request must be rejected (fail-closed).
	// The returned token value is never logged or placed in error messages.
	lookup func(sandboxID string) (token string, ok bool)
}

// BearerInterceptor returns a Connect interceptor that enforces per-sandbox
// bearer tokens on both unary and streaming RPCs. The lookup function is called
// with the sandbox id from the "X-Sandbox-Id" request header. If the header is
// absent the lookup receives an empty string. Fail-closed behavior:
//   - missing or malformed "Authorization: Bearer <token>" header: CodeUnauthenticated
//   - lookup returns ok=false (sandbox not registered): CodeUnauthenticated
//   - token mismatch (constant-time compare): CodeUnauthenticated
//
// Secret token values are never logged or placed in error messages. Only the
// string "unauthenticated" and a remediation hint reach the caller.
func BearerInterceptor(lookup func(sandboxID string) (token string, ok bool)) connect.Interceptor {
	return &bearerInterceptor{lookup: lookup}
}

// checkBearer validates the Authorization header in h against the registered
// token for the sandbox identified by the X-Sandbox-Id header. On success it
// returns the authenticated sandbox id and nil. On failure it returns an empty
// string and a CodeUnauthenticated error, never revealing the token value or the
// registered token in the returned error.
func (b *bearerInterceptor) checkBearer(h interface{ Get(string) string }) (string, error) {
	sandboxID := h.Get(sandboxIDHeader)
	token, ok := b.lookup(sandboxID)
	if !ok || len(token) == 0 {
		// Fail-closed: no token registered for this sandbox. The empty-token
		// guard is defense-in-depth: an empty registered token must never match
		// an empty presented token ("Bearer " with nothing after it), so this
		// public interceptor is self-enforcing even if a caller's lookup
		// returns ("", true).
		return "", connect.NewError(connect.CodeUnauthenticated,
			fmt.Errorf("unauthenticated: no token registered for sandbox; "+
				"provide a valid bearer token in the Authorization header"))
	}

	auth := h.Get("Authorization")
	presented, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		// Missing or malformed Authorization header.
		return "", connect.NewError(connect.CodeUnauthenticated,
			fmt.Errorf("unauthenticated: bearer token required; "+
				"set 'Authorization: Bearer <token>' on the request"))
	}

	if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
		// Token mismatch. Do not echo the presented value or the expected token.
		return "", connect.NewError(connect.CodeUnauthenticated,
			fmt.Errorf("unauthenticated: invalid token; "+
				"verify the bearer token matches the one issued for this sandbox"))
	}
	return sandboxID, nil
}

// WrapUnary wraps a unary RPC handler with the bearer-token check. The check
// runs against the request headers before the handler is called; on failure the
// underlying handler is never invoked. On success the authenticated sandbox id
// is injected into the context so the Service resolver can read it without
// re-reading the header.
func (b *bearerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		sandboxID, err := b.checkBearer(req.Header())
		if err != nil {
			return nil, err
		}
		return next(sandboxIDIntoContext(ctx, sandboxID), req)
	}
}

// WrapStreamingClient is a no-op for server-side interceptors: this interceptor
// is installed on the handler side and never wraps client streams.
func (b *bearerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler wraps a streaming handler with the bearer-token check.
// The check runs against the connection's request headers before any messages
// are received; on failure the handler is never invoked. This covers all
// streaming shapes (client, server, bidi) because Connect delivers all request
// headers at connection open, before any message body is read. On success the
// authenticated sandbox id is injected into the context.
func (b *bearerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		sandboxID, err := b.checkBearer(conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(sandboxIDIntoContext(ctx, sandboxID), conn)
	}
}

// allowTokenlessInterceptor is a pass-through interceptor used by the
// standalone sandbox-server (and by tests of other layers) when no bearer
// token is configured. It never performs any auth check.
type allowTokenlessInterceptor struct{}

// AllowTokenlessInterceptor returns a Connect interceptor that allows all
// requests through without any authentication check. It is used ONLY by the
// standalone sandbox-server, which has no token-minting control plane. forkd
// never uses it: a forkd sandbox without a token fails closed via
// BearerInterceptor. This mirrors the AllowTokenless option on SandboxAPI.
func AllowTokenlessInterceptor() connect.Interceptor {
	return &allowTokenlessInterceptor{}
}

func (a *allowTokenlessInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return next
}

func (a *allowTokenlessInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *allowTokenlessInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
