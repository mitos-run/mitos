package cpupin

import (
	"reflect"
	"testing"
)

// TestParseTopologyHyperthreaded parses a 4-core, 8-logical-CPU node from
// fixture sysfs content where each physical core lists its two siblings.
func TestParseTopologyHyperthreaded(t *testing.T) {
	// online cpus 0-7; core_id and siblings per the canonical Linux layout where
	// cpu N and cpu N+4 are siblings of one physical core.
	fs := fakeSysfs{
		online: "0-7",
		coreID: map[int]string{
			0: "0", 4: "0",
			1: "1", 5: "1",
			2: "2", 6: "2",
			3: "3", 7: "3",
		},
		pkgID: map[int]string{0: "0", 1: "0", 2: "0", 3: "0", 4: "0", 5: "0", 6: "0", 7: "0"},
		siblings: map[int]string{
			0: "0,4", 4: "0,4",
			1: "1,5", 5: "1,5",
			2: "2,6", 6: "2,6",
			3: "3,7", 7: "3,7",
		},
	}
	topo, err := parseTopology(fs)
	if err != nil {
		t.Fatal(err)
	}
	if len(topo.Cores) != 4 {
		t.Fatalf("got %d cores, want 4: %+v", len(topo.Cores), topo.Cores)
	}
	// Cores are sorted by ID; each owns both siblings, sorted.
	want := []PhysicalCore{
		{ID: 0, Logical: []int{0, 4}},
		{ID: 1, Logical: []int{1, 5}},
		{ID: 2, Logical: []int{2, 6}},
		{ID: 3, Logical: []int{3, 7}},
	}
	if !reflect.DeepEqual(topo.Cores, want) {
		t.Fatalf("topology mismatch:\n got %+v\nwant %+v", topo.Cores, want)
	}
}

// TestParseTopologyNoHyperthreading parses a 2-core node with HT disabled: each
// physical core owns a single logical CPU.
func TestParseTopologyNoHyperthreading(t *testing.T) {
	fs := fakeSysfs{
		online:   "0-1",
		coreID:   map[int]string{0: "0", 1: "1"},
		pkgID:    map[int]string{0: "0", 1: "0"},
		siblings: map[int]string{0: "0", 1: "1"},
	}
	topo, err := parseTopology(fs)
	if err != nil {
		t.Fatal(err)
	}
	want := []PhysicalCore{
		{ID: 0, Logical: []int{0}},
		{ID: 1, Logical: []int{1}},
	}
	if !reflect.DeepEqual(topo.Cores, want) {
		t.Fatalf("topology mismatch:\n got %+v\nwant %+v", topo.Cores, want)
	}
}

// TestParseOnlineList covers the cpu-list grammar used by the online file.
func TestParseOnlineList(t *testing.T) {
	cases := map[string][]int{
		"0-3":     {0, 1, 2, 3},
		"0":       {0},
		"0,2,4":   {0, 2, 4},
		"0-1,4-5": {0, 1, 4, 5},
		"2-2":     {2},
		" 0-2 \n": {0, 1, 2},
	}
	for in, want := range cases {
		got, err := parseCPUList(in)
		if err != nil {
			t.Fatalf("parseCPUList(%q): %v", in, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("parseCPUList(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseOnlineListInvalid(t *testing.T) {
	for _, in := range []string{"abc", "1-", "3-1", ""} {
		if _, err := parseCPUList(in); err == nil {
			t.Fatalf("parseCPUList(%q) expected error", in)
		}
	}
}

// fakeSysfs implements sysfsReader from in-memory fixtures so the parser is
// testable on darwin without a real /sys.
type fakeSysfs struct {
	online   string
	coreID   map[int]string
	pkgID    map[int]string
	siblings map[int]string
}

func (f fakeSysfs) Online() (string, error) { return f.online, nil }
func (f fakeSysfs) CoreID(cpu int) (string, error) {
	return f.coreID[cpu], nil
}
func (f fakeSysfs) PackageID(cpu int) (string, error) {
	return f.pkgID[cpu], nil
}
func (f fakeSysfs) ThreadSiblings(cpu int) (string, error) {
	return f.siblings[cpu], nil
}
