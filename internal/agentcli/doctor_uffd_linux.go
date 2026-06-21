//go:build linux

package agentcli

import (
	"context"

	"golang.org/x/sys/unix"
)

// Userfaultfd attempts the userfaultfd(2) syscall to determine whether the kernel
// supports it (issue #167). A kernel built without CONFIG_USERFAULTFD returns
// ENOSYS; a kernel that supports it returns a usable fd, which we immediately
// close. EPERM (unprivileged_userfaultfd disabled and no CAP_SYS_PTRACE) still
// proves the syscall EXISTS, so it counts as supported: forkd runs privileged and
// will be able to use it. Only ENOSYS (or a missing syscall number) means absent.
func (p *realProbe) Userfaultfd(_ context.Context) (bool, error) {
	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, uintptr(unix.O_CLOEXEC), 0, 0)
	if errno == 0 {
		_ = unix.Close(int(fd))
		return true, nil
	}
	if errno == unix.ENOSYS {
		return false, nil
	}
	// Any other errno (e.g. EPERM) means the syscall exists but was refused in
	// this context; the capability is present in the kernel.
	return true, nil
}
