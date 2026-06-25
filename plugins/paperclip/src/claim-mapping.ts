// Pure mapping logic from the Paperclip pluggable sandbox-provider contract
// onto the mitos Sandbox wire shape (mitos.run/v1). This module has NO
// external dependencies on purpose: it encodes only the contract mapping
// (lease lifecycle -> sandbox lifetime TTL/idle; teardown -> sandbox deletion
// with workspace-artifact extraction first; callback-bridge egress -> sandbox-
// time allowlist entry; secrets -> sandbox-time SecretMounts; per-adapter
// install assertion), so it is unit testable with no cluster and no Paperclip
// SDK.
//
// References:
//   - SandboxSpec: api/v1/types.go (source.poolRef, env, secrets, lifetime,
//     network, workspaceRef, outputs).
//   - Network egress model (#219): api/v1/types.go (egress "deny" | "allow",
//     allow host:port allowlist, default-deny otherwise).
//   - Terminate-with-outputs: SandboxSpec.Outputs (OutputSpec) and the TS
//     SDK terminator (sdk/typescript/src/client.ts makeTerminator), which
//     dehydrates the workspace BEFORE the sandbox is deleted.
//   - Secret-inheritance policy: docs/fork-correctness.md. Secret values are
//     injected at sandbox create time and never baked into a pool snapshot.

const API_GROUP = "mitos.run";
const API_VERSION = "v1";

/**
 * A reference to a Kubernetes Secret key, as a Sandbox consumes it. The
 * controller resolves this server-side; the plaintext value never travels
 * through the plugin and is never logged.
 */
export interface SecretKeyRef {
  /** Destination env var name inside the sandbox (also the mount logical name). */
  envVar: string;
  /** Name of the Kubernetes Secret object. */
  secretName: string;
  /** Key within that Secret. */
  secretKey: string;
}

/** A Sandbox spec.secrets entry (api/v1 SecretMount). */
export interface SecretMount {
  name: string;
  secretRef: { name: string; key: string };
  envVar: string;
}

/** The Mitos NetworkPolicy shape this plugin emits (a subset of #219). */
export interface NetworkPolicy {
  /** Default verdict for traffic matching no allow rule. */
  egress: "deny" | "allow";
  /** host:port allowlist entries punched through the default-deny posture. */
  allow: string[];
}

/** A Sandbox spec.outputs entry (api/v1 OutputSpec). */
export interface OutputSpec {
  path?: string;
  diff?: boolean;
}

/** The Sandbox object the plugin hands to a Mitos cluster client. */
export interface MitosSandbox {
  apiVersion: string;
  kind: "Sandbox";
  metadata: { name: string; namespace?: string };
  spec: {
    source: {
      poolRef: { name: string };
    };
    env?: Array<{ name: string; value: string }>;
    secrets?: SecretMount[];
    lifetime?: {
      ttl?: string;
      idleTimeout?: string;
    };
    network?: NetworkPolicy;
    workspaceRef?: { name: string };
  };
}

/**
 * The Paperclip lease lifecycle knobs that map onto sandbox lifetime. Both are
 * optional: an absent value leaves the corresponding sandbox field unset, which
 * the controller reads as "no limit".
 */
export interface LeaseLifetime {
  /** Wall-clock max lifetime in minutes (maps to spec.lifetime.ttl). */
  maxLifetimeMin?: number;
  /** Idle-reap window in minutes (maps to spec.lifetime.idleTimeout). */
  idleTimeoutMin?: number;
}

/**
 * Renders a minute count as a Go time.Duration string (metav1.Duration parses
 * Go durations). Whole minutes stay as "<n>m"; a fractional minute is rendered
 * in seconds to avoid lossy rounding. Non-positive or non-finite inputs yield
 * undefined (no limit), matching the controller's "zero means no limit".
 */
export function minutesToDuration(min: number | undefined): string | undefined {
  if (min === undefined || !Number.isFinite(min) || min <= 0) {
    return undefined;
  }
  if (Number.isInteger(min)) {
    return `${min}m`;
  }
  const seconds = Math.round(min * 60);
  return `${seconds}s`;
}

