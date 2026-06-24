// Package vsock: gRPC-over-net.Conn dialer for the vsock transport.
//
// Transport choice: grpc-go over a raw net.Conn (NOT connect-go over h2c).
//
// grpc-go accepts an arbitrary net.Conn via grpc.WithContextDialer. The dialer
// callback receives (ctx, address) and must return a net.Conn; it is free to
// ignore the address and return an already-dialed vsock connection. The gRPC
// framing (HTTP/2 over the conn) is handled entirely by grpc-go; no h2c glue
// is required. This is the pattern used in the forkd test suite
// (internal/daemon/grpc_service_test.go) with a bufconn.Listener, and it works
// identically with net.Pipe() or a real vsock connection.
//
// Security note: transport credentials are set to insecure because the
// connection is already inside the VM trust boundary. The vsock channel is
// guest-to-host only over Firecracker's virtio-vsock; it is not reachable from
// the host network, tenant code in other sandboxes, or the internet. mTLS over
// vsock is a later hardening slice (docs/threat-model.md).

package vsock

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// DialGRPCOverConn builds a *grpc.ClientConn that sends all RPC traffic over
// connections returned by dial. The target address is always "passthrough:///vsock";
// the dialer ignores it and returns the caller-supplied net.Conn, which is
// typically a vsock connection to the guest agent.
//
// The returned ClientConn is INSECURE (no TLS). This is correct because vsock
// runs inside the Firecracker VM trust boundary and is not reachable from
// tenant code or the host network. See the security note at the top of this
// file.
//
// The dial function is called once per transport connection. For a vsock
// client that needs a fresh connection on every reconnect, dial should open
// a new vsock stream each time it is called. For tests that use net.Pipe the
// dial function is typically called exactly once.
func DialGRPCOverConn(dial func() (net.Conn, error), extraOpts ...grpc.DialOption) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	opts = append(opts, extraOpts...)
	return grpc.NewClient("passthrough:///vsock", opts...)
}
