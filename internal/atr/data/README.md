# Vendored Agent Threat Rules data

These files are generated, not hand-edited. They are the compiled, RE2-compatible,
MCP-scan-path subset of the [Agent Threat Rules](https://github.com/Agent-Threat-Rule/agent-threat-rules)
corpus (ATR-SPEC-v1, MIT-licensed), pinned to the exact upstream SHA recorded in
`manifest.json`.

| File | Purpose |
|---|---|
| `ruleset.json` | Compiled rules loaded at runtime by `internal/atr` (embedded). |
| `skipped.json` | Rules or conditions dropped because their regex needs PCRE-only features Go's RE2 engine cannot compile. |
| `manifest.json` | Provenance (source repo, pinned SHA, license) and the vendor-step counts, including the honest coverage tallies. |
| `conformance.json` | Regression fixture built from the rules' own test cases; embedded into the TEST binary only. |
| `UPSTREAM_LICENSE` | The upstream MIT license, retained for attribution. |

## Regenerating

Check out the upstream repo at the pinned SHA and run the generator from the
repo root:

```bash
git clone https://github.com/Agent-Threat-Rule/agent-threat-rules /tmp/atr
git -C /tmp/atr checkout <sha-from-manifest.json>
go run ./internal/atr/gen -src /tmp/atr
```

The output is deterministic: no timestamps, every list sorted, so regenerating
against the same SHA produces no diff. To adopt newer rules, bump the pinned SHA
in `internal/atr/gen/main.go` and regenerate.
