package guestvitals

import (
	"strings"
	"testing"
)

// fixtureProcStat is two /proc/stat aggregate lines a tick apart. The first
// field group is the cumulative jiffies since boot; the 8th value on the "cpu "
// line is steal. Between t0 and t1 the guest accrued 30 user + 10 system + 100
// idle + 20 steal jiffies, so the steal fraction of busy-or-stolen time over the
// interval is 20 / (30+10+20) = 0.333..., and the steal fraction of the whole
// interval is 20 / 160 = 0.125.
const fixtureProcStatT0 = `cpu  1000 0 500 5000 0 0 0 100 0 0
cpu0 1000 0 500 5000 0 0 0 100 0 0
intr 12345
ctxt 67890
`

const fixtureProcStatT1 = `cpu  1030 0 510 5100 0 0 0 120 0 0
cpu0 1030 0 510 5100 0 0 0 120 0 0
intr 22345
ctxt 77890
`

func TestParseProcStat_StealField(t *testing.T) {
	s, err := ParseProcStat(strings.NewReader(fixtureProcStatT0))
	if err != nil {
		t.Fatalf("ParseProcStat: %v", err)
	}
	if s.User != 1000 {
		t.Errorf("user = %d, want 1000", s.User)
	}
	if s.System != 500 {
		t.Errorf("system = %d, want 500", s.System)
	}
	if s.Idle != 5000 {
		t.Errorf("idle = %d, want 5000", s.Idle)
	}
	if s.Steal != 100 {
		t.Errorf("steal = %d, want 100", s.Steal)
	}
	if s.Total() != 1000+500+5000+100 {
		t.Errorf("total = %d, want 6600", s.Total())
	}
}

func TestParseProcStat_Malformed(t *testing.T) {
	cases := map[string]string{
		"no cpu line":  "intr 5\nctxt 6\n",
		"short fields": "cpu 1 2 3\n",
		"non-numeric":  "cpu a b c d e f g h\n",
		"empty":        "",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseProcStat(strings.NewReader(in)); err == nil {
				t.Errorf("expected error for %q", name)
			}
		})
	}
}

func TestStealDelta(t *testing.T) {
	t0, err := ParseProcStat(strings.NewReader(fixtureProcStatT0))
	if err != nil {
		t.Fatal(err)
	}
	t1, err := ParseProcStat(strings.NewReader(fixtureProcStatT1))
	if err != nil {
		t.Fatal(err)
	}
	d := StealDelta(t0, t1)
	if d.StealJiffies != 20 {
		t.Errorf("steal jiffies = %d, want 20", d.StealJiffies)
	}
	if d.TotalJiffies != 160 {
		t.Errorf("total jiffies = %d, want 160", d.TotalJiffies)
	}
	frac := d.StealFraction()
	if frac < 0.124 || frac > 0.126 {
		t.Errorf("steal fraction = %f, want ~0.125", frac)
	}
}

func TestStealDelta_NonMonotonic(t *testing.T) {
	// A counter reset (snapshot restore can rewind cumulative jiffies) must not
	// produce a negative or absurd fraction; the delta clamps to a zero-length
	// interval and reports 0 steal rather than a garbage value.
	t0, _ := ParseProcStat(strings.NewReader(fixtureProcStatT1))
	t1, _ := ParseProcStat(strings.NewReader(fixtureProcStatT0))
	d := StealDelta(t0, t1)
	if d.StealFraction() != 0 {
		t.Errorf("non-monotonic steal fraction = %f, want 0", d.StealFraction())
	}
}

func TestStealDelta_ZeroInterval(t *testing.T) {
	t0, _ := ParseProcStat(strings.NewReader(fixtureProcStatT0))
	d := StealDelta(t0, t0)
	if d.StealFraction() != 0 {
		t.Errorf("zero-interval steal fraction = %f, want 0", d.StealFraction())
	}
}
