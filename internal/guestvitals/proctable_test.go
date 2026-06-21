package guestvitals

import (
	"strings"
	"testing"
)

// A /proc/<pid>/stat line. Field 1 is pid, field 2 is comm in parens (which may
// itself contain spaces and parens), field 3 is state, field 14/15 are utime and
// stime in jiffies, field 24 is rss in pages. The parser must locate comm by the
// LAST ')' so a process named "(ba) d (proc)" does not desync the field offsets.
const fixturePidStat = `42 (my proc) S 1 42 42 0 -1 4194304 100 0 0 0 30 12 0 0 20 0 1 0 9999 123456789 256 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0`

func TestParsePidStat(t *testing.T) {
	p, err := ParsePidStat([]byte(fixturePidStat))
	if err != nil {
		t.Fatalf("ParsePidStat: %v", err)
	}
	if p.PID != 42 {
		t.Errorf("pid = %d, want 42", p.PID)
	}
	if p.Comm != "my proc" {
		t.Errorf("comm = %q, want %q", p.Comm, "my proc")
	}
	if p.State != "S" {
		t.Errorf("state = %q, want S", p.State)
	}
	if p.UTime != 30 || p.STime != 12 {
		t.Errorf("utime/stime = %d/%d, want 30/12", p.UTime, p.STime)
	}
	if p.RSSPages != 256 {
		t.Errorf("rss pages = %d, want 256", p.RSSPages)
	}
}

func TestParsePidStat_CommWithParens(t *testing.T) {
	// comm itself contains a ')'; the parser must split on the last ')'.
	line := `7 (weird)name) R 1 7 7 0 -1 0 0 0 0 0 5 5 0 0 20 0 1 0 1 100 64 0 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0`
	p, err := ParsePidStat([]byte(line))
	if err != nil {
		t.Fatalf("ParsePidStat: %v", err)
	}
	if p.Comm != "weird)name" {
		t.Errorf("comm = %q, want %q", p.Comm, "weird)name")
	}
	if p.State != "R" {
		t.Errorf("state = %q, want R", p.State)
	}
}

func TestParsePidStat_Malformed(t *testing.T) {
	for _, in := range []string{"", "notanumber (x) S", "42 noparens S 1"} {
		if _, err := ParsePidStat([]byte(in)); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

const fixtureMeminfo = `MemTotal:        2048000 kB
MemFree:          512000 kB
MemAvailable:    1024000 kB
Buffers:           10000 kB
Cached:           200000 kB
`

func TestParseMeminfo(t *testing.T) {
	m, err := ParseMeminfo(strings.NewReader(fixtureMeminfo))
	if err != nil {
		t.Fatalf("ParseMeminfo: %v", err)
	}
	if m.TotalKB != 2048000 {
		t.Errorf("total = %d, want 2048000", m.TotalKB)
	}
	if m.FreeKB != 512000 {
		t.Errorf("free = %d, want 512000", m.FreeKB)
	}
	if m.AvailableKB != 1024000 {
		t.Errorf("available = %d, want 1024000", m.AvailableKB)
	}
	if m.UsedKB() != 2048000-1024000 {
		t.Errorf("used = %d, want 1024000", m.UsedKB())
	}
}

func TestParseMeminfo_Missing(t *testing.T) {
	if _, err := ParseMeminfo(strings.NewReader("Buffers: 10 kB\n")); err == nil {
		t.Error("expected error when MemTotal is absent")
	}
}

// BalloonUsedKB derives the memory the host has reclaimed via the virtio-balloon
// from the guest-visible total and the balloon target. It is a pure arithmetic
// helper so it is testable without a real balloon device.
func TestBalloonUsedKB(t *testing.T) {
	// guest sees 2GB total; host inflated the balloon to reclaim 512MB.
	got := BalloonReclaimedKB(2048000, 1536000)
	if got != 512000 {
		t.Errorf("reclaimed = %d, want 512000", got)
	}
	// A target above total (never inflated) reclaims nothing, never negative.
	if BalloonReclaimedKB(2048000, 4096000) != 0 {
		t.Error("over-target balloon must reclaim 0")
	}
}
