# Run opencode against a Mitos sandbox

[opencode](https://opencode.ai) is an open-source terminal coding agent. There
are two ways to combine it with Mitos:

1. **Give opencode a Mitos sandbox as a tool** (MCP): opencode runs on your
   machine and reaches into a Mitos microVM to create sandboxes, exec, fork, and
   move files.
2. **Host the opencode harness inside a sandbox** and drive it over HTTP: opencode
   itself runs inside a Mitos microVM, one per agent, reachable over an
   authenticated URL.

## 1. Mitos sandbox as an MCP tool

opencode is an MCP client. Point it at the Mitos MCP server, `mitos-mcp`, and the
sandbox lifecycle appears to the agent as tools (`sandbox_create`,
`sandbox_exec`, `sandbox_read_file`, `sandbox_write_file`, `sandbox_fork`,
`sandbox_terminate`).

Install the server so it is on `PATH`
(`go install mitos.run/mitos/cmd/mitos-mcp@latest`), then add a local MCP server
to your `opencode.json` (project) or `~/.config/opencode/opencode.json` (global):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "mitos": {
      "type": "local",
      "command": ["mitos-mcp"],
      "environment": {
        "MITOS_BASE_URL": "https://mitos.run",
        "MITOS_API_KEY": "your-scoped-token"
      },
      "enabled": true
    }
  }
}
```

For a local standalone server, set `MITOS_BASE_URL` to `http://localhost:8080`
and drop the token (the standalone `sandbox-server` is tokenless). `mitos-mcp`
resolves its credential the same way every Mitos surface does: `MITOS_API_KEY`,
then `~/.config/mitos/credentials.json` from `mitos auth login`, then tokenless.

Confirm opencode connected and loaded the tools:

```bash
opencode mcp list
#  ✓ mitos  connected
```

Now in an opencode session the agent can claim a microVM, run the code it
writes, and (on a cluster) fork it for best-of-N. The `pool` passed to
`sandbox_create` must already exist (a `SandboxPool` on a cluster, or a template
on the standalone server via `POST /v1/templates`).

## 2. Host the opencode harness inside a sandbox

opencode ships a headless server (`opencode serve`) that exposes its HTTP API.
You can run that server inside a Mitos microVM and drive it over HTTP, one
isolated opencode per agent. This builds on the
[agent-harness recipe](../recipes/agent-harness.md); the sandbox side is
identical, with `opencode serve` as the daemon.

Start the daemon inside the guest and open a host forward to it:

```bash
# Start opencode's headless server inside the sandbox (port 4096 by default).
curl -fsS -X POST localhost:8080/v1/exec \
  -d '{"sandbox":"sbx-1","command":"nohup opencode serve --hostname 0.0.0.0 --port 4096 >/tmp/oc.log 2>&1 &"}'

# Open a host TCP forward to the guest port and dial the opencode API.
curl -fsS -X POST localhost:8080/v1/sandboxes/sbx-1/forward -d '{"guest_port":4096}'
# -> {"host":"127.0.0.1:NNNNN","guest_port":4096}
curl http://127.0.0.1:NNNNN/    # reaches opencode inside the guest
```

To fan out, warm one base sandbox, fork it, and give each fork its own
authenticated URL with Mitos Expose, so each agent gets a fresh, isolated
opencode:

```bash
mitos workspace serve harness --pool opencode --as agent-1 --port 4096 \
  --expose-domain mitos.app
# -> https://agent-1.mitos.app/
```

See the [agent-harness recipe](../recipes/agent-harness.md) for the full
host-forward, Mitos Expose, and fork-fan-out patterns (authenticated URLs, the
private / link / authenticated / public sharing tiers, and the SDK `serve()`
handles).

## What is verified

opencode 1.x connects to the Mitos MCP server with the `opencode.json` above:
`opencode mcp list` reports `mitos connected`, meaning opencode completed the MCP
`initialize` handshake and imported the sandbox tools. The tools themselves are
exercised end to end against a real Firecracker microVM (a standalone real-mode
`sandbox-server` on a KVM host): `sandbox_create` claims a real VM,
`sandbox_exec` returns a real `{exit_code, stdout}`, and `sandbox_write_file` /
`sandbox_read_file` round-trip file content. The model-driving step uses the LLM
provider you configure in opencode.
