package cpupin

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// sysfsReader abstracts the /sys/devices/system/cpu reads the topology parser
// needs, so the parser is pure and testable from fixtures (the Linux reader
// lives in topology_linux.go; off-Linux it is unavailable). All values are the
// raw file contents Linux exposes.
type sysfsReader interface {
	// Online returns the contents of cpu/online (a cpu list, e.g. "0-7").
	Online() (string, error)
	// CoreID returns cpu/cpuN/topology/core_id.
	CoreID(cpu int) (string, error)
	// PackageID returns cpu/cpuN/topology/physical_package_id.
	PackageID(cpu int) (string, error)
	// ThreadSiblings returns cpu/cpuN/topology/thread_siblings_list (a cpu list).
	ThreadSiblings(cpu int) (string, error)
}

// parseTopology builds a Topology from a sysfsReader. A physical core is keyed
// by (physical_package_id, core_id) so two packages that reuse core_id 0 are
// kept distinct. Each core owns the union of its online logical CPUs, taken from
// the thread_siblings_list of its members. Cores and their logical lists are
// returned sorted ascending, the layout the planner expects.
func parseTopology(fs sysfsReader) (Topology, error) {
	onlineStr, err := fs.Online()
	if err != nil {
		return Topology{}, fmt.Errorf("read cpu/online: %w", err)
	}
	online, err := parseCPUList(onlineStr)
	if err != nil {
		return Topology{}, fmt.Errorf("parse cpu/online %q: %w", onlineStr, err)
	}
	onlineSet := make(map[int]bool, len(online))
	for _, c := range online {
		onlineSet[c] = true
	}

	type coreKey struct{ pkg, core string }
	cores := map[coreKey]map[int]bool{}
	order := []coreKey{}

	for _, cpu := range online {
		pkg, err := fs.PackageID(cpu)
		if err != nil {
			return Topology{}, fmt.Errorf("read package_id for cpu%d: %w", cpu, err)
		}
		core, err := fs.CoreID(cpu)
		if err != nil {
			return Topology{}, fmt.Errorf("read core_id for cpu%d: %w", cpu, err)
		}
		key := coreKey{pkg: strings.TrimSpace(pkg), core: strings.TrimSpace(core)}
		set, ok := cores[key]
		if !ok {
			set = map[int]bool{}
			cores[key] = set
			order = append(order, key)
		}
		set[cpu] = true

		// Fold in the sibling list so a core captures all its online threads even
		// if some appear only as a sibling of another.
		sibStr, err := fs.ThreadSiblings(cpu)
		if err != nil {
			return Topology{}, fmt.Errorf("read thread_siblings_list for cpu%d: %w", cpu, err)
		}
		sibs, err := parseCPUList(sibStr)
		if err != nil {
			return Topology{}, fmt.Errorf("parse thread_siblings_list %q for cpu%d: %w", sibStr, cpu, err)
		}
		for _, s := range sibs {
			if onlineSet[s] {
				set[s] = true
			}
		}
	}

	// Assign each physical core a stable integer ID by ascending lowest-logical
	// CPU, so IDs are dense and deterministic regardless of sysfs core_id reuse.
	type built struct {
		logical []int
	}
	cs := make([]built, 0, len(order))
	for _, key := range order {
		set := cores[key]
		ls := make([]int, 0, len(set))
		for c := range set {
			ls = append(ls, c)
		}
		sort.Ints(ls)
		cs = append(cs, built{logical: ls})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].logical[0] < cs[j].logical[0] })

	topo := Topology{Cores: make([]PhysicalCore, len(cs))}
	for i, c := range cs {
		topo.Cores[i] = PhysicalCore{ID: i, Logical: c.logical}
	}
	return topo, nil
}

// parseCPUList parses the Linux cpu-list grammar ("0-3,5,7-8") into a sorted
// ascending slice. It rejects empty input, non-numeric tokens, and inverted
// ranges (hi < lo).
func parseCPUList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty cpu list")
	}
	seen := map[int]bool{}
	out := []int{}
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			return nil, fmt.Errorf("empty token in cpu list %q", s)
		}
		if lo, hi, ok := strings.Cut(tok, "-"); ok {
			a, err := strconv.Atoi(strings.TrimSpace(lo))
			if err != nil {
				return nil, fmt.Errorf("bad range start %q: %w", lo, err)
			}
			b, err := strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return nil, fmt.Errorf("bad range end %q: %w", hi, err)
			}
			if b < a {
				return nil, fmt.Errorf("inverted range %q", tok)
			}
			for v := a; v <= b; v++ {
				if !seen[v] {
					seen[v] = true
					out = append(out, v)
				}
			}
			continue
		}
		v, err := strconv.Atoi(tok)
		if err != nil {
			return nil, fmt.Errorf("bad cpu %q: %w", tok, err)
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out, nil
}
