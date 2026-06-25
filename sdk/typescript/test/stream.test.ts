import { describe, it, expect, vi } from "vitest";
import { Sandbox } from "../src/sandbox.js";
import { b64, encodeFrame, streamBody } from "./connect_helpers.js";

// connectResponse builds a Connect server-stream Response: the given data
// messages as enveloped frames, then a terminal clean end-stream frame.
function connectResponse(messages: Array<Record<string, unknown>>): Response {
  return new Response(streamBody(messages), {
    status: 200,
    headers: { "Content-Type": "application/connect+json" },
  });
}

describe("streaming exec (Connect ExecStream)", () => {
  it("invokes onStdout/onStderr per chunk and returns aggregate", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      connectResponse([
        { stdout: b64("out1") },
        { stderr: b64("err1") },
        { stdout: b64("out2") },
        { exit: { exitCode: 7, execTimeMs: 2 } },
      ]),
    );
    vi.stubGlobal("fetch", fetchMock);

    const sb = new Sandbox({ id: "sb1", endpoint: "localhost:8080" });
    const out: string[] = [];
    const err: string[] = [];
    const result = await sb.exec("echo hi", {
      onStdout: (b) => out.push(new TextDecoder().decode(b)),
      onStderr: (b) => err.push(new TextDecoder().decode(b)),
    });

    expect(out.join("")).toBe("out1out2");
    expect(err.join("")).toBe("err1");
    expect(result.exitCode).toBe(7);
    expect(result.stdout).toBe("out1out2");
    // The request hit the Connect ExecStream path with the connect content type.
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toMatch(/\/sandbox\.v1\.Sandbox\/ExecStream$/);
    expect((init as RequestInit).headers).toMatchObject({
      "Content-Type": "application/connect+json",
      "Connect-Protocol-Version": "1",
      "X-Sandbox-Id": "sb1",
    });
    vi.unstubAllGlobals();
  });

  it("reassembles a frame split across two fetch chunks", async () => {
    // Build a single full stream body, then deliver it in two byte slices that
    // cut a frame in half, exercising the FrameReader reassembly.
    const full = streamBody([{ stdout: b64("hello world") }, { exit: { exitCode: 0 } }]);
    const cut = 8; // mid-frame, after the prefix but before the payload ends
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(new Uint8Array(full.subarray(0, cut)));
            controller.enqueue(new Uint8Array(full.subarray(cut)));
            controller.close();
          },
        }),
        { status: 200, headers: { "Content-Type": "application/connect+json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    const sb = new Sandbox({ id: "sb1", endpoint: "localhost:8080" });
    const result = await sb.exec("echo");
    expect(result.stdout).toBe("hello world");
    expect(result.exitCode).toBe(0);
    vi.unstubAllGlobals();
  });

  it("execBackground returns a handle whose wait() drains the stream", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        connectResponse([{ stdout: b64("ready") }, { exit: { exitCode: 0, execTimeMs: 1 } }]),
      ),
    );
    const sb = new Sandbox({ id: "sb1", endpoint: "localhost:8080" });
    const proc = await sb.execBackground("sleep 1");
    const result = await proc.wait();
    expect(result.exitCode).toBe(0);
    expect(result.stdout).toBe("ready");
    vi.unstubAllGlobals();
  });

  // A body that ends before the terminal exit frame is a truncated stream and
  // must surface as an error, not exitCode=0 success.
  it("errors when the stream ends before the exit frame", async () => {
    // Frames arrive, then a clean end-stream frame, but no exit frame.
    const body = Buffer.concat([
      encodeFrame(Buffer.from(JSON.stringify({ stdout: b64("out1") }))),
      encodeFrame(Buffer.from(JSON.stringify({ stdout: b64("out2") }))),
      encodeFrame(Buffer.from("{}"), true),
    ]);
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(body, {
          status: 200,
          headers: { "Content-Type": "application/connect+json" },
        }),
      ),
    );
    const sb = new Sandbox({ id: "sb1", endpoint: "localhost:8080" });

    await expect(sb.exec("echo hi", { onStdout: () => {} })).rejects.toMatchObject({
      name: "AgentRunError",
      code: "exec_stream_truncated",
    });
    vi.unstubAllGlobals();
  });

  // kill() aborts the underlying fetch. The signal must be threaded into fetch so
  // a quiet background process is torn down immediately, not only at the next
  // chunk.
  it("threads the AbortSignal into fetch so kill() aborts promptly", async () => {
    let seenSignal: AbortSignal | undefined;
    const fetchMock = vi.fn().mockImplementation((_url, init: RequestInit) => {
      seenSignal = init.signal ?? undefined;
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          // Emit one early data frame, then block; aborting cancels the stream.
          controller.enqueue(
            new Uint8Array(encodeFrame(Buffer.from(JSON.stringify({ stdout: b64("ready") })))),
          );
          const onAbort = () => {
            controller.error(Object.assign(new Error("aborted"), { name: "AbortError" }));
          };
          if (init.signal?.aborted) {
            onAbort();
          } else {
            init.signal?.addEventListener("abort", onAbort, { once: true });
          }
        },
      });
      return Promise.resolve(
        new Response(body, {
          status: 200,
          headers: { "Content-Type": "application/connect+json" },
        }),
      );
    });
    vi.stubGlobal("fetch", fetchMock);

    const sb = new Sandbox({ id: "sb1", endpoint: "localhost:8080" });
    const proc = await sb.execBackground("sleep 1");
    expect(seenSignal).toBeInstanceOf(AbortSignal);
    expect(seenSignal!.aborted).toBe(false);

    proc.kill();
    expect(seenSignal!.aborted).toBe(true);

    // wait() resolves promptly: the abort is recognised as an intentional kill,
    // not a truncation, despite there being no exit frame.
    const result = await proc.wait();
    expect(result.exitCode).toBe(0);
    vi.unstubAllGlobals();
  });

  // An abort surfacing as a rejected read (AbortError) is treated as a kill, not
  // a truncation error.
  it("treats an AbortError from the stream as a kill, not a truncation", async () => {
    const fetchMock = vi.fn().mockImplementation((_url, init: RequestInit) => {
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          const onAbort = () =>
            controller.error(Object.assign(new Error("aborted"), { name: "AbortError" }));
          init.signal?.addEventListener("abort", onAbort, { once: true });
        },
      });
      return Promise.resolve(
        new Response(body, {
          status: 200,
          headers: { "Content-Type": "application/connect+json" },
        }),
      );
    });
    vi.stubGlobal("fetch", fetchMock);

    const sb = new Sandbox({ id: "sb1", endpoint: "localhost:8080" });
    const proc = await sb.execBackground("sleep 1");
    proc.kill();
    await expect(proc.wait()).resolves.toMatchObject({ exitCode: 0 });
    vi.unstubAllGlobals();
  });
});
