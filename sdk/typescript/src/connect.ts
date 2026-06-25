// A dependency-free Connect codec over the global fetch for the
// sandbox.v1.Sandbox runtime protocol (issue #24). The native direct-mode
// runtime calls (exec, files, run_code) speak the Connect sandbox.v1.Sandbox
// service instead of the legacy JSON /v1/* routes. Rather than pull in a
// generated-stub + codegen dependency, this module implements the Connect wire
// directly over fetch, mirroring the Go SDK's connect.go and the Python SDK's
// _connect.py. The proto-JSON message shapes come straight from
// proto/sandbox/v1/sandbox.proto (camelCase field names; bytes fields are
// base64 strings).
//
// The Connect protocol, the two shapes used here:
//
//   - UNARY (List, Stat, Mkdir, Remove): POST /sandbox.v1.Sandbox/<Method> with
//     Content-Type application/json and the proto-JSON request as the body. On
//     2xx the body is the proto-JSON reply. On non-2xx the body is the Connect
//     error envelope {"code","message"}.
//
//   - STREAM (ExecStream, ReadFile, RunCodeStream server-stream; WriteFile
//     client-stream): Content-Type application/connect+json with
//     Connect-Protocol-Version: 1. Every message is an ENVELOPED frame: a 5-byte
//     prefix (1 flag byte + 4-byte big-endian length) then the JSON message
//     bytes. The final server frame sets the end-stream flag (0x02); its payload
//     carries trailers and, on failure, an error object. The client sends its
//     request message(s) as plain (flag 0x00) enveloped frames and closes the
//     request body; it does NOT send an end-stream frame.
//
// The SDK's direct-mode exec/run_code only send the opening message (no live
// stdin), so each streaming call is half-duplex: the full request body (one or
// more enveloped frames) is sent, then the response frames are read
// incrementally from the fetch response body ReadableStream, reassembling frames
// across chunk boundaries.
//
// The bearer token rides on Authorization and is never logged; it is redacted
// from any error cause via the shared redactor in errors.ts.

import { AgentRunError, errorForCode, redact } from "./errors.js";

// The Connect service name and the headers every call carries. The server
// routes a sandbox by the X-Sandbox-Id header (both in the tokenless standalone
// case and the hosted/forkd bearer case).
const CONNECT_SERVICE = "sandbox.v1.Sandbox";
const SANDBOX_ID_HEADER = "X-Sandbox-Id";
const UNARY_CONTENT_TYPE = "application/json";
const STREAM_CONTENT_TYPE = "application/connect+json";

// The end-stream flag on a Connect enveloped frame (bit 1). The final server
// frame sets it; its payload carries trailers and an optional error object.
const FLAG_END_STREAM = 0b00000010;
// The compressed flag (bit 0). The SDK negotiates identity encoding and never
// sends or accepts a compressed frame, so this is only used to detect and refuse
// an unexpected compressed response frame.
const FLAG_COMPRESSED = 0b00000001;

// Guards the frame-length prefix so a malformed or hostile length cannot make
// the SDK allocate unbounded memory.
const MAX_CONNECT_FRAME_BYTES = 64 * 1024 * 1024; // 64 MiB

// The Connect textual error codes the Sandbox service returns. The SDK's typed
// error layer keys its subclass and remediation on the SDK `code`, not an HTTP
// status, so connectError below maps the execution-relevant Connect codes onto
// the SDK's own stable codes (deadline_exceeded -> exec_timeout,
// resource_exhausted -> rate_limited, unauthenticated -> unauthorized) and
// passes the rest through; an unmapped code yields the base AgentRunError. The
// canonical Connect-code-to-HTTP-status table lives in the Go/Python SDKs, which
// carry a status field on their error type; the TS error type does not.

/** The Connect RPC path for a Sandbox method name (e.g. "ReadFile"). */
function connectPath(method: string): string {
  return `/${CONNECT_SERVICE}/${method}`;
}

/**
 * Decode a proto-JSON bytes field (a base64 string) to raw bytes. null,
 * undefined, and the empty string all decode to empty bytes. Uses atob so the
 * codec is dependency-free and works in both Node and the browser.
 */
export function b64ToBytes(value: unknown): Uint8Array {
  if (typeof value !== "string" || value === "") {
    return new Uint8Array();
  }
  const binary = atob(value);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    out[i] = binary.charCodeAt(i);
  }
  return out;
}

/** Encode raw bytes as a proto-JSON bytes field (a base64 string). */
export function bytesToB64(bytes: Uint8Array): string {
  let binary = "";
  for (let i = 0; i < bytes.length; i++) {
    binary += String.fromCharCode(bytes[i]);
  }
  return btoa(binary);
}

// encodeFrame wraps one message payload in the Connect 5-byte envelope prefix:
// 1 flag byte, then a 4-byte big-endian length, then the payload.
function encodeFrame(payload: Uint8Array, endStream = false): Uint8Array {
  const out = new Uint8Array(5 + payload.length);
  out[0] = endStream ? FLAG_END_STREAM : 0;
  const len = payload.length;
  out[1] = (len >>> 24) & 0xff;
  out[2] = (len >>> 16) & 0xff;
  out[3] = (len >>> 8) & 0xff;
  out[4] = len & 0xff;
  out.set(payload, 5);
  return out;
}

