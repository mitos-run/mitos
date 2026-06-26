// A durable, forkable agent workspace handle for the cluster client. Mirrors
// the Python mitos.workspace.Workspace: lazy (no cluster touch until a verb is
// called) and git-shaped (log, diff, fork, revert/checkout). Errors are
// LLM-legible AgentRunErrors carrying a stable code and a remediation.

import { AgentRunError } from "./errors.js";
import type { CustomObject, K8sApi } from "./k8s.js";

const API_GROUP = "mitos.run";
const API_VERSION = "v1";

// reservedExposeLabels mirrors internal/preview.reservedLabels so the SDK can
// validate labels without importing internal/. Keep this list consistent with
// the proxy (internal/preview/route.go reservedLabels map).
const RESERVED_EXPOSE_LABELS = new Set([
  "www", "app", "api", "console", "admin", "auth", "login", "account",
  "mail", "static", "assets", "cdn", "status", "gateway",
]);

// exposeLabelRE matches a valid single DNS label: starts and ends with
// alphanumeric, may contain hyphens in the middle, max 63 characters.
const EXPOSE_LABEL_RE = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

// SERVE_POLL_INTERVAL_MS is the polling interval used while waiting for the
// sandbox to reach Ready. Exposed here so tests can override via the Workspace
// sleep injection.
const SERVE_POLL_INTERVAL_MS = 500;

export interface RevisionInfo {
  name: string;
  phase: string;
  lineage: string;
  resumable: boolean;
  created: string;
}

export interface DiffInfo {
  parent: string;
  added: string[];
  removed: string[];
  modified: string[];
}

/**
 * The handle returned by Workspace.serve. Carries the public HTTPS URL and the
 * identity of the backing sandbox. Token minting is a follow-up: the
 * per-sandbox bearer token is not set on the expose route here; the proxy
 * enforces the sharing tier independently.
 */
export interface ServedWorkspace {
  /** Public HTTPS URL: https://<label>.<exposeDomain>/. */
  url: string;
  /** Name of the Sandbox CRD that backs this serve session. */
  sandboxName: string;
  /** Single DNS label used in the URL subdomain. */
  label: string;
  /** Effective access tier ("private", "link", "org", "authenticated", "public"). */
  sharing: string;
}

