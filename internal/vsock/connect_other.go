//go:build !linux

package vsock

import "fmt"

// ConnectDirect is not available on non-Linux platforms.
func ConnectDirect(cid uint32, port uint32) (*Client, error) {
	return nil, fmt.Errorf("AF_VSOCK not available on this platform (Linux only)")
}
