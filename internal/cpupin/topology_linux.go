//go:build linux

package cpupin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sysfsBase is the Linux CPU topology root. A var so a test on a Linux host
// could point it at a fixture tree, but the parser itself is exercised on every
// platform through the sysfsReader interface and fakeSysfs.
var sysfsBase = "/sys/devices/system/cpu"

// linuxSysfs reads the real /sys/devices/system/cpu topology files.
type linuxSysfs struct{}

func (linuxSysfs) Online() (string, error) {
	return readSysfs(filepath.Join(sysfsBase, "online"))
}

func (linuxSysfs) CoreID(cpu int) (string, error) {
	return readSysfs(filepath.Join(sysfsBase, fmt.Sprintf("cpu%d", cpu), "topology", "core_id"))
}

func (linuxSysfs) PackageID(cpu int) (string, error) {
	return readSysfs(filepath.Join(sysfsBase, fmt.Sprintf("cpu%d", cpu), "topology", "physical_package_id"))
}

func (linuxSysfs) ThreadSiblings(cpu int) (string, error) {
	return readSysfs(filepath.Join(sysfsBase, fmt.Sprintf("cpu%d", cpu), "topology", "thread_siblings_list"))
}

func readSysfs(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // fixed sysfs paths under /sys/devices/system/cpu
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}
