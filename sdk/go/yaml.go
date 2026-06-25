package mitos

// A minimal YAML-to-JSON converter for the kubeconfig subset, so the Go SDK can
// parse a kubeconfig file without a third-party YAML dependency. Direct mode
// stays dependency-free and this code is reached only on the cluster-mode
// kubeconfig path.
//
// Supported subset (sufficient for a standard kubeconfig):
//
//   - block mappings: "key: value" and "key:" introducing a nested block;
//   - block sequences: "- value" and "- key: value" (an inline-map list item);
//   - scalars: plain, single-quoted, and double-quoted strings, integers,
//     booleans, and null;
//   - comments (# ...) and blank lines;
//   - the "---" document separator (only the first document is parsed).
//
// It does NOT support: flow collections ({a: b}, [a, b]), anchors/aliases,
// multi-line block scalars (| and >), or tags. A kubeconfig uses none of these,
// and an unsupported construct surfaces as a typed kubeconfig_parse_failed error
// upstream rather than silently misparsing.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// yamlToJSON parses the supported YAML subset and re-encodes it as JSON bytes so
// the existing encoding/json struct tags on kubeconfig do the final decoding.
func yamlToJSON(src []byte) ([]byte, error) {
	lines := splitYAMLLines(string(src))
	p := &yamlParser{lines: lines}
	value, err := p.parseBlock(0)
	if err != nil {
		return nil, err
	}
	if value == nil {
		value = map[string]any{}
	}
	return json.Marshal(value)
}

// yamlLine is one significant (non-blank, non-comment) line with its indentation
// depth and trimmed content.
type yamlLine struct {
	indent int
	text   string
	raw    string
}

// splitYAMLLines strips comments and blank lines and records each remaining
// line's indentation. Parsing stops at a "---" document separator after the
// first document has started, so only the first YAML document is considered.
func splitYAMLLines(src string) []yamlLine {
	var out []yamlLine
	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "---" || trimmed == "..." {
			if len(out) > 0 {
				break
			}
			continue
		}
		indent := len(line) - len(trimmed)
		// Strip a trailing inline comment that is preceded by whitespace. A "#"
		// inside a quoted scalar is preserved (kubeconfig values do not embed #,
		// so a simple unquoted-prefix check is sufficient here).
		content := stripInlineComment(trimmed)
		out = append(out, yamlLine{indent: indent, text: content, raw: line})
	}
	return out
}

// stripInlineComment removes a " #..." trailing comment from a line unless the
// line begins with a quote (a quoted scalar may legitimately contain #).
func stripInlineComment(s string) string {
	if strings.HasPrefix(s, "'") || strings.HasPrefix(s, "\"") {
		return s
	}
	if i := strings.Index(s, " #"); i >= 0 {
		return strings.TrimRight(s[:i], " ")
	}
	return s
}

// yamlParser walks the significant lines with a cursor.
type yamlParser struct {
	lines []yamlLine
	pos   int
}

// parseBlock parses a mapping or sequence whose items are at indentation
// minIndent. It returns when a line dedents below minIndent.
func (p *yamlParser) parseBlock(minIndent int) (any, error) {
	if p.pos >= len(p.lines) {
		return nil, nil
	}
	first := p.lines[p.pos]
	if first.indent < minIndent {
		return nil, nil
	}
	if strings.HasPrefix(first.text, "- ") || first.text == "-" {
		return p.parseSequence(first.indent)
	}
	return p.parseMapping(first.indent)
}

// parseMapping parses block-mapping entries at exactly the given indent.
func (p *yamlParser) parseMapping(indent int) (any, error) {
	m := map[string]any{}
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if line.indent < indent {
			break
		}
		if line.indent > indent {
			return nil, fmt.Errorf("unexpected indentation at %q", line.raw)
		}
		key, rest, ok := splitKey(line.text)
		if !ok {
			return nil, fmt.Errorf("expected key: value at %q", line.raw)
		}
		p.pos++
		if rest != "" {
			m[key] = parseScalar(rest)
			continue
		}
		// A nested block (mapping or sequence) follows on deeper-indented lines,
		// or the value is empty.
		if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
			child, err := p.parseBlock(p.lines[p.pos].indent)
			if err != nil {
				return nil, err
			}
			m[key] = child
		} else if p.pos < len(p.lines) && p.lines[p.pos].indent == indent &&
			(strings.HasPrefix(p.lines[p.pos].text, "- ") || p.lines[p.pos].text == "-") {
			// A block sequence may be indented at the same column as its key.
			child, err := p.parseSequence(indent)
			if err != nil {
				return nil, err
			}
			m[key] = child
		} else {
			m[key] = nil
		}
	}
	return m, nil
}

