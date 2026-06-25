// A minimal, dependency-free Connect codec over the global fetch for the
// sandbox.v1.Sandbox runtime protocol (issue #358). The mitos sandbox-server no
// longer serves the legacy JSON exec route; the runtime exec call now speaks
// the Connect sandbox.v1.Sandbox ExecStream RPC. Rather than pull in a
// generated-stub + codegen dependency, this module implements just the slice of
// the Connect wire the plugin needs (the server-streaming ExecStream), mirroring
// the mitos TypeScript SDK's connect.ts.
//
// The Connect streaming shape: POST /sandbox.v1.Sandbox/ExecStream with
// Content-Type application/connect+json and Connect-Protocol-Version: 1. The
// request message is sent as one ENVELOPED frame: a 5-byte prefix (1 flag byte +
// a 4-byte big-endian length) then the JSON message bytes. The response is a
// stream of enveloped ExecResponse frames; the terminal frame sets the
// end-stream flag (0x02) and carries trailers and, on failure, an error object.
// Proto-JSON bytes fields (stdout, stderr) are base64 strings.
//
// The bearer token, when present, rides on Authorization and is never logged.

const CONNECT_SERVICE = "sandbox.v1.Sandbox";
const EXEC_METHOD = "ExecStream";
const SANDBOX_ID_HEADER = "X-Sandbox-Id";
const STREAM_CONTENT_TYPE = "application/connect+json";

// Connect enveloped-frame flags. Bit 1 marks the terminal end-stream frame; bit
// 0 marks a compressed frame (the codec negotiates identity and refuses one).
const FLAG_END_STREAM = 0b00000010;
const FLAG_COMPRESSED = 0b00000001;

// Guards the frame-length prefix so a malformed or hostile length cannot make
// the codec allocate unbounded memory.
const MAX_CONNECT_FRAME_BYTES = 64 * 1024 * 1024; // 64 MiB

/** The result shape the legacy exec path returned to its caller. */
export interface ExecOutcome {
  exitCode: number;
  stdout: string;
  stderr: string;
}

/** Optional auth inputs. The token value is never logged. */
export interface ExecAuth {
  sandboxId: string;
  token?: string;
}

/**
 * Decode a proto-JSON bytes field (a base64 string) to a UTF-8 string. null,
 * undefined, and the empty string all decode to the empty string. Uses atob so
 * the codec is dependency-free and works in both Node and the browser.
 */
function b64ToText(value: unknown): string {
  if (typeof value !== "string" || value === "") {
    return "";
  }
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return new TextDecoder().decode(bytes);
}

/** Wrap one message payload in the Connect 5-byte envelope prefix. */
function encodeFrame(payload: Uint8Array): Uint8Array {
  const out = new Uint8Array(5 + payload.length);
  out[0] = 0;
  const len = payload.length;
  out[1] = (len >>> 24) & 0xff;
  out[2] = (len >>> 16) & 0xff;
  out[3] = (len >>> 8) & 0xff;
  out[4] = len & 0xff;
  out.set(payload, 5);
  return out;
}

/**
 * Reassembles Connect enveloped frames from a response body's ReadableStream
 * reader, buffering raw bytes until a full frame (5-byte prefix + payload) is
 * available, so it is robust to fetch delivering arbitrary chunk sizes and to a
 * frame that spans multiple chunks.
 */
class FrameReader {
  private buf = new Uint8Array(0);
  private done = false;

  constructor(
    private readonly reader: ReadableStreamDefaultReader<Uint8Array>,
  ) {}

  async next(): Promise<{ flag: number; payload: Uint8Array } | undefined> {
    for (;;) {
      const frame = this.tryParse();
      if (frame) {
        return frame;
      }
      if (this.done) {
        return undefined;
      }
      const { done, value } = await this.reader.read();
      if (done) {
        this.done = true;
        continue;
      }
      if (value && value.length > 0) {
        this.append(value);
      }
    }
  }

  private append(chunk: Uint8Array): void {
    const merged = new Uint8Array(this.buf.length + chunk.length);
    merged.set(this.buf, 0);
    merged.set(chunk, this.buf.length);
    this.buf = merged;
  }

  private tryParse(): { flag: number; payload: Uint8Array } | undefined {
    if (this.buf.length < 5) {
      return undefined;
    }
    const flag = this.buf[0];
    const raw =
      (this.buf[1] << 24) |
      (this.buf[2] << 16) |
      (this.buf[3] << 8) |
      this.buf[4];
    // The shift can produce a negative number for a length with the high bit
    // set; coerce to an unsigned 32-bit value before the guard.
    const len = raw >>> 0;
    if (len > MAX_CONNECT_FRAME_BYTES) {
      throw new Error(
        `connect: response frame too large (${len} bytes); the runtime sent an unexpectedly large frame`,
      );
    }
    if (this.buf.length < 5 + len) {
      return undefined;
    }
    const payload = this.buf.slice(5, 5 + len);
    this.buf = this.buf.slice(5 + len);
    return { flag, payload };
  }
}

