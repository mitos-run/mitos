/**
 * Mitos Provider for ComputeSDK.
 *
 * Mitos runs snapshot-fork Firecracker microVM sandboxes: a warm pool is kept
 * ready and each create forks a fresh, independent VM from a copy-on-write
 * snapshot, so a sandbox boots in well under a second rather than from cold.
 * This provider talks to the hosted Mitos control plane at https://api.mitos.run
 * through the official Mitos TypeScript SDK (@mitos/sdk), mapping the ComputeSDK
 * provider surface onto the SDK's direct (hosted) mode.
 */

import {
  HttpClient,
  Sandbox as MitosSandbox,
  SandboxServer,
  AgentRunError,
} from "@mitos/sdk";
import { defineProvider, escapeShellArg } from "@computesdk/provider";

import type {
  CommandResult,
  SandboxInfo,
  CreateSandboxOptions,
  FileEntry,
  RunCommandOptions,
} from "computesdk";

/**
 * The hosted Mitos API front door. Overridable with the `baseUrl` config field
 * or the MITOS_BASE_URL environment variable (for a self-hosted control plane or
 * a local standalone sandbox-server).
 */
const DEFAULT_BASE_URL = "https://api.mitos.run";

/**
 * The template every fork starts from when the caller does not name one. Mitos
 * keeps a warm "python" pool on the hosted control plane; it carries the
 * code-interpreter kernel, so both runCommand and code cells work out of the box.
 */
const DEFAULT_TEMPLATE = "python";

/**
 * Mitos-specific configuration options.
 */
export interface MitosConfig {
  /** Mitos API key. Falls back to the MITOS_API_KEY environment variable. */
  apiKey?: string;
  /**
   * Base URL of the Mitos control plane. Defaults to https://api.mitos.run, or
   * the MITOS_BASE_URL environment variable when set. Point this at a
   * self-hosted control plane or a local standalone sandbox-server to run
   * off the hosted service.
   */
  baseUrl?: string;
  /** Template to fork from when the create call does not name one. Defaults to "python". */
  template?: string;
  /** Execution timeout in milliseconds, applied per command when set. */
  timeout?: number;
}

function resolveApiKey(config: MitosConfig): string {
  return (
    config.apiKey ||
    (typeof process !== "undefined" && process.env?.MITOS_API_KEY) ||
    ""
  );
}

function resolveBaseUrl(config: MitosConfig): string {
  return (
    config.baseUrl ||
    (typeof process !== "undefined" && process.env?.MITOS_BASE_URL) ||
    DEFAULT_BASE_URL
  );
}

function isNotFound(error: unknown): boolean {
  return (
    error instanceof AgentRunError &&
    (error.code === "not_found" || error.code === "sandbox_not_found")
  );
}

/**
 * Rebinds a Mitos direct-mode Sandbox to the hosted control plane by id, so the
 * getById and list paths can exec, read, and write against an existing sandbox
 * without re-forking. Exec and file traffic round-trip through the control plane
 * URL, addressed by the sandbox id.
 */
function bindSandbox(baseUrl: string, apiKey: string, id: string): MitosSandbox {
  const http = new HttpClient(baseUrl, apiKey || undefined);
  return new MitosSandbox({
    id,
    endpoint: baseUrl,
    token: apiKey || undefined,
    http,
    terminator: async () => {
      await http.del(`/v1/sandboxes/${encodeURIComponent(id)}`);
      return undefined;
    },
  });
}

const decoder = new TextDecoder();

