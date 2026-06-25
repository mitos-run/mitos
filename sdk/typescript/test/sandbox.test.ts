import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { AddressInfo } from "node:net";

import { Sandbox } from "../src/sandbox.js";
import { AgentRunError, ExecutionDeadlineError } from "../src/errors.js";
import { b64, decodeFrames, streamBody } from "./connect_helpers.js";

interface Recorded {
  method?: string;
  url?: string;
  auth?: string;
  sandboxId?: string;
  contentType?: string;
  raw: Buffer;
  // The decoded body: a single JSON object for the legacy /v1/* routes, or the
  // array of Connect request frames for a /sandbox.v1.Sandbox/* call.
  json?: unknown;
  frames?: Array<Record<string, unknown>>;
}

let server: Server;
let baseUrl: string;
let recorded: Recorded[];
let responder: (req: IncomingMessage, rec: Recorded, res: ServerResponse) => void;

beforeEach(async () => {
  recorded = [];
  server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c) => chunks.push(c as Buffer));
    req.on("end", () => {
      const raw = Buffer.concat(chunks);
      const url = req.url ?? "";
      const isConnect = url.startsWith("/sandbox.v1.Sandbox/");
      const contentType = req.headers["content-type"];
      const rec: Recorded = {
        method: req.method,
        url,
        auth: req.headers["authorization"] as string | undefined,
        sandboxId: req.headers["x-sandbox-id"] as string | undefined,
        contentType,
        raw,
      };
      // A streaming Connect call carries enveloped frames; a unary Connect call
      // and the legacy /v1/* routes carry plain JSON.
      if (isConnect && contentType === "application/connect+json") {
        rec.frames = decodeFrames(raw);
      } else if (raw.length > 0) {
        rec.json = JSON.parse(raw.toString("utf-8"));
      }
      recorded.push(rec);
      responder(req, rec, res);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address() as AddressInfo;
  baseUrl = `127.0.0.1:${addr.port}`;
});

afterEach(async () => {
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

// connectStream ends a Connect server-stream response (application/connect+json
// enveloped frames + a terminal end-stream frame).
function connectStream(
  res: ServerResponse,
  messages: Array<Record<string, unknown>>,
  error?: { code: string; message: string },
) {
  res.setHeader("content-type", "application/connect+json");
  res.end(streamBody(messages, error));
}

describe("Sandbox.exec (Connect ExecStream)", () => {
  it("posts to /sandbox.v1.Sandbox/ExecStream with the bearer + sandbox headers and aggregates", async () => {
    responder = (_req, _rec, res) =>
      connectStream(res, [
        { stdout: b64("hi") },
        { stderr: b64("err") },
        { exit: { exitCode: 7, execTimeMs: 12.5 } },
      ]);
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl, token: "tok-1" });
    const result = await sandbox.exec("echo hi", { timeoutSeconds: 9 });

    expect(result).toEqual({
      exitCode: 7,
      stdout: "hi",
      stderr: "err",
      execTimeMs: 12.5,
    });
    const call = recorded[0];
    expect(call.url).toBe("/sandbox.v1.Sandbox/ExecStream");
    expect(call.auth).toBe("Bearer tok-1");
    expect(call.sandboxId).toBe("sbx-1");
    expect(call.contentType).toBe("application/connect+json");
    // The single request frame is the ExecStreamRequest (camelCase).
    expect(call.frames).toEqual([{ command: "echo hi", timeoutSeconds: 9 }]);
  });

  it("omits timeoutSeconds when not provided", async () => {
    responder = (_req, _rec, res) =>
      connectStream(res, [{ exit: { exitCode: 0, execTimeMs: 1 } }]);
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    const result = await sandbox.exec("ls");
    expect(result.exitCode).toBe(0);
    expect(result.stdout).toBe("");
    expect(recorded[0].frames).toEqual([{ command: "ls" }]);
    // Tokenless: no Authorization header is sent.
    expect(recorded[0].auth).toBeUndefined();
  });

  it("maps the terminal exit code 124 to a typed ExecutionDeadlineError", async () => {
    responder = (_req, _rec, res) =>
      connectStream(res, [{ exit: { exitCode: 124, execTimeMs: 30000 } }]);
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    await expect(sandbox.exec("sleep 999", { timeoutSeconds: 30 })).rejects.toBeInstanceOf(
      ExecutionDeadlineError,
    );
  });
});

