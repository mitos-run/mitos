package fork

import "testing"

// regions models a guest with two memory regions, as Firecracker would hand them
// to the UFFD handler: a base-page region at host 0x10000 mapping mem-file offset
// 0, and a second region at host 0x100000 mapping mem-file offset 0x40000. The
// host base addresses and file offsets differ on purpose so a test that confused
// the two would fail.
func regions() []uffdMapping {
	return []uffdMapping{
		{BaseHostVirtAddr: 0x10000, Size: 0x40000, Offset: 0x0, PageSizeKiB: 4},
		{BaseHostVirtAddr: 0x100000, Size: 0x40000, Offset: 0x40000, PageSizeKiB: 4},
	}
}

func TestFileOffsetForAddrBasePage(t *testing.T) {
	// A fault at 0x10000+0x1234 is in the first region; floored to the 4 KiB page
	// base 0x11000, its mem-file offset is region.Offset + (0x11000-0x10000)=0x1000.
	pageBase, off, ok := fileOffsetForAddr(regions(), 0x10000+0x1234, 4096)
	if !ok {
		t.Fatal("expected the address to resolve to a region")
	}
	if pageBase != 0x11000 {
		t.Errorf("pageBase = %#x, want 0x11000", pageBase)
	}
	if off != 0x1000 {
		t.Errorf("fileOffset = %#x, want 0x1000", off)
	}
}

func TestFileOffsetForAddrSecondRegion(t *testing.T) {
	// A fault in the second region must use that region's own base and offset,
	// not the first region's. Addr 0x100000 -> page base 0x100000 -> file offset
	// 0x40000 + (0x100000-0x100000) = 0x40000.
	pageBase, off, ok := fileOffsetForAddr(regions(), 0x100000+0x10, 4096)
	if !ok {
		t.Fatal("expected resolve in second region")
	}
	if pageBase != 0x100000 || off != 0x40000 {
		t.Errorf("pageBase=%#x off=%#x, want 0x100000 0x40000", pageBase, off)
	}
}

func TestFileOffsetForAddrHugePage(t *testing.T) {
	// A 2 MiB hugepage region: a fault anywhere in the 2 MiB page floors to the
	// 2 MiB-aligned base and its file offset is 2 MiB-aligned.
	hp := []uffdMapping{{BaseHostVirtAddr: 0x40000000, Size: 0x400000, Offset: 0x200000, PageSizeKiB: 2048}}
	const twoMiB = 2 << 20
	pageBase, off, ok := fileOffsetForAddr(hp, 0x40000000+0x1FFFFF, twoMiB)
	if !ok {
		t.Fatal("expected hugepage resolve")
	}
	if pageBase != 0x40000000 {
		t.Errorf("pageBase = %#x, want 0x40000000", pageBase)
	}
	if off != 0x200000 {
		t.Errorf("fileOffset = %#x, want 0x200000", off)
	}
}

func TestFileOffsetForAddrOutOfRange(t *testing.T) {
	if _, _, ok := fileOffsetForAddr(regions(), 0x9999999, 4096); ok {
		t.Error("address outside every region must not resolve")
	}
}

func TestFileOffsetForAddrZeroPageSize(t *testing.T) {
	if _, _, ok := fileOffsetForAddr(regions(), 0x10000, 0); ok {
		t.Error("zero page size must not resolve (would divide by zero)")
	}
}

func TestHostAddrForFileOffsetRoundTrips(t *testing.T) {
	// hostAddrForFileOffset is the inverse used by Preload: a hot-page mem-file
	// offset must map back to the host address whose page covers it.
	host, ok := hostAddrForFileOffset(regions(), 0x40000)
	if !ok {
		t.Fatal("expected file offset to resolve to a region")
	}
	if host != 0x100000 {
		t.Errorf("hostAddr = %#x, want 0x100000", host)
	}
}

func TestHostAddrForFileOffsetOutOfRange(t *testing.T) {
	if _, ok := hostAddrForFileOffset(regions(), 0x80000); ok {
		// 0x80000 is past region 1 (ends 0x40000) and before region 2 (starts 0x40000..0x80000)
		// region2 covers [0x40000,0x80000): 0x80000 is exactly the end, exclusive.
		t.Error("file offset at the exclusive end of all regions must not resolve")
	}
}
