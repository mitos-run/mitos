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
		ParentPID:  1234,
		ParentFD:   7,
		Bytes:      256 << 20,
		FrozenPID:  5678,
		FrozenFD:   9,
		BitmapPath: "/run/vm/mitos-frozen.bm",
		PageSize:   4096,
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
	cases := []string{
		"",
		"1 2 3",             // too few fields
		"0 2 3 4 5 4096 /p", // non-positive parent pid
		"1 2 0 4 5 4096 /p", // zero bytes
		"1 2 3 4 5 0 /p",    // zero page size
		"1 2 3 4 5 4096 ",   // empty bitmap path
		"a b c d e f g",     // non-numeric
	}
	for _, c := range cases {
		if _, err := ParseChildMemfdImport(c); err == nil {
			t.Errorf("ParseChildMemfdImport(%q) = nil error, want rejection", c)
		}
	}
}