// FrameReader reassembles Connect enveloped frames from a response body's
// ReadableStream reader, buffering raw bytes until a full frame (5-byte prefix +
// payload) is available, so it is robust to fetch delivering arbitrary chunk
// sizes and to a frame that spans multiple chunks.
class FrameReader {
  private buf = new Uint8Array(0);
  private done = false;

  constructor(private readonly reader: ReadableStreamDefaultReader<Uint8Array>) {}

  // next returns the next enveloped frame as {flag, payload}, or undefined when
  // the body ends with no further full frame. It pulls more chunks from the
  // reader as needed and concatenates them onto the internal buffer.
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

  // tryParse pulls one complete frame off the front of the buffer, or returns
  // undefined when fewer than a full frame's bytes are buffered.
  private tryParse(): { flag: number; payload: Uint8Array } | undefined {
    if (this.buf.length < 5) {
      return undefined;
    }
    const flag = this.buf[0];
    const length =
      (this.buf[1] << 24) | (this.buf[2] << 16) | (this.buf[3] << 8) | this.buf[4];
    // The shift above can produce a negative number for a length with the high
    // bit set; coerce to an unsigned 32-bit value before the guard.
    const len = length >>> 0;
    if (len > MAX_CONNECT_FRAME_BYTES) {
      throw new AgentRunError(
        `connect: response frame too large (${len} bytes)`,
        {
          code: "internal_error",
          cause: "a Connect response frame exceeded the SDK frame-size guard",
          remediation: "Report this; the runtime sent an unexpectedly large frame.",
        },
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
 * Build a typed AgentRunError from a Connect error code and message. The Connect
 * textual code is mapped to the SDK's own stable codes so the streaming path
 * raises the SAME typed subclass as the legacy path (deadline_exceeded ->
 * exec_timeout -> ExecutionDeadlineError, resource_exhausted -> rate_limited ->
 * RateLimitedError, not_found -> NotFoundError, ...). The message is redacted of
 * any bearer token before it becomes the cause.
 */
function connectError(
  code: string,
  message: string,
  token?: string,
): AgentRunError {
  const cause = redact(message || "", token);
  // The execution-deadline code is named exec_timeout in the SDK's typed
  // hierarchy; map the Connect deadline_exceeded onto it (and resource_exhausted
  // onto rate_limited) so streaming and legacy paths raise the same type. Other
  // Connect codes that match an SDK code (not_found, unauthenticated ->
  // unauthorized, canceled) resolve to their typed subclass via errorForCode.
  let sdkCode = code || "internal_error";
  if (code === "deadline_exceeded") {
    sdkCode = "exec_timeout";
  } else if (code === "resource_exhausted") {
    sdkCode = "rate_limited";
  } else if (code === "unauthenticated") {
    sdkCode = "unauthorized";
  }
  return errorForCode(`sandbox RPC failed: ${code || "internal"}`, {
    code: sdkCode,
    cause: cause || `connect error ${code}`,
    remediation: "Inspect the request against the sandbox.v1.Sandbox contract.",
    context: { connect_code: code },
  });
}

// connectErrorFromBody turns a non-2xx Connect response body into a typed
// AgentRunError. It prefers the Connect error envelope {"code","message"}; when
// the body is not the envelope (a proxy 502, a transport error) it falls back to
// the HTTP status with the raw redacted body as the cause.
function connectErrorFromBody(
  status: number,
  body: string,
  token?: string,
): AgentRunError {
  let code = "";
  let message = "";
  try {
    const parsed = JSON.parse(body) as unknown;
    if (parsed && typeof parsed === "object") {
      const env = parsed as { code?: string; message?: string };
      code = env.code ?? "";
      message = env.message ?? "";
    }
  } catch {
    // Not the Connect JSON envelope; fall through to the status-based error.
  }
  if (code !== "") {
    return connectError(code, message, token);
  }
  // Reuse the HTTP-status error builder so a non-envelope body (a proxy page,
  // an HTML 502) still surfaces a typed AgentRunError with the token redacted.
  return AgentRunError.fromResponse(status, body, token);
}

/**
 * Speaks the Connect sandbox.v1.Sandbox protocol over the global fetch.
 *
 * Constructed with the server base URL, the per-sandbox id, and the optional
 * bearer token. Every call sets X-Sandbox-Id and, when a token is set,
 * Authorization: Bearer <token>. The token value is never logged; it is redacted
 * from any error cause.
 */
export class ConnectClient {
  private readonly baseUrl: string;

  constructor(
    baseUrl: string,
    private readonly sandboxId: string,
    private readonly token?: string,
  ) {
    this.baseUrl = baseUrl.replace(/\/+$/, "");
  }

  private headers(contentType: string): Record<string, string> {
    const h: Record<string, string> = {
      "Content-Type": contentType,
      [SANDBOX_ID_HEADER]: this.sandboxId,
    };
    if (this.token) {
      h["Authorization"] = `Bearer ${this.token}`;
    }
    return h;
  }

  /**
   * Make a unary Connect call and return the proto-JSON reply as a record.
   * Throws a typed AgentRunError on a Connect error envelope or a non-2xx
   * status. Used for List, Stat, Mkdir, Remove.
   */
  async unary(
    method: string,
    message: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const headers = this.headers(UNARY_CONTENT_TYPE);
    const resp = await fetch(this.baseUrl + connectPath(method), {
      method: "POST",
      headers,
      body: JSON.stringify(message),
    });
    if (!resp.ok) {
      const text = await resp.text().catch(() => "");
      throw connectErrorFromBody(resp.status, text, this.token);
    }
    const text = await resp.text();
    if (text === "") {
      return {};
    }
    return JSON.parse(text) as Record<string, unknown>;
  }

  /**
   * Open a server-streaming Connect call: send `message` as the single opening
   * enveloped frame, then yield each response message as a record the instant
   * its frame arrives. Used for ExecStream, ReadFile, and RunCodeStream.
   */
  serverStream(
    method: string,
    message: Record<string, unknown>,
    signal?: AbortSignal,
  ): AsyncIterable<Record<string, unknown>> {
    return this.bidi(method, [message], signal);
  }

  /**
   * Send the given client messages as enveloped request frames, then yield each
   * response message record. The request body is fully buffered (direct-mode
   * streams send only the opening message(s), so the call is half-duplex); the
   * response is read incrementally from the body ReadableStream, reassembling
   * frames across chunk boundaries.
   *
   * On the terminal end-stream frame: a payload with an error object throws a
   * typed AgentRunError; a clean end simply stops the iterator. Used for the
   * client-stream WriteFile (open + data frames) too.
   */
  async *bidi(
    method: string,
    messages: Array<Record<string, unknown>>,
    signal?: AbortSignal,
  ): AsyncIterable<Record<string, unknown>> {
    const encoder = new TextEncoder();
    const frames = messages.map((m) => encodeFrame(encoder.encode(JSON.stringify(m))));
    // Concatenate the request frames into one body buffer.
    const total = frames.reduce((n, f) => n + f.length, 0);
    const body = new Uint8Array(total);
    let off = 0;
    for (const f of frames) {
      body.set(f, off);
      off += f.length;
    }

    const resp = await fetch(this.baseUrl + connectPath(method), {
      method: "POST",
      headers: {
        ...this.headers(STREAM_CONTENT_TYPE),
        "Connect-Protocol-Version": "1",
      },
      body,
      signal,
    });

    if (!resp.ok) {
      // A streaming RPC that fails before the first frame returns a normal HTTP
      // error body (the Connect error envelope), not an end-stream frame.
      const text = await resp.text().catch(() => "");
      throw connectErrorFromBody(resp.status, text, this.token);
    }
    if (!resp.body) {
      return;
    }

    const reader = resp.body.getReader();
    const fr = new FrameReader(reader);
    try {
      for (;;) {
        const frame = await fr.next();
        if (!frame) {
          // A clean transport EOF with no explicit end-stream frame ends the
          // iterator (the caller's truncation guard decides if that is an
          // error for its RPC).
          return;
        }
        if (frame.flag & FLAG_COMPRESSED) {
          throw new AgentRunError(
            "sandbox RPC returned a compressed frame the SDK did not negotiate",
            {
              code: "internal_error",
              cause: "unexpected compressed Connect frame",
              remediation: "Report this; the SDK negotiates identity encoding.",
            },
          );
        }
        if (frame.flag & FLAG_END_STREAM) {
          this.handleEndStream(frame.payload);
          return;
        }
        if (frame.payload.length === 0) {
          continue;
        }
        const text = new TextDecoder().decode(frame.payload);
        yield JSON.parse(text) as Record<string, unknown>;
      }
    } finally {
      // Release the reader so an aborted or early-broken iteration tears the
      // connection down rather than leaking it.
      try {
        await reader.cancel();
      } catch {
        // Already closed or canceled: nothing to do.
      }
    }
  }

  // handleEndStream inspects the terminal end-stream frame payload. A payload
  // carrying an {"error":{code,message}} object throws a typed AgentRunError; a
  // clean end (empty or trailers only) returns normally. A non-JSON payload is
  // tolerated as a clean end.
  private handleEndStream(payload: Uint8Array): void {
    if (payload.length === 0) {
      return;
    }
    let end: unknown;
    try {
      end = JSON.parse(new TextDecoder().decode(payload));
    } catch {
      return; // Malformed trailer: treat as a clean end.
    }
    if (!end || typeof end !== "object") {
      return;
    }
    const err = (end as { error?: { code?: string; message?: string } }).error;
    if (err && typeof err === "object") {
      const code = err.code ?? "";
      const message = err.message ?? "";
      throw connectError(code, message, this.token);
    }
  }
}
