//go:build ignore

// Command gen vendors the RE2-compatible, MCP-scan-path subset of the Agent
// Threat Rules corpus into internal/atr/data. It reads a local checkout of
// github.com/Agent-Threat-Rule/agent-threat-rules pinned at the SHA below,
// parses the rule YAML, drops every rule whose regex needs PCRE-only features
// (lookahead, lookbehind, backreferences) that Go's RE2 engine cannot compile,
// buckets by scan_target, and writes four deterministic artifacts:
//
//	data/ruleset.json     the compiled rules loaded at runtime
//	data/skipped.json     rules or conditions dropped for RE2 incompatibility
//	data/manifest.json    provenance (repo, SHA, license) and vendor counts
//	data/conformance.json a regression fixture built from the rules' test_cases
//
// Run from the repo root:
//
//	go run ./internal/atr/gen -src /path/to/agent-threat-rules
//
// Determinism: no timestamps are written and every list is sorted, so a
// regeneration against the same SHA produces a byte-identical diff.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"mitos.run/mitos/internal/atr"
)

// pinnedSHA is the upstream commit this vendor is generated from. Bump it
// deliberately, like the Firecracker version pin, and regenerate.
const (
	pinnedSHA  = "f49ddd904b50b33e804d3000f95a6d75864f0034"
	sourceRepo = "https://github.com/Agent-Threat-Rule/agent-threat-rules"
	license    = "MIT"
	spec       = "ATR-SPEC-v1"
)

// mcpBucket is the set of scan_target values the mitos-mcp dispatch chokepoint
// screens. Per ATR-SPEC-v1 section 3.3.2 an absent scan_target ("") applies to
// every scan path, so it is included in the MCP evaluate() path.
var mcpBucket = map[string]bool{
	"mcp":           true,
	"tool_args":     true,
	"tool_response": true,
	"both":          true,
	"":              true,
}

// rawRule is the subset of the ATR rule schema this vendor step reads.
type rawRule struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
	Tags     struct {
		Category   string `json:"category"`
		ScanTarget string `json:"scan_target"`
	} `json:"tags"`
	Detection struct {
		Condition  string `json:"condition"`
		Conditions []struct {
			Field    string `json:"field"`
			Operator string `json:"operator"`
			Value    string `json:"value"`
		} `json:"conditions"`
	} `json:"detection"`
	TestCases struct {
		TruePositives []testCase `json:"true_positives"`
		TrueNegatives []testCase `json:"true_negatives"`
	} `json:"test_cases"`
}

type testCase struct {
	// Input is a string in most rules but an object in a few (structured
	// multi-field cases). Keep it raw and coerce to a string; skip non-strings.
	Input json.RawMessage `json:"input"`
}

// stringInput returns the test-case input when it is a plain JSON string.
func (tc testCase) stringInput() (string, bool) {
	var s string
	if err := json.Unmarshal(tc.Input, &s); err != nil {
		return "", false
	}
	return s, s != ""
}

type skippedEntry struct {
	ID               string `json:"id"`
	ScanTarget       string `json:"scan_target"`
	TotalConditions  int    `json:"total_conditions"`
	DroppedCondition int    `json:"dropped_conditions"`
	Reason           string `json:"reason"`
	SampleError      string `json:"sample_error"`
}

type conformanceCase struct {
	RuleID  string            `json:"rule_id"`
	Kind    string            `json:"kind"` // "tp" or "tn"
	Fields  map[string]string `json:"fields"`
	Trigger bool              `json:"trigger"` // evaluator result locked at vendor time
}