/**
 * Run a command in the sandbox over the Connect server-streaming ExecStream RPC
 * and return the accumulated outcome (exit code plus stdout/stderr), the same
 * shape the legacy exec path returned to its caller.
 *
 * Sends X-Sandbox-Id and, when a token is set, Authorization: Bearer <token>.
 * The token value is never logged.
 */
export async function execStream(
  serverUrl: string,
  command: string,
  timeoutSeconds: number,
  auth: ExecAuth,
): Promise<ExecOutcome> {
  const base = serverUrl.replace(/\/+$/, "");
  const url = `${base}/${CONNECT_SERVICE}/${EXEC_METHOD}`;

  const headers: Record<string, string> = {
    "Content-Type": STREAM_CONTENT_TYPE,
    "Connect-Protocol-Version": "1",
    [SANDBOX_ID_HEADER]: auth.sandboxId,
  };
  if (auth.token) {
    headers["Authorization"] = `Bearer ${auth.token}`;
  }

  const request: Record<string, unknown> = { command };
  if (timeoutSeconds > 0) {
    request.timeoutSeconds = timeoutSeconds;
  }
  const body = encodeFrame(new TextEncoder().encode(JSON.stringify(request)));

  const resp = await fetch(url, { method: "POST", headers, body });
  if (!resp.ok) {
    // A streaming RPC that fails before the first frame returns a normal HTTP
    // error body (the Connect error envelope), not an end-stream frame.
    const text = await resp.text().catch(() => "");
    throw new Error(
      `sandbox ExecStream: ${resp.status} ${connectErrorMessage(text)}`,
    );
  }
  if (!resp.body) {
    throw new Error("sandbox ExecStream: empty response body");
  }

  let stdout = "";
  let stderr = "";
  let exitCode: number | undefined;

  const reader = resp.body.getReader();
  const fr = new FrameReader(reader);
  try {
    for (;;) {
      const frame = await fr.next();
      if (!frame) {
        break;
      }
      if (frame.flag & FLAG_COMPRESSED) {
        throw new Error(
          "sandbox ExecStream returned a compressed frame the codec did not negotiate",
        );
      }
      if (frame.flag & FLAG_END_STREAM) {
        const err = endStreamError(frame.payload);
        if (err) {
          throw new Error(`sandbox ExecStream: ${err}`);
        }
        break;
      }
      if (frame.payload.length === 0) {
        continue;
      }
      const msg = JSON.parse(new TextDecoder().decode(frame.payload)) as {
        stdout?: unknown;
        stderr?: unknown;
        exit?: { exitCode?: unknown };
      };
      if (msg.stdout !== undefined) {
        stdout += b64ToText(msg.stdout);
      }
      if (msg.stderr !== undefined) {
        stderr += b64ToText(msg.stderr);
      }
      if (msg.exit && typeof msg.exit === "object") {
        const code = (msg.exit as { exitCode?: unknown }).exitCode;
        exitCode = typeof code === "number" ? code : 0;
      }
    }
  } finally {
    // Release the reader so an early-broken iteration tears the connection down
    // rather than leaking it.
    try {
      await reader.cancel();
    } catch {
      // Already closed or canceled: nothing to do.
    }
  }

  return { exitCode: exitCode ?? 0, stdout, stderr };
}

/**
 * Inspect a terminal end-stream frame payload. A payload carrying an
 * {"error":{code,message}} object returns a human-readable string; a clean end
 * (empty, trailers only, or non-JSON) returns undefined.
 */
function endStreamError(payload: Uint8Array): string | undefined {
  if (payload.length === 0) {
    return undefined;
  }
  let end: unknown;
  try {
    end = JSON.parse(new TextDecoder().decode(payload));
  } catch {
    return undefined;
  }
  if (!end || typeof end !== "object") {
    return undefined;
  }
  const err = (end as { error?: { code?: string; message?: string } }).error;
  if (err && typeof err === "object") {
    const code = err.code ?? "";
    const message = err.message ?? "";
    return [code, message].filter((s) => s !== "").join(": ") || "stream error";
  }
  return undefined;
}

/** Pull the message out of a Connect error envelope body, else return it raw. */
function connectErrorMessage(body: string): string {
  try {
    const parsed = JSON.parse(body) as { code?: string; message?: string };
    if (parsed && typeof parsed === "object") {
      const code = parsed.code ?? "";
      const message = parsed.message ?? "";
      const joined = [code, message].filter((s) => s !== "").join(": ");
      if (joined !== "") {
        return joined;
      }
    }
  } catch {
    // Not the Connect JSON envelope; fall through to the raw body.
  }
  return body;
}
