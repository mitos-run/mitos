import { describe, it, expect, afterEach } from "vitest";
import { WebSocketServer, type WebSocket } from "ws";
import { createPty } from "../src/pty.js";
import { encodeFrame, FrameReader, b64ToBytes, bytesToB64 } from "../src/connect.js";

// A fake in-process ws server that speaks the enveloped Connect protocol the
// host endpoint internal/daemon/exec_ws.go serves: each binary ws message is
// exactly one Connect enveloped frame carrying a sandbox.v1 protojson message.
// The first client frame must be ExecRequest{open}; a stdin frame is echoed back
// as a stdout ExecResponse; stdin == "exit\n" ends the stream with a terminal
// ExecResponse{exit} carrying the end-stream flag.

let server: WebSocketServer | undefined;

afterEach(() => {
  server?.close();
  server = undefined;
});

interface Seen {
  frames: Array<Record<string, unknown>>;
  subprotocol?: string;
}

// decodeOneFrame turns a single binary ws message (one enveloped frame) into its
// {flag, message} pair. The host sends one frame per message, so a per-message
// FrameReader fed the whole buffer yields exactly that frame.
async function decodeOneFrame(
  data: Uint8Array,
): Promise<{ flag: number; message: Record<string, unknown> }> {
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
    throw new Error("expected one enveloped frame in the binary ws message");
  }
  const message = JSON.parse(new TextDecoder().decode(frame.payload)) as Record<
    string,
    unknown
  >;
  return { flag: frame.flag, message };
}

function send(sock: WebSocket, message: Record<string, unknown>, endStream = false): void {
  const payload = new TextEncoder().encode(JSON.stringify(message));
  sock.send(encodeFrame(payload, endStream), { binary: true });
}

function startConnectPtyServer(seen: Seen): Promise<number> {
  return new Promise((resolve) => {
    server = new WebSocketServer({ port: 0 });
    server.on("connection", (sock: WebSocket) => {
      seen.subprotocol = sock.protocol;
      sock.on("message", async (raw: Buffer, isBinary: boolean) => {
        expect(isBinary).toBe(true);
        const { message } = await decodeOneFrame(new Uint8Array(raw));
        seen.frames.push(message);
        if (message.open !== undefined) {
          return; // The open frame just starts the exec; nothing to echo.
        }
        if (message.stdin !== undefined) {
          const bytes = b64ToBytes(message.stdin as string);
          const decoded = new TextDecoder().decode(bytes);
          if (decoded === "exit\n") {
            send(sock, { exit: { exitCode: 0 } }, true);
            return;
          }
          // Echo the stdin bytes back as a stdout ExecResponse frame.
          send(sock, { stdout: bytesToB64(bytes) });
        }
        // resize frames are recorded in seen.frames but produce no response.
      });
    });
    server.on("listening", () => {
      const addr = server!.address();
      resolve(typeof addr === "object" && addr ? addr.port : 0);
    });
  });
}

describe("pty over Connect ws", () => {
  it("opens with pty.size, echoes stdin, resizes, and exits 0", async () => {
    const seen: Seen = { frames: [] };
    const port = await startConnectPtyServer(seen);
    const chunks: Uint8Array[] = [];
    const pty = await createPty({
      url: `ws://127.0.0.1:${port}/sandbox.v1.Sandbox/Exec?sandbox=sb1`,
      cols: 100,
      rows: 30,
      onData: (b) => chunks.push(b),
    });

    pty.resize(120, 40);
    pty.sendInput(new TextEncoder().encode("ts-hi\n"));
    await new Promise((r) => setTimeout(r, 200));
    const got = new TextDecoder().decode(
      Uint8Array.from(chunks.flatMap((c) => Array.from(c))),
    );
    expect(got).toBe("ts-hi\n");

    pty.sendInput(new TextEncoder().encode("exit\n"));
    const code = await pty.wait();
    expect(code).toBe(0);

    // The subprotocol the client negotiated is the Connect one.
    expect(seen.subprotocol).toBe("connect.sandbox.v1");

    // The FIRST frame must be an ExecRequest{open} carrying pty.size cols/rows.
    const open = seen.frames[0];
    expect(open.open).toBeDefined();
    const ptyOpen = (open.open as { pty?: { size?: { cols?: number; rows?: number } } })
      .pty;
    expect(ptyOpen?.size?.cols).toBe(100);
    expect(ptyOpen?.size?.rows).toBe(30);

    // A resize was delivered as an ExecRequest{resize}.
    const resize = seen.frames.find((f) => f.resize !== undefined);
    expect(resize).toBeDefined();
    expect((resize!.resize as { cols?: number; rows?: number }).cols).toBe(120);
    expect((resize!.resize as { cols?: number; rows?: number }).rows).toBe(40);

    // The server saw the typed input as an enveloped ExecRequest{stdin}.
    const stdin = seen.frames.find((f) => {
      if (f.stdin === undefined) {
        return false;
      }
      return new TextDecoder().decode(b64ToBytes(f.stdin as string)) === "ts-hi\n";
    });
    expect(stdin).toBeDefined();
  });
});
