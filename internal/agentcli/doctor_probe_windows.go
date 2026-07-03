//go:build windows

package agentcli

import (
	"context"
	"fmt"
)

// dataDirFree has no statfs on windows. forkd never runs on windows, so the
// data-dir free-space preflight cannot apply; the error folds into the same
// WARN path as a missing data dir on a workstation, keeping doctor usable from
// a windows client against a remote cluster.
func (p *realProbe) dataDirFree(context.Context) (uint64, string, error) {
	return 0, p.dataDirPath, fmt.Errorf("data-dir free-space check is not supported on windows; run mitos doctor on the KVM node or from a linux or darwin client")
}
