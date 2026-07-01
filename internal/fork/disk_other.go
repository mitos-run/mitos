//go:build !linux && !darwin

package fork

import "fmt"

// statfsDiskBytes is the non-unix stub. statfs is unavailable here, so it
// returns an error and GetCapacity leaves the disk fields at zero (the
// controller reads that as an unknown, unlimited budget). The real engine never
// runs on such a host: NewEngine requires /dev/kvm, which only exists on Linux.
func statfsDiskBytes(string) (free, total int64, err error) {
	return 0, 0, fmt.Errorf("statfs not supported on this platform")
}
