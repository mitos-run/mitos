package atr

import (
	_ "embed"
	"encoding/json"
	"testing"
)

// conformanceData is the fixture generated from the pinned upstream SHA by
// internal/atr/gen. It is embedded into the TEST binary only, so it never
// reaches the shipped mitos-mcp image. Each case records the evaluator result
// captured at vendor time; this test asserts the evaluator still reproduces it,
// locking behavior against the real corpus.
//
//go:embed data/conformance.json
var conformanceData []byte

type conformanceFixture struct {
	SourceSHA string `json:"source_sha"`
	Cases     []struct {
		RuleID  string            `json:"rule_id"`
		Kind    string            `json:"kind"`
		Fields  map[string]string `json:"fields"`
		Trigger bool              `json:"trigger"`
	} `json:"cases"`
}

func TestConformanceFixtureLocksEvaluator(t *testing.T) {
	var fix conformanceFixture
	if err := json.Unmarshal(conformanceData, &fix); err != nil {
		t.Fatalf("parse conformance fixture: %v", err)
	}
	rs, err := loadRuleset()
	if err != nil {
		t.Fatalf("load ruleset: %v", err)
	}
	if fix.SourceSHA != rs.SourceSHA {
		t.Fatalf("fixture SHA %s != ruleset SHA %s; regenerate both together", fix.SourceSHA, rs.SourceSHA)
	}
	if len(fix.Cases) == 0 {
		t.Fatal("empty conformance fixture")
	}

	// Index rules by id so each case runs against just its own rule.
	byID := make(map[string]Rule, len(rs.Rules))
	for _, r := range rs.Rules {
		byID[r.ID] = r
	}

	var mismatches int
	for _, c := range fix.Cases {
		r, ok := byID[c.RuleID]
		if !ok {
			t.Errorf("case references unknown rule %s", c.RuleID)
			continue
		}
		e, err := NewEvaluator([]Rule{r})
		if err != nil {
			t.Errorf("rule %s: %v", c.RuleID, err)
			continue
		}
		got := len(e.Evaluate(AgentEvent{Type: "mcp_exchange", Fields: c.Fields})) > 0
		if got != c.Trigger {
			mismatches++
			if mismatches <= 10 {
				t.Errorf("rule %s (%s): evaluator returned %v, fixture locked %v", c.RuleID, c.Kind, got, c.Trigger)
			}
		}
	}
	if mismatches > 0 {
		t.Fatalf("%d of %d conformance cases diverged from the vendored fixture", mismatches, len(fix.Cases))
	}
}

func TestLoadEmbeddedRuleset(t *testing.T) {
	e, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if e.Len() == 0 {
		t.Fatal("embedded ruleset compiled to zero rules")
	}
}

func TestManifestInvariants(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	rs, err := loadRuleset()
	if err != nil {
		t.Fatalf("load ruleset: %v", err)
	}
	if m.SourceSHA != rs.SourceSHA {
		t.Fatalf("manifest SHA %s != ruleset SHA %s", m.SourceSHA, rs.SourceSHA)
	}
	if m.Counts.VendoredRules != len(rs.Rules) {
		t.Fatalf("manifest vendored_rules %d != %d rules in ruleset", m.Counts.VendoredRules, len(rs.Rules))
	}
	if m.Counts.MCPBucketRules < m.Counts.VendoredRules {
		t.Fatalf("mcp_bucket_rules %d < vendored_rules %d", m.Counts.MCPBucketRules, m.Counts.VendoredRules)
	}
	if m.Counts.CorpusRules < m.Counts.MCPBucketRules {
		t.Fatalf("corpus_rules %d < mcp_bucket_rules %d", m.Counts.CorpusRules, m.Counts.MCPBucketRules)
	}
}
