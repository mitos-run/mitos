/**
 * Hosted quickstart: the snippet shown in the TypeScript SDK README.
 *
 * This file is the checked copy of the hosted "Quickstart" hero so the README
 * code cannot drift from the real SDK surface. It type-checks under
 * `npm run check:examples` (tsconfig.examples.json compiles examples against the
 * built dist types), so a renamed or removed method fails the build.
 *
 * It is NOT executed in CI: the hosted default (new SandboxServer() with no URL)
 * talks to https://mitos.run with a real MITOS_API_KEY, which CI does not carry.
 * Wire shapes and error semantics are conformance-tested against a mock server
 * in test/. Point at a local server with new SandboxServer("http://...") to run.
 */

import { SandboxServer } from "@mitos/sdk";

async function main(): Promise<void> {
  // MITOS_API_KEY from the environment; base URL defaults to https://mitos.run.
  const server = new SandboxServer();

  // Fork a sandbox from a template (a fresh, independent VM from a warm snapshot).
  const sandbox = await server.fork("python");

  const result = await sandbox.exec("python3 -c 'print(1 + 1)'");
  console.log(result.exitCode, result.stdout.trim()); // 0  2

  await sandbox.files.write("/workspace/hello.txt", "hello\n");
  console.log((await sandbox.files.read("/workspace/hello.txt")).trim()); // hello

  await sandbox.terminate();
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
