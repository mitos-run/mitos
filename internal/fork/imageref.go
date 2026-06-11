package fork

import (
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// isImageRef decides whether image refers to an OCI image reference (to be
// pulled and turned into a rootfs) or an existing rootfs file path (the
// legacy hand-built rootfs path that must keep working unchanged).
//
// The heuristic, in order:
//
//  1. An empty string is neither; returns false.
//  2. If the string exists as a file on disk it is a file path (false). This
//     is the back-compat anchor: the CI rootfs and every existing file-path
//     test pass an extant path, so they always take the copy path.
//  3. If the string looks like a filesystem path (absolute "/...", or relative
//     "./..." / "../...") it is treated as a file-path intent (false) even when
//     the file does not yet exist, so a typo'd rootfs path surfaces as a clear
//     "copy rootfs" error rather than a confusing registry pull. name.Parse-
//     Reference is permissive enough to accept "/abs/path/rootfs.ext4" as a
//     reference, which is why this guard is required.
//  4. Otherwise, if name.ParseReference accepts it, it is an image ref (true).
//  5. Anything else is false.
func isImageRef(image string) bool {
	if image == "" {
		return false
	}
	if _, err := os.Stat(image); err == nil {
		return false
	}
	if strings.HasPrefix(image, "/") ||
		strings.HasPrefix(image, "./") ||
		strings.HasPrefix(image, "../") {
		return false
	}
	if _, err := name.ParseReference(image); err == nil {
		return true
	}
	return false
}
