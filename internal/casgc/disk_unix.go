//go:build linux || darwin

package casgc

import "syscall"

// DiskUsage reports the used and total bytes of the filesystem containing path,
// via statfs. Bsize differs in type across linux/darwin, so it is widened to
// uint64 before multiplying.
func DiskUsage(path string) (used, total int64, err error) {
	var st syscall.Statfs_t
	if serr := syscall.Statfs(path, &st); serr != nil {
		return 0, 0, serr
	}
	bsize := uint64(st.Bsize)
	total = int64(uint64(st.Blocks) * bsize)
	avail := int64(uint64(st.Bavail) * bsize)
	used = total - avail
	return used, total, nil
}
