package atr

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// vendoredRuleset is the compiled, RE2-compatible, MCP-scan-path subset of the
// ATR corpus, generated from the pinned upstream SHA by internal/atr/gen. It is
// committed so the build and the minimal mitos-mcp image need neither the
// upstream checkout nor a YAML parser at runtime. Regenerate with:
//
//	go run ./internal/atr/gen -src <path-to-agent-threat-rules-checkout>
//
//go:embed data/ruleset.json
var vendoredRuleset []byte

// vendoredManifest records provenance (source repo, pinned SHA, license) and the
// vendor-step counts. It is embedded so callers can log what ruleset is loaded.
//
//go:embed data/manifest.json
var vendoredManifest []byte

// Ruleset is the on-disk shape of data/ruleset.json.
type Ruleset struct {
	Spec      string `json:"spec"`
	SourceSHA string `json:"source_sha"`
	Rules     []Rule `json:"rules"`
}

// Manifest is the on-disk shape of data/manifest.json.
type Manifest struct {
	Spec       string         `json:"spec"`
	SourceRepo string         `json:"source_repo"`
	SourceSHA  string         `json:"source_sha"`
	License    string         `json:"license"`
	Counts     ManifestCounts `json:"counts"`
}

// ManifestCounts are the vendor-step tallies. They are the honest coverage
// accounting for the RE2 subset: how many corpus rules exist, how many fall in
// the MCP scan path, how many vendored cleanly, and how many were dropped.
type ManifestCounts struct {
	CorpusRules            int `json:"corpus_rules"`
	MCPBucketRules         int `json:"mcp_bucket_rules"`
	VendoredRules          int `json:"vendored_rules"`
	SkippedRE2Incompatible int `json:"skipped_re2_incompatible"`
	CompiledConditions     int `json:"compiled_conditions"`
}

// Load parses the embedded vendored ruleset and returns a ready Evaluator.
func Load() (*Evaluator, error) {
	rs, err := loadRuleset()
	if err != nil {
		return nil, err
	}
	return NewEvaluator(rs.Rules)
}

func loadRuleset() (Ruleset, error) {
	var rs Ruleset
	if err := json.Unmarshal(vendoredRuleset, &rs); err != nil {
		return Ruleset{}, fmt.Errorf("parse vendored ruleset: %w", err)
	}
	return rs, nil
}

// LoadManifest parses the embedded vendor manifest.
func LoadManifest() (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(vendoredManifest, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse vendored manifest: %w", err)
	}
	return m, nil
}

// SampleForScan returns the head of s capped to maxBytes and reports whether it
// was truncated. A maxBytes of zero or less means no cap. The cap is applied on
// a UTF-8 rune boundary so a multi-byte rune is never split, which would create
// a spurious non-match. Head-only sampling is a documented limitation: a pattern
// that appears only past the cap is not seen (ATR-SPEC-v1 detection is stateless
// per event and this is report-only, not an enforcement control).
func SampleForScan(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	// Back off to the start of the rune that straddles the cap.
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune (i.e. not a
// 0b10xxxxxx continuation byte).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
