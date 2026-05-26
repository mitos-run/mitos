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

interface SandboxDriverConfig {
  serverUrl: string;
  template: string;
  timeoutMs: number;
  reuseLease: boolean;
}

function parseConfig(raw: Record<string, unknown>): SandboxDriverConfig {
  const serverUrl =
    typeof raw.serverUrl === "string" ? raw.serverUrl.trim() : "";
  return {
    serverUrl: serverUrl.replace(/\/$/, ""),
    template: typeof raw.template === "string" ? raw.template.trim() : "default",
    timeoutMs: Number(raw.timeoutMs ?? 30_000),
    reuseLease: raw.reuseLease === true,
  };
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

    if (!config.serverUrl) {
      errors.push("serverUrl is required.");
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

    try {
      const resp = await sandboxFetch(config.serverUrl, "/v1/exec", {
        method: "POST",
        body: JSON.stringify({
          sandbox: sandboxId,
          command,
          timeout: Math.ceil(config.timeoutMs / 1000),
        }),
      });
      const result = await resp.json();

      return {
        exitCode: result.exit_code ?? 1,
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
