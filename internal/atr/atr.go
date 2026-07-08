// Package atr is a native Go evaluator for the regex-condition subset of
// Agent Threat Rules (ATR-SPEC-v1, https://github.com/Agent-Threat-Rule/agent-threat-rules),
// an MIT-licensed Sigma-style ruleset for AI-agent threats.
//
// The evaluator screens an AgentEvent (a set of named text fields lifted from
// an agent-application chokepoint, such as an MCP tool call) against a set of
// rules and returns the rules that matched. It is a DETECTION layer, not an
// isolation control: it flags patterns in the traffic mitos routes and never
// blocks or replaces the microVM boundary.
//
// Scope of this subset:
//   - Array-format detection (ATR-SPEC-v1 section 3.5.1) with operators
//     regex, contains, exact, and starts_with, combined with condition any/all.
//   - Regex is Go's stdlib regexp (RE2): linear-time, no lookahead, lookbehind,
//     or backreferences. Rules whose patterns need PCRE-only features are dropped
//     at vendor time and recorded in data/skipped.json; they never reach runtime.
//
// The named/behavioral detection form (section 3.5.2: metrics, windows,
// sequences) is intentionally out of scope; it needs stateful monitoring the
// dispatch chokepoint does not have. No rule in the pinned corpus uses it.
package atr

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Operator is a condition match operator (ATR-SPEC-v1 section 3.5.1).
type Operator string

// The supported operators. Only these compile into a runnable condition; a rule
// carrying any other operator is dropped at vendor time.
const (
	OpRegex      Operator = "regex"
	OpContains   Operator = "contains"
	OpExact      Operator = "exact"
	OpStartsWith Operator = "starts_with"
)

// Condition is one detection condition: match Operator of Value against the
// event field named Field.
type Condition struct {
	Field    string   `json:"field"`
	Operator Operator `json:"operator"`
	Value    string   `json:"value"`
}

// Rule is the vendored, RE2-compatible subset of one ATR rule.
type Rule struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Severity   string `json:"severity"`
	Category   string `json:"category"`
	ScanTarget string `json:"scan_target"`
	// Condition is the combinator over Conditions: "any" (OR) or "all" (AND).
	Condition  string      `json:"condition"`
	Conditions []Condition `json:"conditions"`
}

// AgentEvent is the host-supplied event screened against the ruleset. Fields
// maps an ATR field name (content, tool_args, tool_name, tool_response, ...) to
// the text to inspect. A condition whose Field is absent from the map never
// matches, which keeps a user_input rule from firing on tool-call traffic.
type AgentEvent struct {
	// Type is the ATR agent-source type, e.g. mcp_exchange. Informational.
	Type string
	// Fields is the field-name to content map to screen.
	Fields map[string]string
	// Truncated is set by the caller when any field was sampled to a byte cap;
	// it is copied onto every Detection so a capped scan is observable.
	Truncated bool
}

// Detection is one rule match against an AgentEvent.
type Detection struct {
	RuleID        string   `json:"rule_id"`
	Title         string   `json:"title"`
	Severity      string   `json:"severity"`
	Category      string   `json:"category"`
	ScanTarget    string   `json:"scan_target"`
	MatchedFields []string `json:"matched_fields"`
	// Truncated is true when the screened event was sampled to a byte cap, so a
	// trailing payload could have evaded the match.
	Truncated bool `json:"truncated"`
}

// compiledCondition is a Condition with its regexp precompiled (for OpRegex).
type compiledCondition struct {
	field string
	op    Operator
	value string
	re    *regexp.Regexp
}

// compiledRule is a Rule with every condition compiled once at load time.
type compiledRule struct {
	rule       Rule
	all        bool // true when Condition is "all"; false for "any"
	conditions []compiledCondition
}

// Evaluator holds compiled rules ready to screen events. It is read-only after
// construction and safe for concurrent use.
type Evaluator struct {
	rules []compiledRule
}

