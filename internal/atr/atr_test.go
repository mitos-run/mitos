package atr

import (
	"strings"
	"testing"
)

// rule is a small helper for building a Rule in tests.
func rule(id, cond string, conds ...Condition) Rule {
	return Rule{ID: id, Title: id, Severity: "high", Category: "test", ScanTarget: "mcp", Condition: cond, Conditions: conds}
}

func mustEval(t *testing.T, rules ...Rule) *Evaluator {
	t.Helper()
	e, err := NewEvaluator(rules)
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return e
}

func event(field, text string) AgentEvent {
	return AgentEvent{Type: "mcp_exchange", Fields: map[string]string{field: text}}
}

func TestOperators(t *testing.T) {
	tests := []struct {
		name  string
		cond  Condition
		text  string
		match bool
	}{
		{"regex_hit", Condition{"content", OpRegex, `(?i)ignore\s+previous`}, "please IGNORE   previous rules", true},
		{"regex_miss", Condition{"content", OpRegex, `(?i)ignore\s+previous`}, "keep the previous rules", false},
		{"contains_hit", Condition{"content", OpContains, "rm -rf"}, "sudo rm -rf /", true},
		{"contains_miss", Condition{"content", OpContains, "rm -rf"}, "ls -la", false},
		{"exact_hit", Condition{"content", OpExact, "drop table"}, "drop table", true},
		{"exact_miss", Condition{"content", OpExact, "drop table"}, "drop table users", false},
		{"startswith_hit", Condition{"content", OpStartsWith, "curl "}, "curl http://x", true},
		{"startswith_miss", Condition{"content", OpStartsWith, "curl "}, "echo curl ", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := mustEval(t, rule("R", "any", tc.cond))
			got := len(e.Evaluate(event("content", tc.text))) > 0
			if got != tc.match {
				t.Fatalf("match=%v want %v", got, tc.match)
			}
		})
	}
}

func TestConditionAnyAll(t *testing.T) {
	c1 := Condition{"content", OpContains, "read"}
	c2 := Condition{"content", OpContains, "exfiltrate"}

	any := mustEval(t, rule("ANY", "any", c1, c2))
	if got := len(any.Evaluate(event("content", "read the file"))); got != 1 {
		t.Fatalf("any: one condition should trigger, got %d detections", got)
	}

	all := mustEval(t, rule("ALL", "all", c1, c2))
	if got := len(all.Evaluate(event("content", "read the file"))); got != 0 {
		t.Fatalf("all: partial match must not trigger, got %d", got)
	}
	if got := len(all.Evaluate(event("content", "read then exfiltrate"))); got != 1 {
		t.Fatalf("all: full match must trigger, got %d", got)
	}
}

func TestFieldAbsentNeverMatches(t *testing.T) {
	// A rule keyed on user_input must not fire on tool-call traffic that only
	// carries tool_args. This is the honest-layering guard: the dispatch
	// chokepoint sees tool args, not user prompts.
	e := mustEval(t, rule("U", "any", Condition{"user_input", OpContains, "secret"}))
	if got := e.Evaluate(event("tool_args", "leak the secret")); len(got) != 0 {
		t.Fatalf("absent-field rule fired: %+v", got)
	}
	if got := e.Evaluate(event("user_input", "leak the secret")); len(got) != 1 {
		t.Fatalf("present-field rule did not fire, got %d", len(got))
	}
}

func TestMatchedFieldsReported(t *testing.T) {
	e := mustEval(t, rule("M", "any",
		Condition{"content", OpContains, "aws"},
		Condition{"tool_args", OpContains, "aws"},
	))
	ev := AgentEvent{Fields: map[string]string{"content": "aws key", "tool_args": "aws key"}}
	det := e.Evaluate(ev)
	if len(det) != 1 {
		t.Fatalf("want 1 detection, got %d", len(det))
	}
	if len(det[0].MatchedFields) != 2 {
		t.Fatalf("want both fields reported, got %v", det[0].MatchedFields)
	}
}

