package main

import (
	"slices"
	"strings"
	"testing"
)

// The forkd pod is non-privileged and receives its devices only from this
// plugin. /dev/vhost-vsock must stay in the default set: dropping it is exactly
// the regression that left the host with the device but the forkd pod without
// it, so every fork booted a VM whose guest-agent vsock transport never came up
// and the fork hung. This guards that default against a silent removal.
//
// /dev/userfaultfd must also stay: the live-cow write-protect fork creates its
// userfaultfd via the device path (open + ioctl) because the container seccomp
// profile denies the userfaultfd(2) syscall even with CAP_SYS_PTRACE (issue
// #832). Without the injected device the restored source cannot arm and every
// fork silently falls back to the 364ms Full snapshot.
func TestDefaultDevicePathsIncludeRequiredDevices(t *testing.T) {
	got := strings.Split(defaultDevicePaths, ",")
	for _, required := range []string{"/dev/kvm", "/dev/net/tun", "/dev/vhost-vsock", "/dev/userfaultfd"} {
		if !slices.Contains(got, required) {
			t.Errorf("defaultDevicePaths %q is missing required device %q", defaultDevicePaths, required)
		}
	}
}
