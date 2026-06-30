//go:build linux || darwin

package fork

import "testing"

// TestStatfsDiskBytes exercises the production statfs reader against a real
// directory. It runs on linux (CI) and darwin (dev): both have statfs. The
// numbers are not asserted exactly (they depend on the host filesystem), only
// that they are sane: a total is reported and free never exceeds total.
func TestStatfsDiskBytes(t *testing.T) {
	dir := t.TempDir()
	free, total, err := statfsDiskBytes(dir)
	if err != nil {
		t.Fatalf("statfsDiskBytes(%q): %v", dir, err)
	}
	if total <= 0 {
		t.Fatalf("total: got %d want > 0", total)
	}
	if free < 0 || free > total {
		t.Fatalf("free out of range: got %d (total %d)", free, total)
	}
}
