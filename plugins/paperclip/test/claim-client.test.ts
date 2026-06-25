import { describe, expect, it } from "vitest";

import {
  AdapterInstallError,
  acquireWithAssertion,
  teardownWithExtract,
} from "../src/claim-client.js";
import type { MitosClaimClient } from "../src/claim-client.js";
import type {
  InstalledProbe,
  MitosSandbox,
  OutputSpec,
} from "../src/claim-mapping.js";
import { leaseToClaim } from "../src/claim-mapping.js";

/**
 * A recording fake MitosClaimClient. Captures the call order so the teardown
 * extract-then-delete ordering and the install-assertion teardown can be
 * asserted without a cluster.
 */
class FakeClient implements MitosClaimClient {
  calls: string[] = [];
  patchedOutputs?: OutputSpec[];
  deleted: string[] = [];
  constructor(private readonly probeResult: InstalledProbe) {}

  async createClaim(claim: MitosSandbox): Promise<{ endpoint: string }> {
    this.calls.push(`create:${claim.metadata.name}`);
    return { endpoint: "https://node-1:9091" };
  }
  async probeInstalls(claimName: string): Promise<InstalledProbe> {
    this.calls.push(`probe:${claimName}`);
    return this.probeResult;
  }
  async patchOutputs(claimName: string, outputs: OutputSpec[]): Promise<void> {
    this.calls.push(`patch:${claimName}`);
    this.patchedOutputs = outputs;
  }
  async deleteClaim(claimName: string): Promise<void> {
    this.calls.push(`delete:${claimName}`);
    this.deleted.push(claimName);
  }
}

const baseClaim = (): MitosSandbox =>
  leaseToClaim({ name: "run-1", pool: "p", bridgeEndpoint: "bridge:8443" });

describe("acquireWithAssertion (install assertion is sandbox-create-time, not per-run)", () => {
  it("creates then probes, and returns the endpoint when adapters are present", async () => {
    const client = new FakeClient({ present: ["node", "git"] });
    const ready = await acquireWithAssertion(client, baseClaim(), ["node", "git"]);
    expect(ready.endpoint).toBe("https://node-1:9091");
    expect(client.calls).toEqual(["create:run-1", "probe:run-1"]);
    expect(client.deleted).toEqual([]);
  });

  it("tears down and throws an actionable error when an adapter is missing", async () => {
    const client = new FakeClient({ present: ["node"] });
    await expect(
      acquireWithAssertion(client, baseClaim(), ["node", "git"]),
    ).rejects.toBeInstanceOf(AdapterInstallError);
    // The non-conforming sandbox is torn down.
    expect(client.deleted).toEqual(["run-1"]);
    try {
      await acquireWithAssertion(
        new FakeClient({ present: ["node"] }),
        baseClaim(),
        ["node", "git"],
      );
    } catch (e) {
      expect((e as AdapterInstallError).missing).toEqual(["git"]);
      expect((e as AdapterInstallError).message).toContain("baked at pool build");
    }
  });

  it("skips the probe entirely when nothing is required", async () => {
    const client = new FakeClient({ present: [] });
    await acquireWithAssertion(client, baseClaim(), []);
    expect(client.calls).toEqual(["create:run-1"]);
  });
});

describe("teardownWithExtract (extract-then-delete ordering)", () => {
  it("patches outputs BEFORE deleting the sandbox", async () => {
    const client = new FakeClient({ present: [] });
    await teardownWithExtract(client, "run-1", ["/workspace/out"]);
    expect(client.calls).toEqual(["patch:run-1", "delete:run-1"]);
    expect(client.patchedOutputs).toEqual([
      { path: "/workspace/out", diff: true },
    ]);
  });

  it("captures the whole workspace when no extract paths are given", async () => {
    const client = new FakeClient({ present: [] });
    await teardownWithExtract(client, "run-1", []);
    expect(client.patchedOutputs).toEqual([{ diff: true }]);
    expect(client.calls).toEqual(["patch:run-1", "delete:run-1"]);
  });

  it("never deletes when the output patch fails (no artifact loss)", async () => {
    const client = new FakeClient({ present: [] });
    client.patchOutputs = async () => {
      throw new Error("patch rejected");
    };
    await expect(
      teardownWithExtract(client, "run-1", ["/workspace/out"]),
    ).rejects.toThrow("patch rejected");
    expect(client.deleted).toEqual([]);
  });
});
