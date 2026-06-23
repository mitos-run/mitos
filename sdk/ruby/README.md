# Mitos Ruby SDK

Mitos gives AI agents isolated, forkable sandboxes: Firecracker microVMs that
restore from snapshots and fork into parallel attempts, so an agent can branch a
warm environment instead of rebuilding it. Run it fully hosted at
[https://mitos.run](https://mitos.run) or self-hosted on your own Kubernetes
cluster.

This is the Ruby client for the direct sandbox API: create a template, fork a
sandbox, run `exec`, and terminate. It uses only the Ruby standard library
(`net/http`, `json`, `uri`, `securerandom`), so there are no gem dependencies. It
targets Ruby 2.6 and later.

## Install

```bash
gem install mitos
```

Or in a Gemfile:

```ruby
gem "mitos"
```

## Quickstart (hosted)

Get an API key from [https://mitos.run](https://mitos.run) and set it in the
environment. The base URL defaults to the hosted endpoint. The key is sent as
`Authorization: Bearer <key>` and is never logged.

```ruby
require "mitos"

ENV["MITOS_API_KEY"] = "sk-..."           # or pass api_key: to Mitos.server

server = Mitos.server                      # base URL + API key from the env
server.create_template("python")           # build (or get) the template
sandbox = server.fork("python")            # fork a fresh, independent sandbox

result = sandbox.exec("echo hello")
puts result.exit_code                      # 0
puts result.stdout                         # "hello\n"

sandbox.terminate
```

`fork` is the snapshot-fork primitive: each call forks a warm template into a
fresh, independent sandbox, so parallel attempts start from the same state.

Point at a local standalone server by setting `MITOS_BASE_URL` or passing `url:`:

```ruby
server = Mitos.server(url: "http://localhost:8080")
```

## Surface

| Method | HTTP | Returns |
| --- | --- | --- |
| `Mitos.server(url:, api_key:)` | none | `SandboxServer` |
| `SandboxServer#create_template(id, init_wait_seconds:, idempotency_key:)` | `POST /v1/templates` | `Template` |
| `SandboxServer#list_templates` | `GET /v1/templates` | `Array<Template>` |
| `SandboxServer#fork(template, id:, idempotency_key:)` | `POST /v1/fork` | `Sandbox` |
| `SandboxServer#list_sandboxes` | `GET /v1/sandboxes` | `Array<ServerSandbox>` |
| `Sandbox#exec(command, timeout:)` | `POST /v1/exec` | `ExecResult` |
| `Sandbox#terminate` | `DELETE /v1/sandboxes/{id}` | `nil` |

Creating calls (`create_template`, `fork`) send a fresh `Idempotency-Key`, so a
retried call returns the resource the first call created instead of a duplicate.

Value objects:

- `Template`: `id`, `ready` (`ready?`), `created_at`, `creation_time_ms`.
- `ServerSandbox`: `id`, `template_id`, `endpoint`, `created_at`, `fork_time_ms`.
- `ExecResult`: `exit_code`, `stdout`, `stderr`, `exec_time_ms` (`success?`).
- `Sandbox`: `id`, `endpoint`.

`exec` requires a Ready sandbox: the sandbox-server routes exec through the guest
agent over vsock, so calling exec on a sandbox that is not yet up returns a typed
`not_found` error.

## Auth and base URL precedence

Resolution order, highest precedence first:

- API key: the `api_key:` argument, then `MITOS_API_KEY`, then the credential
  file written by `mitos auth login` (`~/.config/mitos/credentials.json`,
  honoring `MITOS_CONFIG_DIR`, the `token` field), then tokenless.
- Base URL: the `url:` argument, then `MITOS_BASE_URL`, then `https://mitos.run`
  (the trailing slash is trimmed).

A missing, unreadable, or non-JSON credential file is never an error: resolution
falls through to tokenless. The key is sent as `Authorization: Bearer <key>`; the
standalone server ignores it, the hosted endpoint verifies it. The key value is
never logged.

## Errors

Every non-2xx response raises `Mitos::MitosError`, which parses the server
envelope `{error:{code, message, cause, remediation}}`. Branch on `code`, never
on the message text.

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

## Sandbox ids

Sandbox ids must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` (the same allowlist
every Mitos SDK enforces). `fork` and `terminate` validate the id and raise a
typed `invalid_sandbox_id` error before sending any request.

## Tests

The tests spin up a WEBrick stub reproducing the sandbox-server wire shapes and
assert the SDK round trips them. They need `minitest` and `webrick`; on Ruby 3.0+
install `webrick` first (`gem install webrick`). The SDK itself has no runtime
dependencies.

```bash
cd sdk/ruby
gem install webrick   # only needed on Ruby 3.0+
ruby -Ilib -Itest test/sandbox_server_test.rb
# or, with Rake:
rake test
```

## Scope

This gem is direct-mode only today. Cluster mode (driving the Kubernetes CRDs)
ships in the Python and TypeScript SDKs and is planned for this gem too, for full
parity (tracked in #304). Beyond the create / fork / exec /
terminate surface above, the following direct-mode endpoints are not part of this
gem: the files API (`/v1/files/*`), interactive PTY (`/v1/pty`), `run_code`,
per-sandbox network posture, `set_timeout`, `pause` / `resume`, and
`get_host(port)` preview URLs.

## The Mitos SDK family

Mitos ships native clients in six languages. All of them share the same
direct-mode surface (create a template, fork, exec, terminate), so the API maps
1:1 across languages; cluster mode (driving the Kubernetes CRDs) ships in Python
and TypeScript today and is planned for the rest, for full parity (#296).

| Language | Install | Covers |
| --- | --- | --- |
| Python | `pip install mitos-run` | direct + cluster + async |
| TypeScript | `npm install @mitos/sdk` | direct + cluster |
| Ruby | `gem install mitos` | direct |
| Rust | `cargo add mitos` | direct |
| Go | `go get github.com/mitos-run/mitos/sdk/go` | direct |
| Java | build from source | direct |

Project home: [https://mitos.run](https://mitos.run). Source and all six SDKs:
[github.com/mitos-run/mitos](https://github.com/mitos-run/mitos).

## License

Apache-2.0.
</content>
