// A running sandbox: exec, file IO, and terminate over the sandbox API
// (forkd :9091 or the standalone sandbox-server). Mirrors the Python Sandbox
// surface (sdk/python/mitos/sandbox.py), camelCased.

import { ConnectClient, b64ToBytes, bytesToB64 } from "./connect.js";
import {
  AgentRunError,
  ExecutionDeadlineError,
  EXEC_TIMEOUT_EXIT_CODE,
  validateTimeout,
} from "./errors.js";
import { HttpClient, validSandboxId } from "./http.js";
import { Pty, createPty } from "./pty.js";
import type {
  BackgroundProcess,
  Execution,
  ExecResult,
  ExecutionError,
  FileInfo,
  Result,
} from "./types.js";

/** A function that tears a sandbox down. Injected by the owning client so the
 * cluster client deletes a SandboxClaim while the direct client issues a
 * DELETE /v1/sandboxes/{id}. */
/**
 * A terminate output: a "/workspace/..." path string keeps only that subtree, a
 * { diff: true } records a content-hash diff, and a { git: {...} } pushes repo
 * paths to a rendezvous remote (mirrors docs/api/v2-spec.md onTerminate.outputs).
 */
export type TerminateOutput = string | Record<string, unknown>;

export interface TerminateOptions {
  /** Narrow and enrich the dehydrated workspace revision. */
  outputs?: TerminateOutput[];
  /** Pair the revision with a VM memory snapshot (resumable head). */
  checkpoint?: boolean;
}

/**
 * Tears the sandbox down. When bound to a workspace, the controller dehydrates
 * /workspace into a new committed revision; the returned string is the bound
 * workspace name (or undefined when unbound).
 */
export type Terminator = (opts?: TerminateOptions) => Promise<string | undefined>;

export interface SandboxOptions {
  id: string;
  endpoint: string;
  token?: string;
  /** Pre-built transport. When omitted, one is built from endpoint + token. */
  http?: HttpClient;
  /** Custom teardown. When omitted, terminate() is a no-op for the bare
   * Sandbox (the owning client supplies one). */
  terminator?: Terminator;
}

// Proto-JSON wire shapes for the Connect sandbox.v1.Sandbox file RPCs. The
// fields are camelCase as proto-JSON emits, and bytes fields are base64 strings.
interface connectFileInfoWire {
  name?: string;
  path?: string;
  isDir?: boolean;
  size?: number | string;
  mode?: number;
  modifiedAtUnix?: number | string;
}

interface listResponseWire {
  entries?: connectFileInfoWire[];
  nextPageToken?: string;
}

/**
 * File operations on a sandbox. Speaks the Connect sandbox.v1.Sandbox file RPCs
 * the sandbox-server and forkd serve at /sandbox.v1.Sandbox/<Method> (issue
 * #24): ReadFile (a server-stream of byte chunks), WriteFile (a client-stream of
 * chunks), and the unary List. The public method signatures and return types are
 * UNCHANGED from the legacy /v1/files/* routes they replace.
 */
export class SandboxFiles {
  constructor(
    private readonly sandbox: Sandbox,
    private readonly connect: ConnectClient,
  ) {}

  /**
   * Read a file via the ReadFile server-stream: concatenate each Chunk's
   * (base64-decoded) bytes until the stream ends, then utf-8 decode them.
   */
  async read(path: string): Promise<string> {
    const parts: Uint8Array[] = [];
    let total = 0;
    for await (const frame of this.connect.serverStream("ReadFile", { path })) {
      const bytes = b64ToBytes(frame["data"]);
      if (bytes.length > 0) {
        parts.push(bytes);
        total += bytes.length;
      }
    }
    const all = new Uint8Array(total);
    let off = 0;
    for (const p of parts) {
      all.set(p, off);
      off += p.length;
    }
    return new TextDecoder().decode(all);
  }

