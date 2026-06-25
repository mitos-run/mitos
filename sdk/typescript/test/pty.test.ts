import { describe, it, expect, afterEach } from "vitest";
import { WebSocketServer, type WebSocket } from "ws";
import { createPty } from "../src/pty.js";
import { encodeFrame, FrameReader, b64ToBytes, bytesToB64 } from "../src/connect.js";

// These tests drive the Pty handle against a fake host that speaks the
// Connect-over-WebSocket Exec protocol (subprotocol connect.sandbox.v1): each
// binary ws message is one enveloped Connect frame carrying a sandbox.v1
// ExecRequest/ExecResponse. The server echoes a stdin frame's bytes back as a
// stdout ExecResponse and ends the stream with a terminal exit frame on "exit\n".

let server: WebSocketServer | undefined;

afterEach(() => {
  server?.close();
  server = undefined;
});

async function decodeMessage(data: Uint8Array): Promise<Record<string, unknown>> {
  const reader = new FrameReader(
    new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(data);
        controller.close();
      },
    }).getReader(),
  );
  const frame = await reader.next();
  if (!frame) {
    throw new Error("expected one enveloped frame");
  }
  return JSON.parse(new TextDecoder().decode(frame.payload)) as Record<string, unknown>;
}

function send(sock: WebSocket, message: Record<string, unknown>, endStream = false): void {
  const payload = new TextEncoder().encode(JSON.stringify(message));
  sock.send(encodeFrame(payload, endStream), { binary: true });
}

function startEchoServer(): Promise<number> {
  return new Promise((resolve) => {
    server = new WebSocketServer({ port: 0 });
    server.on("connection", (sock: WebSocket) => {
      sock.on("message", async (raw: Buffer) => {
        const message = await decodeMessage(new Uint8Array(raw));
        if (message.stdin !== undefined) {
          const bytes = b64ToBytes(message.stdin as string);
          if (new TextDecoder().decode(bytes) === "exit\n") {
            send(sock, { exit: { exitCode: 0 } }, true);
            return;
          }
          send(sock, { stdout: bytesToB64(bytes) });
        }
      });
    });
    server.on("listening", () => {
      const addr = server!.address();
      resolve(typeof addr === "object" && addr ? addr.port : 0);
    });
  });
}

describe("pty", () => {
  it("echoes input and reports exit", async () => {
    const port = await startEchoServer();
    const chunks: Uint8Array[] = [];
    const pty = await createPty({
      url: `ws://127.0.0.1:${port}/sandbox.v1.Sandbox/Exec?sandbox=sb1`,
      cols: 80,
      rows: 24,
      onData: (b) => chunks.push(b),
    });

    pty.sendInput(new TextEncoder().encode("ts-hi\n"));
    await new Promise((r) => setTimeout(r, 200));
    const got = new TextDecoder().decode(
      Uint8Array.from(chunks.flatMap((c) => Array.from(c))),
    );
    expect(got).toBe("ts-hi\n");

    pty.sendInput(new TextEncoder().encode("exit\n"));
    const code = await pty.wait();
    expect(code).toBe(0);
  });

  it("resize does not throw", async () => {
    const port = await startEchoServer();
    const pty = await createPty({
      url: `ws://127.0.0.1:${port}/sandbox.v1.Sandbox/Exec?sandbox=sb1`,
      cols: 80,
      rows: 24,
      onData: () => {},
    });
    pty.resize(120, 40);
    pty.sendInput(new TextEncoder().encode("exit\n"));
    expect(await pty.wait()).toBe(0);
  });
});
