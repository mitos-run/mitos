package templatebuild

import (
	"encoding/json"
	"fmt"
	"strings"

	v1 "mitos.run/mitos/api/v1"
)

// ParseDockerfile parses a minimal Dockerfile into a SandboxTemplateSpec so the
// CLI can build or push a template from a Dockerfile (Daytona
// `create --dockerfile` parity). It supports the subset that maps cleanly onto
// the declarative build: FROM (the base image), RUN, ENV, WORKDIR, COPY (build
// steps in order), and CMD/ENTRYPOINT (the start command). Unsupported
// instructions are ignored with no error so a real Dockerfile that uses extra
// directives still produces a usable spec; the parser is deliberately small,
// not a full Dockerfile frontend.
func ParseDockerfile(content string) (v1.PoolTemplateSpec, error) {
	var spec v1.PoolTemplateSpec
	haveFrom := false

	lines := strings.Split(content, "\n")
	for i := 0; i < len(lines); i++ {
		line := joinContinuation(lines, &i)
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		instr, rest := splitInstruction(line)
		switch strings.ToUpper(instr) {
		case "FROM":
			spec.Image = firstField(rest)
			haveFrom = true
		case "RUN":
			spec.BuildSteps = append(spec.BuildSteps, v1.BuildStep{
				Type: v1.BuildStepRun, Run: rest,
			})
		case "WORKDIR":
			spec.BuildSteps = append(spec.BuildSteps, v1.BuildStep{
				Type: v1.BuildStepWorkdir, Workdir: firstField(rest),
			})
		case "ENV":
			name, value := parseEnv(rest)
			spec.BuildSteps = append(spec.BuildSteps, v1.BuildStep{
				Type: v1.BuildStepEnv, EnvName: name, EnvValue: value,
			})
		case "COPY", "ADD":
			src, dst := parseCopy(rest)
			spec.BuildSteps = append(spec.BuildSteps, v1.BuildStep{
				Type: v1.BuildStepCopy, Source: src, Dest: dst,
			})
		case "CMD", "ENTRYPOINT":
			spec.Command = parseExecForm(rest)
		default:
			// Unsupported instruction: ignore.
		}
	}
	if !haveFrom {
		return spec, fmt.Errorf("dockerfile has no FROM instruction: a base image is required")
	}
	return spec, nil
}

// joinContinuation joins a line and any backslash-continued lines that follow,
// advancing *i past the consumed lines.
func joinContinuation(lines []string, i *int) string {
	line := lines[*i]
	for strings.HasSuffix(strings.TrimRight(line, " \t"), "\\") && *i+1 < len(lines) {
		line = strings.TrimRight(strings.TrimRight(line, " \t"), "\\") + " " + lines[*i+1]
		*i++
	}
	return line
}

func splitInstruction(line string) (instr, rest string) {
	parts := strings.SplitN(line, " ", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.TrimSpace(parts[1])
}

func firstField(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// parseEnv handles both `ENV KEY VALUE` and `ENV KEY=VALUE`.
func parseEnv(s string) (name, value string) {
	if idx := strings.Index(s, "="); idx >= 0 && !strings.Contains(s[:idx], " ") {
		return s[:idx], s[idx+1:]
	}
	fields := strings.SplitN(s, " ", 2)
	if len(fields) == 2 {
		return fields[0], strings.TrimSpace(fields[1])
	}
	return s, ""
}

// parseCopy returns the source and destination of a COPY/ADD with two operands.
// Multiple sources collapse to the first; this is a minimal mapping.
func parseCopy(s string) (src, dst string) {
	fields := strings.Fields(s)
	if len(fields) >= 2 {
		return fields[0], fields[len(fields)-1]
	}
	if len(fields) == 1 {
		return fields[0], ""
	}
	return "", ""
}

// parseExecForm parses CMD/ENTRYPOINT in either JSON exec form (["a","b"]) or
// shell form (a b c).
func parseExecForm(s string) []string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		var argv []string
		if err := json.Unmarshal([]byte(s), &argv); err == nil {
			return argv
		}
	}
	return strings.Fields(s)
}