func main() {
	src := flag.String("src", "", "path to an agent-threat-rules checkout at the pinned SHA")
	out := flag.String("out", "internal/atr/data", "output directory for the vendored data files")
	flag.Parse()
	if *src == "" {
		fatal("pass -src pointing at a checkout of " + sourceRepo + " at " + pinnedSHA)
	}

	files, err := filepath.Glob(filepath.Join(*src, "rules", "*", "*.yaml"))
	if err != nil {
		fatal(err.Error())
	}
	ymls, _ := filepath.Glob(filepath.Join(*src, "rules", "*", "*.yml"))
	files = append(files, ymls...)
	sort.Strings(files)
	if len(files) == 0 {
		fatal("no rule files under " + filepath.Join(*src, "rules"))
	}

	var (
		vendored           []atr.Rule
		skipped            []skippedEntry
		conformances       []conformanceCase
		corpus             int
		mcpTotal           int
		excludedStat       int
		compiledCond       int
		tpTotal, tpMatched int
		tnTotal, tnClean   int
	)

	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			fatal(err.Error())
		}
		var rr rawRule
		if err := yaml.Unmarshal(b, &rr); err != nil {
			fatal(fmt.Sprintf("%s: %v", f, err))
		}
		if rr.ID == "" {
			continue
		}
		corpus++

		// Spec 3.5.3: iterate only non-draft, non-deprecated rules.
		if rr.Status == "draft" || rr.Status == "deprecated" {
			excludedStat++
			continue
		}
		if !mcpBucket[rr.Tags.ScanTarget] {
			continue
		}
		mcpTotal++

		category := rr.Tags.Category
		if category == "" {
			category = filepath.Base(filepath.Dir(f))
		}

		var kept []atr.Condition
		var dropped int
		var sampleErr string
		for _, c := range rr.Detection.Conditions {
			op := atr.Operator(c.Operator)
			if op == atr.OpRegex {
				if _, err := regexp.Compile(c.Value); err != nil {
					dropped++
					if sampleErr == "" {
						sampleErr = trimErr(err.Error())
					}
					continue
				}
			} else if op != atr.OpContains && op != atr.OpExact && op != atr.OpStartsWith {
				// Unsupported operator (e.g. a named/behavioral form). Drop it.
				dropped++
				if sampleErr == "" {
					sampleErr = "unsupported operator " + c.Operator
				}
				continue
			}
			kept = append(kept, atr.Condition{Field: c.Field, Operator: op, Value: c.Value})
		}

		total := len(rr.Detection.Conditions)
		if len(kept) == 0 {
			skipped = append(skipped, skippedEntry{
				ID: rr.ID, ScanTarget: rr.Tags.ScanTarget,
				TotalConditions: total, DroppedCondition: dropped,
				Reason: "re2_incompatible", SampleError: sampleErr,
			})
			continue
		}
		if dropped > 0 {
			skipped = append(skipped, skippedEntry{
				ID: rr.ID, ScanTarget: rr.Tags.ScanTarget,
				TotalConditions: total, DroppedCondition: dropped,
				Reason: "partial_re2_incompatible", SampleError: sampleErr,
			})
		}

		cond := rr.Detection.Condition
		if cond != "all" {
			cond = "any"
		}
		rule := atr.Rule{
			ID: rr.ID, Title: rr.Title, Severity: strings.ToLower(rr.Severity),
			Category: category, ScanTarget: rr.Tags.ScanTarget,
			Condition: cond, Conditions: kept,
		}
		vendored = append(vendored, rule)
		compiledCond += len(kept)

		// Build the conformance fixture from this rule's embedded test cases,
		// evaluating each with the shipped evaluator so the runtime test is a
		// deterministic regression lock.
		single, err := atr.NewEvaluator([]atr.Rule{rule})
		if err != nil {
			fatal(fmt.Sprintf("rule %s: %v", rule.ID, err))
		}
		fieldNames := ruleFields(rule)
		for _, tc := range rr.TestCases.TruePositives {
			input, ok := tc.stringInput()
			if !ok {
				continue
			}
			ev := eventFor(fieldNames, input)
			trig := len(single.Evaluate(ev)) > 0
			tpTotal++
			if trig {
				tpMatched++
			}
			conformances = append(conformances, conformanceCase{RuleID: rule.ID, Kind: "tp", Fields: ev.Fields, Trigger: trig})
		}
		for _, tc := range rr.TestCases.TrueNegatives {
			input, ok := tc.stringInput()
			if !ok {
				continue
			}
			ev := eventFor(fieldNames, input)
			trig := len(single.Evaluate(ev)) > 0
			tnTotal++
			if !trig {
				tnClean++
			}
			conformances = append(conformances, conformanceCase{RuleID: rule.ID, Kind: "tn", Fields: ev.Fields, Trigger: trig})
		}
	}

	sort.Slice(vendored, func(i, j int) bool { return vendored[i].ID < vendored[j].ID })
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].ID < skipped[j].ID })
	sort.Slice(conformances, func(i, j int) bool {
		if conformances[i].RuleID != conformances[j].RuleID {
			return conformances[i].RuleID < conformances[j].RuleID
		}
		if conformances[i].Kind != conformances[j].Kind {
			return conformances[i].Kind < conformances[j].Kind
		}
		return fingerprint(conformances[i].Fields) < fingerprint(conformances[j].Fields)
	})

	skippedRules := 0
	for _, s := range skipped {
		if s.Reason == "re2_incompatible" {
			skippedRules++
		}
	}

	writeJSON(filepath.Join(*out, "ruleset.json"), map[string]any{
		"spec": spec, "source_sha": pinnedSHA, "rules": vendored,
	})
	writeJSON(filepath.Join(*out, "skipped.json"), map[string]any{
		"spec": spec, "source_sha": pinnedSHA, "skipped": skipped,
	})
	writeJSON(filepath.Join(*out, "manifest.json"), map[string]any{
		"spec": spec, "source_repo": sourceRepo, "source_sha": pinnedSHA, "license": license,
		"counts": map[string]int{
			"corpus_rules":             corpus,
			"excluded_by_status":       excludedStat,
			"mcp_bucket_rules":         mcpTotal,
			"vendored_rules":           len(vendored),
			"skipped_re2_incompatible": skippedRules,
			"compiled_conditions":      compiledCond,
		},
		"coverage": map[string]int{
			"true_positive_cases": tpTotal, "true_positive_matched": tpMatched,
			"true_negative_cases": tnTotal, "true_negative_clean": tnClean,
		},
		"mcp_scan_targets": []string{"", "both", "mcp", "tool_args", "tool_response"},
	})
	writeJSON(filepath.Join(*out, "conformance.json"), map[string]any{
		"source_sha": pinnedSHA, "cases": conformances,
	})

	fmt.Printf("vendored %d rules (%d conditions) from %d corpus rules; MCP bucket %d; skipped %d for RE2; conformance cases %d (TP %d/%d, TN %d/%d)\n",
		len(vendored), compiledCond, corpus, mcpTotal, skippedRules, len(conformances), tpMatched, tpTotal, tnClean, tnTotal)
}

// ruleFields returns the distinct field names a rule inspects.
func ruleFields(r atr.Rule) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, c := range r.Conditions {
		if _, ok := seen[c.Field]; ok {
			continue
		}
		seen[c.Field] = struct{}{}
		out = append(out, c.Field)
	}
	sort.Strings(out)
	return out
}

// eventFor builds an AgentEvent that places input in every field the rule reads,
// so a test case exercises whichever condition its rule authored.
func eventFor(fields []string, input string) atr.AgentEvent {
	m := make(map[string]string, len(fields))
	for _, f := range fields {
		m[f] = input
	}
	return atr.AgentEvent{Type: "mcp_exchange", Fields: m}
}

func fingerprint(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
		b.WriteByte(';')
	}
	return b.String()
}

func trimErr(s string) string {
	if len(s) > 160 {
		return s[:160]
	}
	return s
}

func writeJSON(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fatal(err.Error())
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		fatal(err.Error())
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "gen: "+msg)
	os.Exit(1)
}