  /**
   * Write a file via the WriteFile client-stream: an open frame carrying the
   * path and mode, then one data frame with the (base64-encoded) bytes. The
   * single WriteFileResult frame is consumed (bytesWritten is not part of the
   * public API).
   */
  async write(
    path: string,
    content: string,
    opts?: { mode?: number },
  ): Promise<void> {
    const raw = new TextEncoder().encode(content);
    const open: Record<string, unknown> = { path };
    if (opts?.mode !== undefined) {
      open["mode"] = opts.mode;
    }
    const messages: Array<Record<string, unknown>> = [
      { open },
      { data: bytesToB64(raw) },
    ];
    // The client-stream returns a single unary WriteFileResult; drain it.
    for await (const _ of this.connect.bidi("WriteFile", messages)) {
      void _;
    }
  }

  /**
   * List a directory via the unary List RPC. The proto-JSON FileInfo carries
   * camelCase fields and modifiedAtUnix (mtime in unix seconds), which maps onto
   * the public FileInfo.modifiedAt (kept as the unix-second value, as the Python
   * SDK does).
   */
  async list(path: string = "/"): Promise<FileInfo[]> {
    const resp = (await this.connect.unary("List", {
      parent: path,
    })) as listResponseWire;
    const entries = resp.entries ?? [];
    return entries.map((e) => ({
      name: e.name ?? "",
      isDir: e.isDir ?? false,
      size: Number(e.size ?? 0),
      mode: e.mode ?? 0,
      modifiedAt:
        e.modifiedAtUnix !== undefined ? String(e.modifiedAtUnix) : undefined,
    }));
  }
}

/**
 * A running sandbox instance. Holds {id, endpoint, token, http} and exposes
 * exec, files, and terminate.
 */
export class Sandbox {
  readonly id: string;
  readonly endpoint: string;
  readonly files: SandboxFiles;

  private readonly http: HttpClient;
  // Connect client for the runtime RPCs (exec, files, run_code) over the
  // sandbox.v1.Sandbox service (issue #24). The control-plane lifecycle routes
  // (set_timeout, pause, resume) and the PTY WebSocket stay on http/ws.
  private readonly connect: ConnectClient;
  private readonly terminator?: Terminator;
  // Retained so createPty can authenticate the WebSocket upgrade; the PTY
  // endpoint is gated by the same per-sandbox bearer token as the HTTP API.
  // Never logged.
  private readonly token?: string;

  constructor(opts: SandboxOptions) {
    if (!validSandboxId(opts.id)) {
      throw new AgentRunError(`invalid sandbox id: ${JSON.stringify(opts.id)}`, {
        code: "invalid_sandbox_id",
        cause: "id must match ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$",
        remediation:
          "Use a sandbox id of alphanumerics, underscore, or hyphen (no '/' or '..'), up to 64 chars.",
      });
    }
    this.id = opts.id;
    this.endpoint = opts.endpoint;
    this.token = opts.token;
    this.http = opts.http ?? new HttpClient(toBaseUrl(opts.endpoint), opts.token);
    // The Connect runtime client addresses this sandbox by id via X-Sandbox-Id
    // and carries the same bearer token; it talks to the same base URL the
    // legacy /v1/* routes did (the endpoint is the server's own address).
    this.connect = new ConnectClient(toBaseUrl(opts.endpoint), opts.id, opts.token);
    this.terminator = opts.terminator;
    this.files = new SandboxFiles(this, this.connect);
  }

