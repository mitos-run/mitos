# SDK conformance parity suite (issue #22)

This directory holds ONE shared scenario, `scenario.json`, that both SDKs run
against the SAME backend so we can prove they behave identically.

## What it proves

The Python SDK (`sdk/python/mitos`) and the TypeScript SDK (`sdk/typescript`)
drive the SAME ordered control-plane scenario against the SAME standalone
`sandbox-server` in mock mode, and each asserts that its NORMALIZED result equals
the expectation declared in `scenario.json`. Same scenario, same asserted
contract, in two languages.

The shared steps are the control plane both SDKs expose:

1. `createTemplate(id)` -> `{id, ready}`
2. `listTemplates()` contains that template
3. `fork(template, id)` -> a sandbox with `{id, endpoint present}`
4. `listSandboxes()` contains that sandbox with `{id, template_id}`
5. `terminate()` -> the sandbox is gone from `listSandboxes()`

## What is out of scope here

`exec`, `files`, and `run_code` need a guest VM (a vsock guest agent), which the
mock engine does not have. The conformance SURFACE is therefore the control
plane only. The in-VM data path is proven on the KVM CI job (`kvm-test.yaml`),
not here.

## Normalization

The two languages report the same logical result with different surface details
(camelCase vs snake_case keys, an environment dependent endpoint URL, per-run
timing fields). `scenario.json`'s `normalization` block defines the rules that
collapse those differences so the assertions are byte-equal:

- compare only the stable keys (`id`, `ready`, `template_id`);
- ignore all timing fields (`creation_time_ms` / `fork_time_ms` /
  `created_at`) and the `network` echo;
- normalize SDK-native camelCase (`templateId`) and wire snake_case
  (`template_id`) to the snake_case keys this file uses;
- normalize `endpoint` to the boolean `endpoint_present` (its value is the
  server's own URL, which is environment dependent).

## Running it

The runners are gated so the unit-only `make test-python` and `npm test` are
unaffected: they SKIP unless a reachable server URL is provided.

Start the mock server:

```bash
go run ./cmd/sandbox-server --mock   # listens on :8080
```

Python:

```bash
cd sdk/python
MITOS_CONFORMANCE_URL=http://localhost:8080 \
  PYTHONPATH=. python3 -m pytest tests/test_conformance.py -v
```

TypeScript:

```bash
cd sdk/typescript
MITOS_CONFORMANCE_URL=http://localhost:8080 \
  npx vitest run test/conformance.test.ts
```

Unset `MITOS_CONFORMANCE_URL` (and have nothing on `localhost:8080`) and both
runners skip cleanly.

The `sdk-conformance` CI job (`.github/workflows/ci.yaml`) builds and starts the
mock server, waits for `/v1/health`, sets `MITOS_CONFORMANCE_URL`, and runs both
runners against it.
