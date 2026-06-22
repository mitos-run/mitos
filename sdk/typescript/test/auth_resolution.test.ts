// Unit tests for the unified bearer-token resolution in the TypeScript SDK.
// Precedence: explicit argument, then MITOS_API_KEY, then the CLI login
// credential file (MITOS_CONFIG_DIR else ~/.config/mitos/credentials.json, the
// "token" field), then undefined (tokenless). A missing, unreadable, or
// non-JSON file is never an error; it just yields no token. The token VALUE is
// never logged.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { resolveToken } from "../src/server.js";

let dir: string;
const saved: Record<string, string | undefined> = {};

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "mitos-auth-"));
  for (const k of ["MITOS_API_KEY", "MITOS_CONFIG_DIR"]) {
    saved[k] = process.env[k];
    delete process.env[k];
  }
});

afterEach(() => {
  rmSync(dir, { recursive: true, force: true });
  for (const k of ["MITOS_API_KEY", "MITOS_CONFIG_DIR"]) {
    if (saved[k] === undefined) delete process.env[k];
    else process.env[k] = saved[k];
  }
});

function writeCreds(token: string) {
  writeFileSync(
    join(dir, "credentials.json"),
    JSON.stringify({ token, email: "a@b.c", default_org: "org-1" }),
  );
}

describe("resolveToken", () => {
  it("uses the credential file when env is unset", () => {
    writeCreds("file-tok");
    process.env.MITOS_CONFIG_DIR = dir;
    expect(resolveToken()).toBe("file-tok");
  });

  it("MITOS_API_KEY overrides the credential file", () => {
    writeCreds("file-tok");
    process.env.MITOS_CONFIG_DIR = dir;
    process.env.MITOS_API_KEY = "env-tok";
    expect(resolveToken()).toBe("env-tok");
  });

  it("an explicit argument overrides everything", () => {
    writeCreds("file-tok");
    process.env.MITOS_CONFIG_DIR = dir;
    process.env.MITOS_API_KEY = "env-tok";
    expect(resolveToken("arg-tok")).toBe("arg-tok");
  });

  it("no file and no env is tokenless (undefined)", () => {
    process.env.MITOS_CONFIG_DIR = dir; // empty dir, no credentials.json
    expect(resolveToken()).toBeUndefined();
  });

  it("invalid JSON is not an error, yields no token", () => {
    writeFileSync(join(dir, "credentials.json"), "{ not valid json");
    process.env.MITOS_CONFIG_DIR = dir;
    expect(resolveToken()).toBeUndefined();
  });

  it("a file without a token field is tokenless", () => {
    writeFileSync(join(dir, "credentials.json"), JSON.stringify({ email: "a@b.c" }));
    process.env.MITOS_CONFIG_DIR = dir;
    expect(resolveToken()).toBeUndefined();
  });
});
