//go:build !linux

package sniproxy

import "fmt"

// ListenAndServe is the non-linux stub. The SNI proxy listener is part of the
// host-side network datapath, which only runs on the forkd node (linux); on
// other platforms (the darwin dev/test build) it is never started, so this
// returns an error rather than binding. It exists so cmd/forkd compiles on every
// platform, mirroring internal/egressproxy's per-platform listener files.
func (p *Proxy) ListenAndServe(addr string) error {
	return fmt.Errorf("sni proxy listener is only supported on linux (requested addr %s)", addr)
}
