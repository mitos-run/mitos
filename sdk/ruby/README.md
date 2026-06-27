# Mitos Ruby SDK

Mitos gives AI agents isolated, forkable sandboxes: Firecracker microVMs that
restore from snapshots and fork into parallel attempts, so an agent can branch a
warm environment instead of rebuilding it. Run it fully hosted at
[https://mitos.run](https://mitos.run) or self-hosted on your own Kubernetes
cluster.

This is the Ruby client for both modes mitos ships:

- **Direct mode** (`Mitos.server`): the standalone or hosted sandbox-server REST
  API. Create a template, fork a sandbox, run `exec`, and terminate.
- **Cluster mode** (`Mitos::AgentRun`): drive the Kubernetes `mitos.run` CRDs
  (`SandboxPool`, `Sandbox`, `Workspace`) directly over the Kubernetes REST API.

Both modes use only the Ruby standard library, so there are no gem dependencies.
Direct mode uses `net/http`, `json`, `uri`, and `securerandom`; cluster mode adds
`openssl` (TLS and the cluster CA), `yaml` (kubeconfig parsing), and `base64`
(in-cluster and Secret token decoding). It targets Ruby 2.6 and later.

## Install

The gem is not yet on RubyGems; until then, vendor it from source (clone the
repo and point your Gemfile at `sdk/ruby`). Once published:

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
| `Sandbox#exec(command, timeout:)` | `POST /sandbox.v1.Sandbox/ExecStream` (Connect) | `ExecResult` |
| `Sandbox#terminate` | `DELETE /v1/sandboxes/{id}` | `nil` |

Creating calls (`create_template`, `fork`) send a fresh `Idempotency-Key`, so a
retried call returns the resource the first call created instead of a duplicate.

Value objects:

- `Template`: `id`, `ready` (`ready?`), `created_at`, `creation_time_ms`.
- `ServerSandbox`: `id`, `template_id`, `endpoint`, `created_at`, `fork_time_ms`.
- `ExecResult`: `exit_code`, `stdout`, `stderr`, `exec_time_ms` (`success?`).
- `Sandbox`: `id`, `endpoint`.

`exec` runs over the Connect `sandbox.v1.Sandbox` runtime protocol (the
`ExecStream` RPC): the server streams stdout and stderr frames followed by an
exit frame, which the SDK drains into the `ExecResult`. It requires a Ready
sandbox: the sandbox-server routes exec through the guest agent over vsock, so
calling exec on a sandbox that is not yet up returns a typed `not_found` error.

## Cluster mode (Kubernetes)

When mitos is installed on your own Kubernetes cluster (the controller, forkd,
and the `mitos.run` CRDs), `Mitos::AgentRun` drives the CRDs directly. It speaks
the Kubernetes REST API itself, with no Kubernetes client gem: configuration
comes from a kubeconfig (the `kubeconfig:` path, else `KUBECONFIG`, else
`~/.kube/config`) or, in a pod, from the service-account mount
(`in_cluster: true`). It is the Ruby port of the Python `AgentRun`.

```ruby
require "mitos"

run = Mitos.cluster(namespace: "agents")          # or in_cluster: true in a pod

# One-liner: lazily get-or-create the default pool mitos-default-python-3.12,
# then start a Sandbox from it and block until it is Ready.
sb = run.sandbox(image: "python:3.12", ready: true)
puts sb.endpoint

# Or start from an existing pool with env, secrets, a TTL, and a workspace.
sb = run.create(
  pool: "my-pool",
  env: { "LOG_LEVEL" => "debug" },
  secrets: { "OPENAI_API_KEY" => %w[my-secret api-key] },  # env var => [secret, key]
  ttl: "30m",
  workspace: "ws-1"
)

run.list(pool: "my-pool")                          # reconnect handles
again = run.from_name(sb.name)                     # durable reconnect by name
run.pool_status("my-pool").ready_snapshots         # warm capacity
sb.terminate                                       # returns the bound workspace, if any
```

The default pool name is derived deterministically from the image and matches the
Python and TypeScript SDKs byte for byte (lowercased; `/` and `:` and other
unsafe characters become `-`; bounded and trimmed; prefixed `mitos-default-`), so
the same image maps to the same default pool across every SDK. A reused default
pool is checked against the requested image, so a slug collision serving a
different image raises `pool_image_mismatch` rather than silently running the
wrong image.

| Method | Resource | Returns |
| --- | --- | --- |
| `Mitos.cluster(namespace:, kubeconfig:, in_cluster:)` | none | `AgentRun` |
| `AgentRun#sandbox(image:/pool:, env:, secrets:, ttl:, workspace:, ready:)` | get-or-create pool + create Sandbox | `ClusterSandbox` |
| `AgentRun#create(pool:, name:, env:, secrets:, ttl:, workspace:)` | create Sandbox | `ClusterSandbox` |
| `AgentRun#get(name)` / `#from_name(name)` | read Sandbox | `ClusterSandbox` |
| `AgentRun#list(pool:)` | list Sandboxes | `Array<ClusterSandbox>` |
| `AgentRun#create_workspace(name)` / `#workspace(name)` / `#get_workspace(name)` / `#list_workspaces` | Workspaces | `Workspace` |
| `AgentRun#pool_status(name)` | read SandboxPool status | `PoolStatus` |
| `ClusterSandbox#wait_until_ready(timeout:)` | poll Sandbox status | `self` |
| `ClusterSandbox#info` | read Sandbox status | `SandboxInfo` |
| `ClusterSandbox#terminate` | delete Sandbox | workspace name or `nil` |

The per-sandbox bearer token is read from the `<name>-sandbox-token` Secret, held
in memory only, and never logged. Cluster-mode `exec` / `files` / `run_code` over
the sandbox HTTP API are served by the Python and TypeScript SDKs and are not yet
part of this gem (see Scope below).

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

The tests spin up WEBrick stubs reproducing the wire shapes and assert the SDK
round trips them: `test/sandbox_server_test.rb` stubs the sandbox-server REST API
(direct mode) and `test/cluster_test.rb` stubs the Kubernetes API server (cluster
mode). They need `minitest` and `webrick`; on Ruby 3.0+ install `webrick` first
(`gem install webrick`). The SDK itself has no runtime dependencies.

```bash
cd sdk/ruby
gem install webrick   # only needed on Ruby 3.0+
ruby -Ilib -Itest test/sandbox_server_test.rb
ruby -Ilib -Itest test/cluster_test.rb
# or, with Rake (runs every test/**/*_test.rb):
rake test
```

## Scope

This gem ships direct mode (create / fork / exec / terminate) and cluster mode
(the `mitos.run` CRD lifecycle: pools, sandboxes, and workspaces). The following
sandbox HTTP API surface is not yet part of this gem, in either mode: the files
API (`/v1/files/*`), interactive PTY (`/v1/pty`), `run_code`, per-sandbox network
posture, `set_timeout`, `pause` / `resume`, and `get_host(port)` preview URLs.
These are served by the Python and TypeScript SDKs and are planned here for full
parity.

## The Mitos SDK family

Mitos ships native clients in six languages. All of them share the same
direct-mode surface (create a template, fork, exec, terminate), so the API maps
1:1 across languages; cluster mode (driving the Kubernetes CRDs) ships in Python,
TypeScript, and Ruby today and is planned for the rest, for full parity.

| Language | Install | Covers |
| --- | --- | --- |
| Python | `pip install mitos-run` | direct + cluster + async |
| TypeScript | `npm install @mitos/sdk` | direct + cluster |
| Ruby | `gem install mitos` | direct + cluster |
| Rust | `cargo add mitos` | direct |
| Go | `go get github.com/mitos-run/mitos/sdk/go` | direct |
| Java | build from source | direct |

Project home: [https://mitos.run](https://mitos.run). Source and all six SDKs:
[github.com/mitos-run/mitos](https://github.com/mitos-run/mitos).

## License

Apache-2.0.
</content>
