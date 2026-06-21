package main

import "testing"

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig([]string{"--template", "default"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.mode != modeForkExec {
		t.Errorf("mode = %q, want %q", cfg.mode, modeForkExec)
	}
	if cfg.iterations != 50 {
		t.Errorf("iterations = %d, want 50", cfg.iterations)
	}
	if cfg.warmup != 5 {
		t.Errorf("warmup = %d, want 5", cfg.warmup)
	}
	if cfg.template != "default" {
		t.Errorf("template = %q, want default", cfg.template)
	}
}

func TestParseConfigInvalidMode(t *testing.T) {
	_, err := parseConfig([]string{"--mode", "bogus", "--template", "default"})
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestParseConfigMissingTemplate(t *testing.T) {
	_, err := parseConfig([]string{"--mode", "fork-exec"})
	if err == nil {
		t.Fatal("expected error for missing template")
	}
}

func TestParseConfigExecRT(t *testing.T) {
	cfg, err := parseConfig([]string{"--mode", "exec-rt", "--template", "t", "--iterations", "10", "--warmup", "2"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.mode != modeExecRT {
		t.Errorf("mode = %q, want %q", cfg.mode, modeExecRT)
	}
	if cfg.iterations != 10 {
		t.Errorf("iterations = %d, want 10", cfg.iterations)
	}
}

func TestParseConfigMetering(t *testing.T) {
	cfg, err := parseConfig([]string{"--mode", "metering", "--template", "t", "--forks", "4"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.mode != modeMetering {
		t.Errorf("mode = %q, want %q", cfg.mode, modeMetering)
	}
	if cfg.forks != 4 {
		t.Errorf("forks = %d, want 4", cfg.forks)
	}
}

func TestParseConfigMeteringDefaultForks(t *testing.T) {
	cfg, err := parseConfig([]string{"--mode", "metering", "--template", "t"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.forks != 4 {
		t.Errorf("default forks = %d, want 4", cfg.forks)
	}
}

func TestParseConfigMeteringRejectsZeroForks(t *testing.T) {
	if _, err := parseConfig([]string{"--mode", "metering", "--template", "t", "--forks", "0"}); err == nil {
		t.Fatal("expected error for --forks 0 in metering mode")
	}
}

func TestParseConfigFanOutDefaults(t *testing.T) {
	cfg, err := parseConfig([]string{"--mode", "fork-fanout", "--template", "t"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.mode != modeForkFanOut {
		t.Errorf("mode = %q, want %q", cfg.mode, modeForkFanOut)
	}
	want := []int{1, 4, 16, 64}
	if len(cfg.fanOutN) != len(want) {
		t.Fatalf("fanOutN = %v, want %v", cfg.fanOutN, want)
	}
	for i, v := range want {
		if cfg.fanOutN[i] != v {
			t.Errorf("fanOutN[%d] = %d, want %d", i, cfg.fanOutN[i], v)
		}
	}
}

func TestParseConfigFanOutCustomN(t *testing.T) {
	cfg, err := parseConfig([]string{"--mode", "fork-fanout", "--template", "t", "--fanout-n", "2,8"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.fanOutN) != 2 || cfg.fanOutN[0] != 2 || cfg.fanOutN[1] != 8 {
		t.Errorf("fanOutN = %v, want [2 8]", cfg.fanOutN)
	}
}

func TestParseConfigFanOutRejectsNonPositiveN(t *testing.T) {
	if _, err := parseConfig([]string{"--mode", "fork-fanout", "--template", "t", "--fanout-n", "1,0"}); err == nil {
		t.Fatal("expected error for non-positive N in --fanout-n")
	}
}

func TestParseConfigFanOutRejectsEmptyN(t *testing.T) {
	if _, err := parseConfig([]string{"--mode", "fork-fanout", "--template", "t", "--fanout-n", ""}); err == nil {
		t.Fatal("expected error for empty --fanout-n")
	}
}

func TestParseConfigFanOutRejectsGarbageN(t *testing.T) {
	if _, err := parseConfig([]string{"--mode", "fork-fanout", "--template", "t", "--fanout-n", "1,x"}); err == nil {
		t.Fatal("expected error for non-numeric --fanout-n")
	}
}

func TestParseConfigPrefetch(t *testing.T) {
	cfg, err := parseConfig([]string{"--mode", "prefetch", "--template", "t"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.mode != modePrefetch {
		t.Errorf("mode = %q, want %q", cfg.mode, modePrefetch)
	}
}

func TestParseConfigPinning(t *testing.T) {
	cfg, err := parseConfig([]string{"--mode", "pinning", "--template", "t"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.mode != modePinning {
		t.Errorf("mode = %q, want %q", cfg.mode, modePinning)
	}
}
