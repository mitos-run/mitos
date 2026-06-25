import { definePlugin } from "@paperclipai/plugin-sdk";
import type {
  PluginEnvironmentAcquireLeaseParams,
  PluginEnvironmentDestroyLeaseParams,
  PluginEnvironmentExecuteParams,
  PluginEnvironmentExecuteResult,
  PluginEnvironmentLease,
  PluginEnvironmentProbeParams,
  PluginEnvironmentProbeResult,
  PluginEnvironmentRealizeWorkspaceParams,
  PluginEnvironmentRealizeWorkspaceResult,
  PluginEnvironmentReleaseLeaseParams,
  PluginEnvironmentResumeLeaseParams,
  PluginEnvironmentValidateConfigParams,
  PluginEnvironmentValidationResult,
} from "@paperclipai/plugin-sdk";

import {
  acquireWithAssertion,
  teardownWithExtract,
} from "./claim-client.js";
import type { MitosClaimClient } from "./claim-client.js";
import { leaseToClaim } from "./claim-mapping.js";
import type { MitosSandbox, SecretKeyRef } from "./claim-mapping.js";
import { execStream } from "./connect-exec.js";

/**
 * The driver backend. "server" maps the lease onto the standalone
 * sandbox-server REST API (the original skeleton path). "claim" maps it onto a
 * mitos Sandbox on Kubernetes (mitos.run/v1), the workstream-A target:
 * lease lifetime -> sandbox lifetime.ttl/idleTimeout, teardown -> extract then
 * delete, callback-bridge -> sandbox-time egress allow, secrets at create time.
 *
 * The k8s client wiring (a MitosClaimClient over @mitos/sdk AgentRun) is the
 * external follow-up: this repo carries the pure mapping (claim-mapping.ts) and
 * the orchestration (claim-client.ts), both unit tested, while the live cluster
 * binding lands in the Paperclip monorepo where @paperclipai/plugin-sdk and the
 * cluster credentials resolve.
 */
type Backend = "server" | "claim";

interface SandboxDriverConfig {
  backend: Backend;
  serverUrl: string;
  template: string;
  pool: string;
  namespace: string;
  timeoutMs: number;
  reuseLease: boolean;
  /** Lease lifetime knobs that map onto the claim's timeout / idleTimeout. */
  maxLifetimeMin?: number;
  idleTimeoutMin?: number;
  /** The callback-bridge endpoint; becomes the sole claim-time egress allow. */
  bridgeEndpoint?: string;
  /** Adapter binaries asserted (never installed) at claim time. */
  requiredAdapters: string[];
  /** Workspace subtrees extracted on teardown before claim deletion. */
  extractPaths: string[];
}

function parseConfig(raw: Record<string, unknown>): SandboxDriverConfig {
  const serverUrl =
    typeof raw.serverUrl === "string" ? raw.serverUrl.trim() : "";
  const backend: Backend = raw.backend === "claim" ? "claim" : "server";
  return {
    backend,
    serverUrl: serverUrl.replace(/\/$/, ""),
    template: typeof raw.template === "string" ? raw.template.trim() : "default",
    pool: typeof raw.pool === "string" ? raw.pool.trim() : "",
    namespace:
      typeof raw.namespace === "string" && raw.namespace.trim() !== ""
        ? raw.namespace.trim()
        : "default",
    timeoutMs: Number(raw.timeoutMs ?? 30_000),
    reuseLease: raw.reuseLease === true,
    maxLifetimeMin:
      typeof raw.maxLifetimeMin === "number" ? raw.maxLifetimeMin : undefined,
    idleTimeoutMin:
      typeof raw.idleTimeoutMin === "number" ? raw.idleTimeoutMin : undefined,
    bridgeEndpoint:
      typeof raw.bridgeEndpoint === "string" && raw.bridgeEndpoint.trim() !== ""
        ? raw.bridgeEndpoint.trim()
        : undefined,
    requiredAdapters: Array.isArray(raw.requiredAdapters)
      ? raw.requiredAdapters.filter((x): x is string => typeof x === "string")
      : [],
    extractPaths: Array.isArray(raw.extractPaths)
      ? raw.extractPaths.filter((x): x is string => typeof x === "string")
      : [],
  };
}

/**
 * Factory for the cluster client used by claim mode. Left unset in this repo
 * (the live @mitos/sdk AgentRun binding is the external follow-up); a host may
 * inject a client for tests or production. When unset, claim mode raises a
 * descriptive error rather than silently falling back to server mode.
 */
let claimClientFactory:
  | ((config: SandboxDriverConfig) => MitosClaimClient)
  | undefined;

export function setClaimClientFactory(
  factory: (config: SandboxDriverConfig) => MitosClaimClient,
): void {
  claimClientFactory = factory;
}

function requireClaimClient(config: SandboxDriverConfig): MitosClaimClient {
  if (!claimClientFactory) {
    throw new Error(
      "claim backend selected but no Mitos cluster client is wired. " +
        "Inject one via setClaimClientFactory (the @mitos/sdk AgentRun binding " +
        "ships in the Paperclip monorepo; see docs/integrations/paperclip.md).",
    );
  }
  return claimClientFactory(config);
}