// severityRank orders detections critical-first (ATR-SPEC-v1 section 3.5.3).
var severityRank = map[string]int{
	"critical": 0,
	"high":     1,
	"medium":   2,
	"low":      3,
	"info":     4,
}

// severityRankFor returns the sort rank for a severity. An unknown or mistyped
// severity sorts LAST, not first: a bare map lookup returns Go's zero value,
// which collides with "critical" (rank 0) and would silently float an unranked
// detection to the front.
func severityRankFor(sev string) int {
	if r, ok := severityRank[sev]; ok {
		return r
	}
	return len(severityRank)
}

// NewEvaluator compiles rules into an Evaluator. A rule with no usable
// conditions is skipped. An invalid regex is a hard error: the vendor step
// guarantees every embedded pattern compiles under RE2, so a failure here means
// the vendored data is corrupt.
func NewEvaluator(rules []Rule) (*Evaluator, error) {
	compiled := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		cr := compiledRule{rule: r, all: r.Condition == "all"}
		for _, c := range r.Conditions {
			cc := compiledCondition{field: c.Field, op: c.Operator, value: c.Value}
			if c.Operator == OpRegex {
				re, err := regexp.Compile(c.Value)
				if err != nil {
					return nil, fmt.Errorf("rule %s: compile condition on field %q: %w", r.ID, c.Field, err)
				}
				cc.re = re
			}
			cr.conditions = append(cr.conditions, cc)
		}
		if len(cr.conditions) == 0 {
			continue
		}
		compiled = append(compiled, cr)
	}
	return &Evaluator{rules: compiled}, nil
}

// Len reports how many rules are loaded.
func (e *Evaluator) Len() int { return len(e.rules) }

// Evaluate screens event against every loaded rule and returns the matches,
// sorted critical-severity first then by rule id. It never mutates event.
func (e *Evaluator) Evaluate(event AgentEvent) []Detection {
	var out []Detection
	for _, cr := range e.rules {
		matched, fields := cr.eval(event)
		if !matched {
			continue
		}
		out = append(out, Detection{
			RuleID:        cr.rule.ID,
			Title:         cr.rule.Title,
			Severity:      cr.rule.Severity,
			Category:      cr.rule.Category,
			ScanTarget:    cr.rule.ScanTarget,
			MatchedFields: fields,
			Truncated:     event.Truncated,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := severityRankFor(out[i].Severity), severityRankFor(out[j].Severity)
		if ri != rj {
			return ri < rj
		}
		return out[i].RuleID < out[j].RuleID
	})
	return out
}

// eval applies a rule's condition combinator. For "any" it returns on the first
// matching condition; for "all" every condition must match. The returned fields
// are the distinct event fields that contributed a match.
func (cr compiledRule) eval(event AgentEvent) (bool, []string) {
	seen := map[string]struct{}{}
	var fields []string
	addField := func(f string) {
		if _, ok := seen[f]; ok {
			return
		}
		seen[f] = struct{}{}
		fields = append(fields, f)
	}

	if cr.all {
		for _, c := range cr.conditions {
			text, ok := event.Fields[c.field]
			if !ok || !c.match(text) {
				return false, nil
			}
			addField(c.field)
		}
		return true, fields
	}

	// "any": trigger on the first match, but collect all matching fields so the
	// detection names every field that fired.
	for _, c := range cr.conditions {
		text, ok := event.Fields[c.field]
		if !ok {
			continue
		}
		if c.match(text) {
			addField(c.field)
		}
	}
	return len(fields) > 0, fields
}

// match applies one condition operator to a field's text.
func (c compiledCondition) match(text string) bool {
	switch c.op {
	case OpRegex:
		return c.re != nil && c.re.MatchString(text)
	case OpContains:
		return strings.Contains(text, c.value)
	case OpExact:
		return text == c.value
	case OpStartsWith:
		return strings.HasPrefix(text, c.value)
	default:
		return false
	}
}
