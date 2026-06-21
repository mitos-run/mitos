//go:build !linux

package agentcli

import (
	"context"
	"fmt"
)

// Userfaultfd cannot be verified off Linux (userfaultfd is a Linux facility). The
// check folds this into a WARN with a "cannot verify" remediation rather than
// claiming a pass or fail, since the meaningful run is on a Linux KVM node.
func (p *realProbe) Userfaultfd(_ context.Context) (bool, error) {
	return false, fmt.Errorf("userfaultfd is Linux-only; run mitos doctor on the KVM node to verify CONFIG_USERFAULTFD")
}