/**
 * Build the claim-time secret references from the lease params. Only Secret
 * REFERENCES travel; the Mitos controller resolves the plaintext server-side,
 * so values never enter the plugin, the logs, or a pool snapshot.
 */
function secretRefsFor(
  params: PluginEnvironmentAcquireLeaseParams,
): SecretKeyRef[] {
  const raw = (params as { secretRefs?: unknown }).secretRefs;
  if (!Array.isArray(raw)) {
    return [];
  }
  return raw
    .filter(
      (r): r is { envVar: string; secretName: string; secretKey: string } =>
        !!r &&
        typeof r === "object" &&
        typeof (r as { envVar?: unknown }).envVar === "string" &&
        typeof (r as { secretName?: unknown }).secretName === "string" &&
        typeof (r as { secretKey?: unknown }).secretKey === "string",
    )
    .map((r) => ({
      envVar: r.envVar,
      secretName: r.secretName,
      secretKey: r.secretKey,
    }));
}

async function sandboxFetch(
  serverUrl: string,
  path: string,
  options?: RequestInit,
): Promise<Response> {
  const resp = await fetch(`${serverUrl}${path}`, {
    ...options,
    headers: { "Content-Type": "application/json", ...options?.headers },
  });
  if (!resp.ok) {
    const body = await resp.text().catch(() => "");
    throw new Error(`sandbox API ${path}: ${resp.status} ${body}`);
  }
  return resp;
}

