import { afterEach, describe, expect, it, vi } from "vitest";

import { execStream } from "../src/connect-exec.js";

// Connect enveloped-frame flags (mirror connect-exec.ts).
const FLAG_END_STREAM = 0b00000010;

/** Wrap one JSON message in the Connect 5-byte envelope prefix. */
function frame(message: Record<string, unknown>, endStream = false): Uint8Array {
  const payload = new TextEncoder().encode(JSON.stringify(message));
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

/** base64-encode a UTF-8 string the way a proto-JSON bytes field would. */
function b64(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let binary = "";
  for (let i = 0; i < bytes.length; i++) {
    binary += String.fromCharCode(bytes[i]);
  }
  return btoa(binary);
}

/** Decode the single enveloped request frame the client POSTed. */
function decodeRequest(body: Uint8Array): Record<string, unknown> {
  const len =
    ((body[1] << 24) | (body[2] << 16) | (body[3] << 8) | body[4]) >>> 0;
  const payload = body.slice(5, 5 + len);
  return JSON.parse(new TextDecoder().decode(payload)) as Record<
    string,
    unknown
  >;
}

/** Build a streaming Response whose body yields the given enveloped frames. */
function streamResponse(frames: Uint8Array[], init?: ResponseInit): Response {
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const f of frames) {
        controller.enqueue(f);
      }
      controller.close();
    },
  });
  return new Response(stream, { status: 200, ...init });
}

interface Captured {
  url: string;
  headers: Record<string, string>;
  request: Record<string, unknown>;
}

/** Fake the Connect ExecStream server over the global fetch. */
function fakeExecServer(
  frames: Uint8Array[],
  init?: ResponseInit,
): { captured: Captured } {
  const captured: Captured = { url: "", headers: {}, request: {} };
  vi.stubGlobal(
    "fetch",
    async (input: unknown, opts: RequestInit): Promise<Response> => {
      captured.url = String(input);
      captured.headers = Object.fromEntries(
        Object.entries((opts.headers ?? {}) as Record<string, string>),
      );
      captured.request = decodeRequest(opts.body as Uint8Array);
      return streamResponse(frames, init);
    },
  );
  return { captured };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("execStream (Connect sandbox.v1.Sandbox/ExecStream)", () => {
  it("posts the command to the Connect route and returns stdout + exit code", async () => {
    const { captured } = fakeExecServer([
      frame({ stdout: b64("hello\n") }),
      frame({ stderr: b64("warn\n") }),
      frame({ exit: { exitCode: 0 } }),
      frame({}, true),
    ]);

    const result = await execStream(
      "https://sandbox.example",
      "echo hello",
      30,
      { sandboxId: "sbx-1" },
    );

    // The command reaches the server on the Connect ExecStream route.
    expect(captured.url).toBe(
      "https://sandbox.example/sandbox.v1.Sandbox/ExecStream",
    );
    expect(captured.request).toEqual({ command: "echo hello", timeoutSeconds: 30 });
    expect(captured.headers["Content-Type"]).toBe("application/connect+json");
    // The exec result (stdout + exit code) is returned.
    expect(result).toEqual({ exitCode: 0, stdout: "hello\n", stderr: "warn\n" });
  });

  it("sends X-Sandbox-Id and, when a token is present, Authorization Bearer", async () => {
    const { captured } = fakeExecServer([
      frame({ stdout: b64("ok") }),
      frame({ exit: { exitCode: 0 } }),
      frame({}, true),
    ]);

    await execStream("https://sandbox.example", "true", 10, {
      sandboxId: "sbx-7",
      token: "s3cr3t-token",
    });

    expect(captured.headers["X-Sandbox-Id"]).toBe("sbx-7");
    expect(captured.headers["Authorization"]).toBe("Bearer s3cr3t-token");
  });

  it("omits Authorization when no token is set (tokenless standalone server)", async () => {
    const { captured } = fakeExecServer([
      frame({ exit: { exitCode: 0 } }),
      frame({}, true),
    ]);

    await execStream("https://sandbox.example", "true", 5, {
      sandboxId: "sbx-2",
    });

    expect(captured.headers["X-Sandbox-Id"]).toBe("sbx-2");
    expect(captured.headers["Authorization"]).toBeUndefined();
  });

  it("reassembles frames split across stream chunks", async () => {
    // Build the full byte stream, then deliver it one byte at a time so the
    // FrameReader's cross-chunk reassembly is exercised.
    const all = [
      frame({ stdout: b64("part") }),
      frame({ stdout: b64("ial") }),
      frame({ exit: { exitCode: 3 } }),
      frame({}, true),
    ];
    const total = all.reduce((n, f) => n + f.length, 0);
    const merged = new Uint8Array(total);
    let off = 0;
    for (const f of all) {
      merged.set(f, off);
      off += f.length;
    }
    const singleBytes = Array.from(merged).map((b) => Uint8Array.of(b));
    const { captured } = fakeExecServer(singleBytes);

    const result = await execStream("https://sandbox.example", "x", 0, {
      sandboxId: "sbx-3",
    });

    // timeoutSeconds 0 is omitted from the request.
    expect(captured.request).toEqual({ command: "x" });
    expect(result).toEqual({ exitCode: 3, stdout: "partial", stderr: "" });
  });

  it("raises a typed error when the stream ends with an error trailer", async () => {
    fakeExecServer([
      frame({ stdout: b64("partial") }),
      frame(
        { error: { code: "deadline_exceeded", message: "exec timed out" } },
        true,
      ),
    ]);

    await expect(
      execStream("https://sandbox.example", "sleep 99", 1, { sandboxId: "sbx-4" }),
    ).rejects.toThrow(/deadline_exceeded: exec timed out/);
  });

  it("raises on a non-2xx Connect error envelope before the first frame", async () => {
    vi.stubGlobal(
      "fetch",
      async (): Promise<Response> =>
        new Response(
          JSON.stringify({ code: "not_found", message: "no such sandbox" }),
          { status: 404, headers: { "Content-Type": "application/json" } },
        ),
    );

    await expect(
      execStream("https://sandbox.example", "true", 5, { sandboxId: "gone" }),
    ).rejects.toThrow(/404 not_found: no such sandbox/);
  });
});
