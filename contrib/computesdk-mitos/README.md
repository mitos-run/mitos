# @computesdk/mitos

A [ComputeSDK](https://www.computesdk.com) provider for [Mitos](https://mitos.run).

Mitos runs snapshot-fork Firecracker microVM sandboxes. A warm pool is kept
ready and every create forks a fresh, independent VM from a copy-on-write
snapshot, so a sandbox is ready in well under a second instead of booting from
cold. This provider maps the ComputeSDK sandbox surface onto the hosted Mitos
control plane at `https://api.mitos.run`, driving it through the official Mitos
TypeScript SDK ([`@mitos/sdk`](https://github.com/mitos-run/mitos/tree/main/sdk/typescript)).

## Status: staged for submission

This package lives in the Mitos repository under `contrib/computesdk-mitos`. It
is a ready-to-submit ComputeSDK provider, staged here because two dependencies
must be resolved before it can be published into the ComputeSDK monorepo. See
[Submission runbook](#submission-runbook) for the exact remaining steps.

## Features

- Sub-second warm forks from a snapshot pool, not cold boots.
- `create`, `runCommand`, `getById`, `list`, and `destroy` over the hosted API.
- Filesystem operations (`readFile`, `writeFile`, `mkdir`, `readdir`, `exists`,
  `remove`) over the native Mitos file RPCs and shell.
- Python and shell out of the box on the default `python` template, which also
  carries the code-interpreter kernel.

## Installation

```bash
npm install computesdk @computesdk/mitos
```

## Configuration

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `apiKey` | `string` | `MITOS_API_KEY` env | Bearer token for the hosted API. |
| `baseUrl` | `string` | `MITOS_BASE_URL` env, else `https://api.mitos.run` | Control plane URL. Point at a self-hosted control plane or a local standalone sandbox-server to run off the hosted service. |
| `template` | `string` | `"python"` | Template to fork from when a create call names none. |
| `timeout` | `number` | none | Per-command execution timeout in milliseconds. |

Get an API key from [https://mitos.run](https://mitos.run).

## Usage

```typescript
import { compute } from "computesdk";
import { mitos } from "@computesdk/mitos";

compute.setConfig({
  provider: mitos({ apiKey: process.env.MITOS_API_KEY }),
});

const sandbox = await compute.sandbox.create();

const result = await sandbox.runCommand("python3 -c \"print(1 + 1)\"");
console.log(result.stdout.trim()); // 2

await sandbox.filesystem.writeFile("/workspace/hello.txt", "hello\n");
console.log((await sandbox.filesystem.readFile("/workspace/hello.txt")).trim()); // hello

await sandbox.destroy();
```

### What each method maps to

| ComputeSDK method | Mitos SDK call |
| --- | --- |
| `sandbox.create(options?)` | `SandboxServer.fork(template)` |
| `sandbox.runCommand(cmd, opts?)` | `Sandbox.exec(cmd, opts)` over the Connect ExecStream RPC |
| `filesystem.readFile` / `writeFile` / `readdir` | `Sandbox.files.read` / `write` / `list` |
| `filesystem.mkdir` / `exists` / `remove` | shell via `runCommand` |
| `sandbox.destroy(id)` | `DELETE /v1/sandboxes/{id}` |
| `provider.sandbox.getById(id)` / `list()` | `GET /v1/sandboxes` |

`getUrl` throws: per-port public URLs are a separate Mitos feature (named
`<label>.mitos.run` URLs via `mitos workspace serve`), not part of the direct
sandbox surface this provider drives.

## Concurrent burst caveat

The ComputeSDK benchmark harness includes a concurrent burst run alongside the
sequential time-to-interactive run. The hosted Mitos production pool currently
runs on a single KVM node, so the burst row will reflect single-node warm-pool
capacity, not multi-node scale-out. This is honest and expected: the
sequential time-to-interactive number is a warm-pool checkout, while the burst
number is bounded by how many forks one node can serve at once. Multi-node
capacity is tracked in the Mitos repository and is not claimed here.

## Development

```bash
npm install   # resolves @mitos/sdk via the file: reference below
npm run typecheck
npm test
npm run build
```

The `@mitos/sdk` dependency is a `file:../../sdk/typescript` reference into this
repository, so its `dist/` must be built once first:

```bash
cd ../../sdk/typescript && npm ci && npm run build
```

The tests stub `fetch` to exercise the create and destroy REST wire shapes
without a live server. A full create, run, and destroy round trip against
`https://api.mitos.run` needs a real `MITOS_API_KEY` and is the live smoke test
described below.

### Live smoke test

With a real key set:

```bash
export MITOS_API_KEY=...   # never commit or print this
node --input-type=module -e '
  import { compute } from "computesdk";
  import { mitos } from "@computesdk/mitos";
  compute.setConfig({ provider: mitos({ apiKey: process.env.MITOS_API_KEY }) });
  const sb = await compute.sandbox.create();
  const r = await sb.runCommand("python3 -c \"print(21*2)\"");
  console.log(r.exitCode, r.stdout.trim());
  await sb.destroy();
'
```

## Submission runbook

Being listed on the ComputeSDK leaderboard does not require sponsorship; the
ComputeSDK contribution guide states any provider can be added. The remaining
steps are user-gated because they need publish rights and credentials this
repository does not hold:

1. **Publish `@mitos/sdk` to npm.** ComputeSDK packages cannot depend on a
   `file:` path. Publish the Mitos TypeScript SDK
   (`sdk/typescript`, package name `@mitos/sdk`) to the public registry, then in
   this package's `package.json` swap the dependency from
   `"@mitos/sdk": "file:../../sdk/typescript"` to the published version range
   (for example `"@mitos/sdk": "^0.1.0"`).
2. **Copy the package into the ComputeSDK monorepo.** Copy
   `contrib/computesdk-mitos` to `packages/mitos` in a clone of
   [`computesdk/computesdk`](https://github.com/computesdk/computesdk). Change the
   two workspace-managed dependencies to the workspace protocol the monorepo
   uses: `"@computesdk/provider": "workspace:*"` and `"computesdk": "workspace:*"`.
   Add a `vitest.config.ts` and, if desired, wire the
   `@computesdk/test-utils` suite as shown in the monorepo's `ADD-PROVIDER.md`.
3. **Open the pull request.** Run `pnpm install` and `pnpm --filter @computesdk/mitos typecheck build test` at the monorepo root, then open a PR against
   `computesdk/computesdk` following its `ADD-PROVIDER.md` and contribution guide.
4. **Provide benchmark credentials.** Give the ComputeSDK maintainers a
   `MITOS_API_KEY` for the daily run so the provider can create, run, and destroy
   sandboxes during the benchmark. Scope the key to the benchmark and rotate it
   on the usual cadence.

Once those four steps land, Mitos appears in the daily ComputeSDK
time-to-interactive leaderboard.

## License

MIT, matching the ComputeSDK provider packages.
