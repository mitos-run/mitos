// Direct client for the standalone sandbox-server (cmd/sandbox-server). No
// Kubernetes required. Mirrors the Python SandboxServer
// (sdk/python/mitos/direct.py). The bearer token is resolved with the unified
// precedence (argument, then MITOS_API_KEY, then the CLI login credential file);
// the standalone server runs tokenless and ignores it, while the hosted front
// door verifies it.

import { createRequire } from "node:module";

import { HttpClient, validSandboxId } from "./http.js";
import { Sandbox } from "./sandbox.js";
import { AgentRunError } from "./errors.js";

// The hosted production control plane. When neither a url argument nor
// MITOS_BASE_URL is set, the client targets the hosted endpoint so the examples
// work without a base URL. Self-hosted or local standalone users opt out by
// setting MITOS_BASE_URL (e.g. http://localhost:8080). Mirrors the Python
// DEFAULT_BASE_URL.
const DEFAULT_BASE_URL = "https://mitos.run";

// resolveBaseUrl applies the base-URL precedence: explicit argument, then
// MITOS_BASE_URL, then the hosted production endpoint. Parity with the Python
// SDK's _resolve_auth.
function resolveBaseUrl(url?: string): string {
  if (url) return url;
  const env = globalThis.process?.env?.MITOS_BASE_URL;
  if (env) return env;
  return DEFAULT_BASE_URL;
}

// nodeRequire returns a CommonJS-style require usable from this ESM module, or
// undefined when there is no Node module system (a browser bundle), so the
// credential-file fallback is skipped silently off-Node. It loads node:module
// via createRequire seeded from import.meta.url.
function nodeRequire(): NodeRequire | undefined {
  // Off-Node (no process.versions.node): a browser or edge runtime. Skip the
  // credential-file fallback silently. createRequire is the ESM-safe way to load
  // node builtins synchronously; import.meta.url seeds the resolver.
  if (!globalThis.process?.versions?.node) return undefined;
  try {
    return createRequire(import.meta.url);
  } catch {
    return undefined;
  }
}

// credentialsPath returns the location of the CLI login profile written by
// `mitos auth login`, honoring MITOS_CONFIG_DIR else ~/.config/mitos. It returns
// undefined when there is no filesystem context (a browser, or no home dir), in
// which case there is simply no credential-file fallback. Single source of truth
// for the path rule, mirroring the CLI's credentialsPath and the Python SDK.
function credentialsPath(req: NodeRequire): string | undefined {
  const env = globalThis.process?.env;
  if (!env) return undefined;
  const path = req("node:path") as typeof import("node:path");
  const os = req("node:os") as typeof import("node:os");
  if (env.MITOS_CONFIG_DIR) {
    return path.join(env.MITOS_CONFIG_DIR, "credentials.json");
  }
  const home = typeof os.homedir === "function" ? os.homedir() : "";
  if (!home) return undefined;
  return path.join(home, ".config", "mitos", "credentials.json");
}

// tokenFromCredentialFile reads the bearer token from the CLI login profile, or
// undefined. A missing, unreadable, or non-JSON file (or one without a "token")
// is NOT an error: it yields no token so the SDK stays usable tokenless. Only
// reads when fs + homedir are available; otherwise skips silently. The token
// VALUE is never logged.
function tokenFromCredentialFile(): string | undefined {
  const req = nodeRequire();
  if (!req) return undefined;
  try {
    const path = credentialsPath(req);
    if (!path) return undefined;
    const fs = req("node:fs") as typeof import("node:fs");
    if (typeof fs.readFileSync !== "function") return undefined;
    const raw = fs.readFileSync(path, "utf8");
    const data = JSON.parse(raw) as { token?: unknown };
    if (typeof data?.token === "string" && data.token) return data.token;
  } catch {
    return undefined; // Missing / unreadable / non-JSON: no token, no error.
  }
  return undefined;
}

/**
 * Resolves the bearer token for the flat SDK path. Precedence: explicit
 * argument, then MITOS_API_KEY, then the CLI login credential file (so one
 * `mitos auth login` authenticates the SDK too), then undefined (tokenless).
 * The file token is sent as-is and the gateway decides its validity. The token
 * VALUE is never logged.
 */
export function resolveToken(token?: string): string | undefined {
  if (token) return token;
  const env = globalThis.process?.env?.MITOS_API_KEY;
  if (env) return env;
  return tokenFromCredentialFile();
}

// Wire shapes from cmd/sandbox-server.
interface templateWire {
  id: string;
  ready: boolean;
  created_at: string;
  creation_time_ms: number;
}

interface forkWire {
  id: string;
  template_id: string;
  endpoint: string;
  fork_time_ms: number;
}

interface sandboxWire {
  id: string;
  template_id: string;
  endpoint: string;
  created_at: string;
  fork_time_ms: number;
}

/**
 * A template as reported by the sandbox-server.
 */
export interface Template {
  id: string;
  ready: boolean;
  createdAt: string;
  creationTimeMs: number;
}

