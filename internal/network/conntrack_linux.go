//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
)

// FlushSource deletes all conntrack entries whose source IP matches guestIP by
// running `conntrack -D -s <guestIP>`. It is called on a live fork so
// in-flight proxied flows are RST'd and the child re-dials.
//
// conntrack exits with status 1 when zero entries matched
// ("0 flow entries have been deleted"). That outcome is not an error: the
// child may simply not have had any active connections yet. A genuine failure
// (binary missing, permission denied) produces a different exit code or a
// non-ExitError and is returned to the caller.
func (m *linuxManager) FlushSource(ctx context.Context, guestIP net.IP) error {
	err := m.run(ctx, []string{"conntrack", "-D", "-s", guestIP.String()}, "")
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		// No conntrack entries matched; not an error for the live-fork caller.
		return nil
	}
	return fmt.Errorf("conntrack flush for %s: %w", guestIP, err)
}