export const mitos = defineProvider<MitosSandbox, MitosConfig>({
  name: "mitos",
  methods: {
    sandbox: {
      create: async (config: MitosConfig, options?: CreateSandboxOptions) => {
        const apiKey = resolveApiKey(config);
        if (!apiKey) {
          throw new Error(
            `Missing Mitos API key. Provide 'apiKey' in config or set the MITOS_API_KEY environment variable. Get a key from https://mitos.run.`,
          );
        }

        const baseUrl = resolveBaseUrl(config);
        // The requested runtime/template maps onto a Mitos template name. A
        // provider-agnostic templateId or snapshotId wins; then a "runtime"
        // hint; then the configured or default template.
        const runtime = (options as { runtime?: string } | undefined)?.runtime;
        const template =
          options?.templateId ||
          options?.snapshotId ||
          (runtime && runtime !== "node" && runtime !== "javascript"
            ? runtime
            : undefined) ||
          config.template ||
          DEFAULT_TEMPLATE;

        try {
          const server = new SandboxServer(baseUrl, apiKey);
          const sandbox = await server.fork(template);
          return { sandbox, sandboxId: sandbox.id };
        } catch (error) {
          if (error instanceof AgentRunError) {
            if (error.code === "unauthorized") {
              throw new Error(
                `Mitos authentication failed. Please check your MITOS_API_KEY. ${error.remediation ?? ""}`.trim(),
              );
            }
            if (error.code === "rate_limited") {
              throw new Error(
                `Mitos rate limit reached. ${error.remediation ?? "Check your usage at https://mitos.run."}`.trim(),
              );
            }
          }
          throw new Error(
            `Failed to create Mitos sandbox: ${error instanceof Error ? error.message : String(error)}`,
          );
        }
      },

      getById: async (config: MitosConfig, sandboxId: string) => {
        const apiKey = resolveApiKey(config);
        const baseUrl = resolveBaseUrl(config);
        try {
          const server = new SandboxServer(baseUrl, apiKey);
          const summaries = await server.listSandboxes();
          const match = summaries.find((s) => s.id === sandboxId);
          if (!match) {
            return null;
          }
          return { sandbox: bindSandbox(baseUrl, apiKey, sandboxId), sandboxId };
        } catch (error) {
          if (isNotFound(error)) {
            return null;
          }
          throw new Error(
            `Failed to get Mitos sandbox ${sandboxId}: ${error instanceof Error ? error.message : String(error)}`,
          );
        }
      },

      list: async (config: MitosConfig) => {
        const apiKey = resolveApiKey(config);
        const baseUrl = resolveBaseUrl(config);
        try {
          const server = new SandboxServer(baseUrl, apiKey);
          const summaries = await server.listSandboxes();
          return summaries.map((s) => ({
            sandbox: bindSandbox(baseUrl, apiKey, s.id),
            sandboxId: s.id,
          }));
        } catch (error) {
          throw new Error(
            `Failed to list Mitos sandboxes: ${error instanceof Error ? error.message : String(error)}`,
          );
        }
      },

      destroy: async (config: MitosConfig, sandboxId: string) => {
        const apiKey = resolveApiKey(config);
        const baseUrl = resolveBaseUrl(config);
        try {
          const http = new HttpClient(baseUrl, apiKey || undefined);
          await http.del(`/v1/sandboxes/${encodeURIComponent(sandboxId)}`);
        } catch (error) {
          if (isNotFound(error)) {
            return;
          }
          throw new Error(
            `Failed to destroy Mitos sandbox ${sandboxId}: ${error instanceof Error ? error.message : String(error)}`,
          );
        }
      },

      runCommand: async (
        sandbox: MitosSandbox,
        command: string,
        options?: RunCommandOptions,
      ): Promise<CommandResult> => {
        const startTime = Date.now();

        let fullCommand = command;
        if (options?.env && Object.keys(options.env).length > 0) {
          const envPrefix = Object.entries(options.env)
            .map(([k, v]) => `${k}="${escapeShellArg(String(v))}"`)
            .join(" ");
          fullCommand = `${envPrefix} ${fullCommand}`;
        }
        if (options?.cwd) {
          fullCommand = `cd "${escapeShellArg(options.cwd)}" && ${fullCommand}`;
        }
        if (options?.background) {
          fullCommand = `nohup ${fullCommand} > /dev/null 2>&1 &`;
        }

        const execOpts: {
          timeoutSeconds?: number;
          onStdout?: (chunk: Uint8Array) => void;
          onStderr?: (chunk: Uint8Array) => void;
        } = {};
        const timeoutMs = options?.timeout;
        if (typeof timeoutMs === "number" && timeoutMs > 0) {
          execOpts.timeoutSeconds = Math.max(1, Math.ceil(timeoutMs / 1000));
        }
        if (options?.onStdout) {
          const cb = options.onStdout;
          execOpts.onStdout = (chunk) => cb(decoder.decode(chunk, { stream: true }));
        }
        if (options?.onStderr) {
          const cb = options.onStderr;
          execOpts.onStderr = (chunk) => cb(decoder.decode(chunk, { stream: true }));
        }

        try {
          const result = await sandbox.exec(fullCommand, execOpts);
          return {
            stdout: result.stdout,
            stderr: result.stderr,
            exitCode: result.exitCode,
            durationMs: result.execTimeMs ?? Date.now() - startTime,
          };
        } catch (error) {
          // A command that exceeds its deadline surfaces as a typed error; return
          // it as a failed CommandResult (exit 124) rather than throwing, so the
          // caller sees the standard non-zero-exit shape.
          if (error instanceof AgentRunError && error.code === "exec_timeout") {
            return {
              stdout: "",
              stderr: error.message,
              exitCode: 124,
              durationMs: Date.now() - startTime,
            };
          }
          throw new Error(
            `Mitos command execution failed: ${error instanceof Error ? error.message : String(error)}`,
          );
        }
      },

      getInfo: async (sandbox: MitosSandbox): Promise<SandboxInfo> => ({
        id: sandbox.id,
        provider: "mitos",
        status: "running",
        createdAt: new Date(),
        timeout: 300000,
        metadata: { mitosSandboxId: sandbox.id, endpoint: sandbox.endpoint },
      }),

      getUrl: async (
        _sandbox: MitosSandbox,
        options: { port: number; protocol?: string },
      ): Promise<string> => {
        // Public port exposure is a separate Mitos feature (named
        // <label>.mitos.run URLs via `mitos workspace serve`) and is not part of
        // the direct sandbox surface this provider drives.
        throw new Error(
          `Mitos direct-mode sandboxes do not expose per-port URLs (requested port ${options.port}). Use the Mitos Expose feature (mitos workspace serve) for public URLs.`,
        );
      },

      getInstance: (sandbox: MitosSandbox): MitosSandbox => sandbox,

      filesystem: {
        readFile: async (sandbox: MitosSandbox, path: string): Promise<string> => {
          return sandbox.files.read(path);
        },
        writeFile: async (
          sandbox: MitosSandbox,
          path: string,
          content: string,
        ): Promise<void> => {
          await sandbox.files.write(path, content);
        },
        mkdir: async (
          sandbox: MitosSandbox,
          path: string,
          runCommand,
        ): Promise<void> => {
          const result = await runCommand(sandbox, `mkdir -p "${escapeShellArg(path)}"`);
          if (result.exitCode !== 0) {
            throw new Error(`Failed to create directory: ${path}`);
          }
        },
        readdir: async (sandbox: MitosSandbox, path: string): Promise<FileEntry[]> => {
          const entries = await sandbox.files.list(path);
          return entries.map((entry) => ({
            name: entry.name,
            type: entry.isDir ? ("directory" as const) : ("file" as const),
            size: entry.size,
            modified:
              entry.modifiedAt !== undefined
                ? new Date(Number(entry.modifiedAt) * 1000)
                : undefined,
          }));
        },
        exists: async (
          sandbox: MitosSandbox,
          path: string,
          runCommand,
        ): Promise<boolean> => {
          const result = await runCommand(sandbox, `test -e "${escapeShellArg(path)}"`);
          return result.exitCode === 0;
        },
        remove: async (
          sandbox: MitosSandbox,
          path: string,
          runCommand,
        ): Promise<void> => {
          const result = await runCommand(sandbox, `rm -rf "${escapeShellArg(path)}"`);
          if (result.exitCode !== 0) {
            throw new Error(`Failed to remove: ${path}`);
          }
        },
      },
    },
  },
});

export type { Sandbox as MitosSandbox } from "@mitos/sdk";