/**
 * Derives the sandbox-time egress allowlist entry for the Paperclip
 * callback-bridge endpoint. The bridge is the ONLY egress a conforming run
 * needs: the instance bridge endpoint becomes a single allow entry over an
 * otherwise default-deny posture (#219 model). Returns a NetworkPolicy with
 * egress "deny" and exactly the bridge host:port allowed.
 *
 * Accepts a bare host:port, or a URL (http/https/ws/wss); the scheme is
 * stripped and a default port is inferred from the scheme when the URL omits
 * one. Throws on an unparseable endpoint rather than silently widening egress.
 */
export function bridgeToEgressAllow(bridgeEndpoint: string): NetworkPolicy {
  const entry = normalizeHostPort(bridgeEndpoint);
  return { egress: "deny", allow: [entry] };
}

/**
 * Merges additional operator-declared egress allow entries (for example a
 * rendezvous git remote or an inference proxy) onto the bridge-only policy,
 * keeping the default-deny posture and de-duplicating entries. Order is stable:
 * the bridge entry stays first.
 */
export function withExtraEgress(
  policy: NetworkPolicy,
  extra: string[],
): NetworkPolicy {
  const seen = new Set(policy.allow);
  const merged = [...policy.allow];
  for (const raw of extra) {
    const entry = normalizeHostPort(raw);
    if (!seen.has(entry)) {
      seen.add(entry);
      merged.push(entry);
    }
  }
  return { egress: policy.egress, allow: merged };
}

function normalizeHostPort(endpoint: string): string {
  const trimmed = endpoint.trim();
  if (trimmed === "") {
    throw new Error("bridge endpoint is empty; cannot derive an egress allow entry");
  }
  // Bare host:port (no scheme, no path): accept as-is when it has a port.
  if (!trimmed.includes("://")) {
    if (/^[A-Za-z0-9._-]+:\d+$/.test(trimmed)) {
      return trimmed;
    }
    throw new Error(
      `bridge endpoint ${JSON.stringify(trimmed)} is not host:port; provide an explicit port`,
    );
  }
  let url: URL;
  try {
    url = new URL(trimmed);
  } catch {
    throw new Error(`bridge endpoint ${JSON.stringify(trimmed)} is not a valid URL`);
  }
  const host = url.hostname;
  if (host === "") {
    throw new Error(`bridge endpoint ${JSON.stringify(trimmed)} has no host`);
  }
  const port = url.port !== "" ? url.port : defaultPortForScheme(url.protocol);
  if (port === undefined) {
    throw new Error(
      `bridge endpoint ${JSON.stringify(trimmed)} has no port and scheme ${url.protocol} has no default`,
    );
  }
  return `${host}:${port}`;
}

function defaultPortForScheme(protocol: string): string | undefined {
  switch (protocol) {
    case "http:":
    case "ws:":
      return "80";
    case "https:":
    case "wss:":
      return "443";
    default:
      return undefined;
  }
}

/**
 * Maps the run's sandbox-time secrets (git creds, API keys, bridge token) onto
 * Sandbox spec.secrets entries. The plugin only ever moves Secret REFERENCES
 * (name + key); the controller resolves the plaintext server-side. This is the
 * secret-inheritance guard: values are injected at sandbox create time and
 * never live in a pool snapshot.
 */
export function secretsToClaimMounts(refs: SecretKeyRef[]): SecretMount[] {
  return refs.map((ref) => {
    if (ref.envVar.trim() === "") {
      throw new Error("secret ref envVar is empty");
    }
    if (ref.secretName.trim() === "" || ref.secretKey.trim() === "") {
      throw new Error(
        `secret ref for ${ref.envVar} is missing secretName or secretKey`,
      );
    }
    return {
      name: ref.envVar,
      secretRef: { name: ref.secretName, key: ref.secretKey },
      envVar: ref.envVar,
    };
  });
}

