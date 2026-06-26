import { describe, expect, it } from "vitest";

import { AgentRun, defaultPoolName } from "../src/client.js";
import { AgentRunError } from "../src/errors.js";
import type { CustomObject, CustomObjectList, K8sApi } from "../src/k8s.js";

// FakeK8s is a scriptable K8sApi so the cluster logic is tested without a live
// cluster. getSandbox returns a queued sequence of statuses; readSecret returns a
// configured map; createSandbox/deleteSandbox record their inputs.
// notFound builds an error carrying a 404 statusCode, matching how the real
// KubeConfigApi surfaces a missing object so the client can tell absent from a
// real failure.
function notFound(): Error {
  const e = new Error("not found") as Error & { statusCode: number };
  e.statusCode = 404;
  return e;
}

class FakeK8s implements K8sApi {
  createdClaims: CustomObject[] = [];
  deletedClaims: string[] = [];
  createdPools: CustomObject[] = [];
  getPoolCalls = 0;
  getCalls = 0;

  constructor(
    private opts: {
      getResponses: Array<Record<string, unknown>>;
      secret?: Record<string, string>;
      secretThrows?: boolean;
      listItems?: CustomObject[];
      poolExists?: boolean;
      // Image stored inline in the existing default pool's spec.template. When
      // set (with poolExists), getPool returns a pool with spec.template.image
      // so the reuse path can verify it.
      existingPoolImage?: string;
      // When set, getPool rejects with this status code instead of returning a pool.
      poolThrowsStatus?: number;
    },
  ) {}

  async getPool(_ns: string, name: string): Promise<CustomObject> {
    this.getPoolCalls += 1;
    if (this.opts.poolThrowsStatus !== undefined) {
      const e = new Error("pool error") as Error & { statusCode: number };
      e.statusCode = this.opts.poolThrowsStatus;
      throw e;
    }
    if (this.opts.poolExists) {
      return {
        metadata: { name },
        spec: { template: { image: this.opts.existingPoolImage }, replicas: 1 },
      };
    }
    throw notFound();
  }

  async createPool(_ns: string, pool: CustomObject): Promise<void> {
    this.createdPools.push(pool);
  }

  async createClaim(_ns: string, claim: CustomObject): Promise<CustomObject> {
    this.createdClaims.push(claim);
    return claim;
  }

  async getClaim(_ns: string, name: string): Promise<CustomObject> {
    const idx = Math.min(this.getCalls, this.opts.getResponses.length - 1);
    this.getCalls += 1;
    const status = this.opts.getResponses[idx] ?? {};
    return { metadata: { name }, status };
  }

  async deleteClaim(_ns: string, name: string): Promise<void> {
    this.deletedClaims.push(name);
  }

  async listClaims(_ns: string): Promise<CustomObjectList> {
    return { items: this.opts.listItems ?? [] };
  }

  async readSecret(_ns: string, _name: string): Promise<Record<string, string>> {
    if (this.opts.secretThrows) {
      throw new Error("secret not found");
    }
    return this.opts.secret ?? {};
  }

  async createWorkspace(_ns: string, workspace: CustomObject): Promise<CustomObject> {
    return workspace;
  }

  async getWorkspace(_ns: string, name: string): Promise<CustomObject> {
    return { metadata: { name } };
  }

  async listWorkspaces(_ns: string): Promise<CustomObjectList> {
    return { items: [] };
  }

  async deleteWorkspace(_ns: string, _name: string): Promise<void> {}

  async listRevisions(_ns: string): Promise<CustomObjectList> {
    return { items: [] };
  }

  async getRevision(_ns: string, name: string): Promise<CustomObject> {
    return { metadata: { name } };
  }

  async createRevision(_ns: string, revision: CustomObject): Promise<CustomObject> {
    return revision;
  }

  async patchClaim(_ns: string, _name: string, _patch: Record<string, unknown>): Promise<void> {}
}

const noSleep = async () => {};

