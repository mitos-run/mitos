import { afterEach, describe, expect, it, vi } from "vitest";

import { mitos } from "../index";

/**
 * These tests stub global fetch to stand in for the hosted Mitos control plane,
 * so they exercise the create and destroy REST wire shapes deterministically
 * without a live server or a real API key. The exec and file paths ride the
 * Connect streaming protocol and are covered by the live smoke test in the
 * README (against https://api.mitos.run), not here.
 */

type FetchCall = { url: string; init?: RequestInit };

function stubFetch(handler: (call: FetchCall) => Response): FetchCall[] {
  const calls: FetchCall[] = [];
  vi.stubGlobal("fetch", (input: unknown, init?: RequestInit) => {
    const url = typeof input === "string" ? input : String(input);
    const call = { url, init };
    calls.push(call);
    return Promise.resolve(handler(call));
  });
  return calls;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("mitos provider", () => {
  it("is a factory that names the provider 'mitos'", () => {
    const provider = mitos({ apiKey: "test-key" });
    expect(provider.name).toBe("mitos");
    expect(typeof provider.sandbox.create).toBe("function");
    expect(typeof provider.sandbox.destroy).toBe("function");
  });

  it("requires an API key on create", async () => {
    const provider = mitos({});
    const prev = process.env.MITOS_API_KEY;
    delete process.env.MITOS_API_KEY;
    try {
      await expect(provider.sandbox.create()).rejects.toThrow(/Missing Mitos API key/);
    } finally {
      if (prev !== undefined) process.env.MITOS_API_KEY = prev;
    }
  });

  it("forks the default python template against the hosted base URL with bearer auth", async () => {
    const calls = stubFetch(({ url }) => {
      if (url.endsWith("/v1/fork")) {
        return jsonResponse({
          id: "sandbox-abcd1234",
          template_id: "python",
          endpoint: "https://api.mitos.run",
          fork_time_ms: 84,
        });
      }
      return jsonResponse({}, 404);
    });

    const provider = mitos({ apiKey: "test-key" });
    const sandbox = await provider.sandbox.create();

    expect(sandbox.sandboxId).toBe("sandbox-abcd1234");

    const forkCall = calls.find((c) => c.url.endsWith("/v1/fork"));
    expect(forkCall).toBeDefined();
    expect(forkCall!.url).toBe("https://api.mitos.run/v1/fork");
    expect(forkCall!.init?.method).toBe("POST");
    const headers = forkCall!.init?.headers as Record<string, string>;
    expect(headers["Authorization"]).toBe("Bearer test-key");
    expect(JSON.parse(String(forkCall!.init?.body)).template).toBe("python");
  });

  it("honors a custom baseUrl and template", async () => {
    const calls = stubFetch(({ url }) => {
      if (url.endsWith("/v1/fork")) {
        return jsonResponse({
          id: "sandbox-xyz",
          template_id: "node",
          endpoint: "http://localhost:8080",
          fork_time_ms: 5,
        });
      }
      return jsonResponse({}, 404);
    });

    const provider = mitos({
      apiKey: "k",
      baseUrl: "http://localhost:8080",
      template: "node",
    });
    const sandbox = await provider.sandbox.create();

    expect(sandbox.sandboxId).toBe("sandbox-xyz");
    const forkCall = calls.find((c) => c.url.endsWith("/v1/fork"));
    expect(forkCall!.url).toBe("http://localhost:8080/v1/fork");
    expect(JSON.parse(String(forkCall!.init?.body)).template).toBe("node");
  });

  it("destroys a sandbox by id via DELETE and tolerates a 404", async () => {
    const calls = stubFetch(({ url }) => {
      if (url.includes("/v1/sandboxes/")) {
        return new Response("", { status: 200 });
      }
      return jsonResponse({}, 404);
    });

    const provider = mitos({ apiKey: "test-key" });
    await provider.sandbox.destroy("sandbox-abcd1234");

    const delCall = calls.find((c) => c.init?.method === "DELETE");
    expect(delCall).toBeDefined();
    expect(delCall!.url).toBe(
      "https://api.mitos.run/v1/sandboxes/sandbox-abcd1234",
    );

    // A 404 (already gone) is swallowed rather than thrown.
    stubFetch(() => jsonResponse({ error: { code: "not_found" } }, 404));
    await expect(
      provider.sandbox.destroy("sandbox-missing"),
    ).resolves.toBeUndefined();
  });
});