/**
 * The teardown ordering contract: teardown maps to sandbox deletion, but the
 * workspace artifacts must be extracted FIRST. This returns the
 * terminate-with-outputs directives that the controller dehydrates into a
 * committed WorkspaceRevision BEFORE the sandbox is deleted. When extractPaths
 * is empty, the whole /workspace is captured (a single diff-bearing output);
 * otherwise each path becomes a narrowing output. The caller is responsible for
 * patching these onto the sandbox, then deleting it (never the reverse).
 */
export function terminateToOutputs(extractPaths: string[]): OutputSpec[] {
  if (extractPaths.length === 0) {
    return [{ diff: true }];
  }
  return extractPaths.map((path) => ({ path, diff: true }));
}

/** A probe result describing the adapter binaries present in a sandbox. */
export interface InstalledProbe {
  /** Adapter binaries the probe found on PATH inside the (forked) sandbox. */
  present: string[];
}

/** The outcome of asserting the required adapter installs at sandbox create time. */
export interface InstallAssertion {
  ok: boolean;
  /** Required binaries that the probe did not find. */
  missing: string[];
}

/**
 * Asserts that the adapter binaries a run requires were BAKED into the pool's
 * snapshot at build time and are present at sandbox create time. Per the
 * contract, adapter installs are never performed per run: this is a create-time
 * assertion, not an install step. Returns ok=false with the missing binaries
 * when the snapshot lacks a required adapter, so the caller fails the sandbox
 * with an actionable error rather than silently degrading.
 */
export function assertAdapterInstalls(
  required: string[],
  probe: InstalledProbe,
): InstallAssertion {
  const present = new Set(probe.present);
  const missing = required.filter((bin) => !present.has(bin));
  return { ok: missing.length === 0, missing };
}

/**
 * The full create-environment mapping: turns a Paperclip acquire-lease request
 * into a Sandbox object, threading pool, lease lifetime (ttl + idleTimeout),
 * sandbox-time secrets, the bridge-derived default-deny egress policy, and an
 * optional durable workspace binding. This is the heart of workstream A: the
 * provider contract realized as a Sandbox.
 */
export interface ClaimMappingInput {
  name: string;
  namespace?: string;
  pool: string;
  env?: Record<string, string>;
  secrets?: SecretKeyRef[];
  lease?: LeaseLifetime;
  /** The callback-bridge endpoint (becomes the sole egress allow entry). */
  bridgeEndpoint?: string;
  /** Extra operator-declared egress entries merged onto the bridge policy. */
  extraEgress?: string[];
  /** Durable workspace name to bind for hydrate/dehydrate, if any. */
  workspace?: string;
}

export function leaseToClaim(input: ClaimMappingInput): MitosSandbox {
  if (input.name.trim() === "") {
    throw new Error("claim name is required");
  }
  if (input.pool.trim() === "") {
    throw new Error("pool is required");
  }

  const spec: MitosSandbox["spec"] = {
    source: { poolRef: { name: input.pool } },
  };

  if (input.env && Object.keys(input.env).length > 0) {
    spec.env = Object.entries(input.env).map(([name, value]) => ({ name, value }));
  }

  if (input.secrets && input.secrets.length > 0) {
    spec.secrets = secretsToClaimMounts(input.secrets);
  }

  const ttl = minutesToDuration(input.lease?.maxLifetimeMin);
  const idle = minutesToDuration(input.lease?.idleTimeoutMin);
  if (ttl || idle) {
    spec.lifetime = {};
    if (ttl) {
      spec.lifetime.ttl = ttl;
    }
    if (idle) {
      spec.lifetime.idleTimeout = idle;
    }
  }

  if (input.bridgeEndpoint) {
    let policy = bridgeToEgressAllow(input.bridgeEndpoint);
    if (input.extraEgress && input.extraEgress.length > 0) {
      policy = withExtraEgress(policy, input.extraEgress);
    }
    spec.network = policy;
  }

  if (input.workspace) {
    spec.workspaceRef = { name: input.workspace };
  }

  return {
    apiVersion: `${API_GROUP}/${API_VERSION}`,
    kind: "Sandbox",
    metadata: { name: input.name, namespace: input.namespace },
    spec,
  };
}