const plugin = definePlugin({
  async setup(ctx) {
    ctx.logger.info("Sandbox (Firecracker) provider plugin ready");
  },

  async onHealth() {
    return { status: "ok", message: "Sandbox provider plugin healthy" };
  },

  async onEnvironmentValidateConfig(
    params: PluginEnvironmentValidateConfigParams,
  ): Promise<PluginEnvironmentValidationResult> {
    const config = parseConfig(params.config);
    const errors: string[] = [];

    if (config.backend === "claim") {
      if (!config.pool) {
        errors.push("pool is required when backend is claim.");
      }
    } else if (!config.serverUrl) {
      errors.push("serverUrl is required when backend is server.");
    }
    if (config.timeoutMs < 1000 || config.timeoutMs > 600_000) {
      errors.push("timeoutMs must be between 1000 and 600000.");
    }

    if (errors.length > 0) {
      return { ok: false, errors };
    }
    return { ok: true, normalizedConfig: { ...config } };
  },

  async onEnvironmentProbe(
    params: PluginEnvironmentProbeParams,
  ): Promise<PluginEnvironmentProbeResult> {
    const config = parseConfig(params.config);
    try {
      const resp = await sandboxFetch(config.serverUrl, "/v1/health");
      const health = await resp.json();
      return {
        ok: true,
        summary: `Connected to sandbox server. Status: ${health.status}, templates: ${health.templates}`,
        metadata: { provider: "sandbox", ...health },
      };
    } catch (error) {
      return {
        ok: false,
        summary: "Sandbox server probe failed.",
        metadata: {
          provider: "sandbox",
          error: error instanceof Error ? error.message : String(error),
        },
      };
    }
  },

  async onEnvironmentAcquireLease(
    params: PluginEnvironmentAcquireLeaseParams,
  ): Promise<PluginEnvironmentLease> {
    const config = parseConfig(params.config);
    const sandboxId = `paperclip-${params.environmentId}-${params.runId ?? "default"}`;

    if (config.backend === "claim") {
      // Workstream A: realize the provider contract as a Sandbox. Lease
      // lifetime -> sandbox lifetime.ttl/idleTimeout; callback-bridge ->
      // sandbox-time egress allow over default-deny; secrets injected at create
      // time by reference; required adapter binaries asserted (never installed).
      const client = requireClaimClient(config);
      const claim = leaseToClaim({
        name: sandboxId,
        namespace: config.namespace,
        pool: config.pool,
        env: (params as { env?: Record<string, string> }).env,
        secrets: secretRefsFor(params),
        lease: {
          maxLifetimeMin: config.maxLifetimeMin,
          idleTimeoutMin: config.idleTimeoutMin,
        },
        bridgeEndpoint: config.bridgeEndpoint,
      });
      const ready = await acquireWithAssertion(
        client,
        claim,
        config.requiredAdapters,
      );
      return {
        providerLeaseId: sandboxId,
        metadata: {
          provider: "sandbox",
          backend: "claim",
          sandboxId,
          endpoint: ready.endpoint,
          namespace: config.namespace,
          pool: config.pool,
        },
      };
    }

    const resp = await sandboxFetch(config.serverUrl, "/v1/fork", {
      method: "POST",
      body: JSON.stringify({
        template: config.template,
        id: sandboxId,
      }),
    });
    const sandbox = await resp.json();

    return {
      providerLeaseId: sandbox.id,
      metadata: {
        provider: "sandbox",
        sandboxId: sandbox.id,
        templateId: sandbox.template_id,
        endpoint: sandbox.endpoint,
        forkTimeMs: sandbox.fork_time_ms,
        serverUrl: config.serverUrl,
        reuseLease: config.reuseLease,
      },
    };
  },

  async onEnvironmentResumeLease(
    params: PluginEnvironmentResumeLeaseParams,
  ): Promise<PluginEnvironmentLease> {
    const config = parseConfig(params.config);
    if (!params.providerLeaseId) {
      return { providerLeaseId: null, metadata: { expired: true } };
    }

    // Check if sandbox still exists
    try {
      const resp = await sandboxFetch(config.serverUrl, "/v1/sandboxes");
      const sandboxes = await resp.json();
      const found = sandboxes.find(
        (s: { id: string }) => s.id === params.providerLeaseId,
      );
      if (!found) {
        return { providerLeaseId: null, metadata: { expired: true } };
      }
      return {
        providerLeaseId: params.providerLeaseId,
        metadata: {
          provider: "sandbox",
          sandboxId: found.id,
          serverUrl: config.serverUrl,
          resumedLease: true,
        },
      };
    } catch {
      return { providerLeaseId: null, metadata: { expired: true } };
    }
  },

  async onEnvironmentReleaseLease(
    params: PluginEnvironmentReleaseLeaseParams,
  ): Promise<void> {
    if (!params.providerLeaseId) return;
    const config = parseConfig(params.config);
    if (config.reuseLease) return; // keep alive for resume

    await sandboxFetch(
      config.serverUrl,
      `/v1/sandboxes/${params.providerLeaseId}`,
      { method: "DELETE" },
    ).catch(() => undefined);
  },

  async onEnvironmentDestroyLease(
    params: PluginEnvironmentDestroyLeaseParams,
  ): Promise<void> {
    if (!params.providerLeaseId) return;
    const config = parseConfig(params.config);

    if (config.backend === "claim") {
      // Teardown -> claim deletion, but extract the workspace artifacts FIRST.
      // teardownWithExtract patches the terminate-with-outputs directives (the
      // controller dehydrates them into a committed WorkspaceRevision) before
      // it deletes the claim; it never reorders the two.
      const client = requireClaimClient(config);
      await teardownWithExtract(
        client,
        params.providerLeaseId,
        config.extractPaths,
      );
      return;
    }

    await sandboxFetch(
      config.serverUrl,
      `/v1/sandboxes/${params.providerLeaseId}`,
      { method: "DELETE" },
    ).catch(() => undefined);
  },

  async onEnvironmentRealizeWorkspace(
    params: PluginEnvironmentRealizeWorkspaceParams,
  ): Promise<PluginEnvironmentRealizeWorkspaceResult> {
    return {
      cwd: "/workspace",
      metadata: { provider: "sandbox", remoteCwd: "/workspace" },
    };
  },

  async onEnvironmentExecute(
    params: PluginEnvironmentExecuteParams,
  ): Promise<PluginEnvironmentExecuteResult> {
    if (!params.lease.providerLeaseId) {
      return {
        exitCode: 1,
        timedOut: false,
        stdout: "",
        stderr: "No sandbox lease available.",
      };
    }

    const config = parseConfig(params.config);
    const sandboxId = params.lease.providerLeaseId;

    // Build the command
    const args = params.args ?? [];
    const fullCommand = [params.command, ...args].join(" ");

    // Build env export prefix
    const envPrefix = params.env
      ? Object.entries(params.env)
          .map(([k, v]) => `export ${k}='${v.replace(/'/g, `'"'"'`)}'`)
          .join(" && ") + " && "
      : "";

    const cdPrefix = params.cwd ? `cd '${params.cwd}' && ` : "";
    const command = `${envPrefix}${cdPrefix}${fullCommand}`;

    // The runtime exec call speaks the Connect sandbox.v1.Sandbox ExecStream RPC
    // (issue #358); the legacy JSON exec route is gone. A bearer token, when
    // the host puts one on the lease metadata (the hosted/forkd case), rides on
    // Authorization and is never logged; the standalone server case is tokenless
    // and routes the sandbox by the X-Sandbox-Id header.
    const leaseMeta = (params.lease as { metadata?: Record<string, unknown> })
      .metadata;
    const token =
      leaseMeta && typeof leaseMeta.token === "string"
        ? leaseMeta.token
        : undefined;

    try {
      const result = await execStream(
        config.serverUrl,
        command,
        Math.ceil(config.timeoutMs / 1000),
        { sandboxId, token },
      );

      return {
        exitCode: result.exitCode ?? 1,
        timedOut: false,
        stdout: result.stdout ?? "",
        stderr: result.stderr ?? "",
      };
    } catch (error) {
      return {
        exitCode: 1,
        timedOut: false,
        stdout: "",
        stderr: error instanceof Error ? error.message : String(error),
      };
    }
  },
});

export default plugin;