// parseSequence parses block-sequence items ("- ...") at the given indent.
func (p *yamlParser) parseSequence(indent int) (any, error) {
	var seq []any
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if line.indent < indent {
			break
		}
		if line.indent > indent || !(strings.HasPrefix(line.text, "- ") || line.text == "-") {
			break
		}
		item := strings.TrimSpace(strings.TrimPrefix(line.text, "-"))
		if item == "" {
			// The item's content is on the following deeper-indented lines.
			p.pos++
			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				child, err := p.parseBlock(p.lines[p.pos].indent)
				if err != nil {
					return nil, err
				}
				seq = append(seq, child)
			} else {
				seq = append(seq, nil)
			}
			continue
		}
		key, rest, ok := splitKey(item)
		if ok {
			// An inline-map list item: "- key: value" starts a mapping whose
			// first entry is on this line and whose remaining entries are the
			// deeper-indented following lines. The map's column is the position
			// of the item content after the "- ".
			mapIndent := indent + (len(line.text) - len(strings.TrimPrefix(line.text, "- ")))
			// Replace this line with the remainder so parseMapping reads the
			// first entry, then continues with the following lines.
			entry := map[string]any{}
			if rest != "" {
				entry[key] = parseScalar(rest)
			} else if p.pos+1 < len(p.lines) && p.lines[p.pos+1].indent > mapIndent {
				p.pos++
				child, err := p.parseBlock(p.lines[p.pos].indent)
				if err != nil {
					return nil, err
				}
				entry[key] = child
				p.pos--
			} else {
				entry[key] = nil
			}
			p.pos++
			// Continue the same mapping for any following lines at mapIndent.
			for p.pos < len(p.lines) && p.lines[p.pos].indent == mapIndent &&
				!strings.HasPrefix(p.lines[p.pos].text, "- ") {
				ln := p.lines[p.pos]
				k2, r2, ok2 := splitKey(ln.text)
				if !ok2 {
					return nil, fmt.Errorf("expected key: value at %q", ln.raw)
				}
				p.pos++
				if r2 != "" {
					entry[k2] = parseScalar(r2)
					continue
				}
				if p.pos < len(p.lines) && p.lines[p.pos].indent > mapIndent {
					child, err := p.parseBlock(p.lines[p.pos].indent)
					if err != nil {
						return nil, err
					}
					entry[k2] = child
				} else {
					entry[k2] = nil
				}
			}
			seq = append(seq, entry)
			continue
		}
		// A plain scalar list item.
		seq = append(seq, parseScalar(item))
		p.pos++
	}
	return seq, nil
}

// splitKey splits "key: value" into key and the (possibly empty) value. It
// returns ok=false when there is no top-level ": " or trailing ":" separator.
func splitKey(s string) (key, value string, ok bool) {
	// A bare "key:" with no value.
	if strings.HasSuffix(s, ":") {
		return strings.TrimSpace(s[:len(s)-1]), "", true
	}
	if i := strings.Index(s, ": "); i >= 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+2:]), true
	}
	return "", "", false
}

// parseScalar converts a YAML scalar token to its Go value: a quoted or plain
// string, an integer, a float, a bool, or nil.
func parseScalar(s string) any {
	s = strings.TrimSpace(s)
	if s == "" || s == "~" || s == "null" || s == "Null" || s == "NULL" {
		return nil
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if unq, err := strconv.Unquote(s); err == nil {
			return unq
		}
		return s[1 : len(s)-1]
	}
	switch s {
	case "true", "True", "TRUE":
		return true
	case "false", "False", "FALSE":
		return false
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}
