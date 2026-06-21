import { describe, expect, it } from "vitest";

import {
  assertAdapterInstalls,
  bridgeToEgressAllow,
  leaseToClaim,
  minutesToDuration,
  secretsToClaimMounts,
  terminateToOutputs,
  withExtraEgress,
} from "../src/claim-mapping.js";

describe("minutesToDuration (lease lifetime -> claim duration)", () => {
  it("renders whole minutes as Go minute durations", () => {
    expect(minutesToDuration(30)).toBe("30m");
    expect(minutesToDuration(1)).toBe("1m");
  });

  it("renders fractional minutes in seconds without lossy rounding", () => {
    expect(minutesToDuration(0.5)).toBe("30s");
    expect(minutesToDuration(1.5)).toBe("90s");
  });

  it("treats zero, negative and non-finite as no limit (undefined)", () => {
    expect(minutesToDuration(0)).toBeUndefined();
    expect(minutesToDuration(-5)).toBeUndefined();
    expect(minutesToDuration(Number.NaN)).toBeUndefined();
    expect(minutesToDuration(undefined)).toBeUndefined();
  });
});

describe("bridgeToEgressAllow (callback-bridge -> claim-time egress allow)", () => {
  it("defaults to deny and allows exactly the bridge host:port", () => {
    expect(bridgeToEgressAllow("bridge.internal:8443")).toEqual({
      egress: "deny",
      allow: ["bridge.internal:8443"],
    });
  });

  it("strips the scheme and infers the default port from a URL", () => {
    expect(bridgeToEgressAllow("https://bridge.example.com/callback")).toEqual({
      egress: "deny",
      allow: ["bridge.example.com:443"],
    });
    expect(bridgeToEgressAllow("http://bridge.example.com")).toEqual({
      egress: "deny",
      allow: ["bridge.example.com:80"],
    });
    expect(bridgeToEgressAllow("wss://bridge.example.com")).toEqual({
      egress: "deny",
      allow: ["bridge.example.com:443"],
    });
  });

  it("honors an explicit URL port over the scheme default", () => {
    expect(bridgeToEgressAllow("https://bridge.example.com:9443/cb")).toEqual({
      egress: "deny",
      allow: ["bridge.example.com:9443"],
    });
  });

  it("refuses to widen egress on an unparseable or portless endpoint", () => {
    expect(() => bridgeToEgressAllow("")).toThrow();
    expect(() => bridgeToEgressAllow("bridge.internal")).toThrow();
    expect(() => bridgeToEgressAllow("ftp://bridge.internal")).toThrow();
  });
});

describe("withExtraEgress (operator-declared extra allow entries)", () => {
  it("appends and de-duplicates while keeping the bridge entry first", () => {
    const base = bridgeToEgressAllow("bridge.internal:8443");
    const merged = withExtraEgress(base, [
      "https://github.com",
      "bridge.internal:8443",
    ]);
    expect(merged).toEqual({
      egress: "deny",
      allow: ["bridge.internal:8443", "github.com:443"],
    });
  });
});

describe("secretsToClaimMounts (claim-time secret injection)", () => {
  it("maps secret refs to SecretMount entries by reference only", () => {
    const mounts = secretsToClaimMounts([
      { envVar: "GIT_TOKEN", secretName: "run-creds", secretKey: "git" },
      { envVar: "BRIDGE_TOKEN", secretName: "run-creds", secretKey: "bridge" },
    ]);
    expect(mounts).toEqual([
      {
        name: "GIT_TOKEN",
        secretRef: { name: "run-creds", key: "git" },
        envVar: "GIT_TOKEN",
      },
      {
        name: "BRIDGE_TOKEN",
        secretRef: { name: "run-creds", key: "bridge" },
        envVar: "BRIDGE_TOKEN",
      },
    ]);
  });

  it("never carries a plaintext value (only name + key references)", () => {
    const mounts = secretsToClaimMounts([
      { envVar: "API_KEY", secretName: "s", secretKey: "k" },
    ]);
    const serialized = JSON.stringify(mounts);
    expect(serialized).toContain("secretRef");
    expect(serialized).not.toContain("value");
  });

  it("rejects an incomplete secret ref", () => {
    expect(() =>
      secretsToClaimMounts([{ envVar: "", secretName: "s", secretKey: "k" }]),
    ).toThrow();
    expect(() =>
      secretsToClaimMounts([{ envVar: "X", secretName: "", secretKey: "k" }]),
    ).toThrow();
  });
});

