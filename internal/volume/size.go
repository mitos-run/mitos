package volume

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

// DefaultSizeMB is the backing-image size used when a volume spec leaves Size
// empty.
const DefaultSizeMB = 1024

// bytesPerMB is the divisor used to convert a parsed quantity (bytes) to
// whole megabytes.
const bytesPerMB = 1024 * 1024

// ParseSizeMB parses a Kubernetes resource quantity string such as "5Gi" or
// "512Mi" into whole megabytes. An empty string yields DefaultSizeMB. The
// result is rounded down to the nearest megabyte.
func ParseSizeMB(s string) (int, error) {
	if s == "" {
		return DefaultSizeMB, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, fmt.Errorf("parse volume size %q: %w", s, err)
	}
	bytes := q.Value()
	mb := bytes / bytesPerMB
	return int(mb), nil
}
