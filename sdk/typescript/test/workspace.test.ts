import { describe, it, expect, afterEach, beforeEach } from "vitest";

import { AgentRun } from "../src/client.js";
import { AgentRunError } from "../src/errors.js";
import { Workspace } from "../src/workspace.js";
import type { CustomObject, CustomObjectList, K8sApi } from "../src/k8s.js";

const noSleep = async () => {};

// FakeK8s implements the workspace-relevant slice of K8sApi. The sandbox verbs
// throw if hit (unused by these tests).
class FakeK8s implements K8sApi {
  createdWorkspaces: CustomObject[] = [];
  createdRevisions: CustomObject[] = [];
  patchedClaims: Array<{ name: string; patch: Record<string, unknown> }> = [];
  deletedClaims: string[] = [];

  constructor(
    private opts: {
      revisions?: CustomObject[];
      revision?: CustomObject;
      claim?: CustomObject;
    } = {},
  ) {}

  async createWorkspace(_ns: string, ws: CustomObject): Promise<CustomObject> {
    this.createdWorkspaces.push(ws);
    return ws;
  }
  async getWorkspace(_ns: string, name: string): Promise<CustomObject> {
    return { metadata: { name }, status: {} };
  }
  async listWorkspaces(_ns: string): Promise<CustomObjectList> {
    return { items: this.createdWorkspaces };
  }
  async deleteWorkspace(_ns: string, _name: string): Promise<void> {}
  async listRevisions(_ns: string): Promise<CustomObjectList> {
    return { items: this.opts.revisions ?? [] };
  }
  async getRevision(_ns: string, name: string): Promise<CustomObject> {
    return this.opts.revision ?? { metadata: { name } };
  }
  async createRevision(_ns: string, rev: CustomObject): Promise<CustomObject> {
    this.createdRevisions.push(rev);
    return { ...rev, metadata: { ...(rev.metadata ?? {}), name: "branch-generated" } };
  }
  async patchClaim(_ns: string, name: string, patch: Record<string, unknown>): Promise<void> {
    this.patchedClaims.push({ name, patch });
  }

  createdClaims: CustomObject[] = [];

  // Sandbox slice used by serve() tests; stub for non-serve tests.
  async createClaim(_ns: string, claim: CustomObject): Promise<CustomObject> {
    this.createdClaims.push(claim);
    return claim;
  }
  async getClaim(_ns: string, name: string): Promise<CustomObject> {
    return this.opts.claim ?? { metadata: { name }, status: { phase: "Ready" } };
  }
  async deleteClaim(_ns: string, name: string): Promise<void> {
    this.deletedClaims.push(name);
  }
  async listClaims(): Promise<CustomObjectList> {
    return { items: [] };
  }
  async getPool(): Promise<CustomObject> {
    throw new Error("unused");
  }
  async createPool(): Promise<void> {}
  async createTemplate(): Promise<void> {}
  async getTemplate(): Promise<CustomObject> {
    throw new Error("unused");
  }
  async readSecret(): Promise<Record<string, string>> {
    return {};
  }
}