describe("Sandbox.files (Connect file RPCs)", () => {
  it("read concatenates ReadFile chunks and decodes them", async () => {
    responder = (_req, _rec, res) =>
      connectStream(res, [{ data: b64("file ") }, { data: b64("body") }]);
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    const out = await sandbox.files.read("/etc/hosts");
    expect(out).toBe("file body");
    expect(recorded[0].url).toBe("/sandbox.v1.Sandbox/ReadFile");
    expect(recorded[0].frames).toEqual([{ path: "/etc/hosts" }]);
  });

  it("write streams an open frame and a base64 data frame", async () => {
    responder = (_req, _rec, res) => connectStream(res, [{ bytesWritten: "4" }]);
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    await sandbox.files.write("/tmp/x", "data", { mode: 0o600 });
    expect(recorded[0].url).toBe("/sandbox.v1.Sandbox/WriteFile");
    expect(recorded[0].frames).toEqual([
      { open: { path: "/tmp/x", mode: 0o600 } },
      { data: b64("data") },
    ]);
  });

  it("write omits mode when not given", async () => {
    responder = (_req, _rec, res) => connectStream(res, [{ bytesWritten: "4" }]);
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    await sandbox.files.write("/tmp/x", "data");
    expect(recorded[0].frames).toEqual([
      { open: { path: "/tmp/x" } },
      { data: b64("data") },
    ]);
  });

  it("list calls the unary List RPC and maps proto-JSON FileInfo entries", async () => {
    responder = (_req, _rec, res) => {
      res.setHeader("content-type", "application/json");
      res.end(
        JSON.stringify({
          entries: [
            { name: "a", isDir: false, size: 3, mode: 420, modifiedAtUnix: 1700000000 },
            { name: "sub", isDir: true, size: 0 },
          ],
          nextPageToken: "",
        }),
      );
    };
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    const entries = await sandbox.files.list("/workspace");
    expect(recorded[0].url).toBe("/sandbox.v1.Sandbox/List");
    expect(recorded[0].contentType).toBe("application/json");
    expect(recorded[0].json).toEqual({ parent: "/workspace" });
    expect(entries).toEqual([
      { name: "a", isDir: false, size: 3, mode: 420, modifiedAt: "1700000000" },
      { name: "sub", isDir: true, size: 0, mode: 0, modifiedAt: undefined },
    ]);
  });

  it("list defaults the parent to /", async () => {
    responder = (_req, _rec, res) => {
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ entries: [] }));
    };
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    await sandbox.files.list();
    expect(recorded[0].json).toEqual({ parent: "/" });
  });
});

describe("Sandbox Connect errors and validation", () => {
  it("rejects an unsafe sandbox id before any request", () => {
    expect(() => new Sandbox({ id: "../etc", endpoint: baseUrl })).toThrow(AgentRunError);
    expect(() => new Sandbox({ id: "a/b", endpoint: baseUrl })).toThrow(AgentRunError);
    // No request should have reached the server.
    expect(recorded.length).toBe(0);
  });

  it("surfaces a non-2xx Connect error envelope as an AgentRunError without the token", async () => {
    const token = "leaky-token-value";
    responder = (_req, _rec, res) => {
      res.writeHead(500, { "content-type": "application/json" });
      res.end(JSON.stringify({ code: "internal", message: `failure ${token}` }));
    };
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl, token });
    let caught: AgentRunError | undefined;
    try {
      await sandbox.exec("boom");
    } catch (e) {
      caught = e as AgentRunError;
    }
    expect(caught).toBeInstanceOf(AgentRunError);
    expect(JSON.stringify(caught)).not.toContain(token);
    expect(caught!.code).toBe("internal");
  });

  it("raises the typed error carried on a Connect end-stream error frame", async () => {
    responder = (_req, _rec, res) =>
      connectStream(
        res,
        [{ stdout: b64("partial") }],
        { code: "not_found", message: "no such sandbox" },
      );
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    let caught: AgentRunError | undefined;
    try {
      await sandbox.exec("boom");
    } catch (e) {
      caught = e as AgentRunError;
    }
    expect(caught).toBeInstanceOf(AgentRunError);
    expect(caught!.name).toBe("NotFoundError");
    expect(caught!.code).toBe("not_found");
  });
});

describe("Sandbox.terminate", () => {
  it("invokes the injected terminator", async () => {
    responder = (_req, _rec, res) => res.end("{}");
    let called = false;
    const sandbox = new Sandbox({
      id: "sbx-1",
      endpoint: baseUrl,
      terminator: async () => {
        called = true;
      },
    });
    await sandbox.terminate();
    expect(called).toBe(true);
  });
});

describe("Sandbox lifecycle (issue #218)", () => {
  it("setTimeout posts the new TTL and returns the deadline", async () => {
    responder = (_req, _rec, res) => {
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ status: "ok", deadline_unix: 1700000600, timeout_seconds: 600 }));
    };
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl, token: "tok-1" });
    const deadline = await sandbox.setTimeout(600);
    expect(deadline).toBe(1700000600);
    expect(recorded[0].url).toBe("/v1/set_timeout");
    expect(recorded[0].json).toEqual({ sandbox: "sbx-1", timeout_seconds: 600 });
  });

  it("setTimeout rejects an over-ceiling value without a request", async () => {
    responder = (_req, _rec, res) => res.end("{}");
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    await expect(sandbox.setTimeout(10 ** 9)).rejects.toThrow();
    expect(recorded.length).toBe(0);
  });

  it("pause and resume post to the lifecycle endpoints", async () => {
    responder = (_req, _rec, res) => {
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ status: "ok" }));
    };
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl, token: "tok-1" });
    await sandbox.pause();
    await sandbox.resume();
    expect(recorded[0].url).toBe("/v1/pause");
    expect(recorded[1].url).toBe("/v1/resume");
  });
});
