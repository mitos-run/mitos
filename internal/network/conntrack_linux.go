//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// FlushSource deletes all conntrack entries whose source IP matches guestIP by
// running `conntrack -D -s <guestIP>`. It is called on a live fork so
// in-flight proxied flows are RST'd and the child re-dials.
//
// conntrack exits with status 1 and prints a summary line containing "flow
// entries have been deleted" (e.g. "conntrack v1.4.6: 0 flow entries have been
// deleted.") when zero entries matched. That outcome is not an error: the child
// may simply not have had any active connections yet. Any other nonzero exit
// (binary missing, permission denied, etc.) does NOT carry the deletion marker
// and is returned as an error so the caller can log and act on it.
func (m *linuxManager) FlushSource(ctx context.Context, guestIP net.IP) error {
	out, err := m.flush(ctx, []string{"conntrack", "-D", "-s", guestIP.String()})
	if err == nil {
		return nil
	}
	// Treat a nonzero exit as success only when the output contains the stable
	// "flow entries have been deleted" marker: conntrack ran successfully and
	// reported a deletion count (including 0). Any other nonzero exit (e.g.
	// "Operation not permitted") lacks this marker and is a genuine failure.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && strings.Contains(strings.ToLower(out), "flow entries have been deleted") {
		return nil
	}
	return fmt.Errorf("conntrack flush for %s: %w", guestIP, err)
}