describe("workspace", () => {
  it("create posts a Workspace CRD", async () => {
    const fake = new FakeK8s();
    const c = new AgentRun({ k8s: fake, namespace: "ns", sleep: noSleep });
    const ws = await c.createWorkspace("proj-x");
    expect(ws.name).toBe("proj-x");
    expect(fake.createdWorkspaces).toHaveLength(1);
    expect(fake.createdWorkspaces[0].kind).toBe("Workspace");
    expect(fake.createdWorkspaces[0].metadata?.name).toBe("proj-x");
  });

  it("log returns revisions newest first", async () => {
    const fake = new FakeK8s({
      revisions: [
        {
          metadata: { name: "proj-x-1", creationTimestamp: "2026-06-01T00:00:00Z" },
          spec: { workspaceRef: { name: "proj-x" }, source: { fromClaim: "c1" } },
          status: { phase: "Committed" },
        },
        {
          metadata: { name: "proj-x-2", creationTimestamp: "2026-06-02T00:00:00Z" },
          spec: { workspaceRef: { name: "proj-x" }, source: { fromClaim: "c2" } },
          status: { phase: "Committed" },
        },
      ],
    });
    const c = new AgentRun({ k8s: fake, namespace: "ns", sleep: noSleep });
    const revs = await c.workspace("proj-x").log();
    expect(revs.map((r) => r.name)).toEqual(["proj-x-2", "proj-x-1"]);
    expect(revs[0].lineage).toBe("fromClaim:c2");
  });

  it("fork of an uncommitted revision throws an LLM-legible error", async () => {
    const fake = new FakeK8s({
      revision: {
        metadata: { name: "proj-x-1" },
        spec: { workspaceRef: { name: "proj-x" } },
        status: { phase: "Pending" },
      },
    });
    const c = new AgentRun({ k8s: fake, namespace: "ns", sleep: noSleep });
    const ws = c.workspace("proj-x");
    await expect(ws.fork("proj-x-1", "branch")).rejects.toMatchObject({
      code: "revision_not_committed",
    });
  });

  it("fork of a committed revision creates a fromWorkspaceRevision edge", async () => {
    const fake = new FakeK8s({
      revision: {
        metadata: { name: "proj-x-1" },
        spec: { workspaceRef: { name: "proj-x" }, contentManifest: "deadbeef" },
        status: { phase: "Committed" },
      },
    });
    const c = new AgentRun({ k8s: fake, namespace: "ns", sleep: noSleep });
    const newRev = await c.workspace("proj-x").fork("proj-x-1", "branch");
    expect(newRev).toBe("branch-generated");
    const created = fake.createdRevisions[0];
    expect(created.spec?.["source"]).toEqual({
      fromWorkspaceRevision: { workspace: "proj-x", revision: "proj-x-1" },
    });
    expect(created.spec?.["contentManifest"]).toBe("deadbeef");
  });
});