/**
 * A sandbox summary as reported by the sandbox-server.
 */
export interface ServerSandbox {
  id: string;
  templateId: string;
  endpoint: string;
  createdAt: string;
  forkTimeMs: number;
}

function randomSandboxId(): string {
  // 8 random hex chars, matching the Python "sandbox-<hex>" convention.
  const bytes = new Uint8Array(4);
  globalThis.crypto.getRandomValues(bytes);
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  return `sandbox-${hex}`;
}

/**
 * Client for the standalone sandbox-server REST API. fork() returns a Sandbox
 * bound to the server (exec and files round-trip through the server URL, and
 * terminate issues DELETE /v1/sandboxes/{id}).
 */
export class SandboxServer {
  readonly url: string;
  private readonly http: HttpClient;

  /**
   * Builds a client for the sandbox-server / hosted control plane.
   *
   * The base URL follows the usual precedence (argument, then MITOS_BASE_URL,
   * then the hosted endpoint). The bearer token follows the unified precedence:
   * the `token` argument, then MITOS_API_KEY, then the CLI login credential file
   * written by `mitos auth login` (so one login authenticates the SDK too), then
   * none (tokenless). The standalone server ignores the token; the hosted front
   * door verifies it. The token VALUE is never logged.
   */
  constructor(url?: string, token?: string) {
    this.url = resolveBaseUrl(url).replace(/\/+$/, "");
    this.http = new HttpClient(this.url, resolveToken(token));
  }

  async listTemplates(): Promise<Template[]> {
    const out = await this.http.get<templateWire[]>("/v1/templates");
    return (out ?? []).map(toTemplate);
  }

  async createTemplate(
    id: string,
    opts?: { initWaitSeconds?: number; idempotencyKey?: string },
  ): Promise<Template> {
    const out = await this.http.post<templateWire>(
      "/v1/templates",
      {
        id,
        init_wait_seconds: opts?.initWaitSeconds ?? 5,
      },
      { "Idempotency-Key": opts?.idempotencyKey ?? newIdempotencyKey() },
    );
    return toTemplate(out);
  }

  /**
   * Forks a sandbox from a named template. Returns a Sandbox bound to this
   * server (the per-sandbox bearer token applies only in cluster mode; direct
   * mode is tokenless). When `id` is omitted a random one is generated.
   */
  async fork(template: string, id?: string, opts?: { idempotencyKey?: string }): Promise<Sandbox> {
    const sandboxId = id ?? randomSandboxId();
    if (!validSandboxId(sandboxId)) {
      throw new AgentRunError(`invalid sandbox id: ${JSON.stringify(sandboxId)}`, {
        code: "invalid_sandbox_id",
        cause: "id must match ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$",
        remediation:
          "Pass a sandbox id of alphanumerics, underscore, or hyphen, up to 64 chars.",
      });
    }
    const out = await this.http.post<forkWire>(
      "/v1/fork",
      {
        template,
        id: sandboxId,
      },
      { "Idempotency-Key": opts?.idempotencyKey ?? newIdempotencyKey() },
    );
    const resolvedId = out.id || sandboxId;
    // Exec and files round-trip through the server URL (the returned endpoint is
    // the server's own address); terminate deletes via the server.
    return new Sandbox({
      id: resolvedId,
      endpoint: this.url,
      http: this.http,
      terminator: async () => {
        // Direct mode has no workspaces; terminate deletes and reports unbound.
        await this.terminate(resolvedId);
        return undefined;
      },
    });
  }

  async listSandboxes(): Promise<ServerSandbox[]> {
    const out = await this.http.get<sandboxWire[]>("/v1/sandboxes");
    return (out ?? []).map(toServerSandbox);
  }

  private async terminate(id: string): Promise<void> {
    if (!validSandboxId(id)) {
      throw new AgentRunError(`invalid sandbox id: ${JSON.stringify(id)}`, {
        code: "invalid_sandbox_id",
        cause: "id must match ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$",
        remediation: "Terminate only ids that match the sandbox id allowlist.",
      });
    }
    await this.http.del(`/v1/sandboxes/${encodeURIComponent(id)}`);
  }
}

// newIdempotencyKey returns a fresh client-side key so a retried creating call
// (template build or fork) is de-duplicated by the server rather than creating a
// second resource. Parity with the Python SDK, which sends one on every creating
// call.
function newIdempotencyKey(): string {
  return globalThis.crypto.randomUUID().replace(/-/g, "");
}

function toTemplate(t: templateWire): Template {
  return {
    id: t.id,
    ready: t.ready,
    createdAt: t.created_at,
    creationTimeMs: t.creation_time_ms,
  };
}

function toServerSandbox(s: sandboxWire): ServerSandbox {
  return {
    id: s.id,
    templateId: s.template_id,
    endpoint: s.endpoint,
    createdAt: s.created_at,
    forkTimeMs: s.fork_time_ms,
  };
}