func TestSeveritySort(t *testing.T) {
	low := Rule{ID: "LOW", Severity: "low", Category: "t", Condition: "any", Conditions: []Condition{{"content", OpContains, "x"}}}
	crit := Rule{ID: "CRIT", Severity: "critical", Category: "t", Condition: "any", Conditions: []Condition{{"content", OpContains, "x"}}}
	e := mustEval(t, low, crit)
	det := e.Evaluate(event("content", "x"))
	if len(det) != 2 {
		t.Fatalf("want 2 detections, got %d", len(det))
	}
	if det[0].RuleID != "CRIT" {
		t.Fatalf("critical must sort first, got %s", det[0].RuleID)
	}
}

func TestUnknownSeveritySortsLast(t *testing.T) {
	// A mistyped or absent severity must not collide with critical (rank 0) and
	// float to the front. It sorts after every known severity.
	unknown := Rule{ID: "UNKNOWN", Severity: "bogus", Category: "t", Condition: "any", Conditions: []Condition{{"content", OpContains, "x"}}}
	low := Rule{ID: "LOW", Severity: "low", Category: "t", Condition: "any", Conditions: []Condition{{"content", OpContains, "x"}}}
	crit := Rule{ID: "CRIT", Severity: "critical", Category: "t", Condition: "any", Conditions: []Condition{{"content", OpContains, "x"}}}
	e := mustEval(t, unknown, crit, low)
	det := e.Evaluate(event("content", "x"))
	if len(det) != 3 {
		t.Fatalf("want 3 detections, got %d", len(det))
	}
	if det[0].RuleID != "CRIT" || det[len(det)-1].RuleID != "UNKNOWN" {
		t.Fatalf("order wrong: got %s ... %s, want CRIT first and UNKNOWN last", det[0].RuleID, det[len(det)-1].RuleID)
	}
}

func TestInvalidRegexIsError(t *testing.T) {
	_, err := NewEvaluator([]Rule{rule("BAD", "any", Condition{"content", OpRegex, `(?=lookahead)`})})
	if err == nil {
		t.Fatal("expected error compiling a PCRE-only pattern under RE2")
	}
}

func TestConditionlessRuleSkipped(t *testing.T) {
	e := mustEval(t, Rule{ID: "EMPTY", Condition: "any"})
	if e.Len() != 0 {
		t.Fatalf("rule with no conditions should be skipped, Len=%d", e.Len())
	}
}

func TestSampleForScan(t *testing.T) {
	if got, trunc := SampleForScan("hello", 0); got != "hello" || trunc {
		t.Fatalf("zero cap must not truncate, got %q trunc=%v", got, trunc)
	}
	if got, trunc := SampleForScan("hello", 100); got != "hello" || trunc {
		t.Fatalf("under cap must not truncate, got %q trunc=%v", got, trunc)
	}
	got, trunc := SampleForScan("hello world", 5)
	if got != "hello" || !trunc {
		t.Fatalf("over cap: got %q trunc=%v want %q true", got, trunc, "hello")
	}
	// A multi-byte rune straddling the cap must not be split.
	s := "ab" + strings.Repeat("é", 4) // é is two bytes each
	out, trunc := SampleForScan(s, 3)  // cap lands inside the first é
	if !trunc {
		t.Fatal("expected truncation")
	}
	for _, r := range out {
		if r == '�' {
			t.Fatal("truncation split a UTF-8 rune")
		}
	}
	if out != "ab" {
		t.Fatalf("expected backoff to rune boundary, got %q", out)
	}
}

func TestTruncatedFlagPropagates(t *testing.T) {
	e := mustEval(t, rule("T", "any", Condition{"content", OpContains, "x"}))
	ev := AgentEvent{Fields: map[string]string{"content": "x"}, Truncated: true}
	det := e.Evaluate(ev)
	if len(det) != 1 || !det[0].Truncated {
		t.Fatalf("truncated flag must propagate to detection: %+v", det)
	}
}
