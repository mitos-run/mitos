//go:build unix

package agentcli

import (
	"context"
	"syscall"
)

// dataDirFree is the unix implementation behind realProbe.DataDirFree; see the
// doc comment there. Bsize and Bavail widths differ across platforms, so both
// are converted explicitly.
func (p *realProbe) dataDirFree(context.Context) (uint64, string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p.dataDirPath, &st); err != nil {
		return 0, p.dataDirPath, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), p.dataDirPath, nil
}
