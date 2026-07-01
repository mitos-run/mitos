//go:build !linux

package network

import (
	"context"
	"fmt"
	"net"
	"runtime"
)

// FlushSource is not supported on non-Linux platforms; conntrack and the
// tap/nftables stack are Linux-only.
func (notSupportedManager) FlushSource(_ context.Context, _ net.IP) error {
	return fmt.Errorf("conntrack flush is not supported on %s; requires Linux", runtime.GOOS)
}
