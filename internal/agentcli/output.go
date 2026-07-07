package agentcli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// extractOutputFlag pulls the output-format flags out of args and reports
// whether structured JSON was requested. It recognizes, in any position:
//
//	--json
//	-o json        / -o=json
//	--output json  / --output=json
//
// The value must be "json" or a human format ("table", "text", "human"); an
// unrecognized value is a usage error so an agent never silently gets the human
// render when it asked for a machine one. The returned rest has the consumed
// tokens removed so the caller can pass it to its own flag set.
func extractOutputFlag(args []string) (jsonOut bool, rest []string, err error) {
	rest = make([]string, 0, len(args))
	setFormat := func(v string) error {
		switch v {
		case "json":
			jsonOut = true
		case "table", "text", "human", "":
			// The human render is the default; nothing to set.
		default:
			return fmt.Errorf("unknown output format %q (want json or table)", v)
		}
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			if e := setFormat("json"); e != nil {
				return false, nil, e
			}
		case a == "-o" || a == "--output":
			if i+1 >= len(args) {
				return false, nil, fmt.Errorf("%s requires a value (json or table)", a)
			}
			i++
			if e := setFormat(args[i]); e != nil {
				return false, nil, e
			}
		case strings.HasPrefix(a, "-o="):
			if e := setFormat(strings.TrimPrefix(a, "-o=")); e != nil {
				return false, nil, e
			}
		case strings.HasPrefix(a, "--output="):
			if e := setFormat(strings.TrimPrefix(a, "--output=")); e != nil {
				return false, nil, e
			}
		default:
			rest = append(rest, a)
		}
	}
	return jsonOut, rest, nil
}

// encodeJSON renders v as indented JSON with a trailing newline. It is the one
// place the CLI's structured output is formatted so every -o json shape is
// consistent.
func encodeJSON(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode json: %w", err)
	}
	return string(b) + "\n", nil
}

// sandboxJSONRow is the stable per-sandbox JSON shape emitted by `sandbox ls
// -o json`. Age is reported in whole seconds so a consumer does not have to
// parse the human "90s"/"2m" rendering.
type sandboxJSONRow struct {
	Name       string `json:"name"`
	Pool       string `json:"pool"`
	Phase      string `json:"phase"`
	Node       string `json:"node"`
	Endpoint   string `json:"endpoint"`
	AgeSeconds int    `json:"ageSeconds"`
}

// jsonSandboxInfos renders a sandbox listing as the documented JSON envelope
// {"sandboxes": [...]}. An empty listing renders an empty array, never null.
func jsonSandboxInfos(infos []SandboxInfo) (string, error) {
	rows := make([]sandboxJSONRow, 0, len(infos))
	for i := range infos {
		in := &infos[i]
		age := in.Age
		if age < 0 {
			age = 0
		}
		rows = append(rows, sandboxJSONRow{
			Name:       in.Name,
			Pool:       in.Pool,
			Phase:      in.Phase,
			Node:       in.Node,
			Endpoint:   in.Endpoint,
			AgeSeconds: int(age.Seconds()),
		})
	}
	return encodeJSON(struct {
		Sandboxes []sandboxJSONRow `json:"sandboxes"`
	}{Sandboxes: rows})
}

// workspaceJSONRow is the stable per-workspace JSON shape for `ws ls -o json`.
type workspaceJSONRow struct {
	Name      string `json:"name"`
	Head      string `json:"head"`
	Revisions int    `json:"revisions"`
	Resumable bool   `json:"resumable"`
}

// jsonWorkspaceInfos renders a workspace listing as {"workspaces": [...]}.
func jsonWorkspaceInfos(infos []WorkspaceInfo) (string, error) {
	rows := make([]workspaceJSONRow, 0, len(infos))
	for _, w := range infos {
		rows = append(rows, workspaceJSONRow{
			Name:      w.Name,
			Head:      w.Head,
			Revisions: w.Revisions,
			Resumable: w.Resumable,
		})
	}
	return encodeJSON(struct {
		Workspaces []workspaceJSONRow `json:"workspaces"`
	}{Workspaces: rows})
}

// revisionJSONRow is the stable per-revision JSON shape for `ws log -o json`.
type revisionJSONRow struct {
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Resumable bool   `json:"resumable"`
	Lineage   string `json:"lineage"`
}

// jsonRevisionLog renders a revision log as {"revisions": [...]}.
func jsonRevisionLog(revs []RevisionInfo) (string, error) {
	rows := make([]revisionJSONRow, 0, len(revs))
	for _, r := range revs {
		rows = append(rows, revisionJSONRow{
			Name:      r.Name,
			Phase:     r.Phase,
			Resumable: r.Resumable,
			Lineage:   r.Lineage,
		})
	}
	return encodeJSON(struct {
		Revisions []revisionJSONRow `json:"revisions"`
	}{Revisions: rows})
}