describe("defaultPoolName", () => {
  it("slugifies an image deterministically", () => {
    expect(defaultPoolName("python")).toBe("mitos-default-python");
    expect(defaultPoolName("python:3.12-slim")).toBe("mitos-default-python-3.12-slim");
    expect(defaultPoolName("Python")).toBe("mitos-default-python"); // lowercased
  });

  it("strips a trailing '.' so the name stays a valid object name", () => {
    expect(defaultPoolName("python.")).toBe("mitos-default-python");
    expect(defaultPoolName("python-")).toBe("mitos-default-python");
  });

  it("bounds the slug to 40 chars after the prefix", () => {
    const long = defaultPoolName(
      "ghcr.io/mitos-run/agent-python-with-a-very-long-tag:3.12",
    );
    expect(long.startsWith("mitos-default-")).toBe(true);
    expect(long.slice("mitos-default-".length).length).toBeLessThanOrEqual(40);
  });
});

describe("AgentRun construction", () => {
  it("throws a clear error when no K8sApi is provided", () => {
    expect(() => new AgentRun()).toThrow(AgentRunError);
  });
});

describe("AgentRun.create", () => {
  it("builds the right sandbox spec, polls to Ready, reads the token, and binds the Sandbox", async () => {
    const fake = new FakeK8s({
      getResponses: [
        { phase: "Pending" },
        { phase: "Restoring" },
        { phase: "Ready", endpoint: "10.0.0.5:9091", sandboxID: "sbx-abc" },
      ],
      secret: { token: "tok-cluster-secret", endpoint: "10.0.0.5:9091" },
    });
    const run = new AgentRun({ k8s: fake, namespace: "team-a", sleep: noSleep });

    const sandbox = await run.create("python-pool", {
      name: "sbx-1",
      env: { FOO: "bar" },
      timeout: "30m",
    });

    // Sandbox spec is correct.
    expect(fake.createdClaims).toHaveLength(1);
    const claim = fake.createdClaims[0];
    expect(claim.apiVersion).toBe("mitos.run/v1");
    expect(claim.kind).toBe("Sandbox");
    expect(claim.metadata).toEqual({ name: "sbx-1", namespace: "team-a" });
    expect(claim.spec).toEqual({
      source: { poolRef: { name: "python-pool" } },
      env: [{ name: "FOO", value: "bar" }],
      lifetime: { ttl: "30m" },
    });

    // Polled until Ready.
    expect(fake.getCalls).toBe(3);

    // Sandbox carries the endpoint and token.
    expect(sandbox.id).toBe("sbx-1");
    expect(sandbox.endpoint).toBe("http://10.0.0.5:9091");
  });

  it("fork(n) creates a fork Sandbox with replicas=n and returns n Ready children (#311)", async () => {
    const fake = new FakeK8s({
      getResponses: [
        // create() waitReady for the source sandbox.
        { phase: "Ready", endpoint: "10.0.0.5:9091", sandboxID: "sbx-src" },
        // fork() poll: the fork object's status.children, all Ready.
        {
          children: [
            { name: "sbx-1-fork-aa-0", phase: "Ready", endpoint: "10.0.0.6:9091", sandboxID: "c0" },
            { name: "sbx-1-fork-aa-1", phase: "Ready", endpoint: "10.0.0.7:9091", sandboxID: "c1" },
          ],
        },
      ],
      secret: { token: "tok", endpoint: "10.0.0.5:9091" },
    });
    const run = new AgentRun({ k8s: fake, namespace: "team-a", sleep: noSleep });
    const sb = await run.create("python-pool", { name: "sbx-1" });

    const children = await sb.fork(2);

    // Returns N live children.
    expect(children).toHaveLength(2);
    expect(children.map((c) => c.endpoint).sort()).toEqual([
      "http://10.0.0.6:9091",
      "http://10.0.0.7:9091",
    ]);

    // The fork object is a Sandbox with replicas=2 and source.fromSandbox.
    const forkClaim = fake.createdClaims[1];
    expect(forkClaim.kind).toBe("Sandbox");
    const spec = forkClaim.spec as Record<string, unknown>;
    expect(spec["replicas"]).toBe(2);
    expect(spec["source"]).toEqual({ fromSandbox: { name: "sbx-1", pauseSource: false } });

    // Children can fork again (the forker is injected into each child).
    expect(typeof children[0].fork).toBe("function");
  });

  it("fork(n) raises an LLM-legible error when children are not ready in time (#311)", async () => {
    const fake = new FakeK8s({
      getResponses: [
        { phase: "Ready", endpoint: "10.0.0.5:9091" },
        { children: [] }, // never enough Ready children
      ],
      secret: { token: "tok" },
    });
    const run = new AgentRun({ k8s: fake, pollTimeoutMs: 5, sleep: noSleep });
    const sb = await run.create("p", { name: "sbx-1" });
    await expect(sb.fork(2)).rejects.toMatchObject({ code: "ready_timeout" });
  });

  it("fork(n) surfaces a rejected fork as an LLM-legible error, not a timeout (#311)", async () => {
    const fake = new FakeK8s({
      getResponses: [
        { phase: "Ready", endpoint: "10.0.0.5:9091" },
        {
          conditions: [
            {
              type: "Rejected",
              status: "True",
              reason: "SecretInheritanceDenied",
              message:
                "source sandbox holds secrets; recreate the fork with spec.secretInheritance=inherit to permit it",
            },
          ],
        },
      ],
      secret: { token: "tok" },
    });
    const run = new AgentRun({ k8s: fake, pollTimeoutMs: 50, sleep: noSleep });
    const sb = await run.create("p", { name: "sbx-1" });
    await expect(sb.fork(2)).rejects.toMatchObject({
      code: "fork_rejected",
      errorCause: "SecretInheritanceDenied",
    });
  });

  it("times out with a clear error when the sandbox never becomes Ready", async () => {
    const fake = new FakeK8s({ getResponses: [{ phase: "Pending" }] });
    const run = new AgentRun({ k8s: fake, pollTimeoutMs: 5, sleep: noSleep });

    let caught: AgentRunError | undefined;
    try {
      await run.create("python-pool");
    } catch (e) {
      caught = e as AgentRunError;
    }
    expect(caught).toBeInstanceOf(AgentRunError);
    expect(caught!.code).toBe("ready_timeout");
    expect(caught!.message).toContain("not ready");
  });

  it("never surfaces the token in a thrown error", async () => {
    const token = "ultra-secret-bearer";
    // Sandbox goes Ready, secret is read, but a later exec fails with the token
    // echoed back. The token must not appear in the thrown error.
    const fake = new FakeK8s({
      getResponses: [{ phase: "Ready", endpoint: "127.0.0.1:1/will-refuse" }],
      secret: { token },
    });
    const run = new AgentRun({ k8s: fake, sleep: noSleep });
    const sandbox = await run.create("p");

    let caught: unknown;
    try {
      // Endpoint is unroutable, so exec rejects; assert the token is absent.
      await sandbox.exec("noop");
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeTruthy();
    expect(JSON.stringify(caught)).not.toContain(token);
    expect(String((caught as Error).message)).not.toContain(token);
  });

  it("tolerates a missing token Secret (stays tokenless)", async () => {
    const fake = new FakeK8s({
      getResponses: [{ phase: "Ready", endpoint: "10.0.0.9:9091" }],
      secretThrows: true,
    });
    const run = new AgentRun({ k8s: fake, sleep: noSleep });
    const sandbox = await run.create("p");
    expect(sandbox.endpoint).toBe("http://10.0.0.9:9091");
  });

  it("fails fast when the sandbox reaches Failed", async () => {
    const fake = new FakeK8s({ getResponses: [{ phase: "Failed" }] });
    const run = new AgentRun({ k8s: fake, sleep: noSleep });
    await expect(run.create("p")).rejects.toMatchObject({ code: "sandbox_failed" });
  });

  it("terminate deletes the sandbox", async () => {
    const fake = new FakeK8s({
      getResponses: [{ phase: "Ready", endpoint: "10.0.0.9:9091" }],
      secret: { token: "t" },
    });
    const run = new AgentRun({ k8s: fake, sleep: noSleep });
    const sandbox = await run.create("p", { name: "sbx-del" });
    await sandbox.terminate();
    expect(fake.deletedClaims).toEqual(["sbx-del"]);
  });
});

describe("AgentRun.list", () => {
  it("maps sandboxes to SandboxInfo and filters by pool", async () => {
    const items: CustomObject[] = [
      {
        metadata: { name: "a" },
        spec: { source: { poolRef: { name: "p1" } } },
        status: {
          phase: "Ready",
          endpoint: "10.0.0.1:9091",
          node: "n1",
          sandboxID: "sbx-a",
          startupLatencyMs: 2.5,
        },
      },
      {
        metadata: { name: "b" },
        spec: { source: { poolRef: { name: "p2" } } },
        status: { phase: "Pending" },
      },
    ];
    const fake = new FakeK8s({ getResponses: [], listItems: items });
    const run = new AgentRun({ k8s: fake, sleep: noSleep });

    const all = await run.list();
    expect(all).toHaveLength(2);
    expect(all[0]).toEqual({
      name: "a",
      phase: "Ready",
      endpoint: "10.0.0.1:9091",
      node: "n1",
      sandboxId: "sbx-a",
      forkTimeMs: 2.5,
      pool: "p1",
    });
    expect(all[1].phase).toBe("Pending");
    expect(all[1].pool).toBe("p2");

    const filtered = await run.list("p1");
    expect(filtered.map((x) => x.name)).toEqual(["a"]);
  });
});

describe("AgentRun.sandbox(image) lazy default pool", () => {
  const ready = [{ phase: "Ready", endpoint: "10.0.0.5:9091", sandboxID: "sbx" }];

  it("creates mitos-default-<image> with inline spec.template when the pool is absent, then claims from it", async () => {
    const fake = new FakeK8s({ getResponses: ready, poolExists: false });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    const sb = await c.sandbox("python");
    // v1: one SandboxPool with inline spec.template; no separate SandboxTemplate.
    expect(fake.createdPools).toHaveLength(1);
    expect(fake.createdPools[0].metadata?.name).toBe("mitos-default-python");
    expect(fake.createdPools[0].spec).toEqual({
      template: { image: "python" },
      replicas: 1,
    });
    expect(fake.createdClaims[0].spec).toEqual({
      source: { poolRef: { name: "mitos-default-python" } },
    });
    expect(sb.id).toMatch(/^sandbox-/);
  });

  it("reuses an existing default pool (no create)", async () => {
    const fake = new FakeK8s({
      getResponses: ready,
      poolExists: true,
      existingPoolImage: "python",
    });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    await c.sandbox("python");
    expect(fake.createdPools).toHaveLength(0);
  });

  it("raises pool_image_mismatch when a colliding slug reuses a pool for a different image", async () => {
    // image A ("python-3.11") created the default pool; calling with image B
    // ("python:3.11") collides to the same slug mitos-default-python-3.11.
    expect(defaultPoolName("python:3.11")).toBe(defaultPoolName("python-3.11"));
    const fake = new FakeK8s({
      getResponses: ready,
      poolExists: true,
      existingPoolImage: "python-3.11",
    });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    await expect(c.sandbox("python:3.11")).rejects.toMatchObject({
      code: "pool_image_mismatch",
    });
    expect(fake.createdClaims).toHaveLength(0); // no sandbox was created
  });

  it("fails closed when the reused pool has no readable inline image", async () => {
    const fake = new FakeK8s({
      getResponses: ready,
      poolExists: true,
      // existingPoolImage is undefined: pool returns spec.template.image = undefined
    });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    await expect(c.sandbox("python")).rejects.toMatchObject({
      code: "pool_image_mismatch",
    });
  });

  it("explicit pool never creates a pool", async () => {
    const fake = new FakeK8s({ getResponses: ready, poolExists: false });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    await c.sandbox("python", { pool: "my-pool" });
    expect(fake.getPoolCalls).toBe(0);
    expect(fake.createdPools).toHaveLength(0);
    expect(fake.createdClaims[0].spec).toEqual({
      source: { poolRef: { name: "my-pool" } },
    });
  });

  it("fromName reconnects a Ready sandbox", async () => {
    const fake = new FakeK8s({
      getResponses: [{ phase: "Ready", endpoint: "10.0.0.9:8443", sandboxID: "sbx" }],
      secret: { token: "tok" },
    });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    const sb = await c.fromName("agent-session-1");
    expect(sb.id).toBe("agent-session-1");
    expect(sb.endpoint).toContain("10.0.0.9:8443");
  });

  it("opt-out raises without a pool", async () => {
    const fake = new FakeK8s({ getResponses: ready, poolExists: false });
    const c = new AgentRun({ k8s: fake, allowDefaultPool: false, sleep: noSleep });
    await expect(c.sandbox("python")).rejects.toMatchObject({ code: "no_default_pool" });
  });

  it("requires an image or a pool", async () => {
    const fake = new FakeK8s({ getResponses: ready });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    await expect(c.sandbox()).rejects.toMatchObject({ code: "missing_image_or_pool" });
  });
});
