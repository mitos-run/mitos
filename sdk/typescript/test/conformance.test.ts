// SDK conformance parity suite (issue #22): TypeScript runner.
//
// This runs the SHARED scenario in sdk/conformance/scenario.json against a live
// standalone sandbox-server in mock mode and asserts that each step's NORMALIZED
// result equals the shared expectation. The Python runner
// (sdk/python/tests/test_conformance.py) runs the IDENTICAL scenario against the
// SAME server and asserts the IDENTICAL normalized expectations, so the two
// languages prove byte-equal logical behavior for the shared control plane.
//
// Scope: the mock engine has NO guest VM, so exec / files / run_code are out of
// scope here (they need a vsock guest agent, proven on the KVM CI job). The
// conformance SURFACE is the control plane both SDKs share: create template,
// list templates, fork, list sandboxes, terminate.
//
// Gating (matches the existing integration style: target a live server, skip
// when absent): this suite SKIPS unless a reachable server URL is provided, so
// the unit-only `npm test` is unaffected. The URL comes from
// MITOS_CONFORMANCE_URL, else the default http://localhost:8080; if nothing
// answers /v1/health, the suite skips.

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { beforeAll, describe, expect, it } from "vitest";

import { SandboxServer } from "../src/server.js";

// Resolve the shared scenario, defined ONCE for both languages. From
// sdk/typescript/test/ up to sdk/, then into conformance/.
const here = dirname(fileURLToPath(import.meta.url));
const scenarioPath = resolve(here, "..", "..", "conformance", "scenario.json");

interface Step {
  name: string;
  op: string;
  args?: Record<string, unknown>;
  expect?: Record<string, unknown>;
  expect_contains?: Record<string, unknown>;
  expect_absent_from_sandboxes?: string;
}

interface Scenario {
  ids: { template: string; sandbox: string };
  steps: Step[];
  normalization: {
    template_keys: string[];
    sandbox_list_keys: string[];
  };
}

const scenario: Scenario = JSON.parse(readFileSync(scenarioPath, "utf8"));

const DEFAULT_URL = "http://localhost:8080";

async function resolveUrl(): Promise<string | null> {
  // Opt-in: an explicit MITOS_CONFORMANCE_URL is used as-is. Otherwise probe
  // the default localhost:8080 and use it only if it answers /v1/health, so a
  // developer's unit-only run (no server) skips cleanly.
  const explicit = process.env.MITOS_CONFORMANCE_URL;
  if (explicit) {
    return explicit.replace(/\/+$/, "");
  }
  try {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 500);
    const r = await fetch(`${DEFAULT_URL}/v1/health`, { signal: controller.signal });
    clearTimeout(timer);
    if (r.ok) {
      return DEFAULT_URL;
    }
  } catch {
    return null;
  }
  return null;
}

// Vitest evaluates describe synchronously, so resolve the URL up front. The
// top-level await is fine under vitest's ESM loader.
const serverUrl = await resolveUrl();

function step(name: string): Step {
  const s = scenario.steps.find((x) => x.name === name);
  if (!s) {
    throw new Error(`scenario step not found: ${name}`);
  }
  return s;
}

// Normalize an SDK Template (camelCase) to the scenario's stable snake_case
// keys. Only id + ready are compared; timing fields are dropped.
function normTemplate(t: { id: string; ready: boolean }): Record<string, unknown> {
  return { id: t.id, ready: t.ready };
}

// Normalize a listSandboxes entry (camelCase templateId) to the stable keys
// (id, template_id). Timing / endpoint / createdAt are dropped.
function normSandboxListEntry(s: {
  id: string;
  templateId: string;
}): Record<string, unknown> {
  return { id: s.id, template_id: s.templateId };
}

describe.skipIf(serverUrl === null)("SDK conformance parity (issue #22)", () => {
  let server: SandboxServer;
  const templateId = scenario.ids.template;
  const sandboxId = scenario.ids.sandbox;

  beforeAll(async () => {
    server = new SandboxServer(serverUrl as string);
    // Clean slate: a prior run may have left the sandbox behind.
    const existing = await server.listSandboxes();
    for (const s of existing) {
      if (s.id === sandboxId) {
        await fetch(`${serverUrl}/v1/sandboxes/${sandboxId}`, { method: "DELETE" });
      }
    }
  });

  it("runs the shared scenario and matches the shared expectations", async () => {
    // Step 1: createTemplate(id) -> {id, ready}.
    const s1 = step("create_template");
    const created = await server.createTemplate(templateId, {
      initWaitSeconds: (s1.args?.init_wait_seconds as number) ?? 1,
    });
    expect(normTemplate(created)).toEqual(s1.expect);

    // Step 2: listTemplates() contains the template.
    const s2 = step("list_templates_contains");
    const templates = (await server.listTemplates()).map(normTemplate);
    expect(templates).toContainEqual(s2.expect_contains);

    // Step 3: fork(template, id) -> {id, endpoint present}.
    const s3 = step("fork");
    const sandbox = await server.fork(
      s3.args?.template as string,
      s3.args?.id as string,
    );
    expect({ id: sandbox.id, endpoint_present: Boolean(sandbox.endpoint) }).toEqual(
      s3.expect,
    );

    // Step 4: listSandboxes() contains the sandbox with {id, template_id}.
    const s4 = step("list_sandboxes_contains");
    const sandboxes = (await server.listSandboxes()).map(normSandboxListEntry);
    expect(sandboxes).toContainEqual(s4.expect_contains);

    // Step 5: terminate() -> the sandbox is gone from listSandboxes().
    const s5 = step("terminate");
    await sandbox.terminate();
    const remainingIds = (await server.listSandboxes()).map((x) => x.id);
    expect(remainingIds).not.toContain(s5.expect_absent_from_sandboxes);
  });
});