describe("terminateToOutputs (teardown -> extract directives)", () => {
  it("captures the whole workspace with a diff when no paths are given", () => {
    expect(terminateToOutputs([])).toEqual([{ diff: true }]);
  });

  it("narrows to each path and records a diff per subtree", () => {
    expect(terminateToOutputs(["/workspace/out", "/workspace/dist"])).toEqual([
      { path: "/workspace/out", diff: true },
      { path: "/workspace/dist", diff: true },
    ]);
  });
});

describe("assertAdapterInstalls (per-adapter install assertion)", () => {
  it("passes when every required binary is present in the snapshot", () => {
    expect(
      assertAdapterInstalls(["node", "git"], { present: ["node", "git", "bash"] }),
    ).toEqual({ ok: true, missing: [] });
  });

  it("reports the missing binaries when the snapshot lacks an adapter", () => {
    expect(
      assertAdapterInstalls(["node", "git", "python"], { present: ["node"] }),
    ).toEqual({ ok: false, missing: ["git", "python"] });
  });

  it("passes vacuously when nothing is required", () => {
    expect(assertAdapterInstalls([], { present: [] })).toEqual({
      ok: true,
      missing: [],
    });
  });
});

describe("leaseToClaim (full provider-contract -> SandboxClaim)", () => {
  it("threads pool, lease lifetime, secrets, bridge egress and workspace", () => {
    const claim = leaseToClaim({
      name: "paperclip-run-1",
      namespace: "agents",
      pool: "claude-pool",
      env: { RUN_ID: "abc" },
      secrets: [
        { envVar: "GIT_TOKEN", secretName: "run-creds", secretKey: "git" },
      ],
      lease: { maxLifetimeMin: 30, idleTimeoutMin: 5 },
      bridgeEndpoint: "https://bridge.internal:8443/cb",
      workspace: "ws-1",
    });

    expect(claim.apiVersion).toBe("mitos.run/v1alpha1");
    expect(claim.kind).toBe("SandboxClaim");
    expect(claim.metadata).toEqual({ name: "paperclip-run-1", namespace: "agents" });
    expect(claim.spec.poolRef).toEqual({ name: "claude-pool" });
    expect(claim.spec.env).toEqual([{ name: "RUN_ID", value: "abc" }]);
    expect(claim.spec.secrets).toEqual([
      {
        name: "GIT_TOKEN",
        secretRef: { name: "run-creds", key: "git" },
        envVar: "GIT_TOKEN",
      },
    ]);
    expect(claim.spec.timeout).toBe("30m");
    expect(claim.spec.idleTimeout).toBe("5m");
    expect(claim.spec.networkPolicy).toEqual({
      egress: "deny",
      allow: ["bridge.internal:8443"],
    });
    expect(claim.spec.workspaceRef).toEqual({ name: "ws-1" });
  });

  it("omits optional fields when not provided (no limits, no egress widening)", () => {
    const claim = leaseToClaim({ name: "run", pool: "p" });
    expect(claim.spec).toEqual({ poolRef: { name: "p" } });
    expect(claim.spec.timeout).toBeUndefined();
    expect(claim.spec.idleTimeout).toBeUndefined();
    expect(claim.spec.networkPolicy).toBeUndefined();
    expect(claim.spec.secrets).toBeUndefined();
  });

  it("requires a name and a pool", () => {
    expect(() => leaseToClaim({ name: "", pool: "p" })).toThrow();
    expect(() => leaseToClaim({ name: "run", pool: "" })).toThrow();
  });
});