  /**
   * Open an interactive PTY (a shell) in the sandbox over a WebSocket. Output
   * bytes arrive via onData; the returned Pty has sendInput, resize, kill, and
   * wait() -> exitCode. Gated by the per-sandbox bearer token.
   */
  async createPty(
    onData: (data: Uint8Array) => void,
    opts?: { cols?: number; rows?: number },
  ): Promise<Pty> {
    const cols = opts?.cols ?? 80;
    const rows = opts?.rows ?? 24;
    const wsBase = toBaseUrl(this.endpoint)
      .replace(/^http:\/\//, "ws://")
      .replace(/^https:\/\//, "wss://");
    // The PTY rides the Connect sandbox.v1.Sandbox/Exec RPC over a WebSocket; the
    // size travels in the opening ExecRequest{open}, not the URL query.
    const url = `${wsBase}/sandbox.v1.Sandbox/Exec?sandbox=${this.id}`;
    return createPty({ url, cols, rows, token: this.token, onData });
  }

  /**
   * Runs a command in the sandbox over the Connect ExecStream server-streaming
   * RPC (sandbox.v1.Sandbox/ExecStream, issue #24). With no stream callbacks it
   * drains the stream into the aggregate ExecResult; with onStdout/onStderr it
   * fires the callbacks per output chunk while still resolving the full
   * aggregate. Connect serves server-streaming over HTTP/1.1, which the
   * fetch-based, dependency-free client reaches.
   */
  async exec(
    command: string,
    opts?: {
      timeoutSeconds?: number;
      onStdout?: (chunk: Uint8Array) => void;
      onStderr?: (chunk: Uint8Array) => void;
    },
  ): Promise<ExecResult> {
    // Determinism (issue #216): reject an over-ceiling timeout with a typed
    // TimeoutTooLargeError BEFORE the request, never silently reduce it.
    if (opts?.timeoutSeconds !== undefined) {
      validateTimeout(opts.timeoutSeconds);
    }
    return this.streamExec(command, opts ?? {});
  }

  /**
   * Starts a long-running command and returns a handle. wait() drains the
   * stream; kill() aborts it so forkd cancels the guest process group. The
   * default timeout is one day so a background server is not reaped by the
   * per-exec timeout.
   */
  async execBackground(
    command: string,
    opts?: {
      timeoutSeconds?: number;
      onStdout?: (chunk: Uint8Array) => void;
      onStderr?: (chunk: Uint8Array) => void;
    },
  ): Promise<BackgroundProcess> {
    const controller = new AbortController();
    const timeout = opts?.timeoutSeconds ?? 86400;
    validateTimeout(timeout);
    // A background process may legitimately set a very long timeout; do NOT map
    // exit 124 to ExecutionDeadlineError here (the caller wants the raw result).
    const promise = this.streamExec(
      command,
      { ...opts, timeoutSeconds: timeout },
      controller.signal,
      false,
    );
    return {
      wait: () => promise,
      kill: () => controller.abort(),
    };
  }

  /**
   * Adjusts this RUNNING sandbox's TTL to now + timeoutSeconds (issue #218).
   * Returns the new absolute deadline as a unix timestamp. A value over the
   * server ceiling throws a typed TimeoutTooLargeError; the server never
   * silently clamps it (issue #216). This is the native method the E2B compat
   * shim (#206) maps its setTimeout onto.
   */
  async setTimeout(timeoutSeconds: number): Promise<number> {
    validateTimeout(timeoutSeconds);
    const resp = await this.http.post<{ deadline_unix?: number }>(
      "/v1/set_timeout",
      { sandbox: this.id, timeout_seconds: timeoutSeconds },
    );
    return resp.deadline_unix ?? 0;
  }

  /**
   * Pauses this sandbox: snapshots full state (memory + filesystem) and stops
   * the clock (issue #218). A paused sandbox is never idle-reaped. resume()
   * restores it.
   */
  async pause(): Promise<void> {
    await this.http.post<{ status?: string }>("/v1/pause", { sandbox: this.id });
  }

  /**
   * Resumes a paused sandbox: restores its full state and restarts the clock
   * (issue #218).
   */
  async resume(): Promise<void> {
    await this.http.post<{ status?: string }>("/v1/resume", { sandbox: this.id });
  }

  private async streamExec(
    command: string,
    opts: {
      timeoutSeconds?: number;
      onStdout?: (chunk: Uint8Array) => void;
      onStderr?: (chunk: Uint8Array) => void;
    },
    signal?: AbortSignal,
    mapDeadline = true,
  ): Promise<ExecResult> {
    if (opts.timeoutSeconds !== undefined) {
      validateTimeout(opts.timeoutSeconds);
    }
    // ExecStreamRequest (proto-JSON, camelCase): command and the optional
    // timeoutSeconds. The reply is a stream of ExecResponse oneof frames
    // (stdout/stderr base64 chunks, then a terminal exit).
    const req: Record<string, unknown> = { command };
    if (opts.timeoutSeconds !== undefined) {
      req["timeoutSeconds"] = opts.timeoutSeconds;
    }
    const td = new TextDecoder();
    let exitCode = 0;
    let execTimeMs: number | undefined;
    let sawExit = false;
    const outParts: string[] = [];
    const errParts: string[] = [];

    const handleFrame = (frame: Record<string, unknown>) => {
      if ("exit" in frame) {
        const exit = (frame["exit"] ?? {}) as {
          exitCode?: number;
          execTimeMs?: number;
          error?: string;
        };
        exitCode = exit.exitCode ?? 0;
        execTimeMs = exit.execTimeMs;
        sawExit = true;
        // A spawn/transport failure rides ExecExit.error (an LLM-legible
        // remediation string, never a secret); surface it rather than a
        // misleading clean exit.
        if (exit.error) {
          throw new AgentRunError(`exec stream error: ${exit.error}`, {
            code: "exec_stream_error",
            cause: exit.error,
            remediation: "Inspect the command and the forkd logs for the failure.",
          });
        }
        return;
      }
      if ("stderr" in frame) {
        const bytes = b64ToBytes(frame["stderr"]);
        if (bytes.length === 0) return;
        errParts.push(td.decode(bytes));
        opts.onStderr?.(bytes);
        return;
      }
      if ("stdout" in frame) {
        const bytes = b64ToBytes(frame["stdout"]);
        if (bytes.length === 0) return;
        outParts.push(td.decode(bytes));
        opts.onStdout?.(bytes);
      }
    };

    let aborted = false;
    try {
      for await (const frame of this.connect.serverStream("ExecStream", req, signal)) {
        if (signal?.aborted) {
          aborted = true;
          break;
        }
        handleFrame(frame);
      }
    } catch (e) {
      // An abort tears the fetch down: the iterator rejects with an AbortError.
      // That is an intentional kill, not a truncation; fall through and return
      // the partial result rather than the truncation error below.
      if (signal?.aborted || (e instanceof Error && e.name === "AbortError")) {
        aborted = true;
      } else {
        throw e;
      }
    }

    if (!aborted && !sawExit) {
      // The body ended before the terminal exit frame: the stream was
      // truncated or dropped. Surface it as an error rather than a misleading
      // exitCode=0 success.
      throw new AgentRunError(
        "exec stream ended before the terminal exit frame",
        {
          code: "exec_stream_truncated",
          cause:
            "the connection was truncated or dropped; the exit code is unknown",
          remediation:
            "Retry the command; if it persists, inspect the forkd or sandbox-server logs for a dropped connection.",
        },
      );
    }

    if (mapDeadline && !aborted && exitCode === EXEC_TIMEOUT_EXIT_CODE) {
      // The streaming terminal frame reports 124 inside a 200 response (the
      // status header is already sent); surface the typed deadline error so the
      // streaming path matches the blocking path's 504 exec_timeout (issue #216).
      const timeoutS = opts.timeoutSeconds ?? 30;
      throw new ExecutionDeadlineError(
        `command exceeded its ${timeoutS}s execution deadline and was terminated`,
        {
          code: "exec_timeout",
          cause: `command ran past its ${timeoutS}s deadline (exit 124)`,
          remediation:
            "Raise the timeout on the exec call or split the work into shorter steps.",
          context: { timeout_s: timeoutS },
        },
      );
    }

    return {
      exitCode,
      stdout: outParts.join(""),
      stderr: errParts.join(""),
      execTimeMs,
    };
  }

  /**
   * Runs a code snippet in the sandbox's stateful kernel over the Connect
   * RunCodeStream server-streaming RPC (sandbox.v1.Sandbox/RunCodeStream, issue
   * #24). State persists across runCode calls for the sandbox lifetime. Streams
   * stdout/stderr/results via the callbacks and resolves to the full Execution.
   * Requires a base image with the code-interpreter kernel; without it the
   * Execution carries a KernelUnavailable error.
   */
  async runCode(
    code: string,
    opts?: { language?: string; timeoutSeconds?: number } & RunCodeCallbacks,
  ): Promise<Execution> {
    if (opts?.timeoutSeconds !== undefined) {
      validateTimeout(opts.timeoutSeconds);
    }
    // RunCodeStreamRequest (proto-JSON, camelCase): code, language, and the
    // optional timeoutSeconds.
    const req: Record<string, unknown> = {
      code,
      language: opts?.language ?? "python",
    };
    if (opts?.timeoutSeconds !== undefined) {
      req["timeoutSeconds"] = opts.timeoutSeconds;
    }
    return parseRunCodeConnect(this.connect.serverStream("RunCodeStream", req), {
      onStdout: opts?.onStdout,
      onStderr: opts?.onStderr,
      onResult: opts?.onResult,
    });
  }

  /**
   * Tears the sandbox down via the injected terminator. A bare Sandbox with no
   * terminator is a no-op. When bound to a workspace, outputs narrow and enrich
   * the dehydrated revision and checkpoint pairs it with a memory snapshot;
   * returns the bound workspace name (or undefined when unbound or a no-op).
   */
  async terminate(opts?: TerminateOptions): Promise<string | undefined> {
    if (this.terminator) {
      return this.terminator(opts);
    }
    return undefined;
  }
}

export interface RunCodeCallbacks {
  onStdout?: (text: string) => void;
  onStderr?: (text: string) => void;
  onResult?: (result: Result) => void;
}

function decodeStreamBytes(value: unknown): string {
  if (typeof value !== "string") {
    return "";
  }
  try {
    return Buffer.from(value, "base64").toString("utf-8");
  } catch {
    return value;
  }
}

/**
 * Decode a Connect RunResult.data map (proto-JSON map<string,bytes>: every value
 * is a base64 string) back to the MIME->payload form the Result type expects.
 * The guest stores each display value as the raw bytes of the kernel's string
 * output (text/plain is "42"; image/png is the already-base64 PNG string), so
 * base64-decoding recovers exactly the kernel value: text stays text and an
 * image stays its base64 payload, matching Result.text / the image slot.
 */
function decodeResultData(data: Record<string, unknown>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [mime, value] of Object.entries(data ?? {})) {
    if (typeof value !== "string") {
      out[mime] = value === undefined || value === null ? "" : String(value);
      continue;
    }
    try {
      out[mime] = new TextDecoder().decode(b64ToBytes(value));
    } catch {
      out[mime] = value;
    }
  }
  return out;
}

/**
 * Folds a Connect RunCodeStream response-frame stream into an Execution, firing
 * the callbacks live as frames arrive. The proto-JSON frames are the
 * RunCodeResponse oneof: stdout/stderr (base64 bytes), result
 * (RunResult{text,data}), error (RunError{name,value,traceback}), and the
 * terminal exitCode. Result and error payloads are tenant code output and are
 * never logged here.
 */
export async function parseRunCodeConnect(
  source: AsyncIterable<Record<string, unknown>>,
  cb: RunCodeCallbacks,
): Promise<Execution> {
  const ex: Execution = {
    text: null,
    logs: { stdout: [], stderr: [] },
    results: [],
    error: null,
  };
  const td = new TextDecoder();
  let sawExit = false;
  for await (const frame of source) {
    if ("stdout" in frame) {
      const text = td.decode(b64ToBytes(frame["stdout"]));
      ex.logs.stdout.push(text);
      cb.onStdout?.(text);
    } else if ("stderr" in frame) {
      const text = td.decode(b64ToBytes(frame["stderr"]));
      ex.logs.stderr.push(text);
      cb.onStderr?.(text);
    } else if ("result" in frame) {
      const payload = (frame["result"] ?? {}) as {
        text?: string;
        data?: Record<string, unknown>;
      };
      const data = decodeResultData(payload.data ?? {});
      const text = payload.text ?? "";
      const isMain = Boolean(text);
      // The REPL last-value is delivered in RunResult.text; mirror it into the
      // text/plain slot so Result.text resolves the same way the NDJSON path did.
      if (isMain && data["text/plain"] === undefined) {
        data["text/plain"] = text;
      }
      const result: Result = { data, isMainResult: isMain };
      ex.results.push(result);
      if (isMain && text) {
        ex.text = text;
      }
      cb.onResult?.(result);
    } else if ("error" in frame) {
      const payload = (frame["error"] ?? {}) as Partial<ExecutionError>;
      ex.error = {
        name: payload.name ?? "",
        value: payload.value ?? "",
        traceback: payload.traceback ?? [],
      };
    } else if ("exitCode" in frame) {
      sawExit = true;
      return ex;
    }
  }
  if (!sawExit) {
    // The stream ended before the terminal exit frame: it was truncated or
    // dropped. Surface it rather than a misleading clean Execution success.
    throw new AgentRunError("run_code stream ended before the terminal exit frame", {
      code: "run_code_stream_truncated",
      cause: "the connection was truncated or dropped; the result is unknown",
      remediation:
        "Retry the snippet; if it persists, inspect the forkd or sandbox-server logs for a dropped connection.",
    });
  }
  return ex;
}

/**
 * Folds an NDJSON ExecStreamFrame line stream into an Execution, firing
 * callbacks live as frames arrive. Result and error payloads are tenant code
 * output and are never logged.
 */
export async function parseRunCodeStream(
  source: AsyncIterable<string>,
  cb: RunCodeCallbacks,
): Promise<Execution> {
  const ex: Execution = {
    text: null,
    logs: { stdout: [], stderr: [] },
    results: [],
    error: null,
  };
  let sawExit = false;
  for await (const raw of source) {
    const line = raw.trim();
    if (!line) continue;
    const frame = JSON.parse(line) as Record<string, unknown>;
    switch (frame["kind"]) {
      case "stdout": {
        const text = decodeStreamBytes(frame["stdout"]);
        ex.logs.stdout.push(text);
        cb.onStdout?.(text);
        break;
      }
      case "stderr": {
        const text = decodeStreamBytes(frame["stderr"]);
        ex.logs.stderr.push(text);
        cb.onStderr?.(text);
        break;
      }
      case "result": {
        const payload = (frame["result"] ?? {}) as { text?: string; data?: Record<string, string> };
        const text = payload.text ?? "";
        const result: Result = { data: payload.data ?? {}, isMainResult: Boolean(text) };
        ex.results.push(result);
        if (text) ex.text = text;
        cb.onResult?.(result);
        break;
      }
      case "error": {
        const payload = (frame["error"] ?? {}) as Partial<ExecutionError>;
        ex.error = {
          name: payload.name ?? "",
          value: payload.value ?? "",
          traceback: payload.traceback ?? [],
        };
        break;
      }
      case "exit":
        sawExit = true;
        return ex;
    }
  }
  if (!sawExit) {
    // The body ended before the terminal exit frame: the stream was truncated
    // or dropped. Surface it as an error rather than a misleading clean
    // Execution success.
    throw new AgentRunError(
      "run_code stream ended before the terminal exit frame",
      {
        code: "run_code_stream_truncated",
        cause: "the connection was truncated or dropped; the result is unknown",
        remediation:
          "Retry the snippet; if it persists, inspect the forkd or sandbox-server logs for a dropped connection.",
      },
    );
  }
  return ex;
}

/**
 * Normalises an endpoint into a base URL. An endpoint that already carries a
 * scheme is used as-is; a bare host:port (as the cluster status reports) gets
 * an http:// prefix.
 */
export function toBaseUrl(endpoint: string): string {
  if (/^https?:\/\//.test(endpoint)) {
    return endpoint;
  }
  return `http://${endpoint}`;
}