describe("workspace.serve()", () => {
  // Save and restore MITOS_EXPOSE_DOMAIN around each test.
  let savedDomain: string | undefined;
  beforeEach(() => {
    savedDomain = process.env["MITOS_EXPOSE_DOMAIN"];
    delete process.env["MITOS_EXPOSE_DOMAIN"];
  });
  afterEach(() => {
    if (savedDomain !== undefined) {
      process.env["MITOS_EXPOSE_DOMAIN"] = savedDomain;
    } else {
      delete process.env["MITOS_EXPOSE_DOMAIN"];
    }
  });

  it("creates a Sandbox with spec.expose and returns the public URL", async () => {
    const fake = new FakeK8s();
    const ws = new Workspace("proj-x", "ns", fake, noSleep);
    const result = await ws.serve({
      pool: "my-pool",
      exposeDomain: "mitos.app",
    });

    expect(result.url).toMatch(/^https:\/\/sandbox-[0-9a-f]{8}\.mitos\.app\/$/);
    expect(result.sandboxName).toMatch(/^sandbox-[0-9a-f]{8}$/);
    expect(result.label).toBe(result.sandboxName);
    expect(result.sharing).toBe("private");

    expect(fake.createdClaims).toHaveLength(1);
    const claim = fake.createdClaims[0];
    expect(claim.kind).toBe("Sandbox");
    expect(claim.spec?.["source"]).toEqual({ poolRef: { name: "my-pool" } });
    expect(claim.spec?.["workspaceRef"]).toEqual({ name: "proj-x" });
    const expose = claim.spec?.["expose"] as Record<string, unknown>;
    expect(expose["port"]).toBe(8080);
    expect(expose["sharing"]).toBe("private");
    expect(expose["label"]).toBe(result.label);
  });

  it("uses an explicit label, port, and sharing when provided", async () => {
    const fake = new FakeK8s();
    const ws = new Workspace("proj-x", "ns", fake, noSleep);
    const result = await ws.serve({
      pool: "my-pool",
      port: 3000,
      sharing: "link",
      label: "my-app",
      exposeDomain: "mitos.app",
    });

    expect(result.url).toBe("https://my-app.mitos.app/");
    expect(result.label).toBe("my-app");
    expect(result.sharing).toBe("link");

    const expose = fake.createdClaims[0].spec?.["expose"] as Record<string, unknown>;
    expect(expose["port"]).toBe(3000);
  });

  it("lowercases the label", async () => {
    const fake = new FakeK8s();
    const ws = new Workspace("proj-x", "ns", fake, noSleep);
    const result = await ws.serve({
      pool: "my-pool",
      label: "MyApp",
      exposeDomain: "mitos.app",
    });
    expect(result.label).toBe("myapp");
    expect(result.url).toBe("https://myapp.mitos.app/");
  });

  it("reads exposeDomain from MITOS_EXPOSE_DOMAIN env var", async () => {
    process.env["MITOS_EXPOSE_DOMAIN"] = "env.example.com";
    const fake = new FakeK8s();
    const ws = new Workspace("proj-x", "ns", fake, noSleep);
    const result = await ws.serve({ pool: "my-pool" });
    expect(result.url).toMatch(/^https:\/\/.+\.env\.example\.com\/$/);
  });

  it("throws missing_serve_pool when pool is empty", async () => {
    const fake = new FakeK8s();
    const ws = new Workspace("proj-x", "ns", fake, noSleep);
    await expect(
      ws.serve({ pool: "", exposeDomain: "mitos.app" }),
    ).rejects.toMatchObject({ code: "missing_serve_pool" });
  });

  it("throws missing_expose_domain when neither option nor env var is set", async () => {
    const fake = new FakeK8s();
    const ws = new Workspace("proj-x", "ns", fake, noSleep);
    await expect(ws.serve({ pool: "my-pool" })).rejects.toMatchObject({
      code: "missing_expose_domain",
    });
  });

  it("throws invalid_serve_port for out-of-range ports", async () => {
    const fake = new FakeK8s();
    const ws = new Workspace("proj-x", "ns", fake, noSleep);
    await expect(
      ws.serve({ pool: "my-pool", port: 0, exposeDomain: "mitos.app" }),
    ).rejects.toMatchObject({ code: "invalid_serve_port" });
    await expect(
      ws.serve({ pool: "my-pool", port: 65536, exposeDomain: "mitos.app" }),
    ).rejects.toMatchObject({ code: "invalid_serve_port" });
  });

  it("throws invalid_expose_label for labels that do not match the DNS pattern", async () => {
    const fake = new FakeK8s();
    const ws = new Workspace("proj-x", "ns", fake, noSleep);
    await expect(
      ws.serve({ pool: "my-pool", label: "-bad", exposeDomain: "mitos.app" }),
    ).rejects.toMatchObject({ code: "invalid_expose_label" });
    await expect(
      ws.serve({ pool: "my-pool", label: "bad-", exposeDomain: "mitos.app" }),
    ).rejects.toMatchObject({ code: "invalid_expose_label" });
    await expect(
      ws.serve({ pool: "my-pool", label: "a".repeat(64), exposeDomain: "mitos.app" }),
    ).rejects.toMatchObject({ code: "invalid_expose_label" });
  });

  it("throws reserved_expose_label for each reserved label", async () => {
    const reserved = [
      "www", "app", "api", "console", "admin", "auth", "login",
      "account", "mail", "static", "assets", "cdn", "status", "gateway",
    ];
    const fake = new FakeK8s();
    const ws = new Workspace("proj-x", "ns", fake, noSleep);
    for (const label of reserved) {
      await expect(
        ws.serve({ pool: "my-pool", label, exposeDomain: "mitos.app" }),
      ).rejects.toMatchObject({ code: "reserved_expose_label" });
    }
  });

  it("throws sandbox_failed when the sandbox reaches Failed phase", async () => {
    const fake = new FakeK8s({
      claim: { metadata: { name: "s" }, status: { phase: "Failed" } },
    });
    const ws = new Workspace("proj-x", "ns", fake, noSleep);
    await expect(
      ws.serve({ pool: "my-pool", exposeDomain: "mitos.app" }),
    ).rejects.toMatchObject({ code: "sandbox_failed" });
  });

  it("polls until the sandbox reaches Ready", async () => {
    let callCount = 0;
    const phases = ["Pending", "Pending", "Ready"];
    const fake = new FakeK8s();
    // Override getClaim to return different phases per call.
    fake.getClaim = async (_ns: string, name: string) => {
      const phase = phases[callCount] ?? "Ready";
      callCount++;
      return { metadata: { name }, status: { phase } };
    };
    const ws = new Workspace("proj-x", "ns", fake, noSleep);
    const result = await ws.serve({ pool: "my-pool", exposeDomain: "mitos.app" });
    expect(callCount).toBe(3);
    expect(result.url).toMatch(/^https:\/\/.+\.mitos\.app\/$/);
  });
});
