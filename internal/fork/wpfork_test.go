package fork

import "testing"

// TestLiveCowParentEnv proves the parent-launch env assembly: the three
// FIRECRACKER_MITOS_* vars that flip the patched Firecracker onto the live-cow
// path are emitted only when both a WP socket and a memfd export path are
// present, and never otherwise (so a mis-wired flag can never half-arm the
// parent). Pure; runs on any host.
func TestLiveCowParentEnv(t *testing.T) {
	t.Run("both set emits three vars", func(t *testing.T) {
		got := LiveCowParentEnv("/run/wp.sock", "/run/memfd")
		want := []string{
			"FIRECRACKER_MITOS_SHARED_MEM=1",
			"FIRECRACKER_MITOS_SHARED_MEM_EXPORT=/run/memfd",
			"FIRECRACKER_MITOS_WP_UDS=/run/wp.sock",
		}
		if len(got) != len(want) {
			t.Fatalf("got %d env entries, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("env[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("missing socket emits nothing", func(t *testing.T) {
		if got := LiveCowParentEnv("", "/run/memfd"); got != nil {
			t.Errorf("no WP socket must emit no env; got %v", got)
		}
	})

	t.Run("missing export emits nothing", func(t *testing.T) {
		if got := LiveCowParentEnv("/run/wp.sock", ""); got != nil {
			t.Errorf("no memfd export must emit no env; got %v", got)
		}
	})
}

func TestParseMemfdExport(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		e, err := parseMemfdExport("4242 17 536870912\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if e.pid != 4242 || e.fd != 17 || e.bytes != 536870912 {
			t.Errorf("parsed %+v, want pid=4242 fd=17 bytes=536870912", e)
		}
	})

	for _, bad := range []string{"", "4242 17", "0 17 4096", "4242 17 0", "abc def ghi"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			if _, err := parseMemfdExport(bad); err == nil {
				t.Errorf("parseMemfdExport(%q) must error", bad)
			}
		})
	}
}

func TestFrozenBitmap(t *testing.T) {
	// 3 pages of 4 KiB = 12 KiB -> ceil(3/8) = 1 byte.
	if got := frozenBitmapBytes(3*4096, 4096); got != 1 {
		t.Errorf("frozenBitmapBytes(3 pages) = %d, want 1", got)
	}
	// 9 pages -> 2 bytes.
	if got := frozenBitmapBytes(9*4096, 4096); got != 2 {
		t.Errorf("frozenBitmapBytes(9 pages) = %d, want 2", got)
	}
	if got := frozenBitmapBytes(4096, 0); got != 0 {
		t.Errorf("zero page size must yield 0; got %d", got)
	}

	bm := make([]byte, 2)
	if testFrozenBit(bm, 5) {
		t.Error("bit 5 must start clear")
	}
	setFrozenBit(bm, 5)
	if !testFrozenBit(bm, 5) {
		t.Error("bit 5 must be set after setFrozenBit")
	}
	if testFrozenBit(bm, 6) {
		t.Error("setting bit 5 must not set bit 6")
	}
	// Out-of-range index is a no-op, not a panic.
	setFrozenBit(bm, 9999)
	if testFrozenBit(bm, 9999) {
		t.Error("out-of-range bit must read clear")
	}
}
