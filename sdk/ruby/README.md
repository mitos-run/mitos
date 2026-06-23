# mitos Ruby SDK

A thin, dependency-free Ruby client for the standalone and hosted mitos
sandbox-server REST API. It mirrors the direct-mode surface of the Python SDK
(`sdk/python/mitos/direct.py`) and the TypeScript SDK
(`sdk/typescript/src/server.ts`): create a template, fork a sandbox, run `exec`,
and terminate.

The SDK uses only the Ruby standard library (`net/http`, `json`, `uri`,
`securerandom`), so there are no gem dependencies to build. It targets Ruby
2.6 and later.

## Scope

This gem covers DIRECT mode only: the standalone `cmd/sandbox-server` and the
hosted control plane at `https://mitos.run`. The Kubernetes / cluster mode (the
controller, forkd, and the `mitos.run/v1` CRDs: `Sandbox`, `SandboxPool`,
`Workspace`, `WorkspaceRevision`) is served by the Python and TypeScript SDKs
only and is NOT part of this gem.

## Install

Not yet published to RubyGems. Install from source:

```ruby
# Gemfile
gem "mitos", path: "path/to/mitos/sdk/ruby"
```

Or add the `lib` directory to the load path directly:

```ruby
$LOAD_PATH.unshift("path/to/mitos/sdk/ruby/lib")
require "mitos"
```

## Quickstart (hosted)

The base URL defaults to the hosted endpoint `https://mitos.run`. Set your API
key in the environment; it is sent as `Authorization: Bearer <key>` and is never
logged.

```ruby
require "mitos"

ENV["MITOS_API_KEY"] = "sk-..."          # or pass api_key: to Mitos.server

server = Mitos.server                     # base URL + API key from the env
server.create_template("python")          # build (or get) the template
sandbox = server.fork("python")           # fork a fresh sandbox

result = sandbox.exec("echo hello")
puts result.exit_code                     # 0
puts result.stdout                        # "hello\n"

sandbox.terminate
```

Point at a local standalone server by setting `MITOS_BASE_URL` (or passing
`url:`):

```ruby
server = Mitos.server(url: "http://localhost:8080")
```

`exec` requires a Ready sandbox: the sandbox-server routes exec through the
guest agent over vsock, so calling exec on a sandbox that is not yet up returns a
typed `not_found` error.

## Surface

| Method | HTTP | Returns | Notes |
| --- | --- | --- | --- |
| `Mitos.server(url:, api_key:)` | - | `SandboxServer` | Base URL: arg, else `MITOS_BASE_URL`, else `https://mitos.run`. API key: arg, else `MITOS_API_KEY`, else the `mitos auth login` credential (`~/.config/mitos/credentials.json`), else tokenless. |
| `SandboxServer#create_template(id, init_wait_seconds:, idempotency_key:)` | `POST /v1/templates` | `Template` | Sends a fresh `Idempotency-Key`. |
| `SandboxServer#list_templates` | `GET /v1/templates` | `Array<Template>` | |
| `SandboxServer#fork(template, id:, idempotency_key:)` | `POST /v1/fork` | `Sandbox` | Generates a `sandbox-<hex>` id when `id` is nil; validates against the id allowlist. Sends a fresh `Idempotency-Key`. |
| `SandboxServer#list_sandboxes` | `GET /v1/sandboxes` | `Array<ServerSandbox>` | |
| `Sandbox#exec(command, timeout:)` | `POST /v1/exec` | `ExecResult` | Needs a Ready sandbox. |
| `Sandbox#terminate` | `DELETE /v1/sandboxes/{id}` | `nil` | |

Value objects:

- `Template`: `id`, `ready` (`ready?`), `created_at`, `creation_time_ms`.
- `ServerSandbox`: `id`, `template_id`, `endpoint`, `created_at`, `fork_time_ms`.
- `ExecResult`: `exit_code`, `stdout`, `stderr`, `exec_time_ms` (`success?`).
- `Sandbox`: `id`, `endpoint`.

## Errors

Every non-2xx response raises `Mitos::MitosError`, which parses the server
envelope `{error:{code, message, cause, remediation}}`. Branch on `code`, never
on the message text:

```ruby
begin
  sandbox.exec("echo hi")
rescue Mitos::MitosError => e
  warn e.code         # e.g. "not_found"
  warn e.status       # e.g. 404
  warn e.remediation  # actionable hint
end
```

The configured API key is redacted from any error body before it becomes the
error cause.

## The sandbox id allowlist

Sandbox ids must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` (the same allowlist the
Go daemon, the Python SDK, and the TypeScript SDK enforce). `fork` and
`terminate` validate the id and raise a typed `invalid_sandbox_id` error before
sending any request.

## Tests

The tests spin up a WEBrick stub reproducing the sandbox-server wire shapes and
assert the SDK round trips them. They need `minitest` and `webrick`; both shipped
in the stdlib through Ruby 2.7, but `webrick` became a separate gem in Ruby 3.0,
so on Ruby 3+ install it first (`gem install webrick`). The SDK itself still has
no runtime dependencies; this is a test-only dependency.

```bash
cd sdk/ruby
gem install webrick   # only needed on Ruby 3.0+
ruby -Ilib -Itest test/sandbox_server_test.rb
# or, with Rake:
rake test
```

## Deferred

Not yet implemented in the Ruby SDK (covered by the Python / TypeScript SDKs):

- Kubernetes / cluster mode (controller, forkd, `mitos.run/v1` CRDs).
- The files API (`/v1/files/*`).
- Interactive PTY (`/v1/pty`).
- `run_code`: the server exposes a streaming-only route
  (`POST /v1/run_code/stream`); a synchronous Ruby wrapper is deferred until a
  non-streaming contract exists.
- Per-sandbox network posture, `set_timeout`, `pause` / `resume`, and
  `get_host(port)` preview URLs.
- RubyGems publishing.
