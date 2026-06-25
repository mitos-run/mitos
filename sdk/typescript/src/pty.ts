// An interactive pseudo-terminal in a sandbox, mirroring E2B's sandbox.pty.
// Transport is a WebSocket to the host's Connect endpoint
// {wsBase}/sandbox.v1.Sandbox/Exec?sandbox={id} (subprotocol connect.sandbox.v1),
// gated by the per-sandbox bearer token. The token is sent in the Authorization
// header (the Node `ws` client supports request headers, which the platform
// global WebSocket does not). It is never logged.
//
// The wire is Connect-over-WebSocket: every ws message is BINARY and carries
// exactly ONE Connect enveloped frame (a 5-byte prefix then the protojson
// payload). The framing is the same one connect.ts uses for the HTTP streaming
// path, so this module REUSES its encodeFrame, FrameReader, and the base64
// helpers rather than duplicating them. The CLIENT sends sandbox.v1 ExecRequest
// messages: the FIRST frame MUST be {open:{pty:{size:{cols,rows}}}}, then
// {stdin:"<b64>"} for keystrokes and {resize:{cols,rows}} for window changes. The
// SERVER sends ExecResponse messages: {stdout|stderr:"<b64>"} -> onData(bytes) and
// a terminal {exit:{exitCode}} frame carrying the end-stream flag.

import WebSocket from "ws";
import { encodeFrame, FrameReader, b64ToBytes, bytesToB64 } from "./connect.js";

// The ws subprotocol the host's Connect-over-ws Exec endpoint negotiates.
const CONNECT_WS_SUBPROTOCOL = "connect.sandbox.v1";

export interface CreatePtyOptions {
  /** Full ws:// or wss:// URL: {wsBase}/sandbox.v1.Sandbox/Exec?sandbox=<id>. */
  url: string;
  /** Terminal width in columns for the opening ExecRequest{open}. */
  cols: number;
  /** Terminal height in rows for the opening ExecRequest{open}. */
  rows: number;
  /** Per-sandbox bearer token; sent in the Authorization header. */
  token?: string;
  /** Receives raw terminal output bytes as they arrive. */
  onData: (data: Uint8Array) => void;
}

const encoder = new TextEncoder();

// sendFrame protojson-encodes one sandbox.v1 message and writes it as a single
// BINARY enveloped Connect frame. The host reads one frame per binary message.
function sendFrame(ws: WebSocket, message: Record<string, unknown>): void {
  const payload = encoder.encode(JSON.stringify(message));
  ws.send(encodeFrame(payload), { binary: true });
}

/** A live interactive terminal handle. */
export class Pty {
  private readonly ws: WebSocket;
  private readonly onData: (data: Uint8Array) => void;
  private exitCode: number | undefined;
  private readonly exited: Promise<number>;
  private resolveExit!: (code: number) => void;

  constructor(ws: WebSocket, onData: (data: Uint8Array) => void) {
    this.ws = ws;
    this.onData = onData;
    this.exited = new Promise<number>((resolve) => {
      this.resolveExit = resolve;
    });
    this.ws.on("message", (raw: WebSocket.RawData) => {
      void this.handleMessage(raw);
    });
    this.ws.on("close", () => {
      if (this.exitCode === undefined) {
        this.exitCode = -1;
        this.resolveExit(-1);
      }
    });
  }

  // handleMessage decodes one binary ws message (exactly one Connect enveloped
  // frame) as a sandbox.v1 ExecResponse. stdout/stderr bytes go to onData; the
  // terminal exit frame resolves wait() and closes the socket.
  private async handleMessage(raw: WebSocket.RawData): Promise<void> {
    const bytes = toUint8Array(raw);
    const reader = new FrameReader(singleChunkReader(bytes));
    const frame = await reader.next();
    if (!frame) {
      return;
    }
    const resp = JSON.parse(new TextDecoder().decode(frame.payload)) as {
      stdout?: string;
      stderr?: string;
      exit?: { exitCode?: number; exit_code?: number };
    };
    if (resp.stdout !== undefined) {
      this.onData(b64ToBytes(resp.stdout));
    }
    if (resp.stderr !== undefined) {
      this.onData(b64ToBytes(resp.stderr));
    }
    if (resp.exit !== undefined) {
      // camelCase exitCode per protojson; accept exit_code defensively.
      const code = resp.exit.exitCode ?? resp.exit.exit_code ?? 0;
      this.exitCode = code;
      this.resolveExit(code);
      this.ws.close();
    }
  }

  /** Send raw keystroke bytes to the shell as an ExecRequest{stdin}. */
  sendInput(data: Uint8Array): void {
    sendFrame(this.ws, { stdin: bytesToB64(data) });
  }

  /** Resize the terminal (TIOCSWINSZ in the guest, then SIGWINCH) via
   * ExecRequest{resize}. */
  resize(cols: number, rows: number): void {
    sendFrame(this.ws, { resize: { cols, rows } });
  }

  /** Force-close; the guest kills the shell process group on disconnect. */
  kill(): void {
    this.ws.close();
    if (this.exitCode === undefined) {
      this.exitCode = -1;
      this.resolveExit(-1);
    }
  }

  /** Resolve with the shell exit code (or -1 if the connection dropped before
   * a terminal exit frame). */
  wait(): Promise<number> {
    return this.exited;
  }
}

/** Open the Connect Exec WebSocket and resolve once it is open, after sending
 * the opening ExecRequest{open} carrying the pty size. The bearer token rides the
 * Authorization request header (supported by the Node `ws` client), so the host's
 * exec auth gate sees it on the upgrade. */
export function createPty(opts: CreatePtyOptions): Promise<Pty> {
  return new Promise((resolve, reject) => {
    const headers: Record<string, string> = {};
    if (opts.token) {
      headers["Authorization"] = `Bearer ${opts.token}`;
    }
    const ws = new WebSocket(opts.url, [CONNECT_WS_SUBPROTOCOL], { headers });
    const pty = new Pty(ws, opts.onData);
    ws.on("open", () => {
      // The FIRST frame on the stream MUST be the open ExecRequest with the
      // terminal size; the host opens the pty before accepting stdin.
      sendFrame(ws, { open: { pty: { size: { cols: opts.cols, rows: opts.rows } } } });
      resolve(pty);
    });
    ws.on("error", (err: Error) =>
      reject(new Error(`pty websocket error: ${err.message}`)),
    );
  });
}

// toUint8Array normalizes a ws RawData (Buffer, ArrayBuffer, or Buffer[]) into a
// single Uint8Array view of the binary frame.
function toUint8Array(raw: WebSocket.RawData): Uint8Array {
  if (Array.isArray(raw)) {
    return new Uint8Array(Buffer.concat(raw));
  }
  if (raw instanceof ArrayBuffer) {
    return new Uint8Array(raw);
  }
  // A Node Buffer is a Uint8Array; copy its exact byte window.
  return new Uint8Array(raw.buffer, raw.byteOffset, raw.byteLength);
}

// singleChunkReader adapts one in-memory byte chunk to the ReadableStream reader
// FrameReader consumes, so the same reassembly logic decodes a ws message.
function singleChunkReader(
  bytes: Uint8Array,
): ReadableStreamDefaultReader<Uint8Array> {
  return new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(bytes);
      controller.close();
    },
  }).getReader();
}