function randomHex(): string {
  const bytes = new Uint8Array(4);
  globalThis.crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

// buildServeUrl validates label and exposeDomain and returns the HTTPS expose
// URL. It is the SDK-local equivalent of internal/agentcli.BuildExposeURL; the
// SDK must not import internal/.
function buildServeUrl(label: string, exposeDomain: string): string {
  if (!label) {
    throw new AgentRunError("expose label is required", {
      code: "invalid_expose_label",
      cause: "label is empty",
      remediation:
        "Pass label: 'name' or use a sandbox name that is a valid single DNS label.",
    });
  }
  if (label.length > 63) {
    throw new AgentRunError(`expose label "${label}" exceeds 63 characters`, {
      code: "invalid_expose_label",
      cause: `label length ${label.length} > 63`,
      remediation: "Use a shorter label (at most 63 characters).",
    });
  }
  if (!EXPOSE_LABEL_RE.test(label)) {
    throw new AgentRunError(`expose label "${label}" is not a valid single DNS label`, {
      code: "invalid_expose_label",
      cause: "label must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$",
      remediation:
        "Use only lowercase letters, digits, and hyphens; do not start or end with a hyphen.",
    });
  }
  if (RESERVED_EXPOSE_LABELS.has(label)) {
    throw new AgentRunError(
      `expose label "${label}" is reserved and may not be used by tenants`,
      {
        code: "reserved_expose_label",
        cause: `label "${label}" is in the reserved set`,
        remediation:
          "Choose a different label that is not a well-known control-plane name.",
      },
    );
  }
  return `https://${label}.${exposeDomain}/`;
}

function lineageOf(spec: Record<string, unknown> | undefined): string {
  const src = (spec?.["source"] ?? {}) as Record<string, unknown>;
  const fromClaim = src["fromClaim"];
  if (typeof fromClaim === "string" && fromClaim !== "") {
    return "fromClaim:" + fromClaim;
  }
  const fwr = src["fromWorkspaceRevision"] as { revision?: string } | undefined;
  if (fwr) {
    return "fromWorkspaceRevision:" + (fwr.revision ?? "");
  }
  return "root";
}

/**
 * A durable, forkable agent workspace handle. Construct via AgentRun.workspace,
 * createWorkspace, or getWorkspace.
 */
export class Workspace {
  readonly name: string;

  private readonly namespace: string;
  private readonly k8s: K8sApi;
  private readonly sleep: (ms: number) => Promise<void>;

  constructor(
    name: string,
    namespace: string,
    k8s: K8sApi,
    sleep?: (ms: number) => Promise<void>,
  ) {
    this.name = name;
    this.namespace = namespace;
    this.k8s = k8s;
    this.sleep = sleep ?? ((ms) => new Promise((r) => setTimeout(r, ms)));
  }

  /** The latest committed revision name, or "" until the first revision commits. */
  async head(): Promise<string> {
    const ws = await this.k8s.getWorkspace(this.namespace, this.name);
    return ((ws.status ?? {})["head"] as string) ?? "";
  }

  /** Whether the workspace head pairs with a memory snapshot (resumable). */
  async resumable(): Promise<boolean> {
    const ws = await this.k8s.getWorkspace(this.namespace, this.name);
    return Boolean((ws.status ?? {})["resumable"]);
  }

  /** Lists the workspace's revisions, newest first. */
  async log(): Promise<RevisionInfo[]> {
    const list = await this.k8s.listRevisions(this.namespace);
    const revs: RevisionInfo[] = [];
    for (const o of list.items ?? []) {
      const spec = o.spec ?? {};
      const ref = (spec["workspaceRef"] ?? {}) as { name?: string };
      if (ref.name !== this.name) {
        continue;
      }
      revs.push({
        name: o.metadata?.name ?? "",
        phase: ((o.status ?? {})["phase"] as string) ?? "",
        lineage: lineageOf(spec),
        resumable: spec["memorySnapshotRef"] != null,
        created: o.metadata?.creationTimestamp ?? "",
      });
    }
    revs.sort((a, b) => (a.created < b.created ? 1 : a.created > b.created ? -1 : 0));
    return revs;
  }

  /** Returns the recorded content-hash diff of a revision against its parent. */
  async diff(revision: string): Promise<DiffInfo> {
    const o = await this.k8s.getRevision(this.namespace, revision);
    const summary = (o.status ?? {})["diffSummary"] as
      | { parentRevision?: string; added?: string[]; removed?: string[]; modified?: string[] }
      | undefined;
    if (!summary) {
      throw new AgentRunError(`revision ${revision} has no recorded diff`, {
        code: "no_diff",
        cause: "the revision was not captured with a {diff: true} output",
        remediation: "Terminate with outputs: [{ diff: true }] to record a diff.",
      });
    }
    return {
      parent: summary.parentRevision ?? "",
      added: summary.added ?? [],
      removed: summary.removed ?? [],
      modified: summary.modified ?? [],
    };
  }

  /**
   * Branch a committed revision into dstWorkspace (a content-addressed branch).
   * Returns the new revision name. dstWorkspace must exist.
   */
  async fork(revision: string, dstWorkspace: string): Promise<string> {
    const parent = await this.k8s.getRevision(this.namespace, revision);
    const manifest = (parent.spec ?? {})["contentManifest"] as string | undefined;
    const phase = (parent.status ?? {})["phase"] as string | undefined;
    if (phase !== "Committed" || !manifest) {
      throw new AgentRunError(`cannot fork uncommitted revision ${revision}`, {
        code: "revision_not_committed",
        cause: `revision ${revision} is not committed`,
        remediation: "Wait for the revision to commit before forking it.",
      });
    }
    const body: CustomObject = {
      apiVersion: `${API_GROUP}/${API_VERSION}`,
      kind: "WorkspaceRevision",
      metadata: {
        name: undefined,
        namespace: this.namespace,
      },
      spec: {
        workspaceRef: { name: dstWorkspace },
        source: { fromWorkspaceRevision: { workspace: this.name, revision } },
        contentManifest: manifest,
      },
    };
    // generateName + labels live alongside name; set them without losing the
    // typed metadata shape.
    (body.metadata as Record<string, unknown>)["generateName"] = dstWorkspace + "-";
    (body.metadata as Record<string, unknown>)["labels"] = {
      "mitos.run/workspace": dstWorkspace,
    };
    const created = await this.k8s.createRevision(this.namespace, body);
    return created.metadata?.name ?? "";
  }

  /**
   * Set this workspace head to a past revision by creating a new tip that
   * shares its content (revisions are immutable; a revert is a new tip).
   */
  async revert(revision: string): Promise<string> {
    return this.fork(revision, this.name);
  }

  /** checkout is an alias for revert: make a past state the new head. */
  async checkout(revision: string): Promise<string> {
    return this.revert(revision);
  }

  /**
   * Create a workspace-bound Sandbox with spec.expose set and wait until it
   * reaches Ready. Returns a ServedWorkspace carrying the public HTTPS URL.
   *
   * opts.pool is required. opts.exposeDomain defaults to
   * process.env.MITOS_EXPOSE_DOMAIN when not supplied. opts.port defaults to
   * 8080. opts.sharing defaults to "private". opts.label defaults to the
   * generated sandbox name (lowercased).
   *
   * Token minting is a follow-up: the per-sandbox bearer token is not wired
   * here; the proxy enforces the sharing tier independently.
   */
  async serve(opts: {
    pool: string;
    port?: number;
    sharing?: string;
    label?: string;
    exposeDomain?: string;
  }): Promise<ServedWorkspace> {
    if (!opts.pool) {
      throw new AgentRunError("serve() needs a pool", {
        code: "missing_serve_pool",
        cause: "opts.pool was not provided",
        remediation: "Pass pool: 'name' to select the SandboxPool to claim from.",
      });
    }

    const port = opts.port ?? 8080;
    if (port < 1 || port > 65535) {
      throw new AgentRunError("serve port out of range", {
        code: "invalid_serve_port",
        cause: `port ${port} is not in 1-65535`,
        remediation: "Pass port: n with a port in the range 1-65535.",
      });
    }

    // Resolve expose domain: option first, then env var.
    const exposeDomain = opts.exposeDomain ?? process.env["MITOS_EXPOSE_DOMAIN"] ?? "";
    if (!exposeDomain) {
      throw new AgentRunError("expose domain is required", {
        code: "missing_expose_domain",
        cause: "no expose domain was provided and MITOS_EXPOSE_DOMAIN is not set",
        remediation:
          "Pass exposeDomain: 'domain' or set the MITOS_EXPOSE_DOMAIN environment variable.",
      });
    }

    const sharing = opts.sharing ?? "private";

    // Generate the sandbox name up front so it can serve as the default label
    // before the server assigns it (we control the name here).
    const sbName = "sandbox-" + randomHex();

    // Determine the effective label; if not explicit, use the sandbox name.
    const rawLabel = (opts.label ?? sbName).toLowerCase();

    // Validate label and build the URL before creating anything in the cluster
    // so a bad label fails fast without leaving a partially configured sandbox.
    const url = buildServeUrl(rawLabel, exposeDomain);

    // Build the Sandbox CRD body with spec.expose included in the initial POST.
    // This matches the api/v1 SandboxExpose JSON shape: port, label, sharing.
    const claim: CustomObject = {
      apiVersion: `${API_GROUP}/${API_VERSION}`,
      kind: "Sandbox",
      metadata: { name: sbName, namespace: this.namespace },
      spec: {
        source: { poolRef: { name: opts.pool } },
        workspaceRef: { name: this.name },
        expose: {
          port,
          label: rawLabel,
          sharing,
        },
      },
    };

    await this.k8s.createClaim(this.namespace, claim);

    await this.waitSandboxReady(sbName);

    return { url, sandboxName: sbName, label: rawLabel, sharing };
  }

  // waitSandboxReady polls the Sandbox until it reaches Ready or the context is
  // replaced by a timeout. A Failed phase is returned as an error immediately.
  private async waitSandboxReady(name: string): Promise<void> {
    for (;;) {
      const obj = await this.k8s.getClaim(this.namespace, name);
      const phase = ((obj.status ?? {})["phase"] as string) ?? "";
      if (phase === "Ready") {
        return;
      }
      if (phase === "Failed") {
        throw new AgentRunError(`sandbox ${name} reached Failed phase`, {
          code: "sandbox_failed",
          cause: "the controller reported a Failed phase before Ready",
          remediation:
            `Check the Sandbox status for more detail (kubectl describe sandbox ${name}).`,
        });
      }
      await this.sleep(SERVE_POLL_INTERVAL_MS);
    }
  }
}
