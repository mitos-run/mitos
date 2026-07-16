package fork

import "testing"

// lazyChunkForAddr must align to the 2 MiB populate granularity WITHIN a region and
// clip at the region end: a chunk that straddled two regions would UFFDIO_COPY into
// the wrong region's host addresses, and one that ran past the region end would read
// past that region's slice of the mem file.
func TestLazyChunkForAddr(t *testing.T) {
	regions := []uffdMapping{
		{BaseHostVirtAddr: 0x1000_0000, Size: 3 * lazyChunkBytes, Offset: 0},
		// Second region is deliberately NOT chunk-aligned in size.
		{BaseHostVirtAddr: 0x9000_0000, Size: lazyChunkBytes + 4096, Offset: 3 * lazyChunkBytes},
	}
	cases := []struct {
		name                    string
		addr                    uint64
		dst, fileOffset, length uint64
		ok                      bool
	}{
		{"region start", 0x1000_0000, 0x1000_0000, 0, lazyChunkBytes, true},
		{"mid first chunk", 0x1000_0000 + 4096, 0x1000_0000, 0, lazyChunkBytes, true},
		{"second chunk", 0x1000_0000 + lazyChunkBytes + 8, 0x1000_0000 + lazyChunkBytes, lazyChunkBytes, lazyChunkBytes, true},
		{"second region carries its file offset", 0x9000_0000, 0x9000_0000, 3 * lazyChunkBytes, lazyChunkBytes, true},
		{"tail chunk clipped to region end", 0x9000_0000 + lazyChunkBytes, 0x9000_0000 + lazyChunkBytes, 4 * lazyChunkBytes, 4096, true},
		{"below every region", 0x0fff_ffff, 0, 0, 0, false},
		{"above every region", 0x9000_0000 + lazyChunkBytes + 4096, 0, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst, off, ln, ok := lazyChunkForAddr(regions, tc.addr)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if dst != tc.dst || off != tc.fileOffset || ln != tc.length {
				t.Errorf("got dst=%#x off=%#x len=%#x; want dst=%#x off=%#x len=%#x",
					dst, off, ln, tc.dst, tc.fileOffset, tc.length)
			}
		})
	}
}
