import { describe, expect, it } from "vitest";
import {
  AgentRunError,
  ExecutionDeadlineError,
  IdleTimeoutError,
  NotFoundError,
  RateLimitedError,
  RequestCanceledError,
  TimeoutTooLargeError,
  UnauthorizedError,
  MAX_EXEC_TIMEOUT_SECONDS,
  validateTimeout,
} from "../src/errors.js";

function envelope(
  status: number,
  code: string,
  context?: Record<string, unknown>,
): AgentRunError {
  const body = JSON.stringify({
    error: { code, message: `${code} happened`, remediation: "do x", context },
  });
  return AgentRunError.fromResponse(status, body);
}

const cases: Array<[string, new (...a: never[]) => AgentRunError]> = [
  ["idle_timeout", IdleTimeoutError],
  ["exec_timeout", ExecutionDeadlineError],
  ["canceled", RequestCanceledError],
  ["rate_limited", RateLimitedError],
  ["not_found", NotFoundError],
  ["unauthorized", UnauthorizedError],
  ["timeout_too_large", TimeoutTooLargeError],
];

describe("typed discriminable errors (issue #216)", () => {
  for (const [code, cls] of cases) {
    it(`maps server code ${code} to its typed subclass`, () => {
      const err = envelope(400, code);
      expect(err).toBeInstanceOf(cls);
      // Every typed subclass is still an AgentRunError for a broad catch.
      expect(err).toBeInstanceOf(AgentRunError);
      expect(err.code).toBe(code);
      expect(err.remediation).toBe("do x");
    });
  }

  it("falls back to the base AgentRunError for an unknown code", () => {
    const err = envelope(500, "some_new_code");
    expect(err).toBeInstanceOf(AgentRunError);
    expect(err).not.toBeInstanceOf(IdleTimeoutError);
    expect(err.code).toBe("some_new_code");
  });

  it("tells idle vs deadline vs canceled apart by TYPE, not message", () => {
    const idle = envelope(410, "idle_timeout");
    const deadline = envelope(504, "exec_timeout");
    const canceled = envelope(499, "canceled");

    expect(idle).toBeInstanceOf(IdleTimeoutError);
    expect(idle).not.toBeInstanceOf(ExecutionDeadlineError);
    expect(idle).not.toBeInstanceOf(RequestCanceledError);

    expect(deadline).toBeInstanceOf(ExecutionDeadlineError);
    expect(deadline).not.toBeInstanceOf(IdleTimeoutError);

    expect(canceled).toBeInstanceOf(RequestCanceledError);
    expect(canceled).not.toBeInstanceOf(IdleTimeoutError);
  });

  it("preserves context on a typed subclass", () => {
    const err = envelope(400, "timeout_too_large", {
      requested_s: 1000,
      max_timeout_s: 100,
    });
    expect(err).toBeInstanceOf(TimeoutTooLargeError);
    expect(err.context.max_timeout_s).toBe(100);
  });

  it("preserves the typed subclass for a status fallback (no envelope)", () => {
    // A 404 with no envelope body still routes to the typed NotFoundError via
    // the status-derived code.
    const err = AgentRunError.fromResponse(404, "");
    expect(err).toBeInstanceOf(NotFoundError);
  });
});

describe("client-side timeout ceiling (issue #216)", () => {
  it("rejects an over-ceiling timeout with a typed error, never clamps", () => {
    expect(() => validateTimeout(MAX_EXEC_TIMEOUT_SECONDS)).not.toThrow();
    let thrown: unknown;
    try {
      validateTimeout(MAX_EXEC_TIMEOUT_SECONDS + 1);
    } catch (e) {
      thrown = e;
    }
    expect(thrown).toBeInstanceOf(TimeoutTooLargeError);
    expect((thrown as TimeoutTooLargeError).context.max_timeout_s).toBe(
      MAX_EXEC_TIMEOUT_SECONDS,
    );
  });
});
