package fork

import "testing"

// These tests cover the platform-independent child-import contract (the env the
// child Firecracker reads and the export line the parent handler publishes). The
// Linux memory-attach that consumes it is proven on real KVM in
// wpfork_kvm_test.go (TestLiveCowChildBootsFromSharedMemfd).

func TestChildMemfdEnv(t *testing.T) {
	got := ChildMemfdEnv("/run/child.export")
	want := EnvChildMemfd + "=/run/child.export"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("ChildMemfdEnv = %v, want [%q]", got, want)
	}
	if ChildMemfdEnv("") != nil {
		t.Fatalf("ChildMemfdEnv(\"\") = %v, want nil (child falls back to disk restore)", ChildMemfdEnv(""))
	}
}

func TestChildMemfdImportRoundTrip(t *testing.T) {
	in := ChildMemfdImport{
		ParentPID: 1234,
		ParentFD:  7,
		ParentIno: 111,
		ParentDev: 42,
		Bytes:     256 << 20,
		FrozenPID: 5678,
		FrozenFD:  9,
		FrozenIno: 222,
		FrozenDev: 42,
		BitmapPID: 5678,
		BitmapFD:  10,
		BitmapIno: 333,
		BitmapDev: 42,
		PageSize:  4096,
	}
	out, err := ParseChildMemfdImport(in.ExportLine())
	if err != nil {
		t.Fatalf("ParseChildMemfdImport(%q): %v", in.ExportLine(), err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestParseChildMemfdImportRejectsBad(t *testing.T) {
	// A well-formed line, for reference (14 numeric fields):
	// parentPID parentFD parentIno parentDev bytes frozenPID frozenFD frozenIno
	// frozenDev bitmapPID bitmapFD bitmapIno bitmapDev pageSize
	cases := []string{
		"",
		"1 2 3", // too few fields
		"1 2 111 42 256 5 6 222 42 7 8 333 42 4096 99", // one field too many (trailing not consumed)
		"0 2 111 42 256 5 6 222 42 7 8 333 42 4096",    // non-positive parent pid
		"1 2 111 42 0 5 6 222 42 7 8 333 42 4096",      // zero bytes
		"1 2 111 42 256 5 6 222 42 7 8 333 42 0",       // zero page size
		"1 2 0 42 256 5 6 222 42 7 8 333 42 4096",      // zero parent inode identity
		"1 2 111 42 256 5 6 222 42 7 8 0 42 4096",      // zero bitmap inode identity
		"a b c d e f g h i j k l m n",                  // non-numeric
		"-1 2 111 42 256 5 6 222 42 7 8 333 42 4096",   // negative field rejected by the unsigned parse
	}
	for _, c := range cases {
		if _, err := ParseChildMemfdImport(c); err == nil {
			t.Errorf("ParseChildMemfdImport(%q) = nil error, want rejection", c)
		}
	}
}

// TestParseChildMemfdImportNoWhitespaceTruncation proves the all-numeric encoding
// cannot be silently truncated the way the previous fmt.Sscanf %s path could: an
// embedded space makes the field count wrong and the parse fails closed instead of
// accepting a partial value.
func TestParseChildMemfdImportNoWhitespaceTruncation(t *testing.T) {
	in := ChildMemfdImport{
		ParentPID: 1, ParentFD: 2, ParentIno: 111, ParentDev: 42, Bytes: 4096,
		FrozenPID: 5, FrozenFD: 6, FrozenIno: 222, FrozenDev: 42,
		BitmapPID: 5, BitmapFD: 7, BitmapIno: 333, BitmapDev: 42, PageSize: 4096,
	}
	line := in.ExportLine()
	// Any stray internal whitespace changes the field count and must be rejected,
	// never truncated to a shorter valid-looking import.
	if _, err := ParseChildMemfdImport(line + "  extra"); err == nil {
		t.Error("trailing token must be rejected, not ignored")
	}
	if _, err := ParseChildMemfdImport("  " + line + "  "); err != nil {
		t.Errorf("leading/trailing whitespace must be tolerated: %v", err)
	}
}
