//go:build linux || darwin

package fork

import "syscall"

// statfsDiskBytes reports the free and total bytes of the filesystem containing
// path, via statfs. It is the production disk-headroom reader GetCapacity uses
// for the data dir. Bsize differs in type across linux/darwin, so it is widened
// to uint64 before multiplying. Free uses Bavail (blocks available to an
// unprivileged process), the honest headroom the scheduler should back off
// against, rather than Bfree (which includes root-reserved blocks).
func statfsDiskBytes(path string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if serr := syscall.Statfs(path, &st); serr != nil {
		return 0, 0, serr
	}
	bsize := uint64(st.Bsize)
	total = int64(uint64(st.Blocks) * bsize)
	free = int64(uint64(st.Bavail) * bsize)
	return free, total, nil
}
