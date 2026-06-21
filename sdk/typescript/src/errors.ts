// Errors for the mitos TypeScript SDK. AgentRunError carries an
// LLM-legible code, cause, and remediation so a failure can be acted on
// programmatically. fromResponse builds one from a non-2xx HTTP response and
// redacts any echo of the bearer token from the body first, so a token a
// hostile or misconfigured server reflects into its error body never surfaces.

export interface AgentRunErrorOptions {
  code: string;
  cause?: string;
  remediation?: string;
  context?: Record<string, unknown>;
}

/**
 * An error from the SDK. `code` is a stable machine-readable identifier;
 * `cause` is the underlying detail (server body, redacted); `remediation` is a
 * short actionable hint; `context` is the structured envelope context (ids,
 * paths, op names; never a secret). The token never appears in any of these
 * fields. Mirrors the server envelope in docs/api/errors.md.
 */
export class AgentRunError extends Error {
  readonly code: string;
  readonly errorCause?: string;
  readonly remediation?: string;
  readonly context: Record<string, unknown>;

  constructor(message: string, opts: AgentRunErrorOptions) {
    super(message);
    // Use the concrete constructor name so a typed subclass (IdleTimeoutError,
    // ExecutionDeadlineError, ...) reports itself; instanceof is the contract,
    // this just makes logs and stack traces legible.
    this.name = new.target.name;
    this.code = opts.code;
    this.errorCause = opts.cause;
    this.remediation = opts.remediation;
    this.context = opts.context ?? {};
  }

  /**
   * Builds an AgentRunError from a non-2xx HTTP response. The body is redacted
   * for the token before it becomes the error `cause`, so a reflected token is
   * never surfaced in a message, log, or thrown value.
   */
  static fromResponse(
    status: number,
    bodyText: string,
    token?: string,
  ): AgentRunError {
    const safeBody = redact(bodyText, token).trim();
    let code = codeForStatus(status);
    let message = `sandbox API request failed: HTTP ${status} (${code})`;
    let cause = safeBody === "" ? `HTTP ${status}` : safeBody;
    let remediation = remediationForStatus(status);
    let context: Record<string, unknown> = {};

    try {
      const parsed = JSON.parse(safeBody) as unknown;
      if (parsed && typeof parsed === "object" && "error" in parsed) {
        const err = (parsed as { error: unknown }).error;
        if (err && typeof err === "object") {
          const e = err as {
            code?: string;
            message?: string;
            cause?: string;
            remediation?: string;
            context?: Record<string, unknown>;
          };
          code = e.code || code;
          message = e.message || message;
          cause = (e.cause && redact(e.cause, token)) || cause;
          remediation = e.remediation || remediation;
          if (e.context && typeof e.context === "object") {
            context = e.context;
          }
        } else if (typeof err === "string") {
          // Legacy bare {"error": "msg"} body.
          cause = redact(err, token) || cause;
        }
      }
    } catch {
      // Not JSON; keep the status-derived defaults with the text body as cause.
    }

    return errorForCode(message, { code, cause, remediation, context });
  }
}

/**
 * The sandbox was reaped after exceeding its idle timeout, so the call hit a
 * sandbox that is no longer running. Distinct from NotFoundError (never existed)
 * and ExecutionDeadlineError (a per-command deadline). Server code
 * `idle_timeout` (HTTP 410).
 */
export class IdleTimeoutError extends AgentRunError {}

/**
 * A command or run_code execution ran past its requested timeout (its execution
 * deadline) and was terminated. Distinct from IdleTimeoutError (sandbox
 * inactivity). Server code `exec_timeout` (HTTP 504); also raised when an exec
 * returns the conventional timeout exit code 124.
 */
export class ExecutionDeadlineError extends AgentRunError {}

/**
 * The request was canceled by the caller (the client hung up or the request was
 * aborted) before it completed. Server code `canceled` (HTTP 499).
 */
export class RequestCanceledError extends AgentRunError {}

/**
 * The request rate limit was exceeded. Distinct from too_many_streams (a
 * concurrent-stream ceiling): this is a per-window request-rate refusal. Server
 * code `rate_limited` (HTTP 429); context.retry_after_ms carries the back-off.
 */
export class RateLimitedError extends AgentRunError {}

/** No such sandbox. Server code `not_found` (HTTP 404). */
export class NotFoundError extends AgentRunError {}

/**
 * The per-sandbox bearer token is missing or invalid. Server code `unauthorized`
 * (HTTP 401).
 */
export class UnauthorizedError extends AgentRunError {}

/**
 * The requested timeout exceeds the server ceiling and was REJECTED, never
 * silently reduced (the determinism rule, issue #216). Server code
 * `timeout_too_large` (HTTP 400); context.max_timeout_s carries the ceiling.
 */
export class TimeoutTooLargeError extends AgentRunError {}

// Maps the server error `code` (the apierr catalogue, docs/api/errors.md) to the
// typed subclass a caller branches on. A code absent here yields the base
// AgentRunError so an unknown or newly added code never breaks a client.
const CODE_TO_CLASS: Record<
  string,
  new (message: string, opts: AgentRunErrorOptions) => AgentRunError
> = {
  idle_timeout: IdleTimeoutError,
  exec_timeout: ExecutionDeadlineError,
  canceled: RequestCanceledError,
  rate_limited: RateLimitedError,
  not_found: NotFoundError,
  unauthorized: UnauthorizedError,
  timeout_too_large: TimeoutTooLargeError,
};

/**
 * Construct the typed AgentRunError subclass for a server `code`. An unknown
 * code falls back to the base AgentRunError so a caller's `instanceof
 * AgentRunError` keeps working as the catalogue grows (issue #216).
 */
export function errorForCode(
  message: string,
  opts: AgentRunErrorOptions,
): AgentRunError {
  const Cls = CODE_TO_CLASS[opts.code] ?? AgentRunError;
  return new Cls(message, opts);
}

/**
 * The exec/run_code timeout ceiling (seconds). Mirrors the server default
 * (--max-exec-timeout-seconds, 24h): a requested timeout over it is REJECTED
 * with a typed TimeoutTooLargeError, never silently reduced (issue #216).
 */
export const MAX_EXEC_TIMEOUT_SECONDS = 86400;

/** The conventional exit code the guest reports for a command killed at its
 * execution deadline (matching the shell `timeout` utility). */
export const EXEC_TIMEOUT_EXIT_CODE = 124;

/**
 * Reject a requested timeout over the ceiling with a typed TimeoutTooLargeError
 * (issue #216): the SDK never silently clamps a requested deadline, it rejects
 * it so the deadline you set is the deadline you get.
 */
export function validateTimeout(timeoutSeconds: number): void {
  if (timeoutSeconds > MAX_EXEC_TIMEOUT_SECONDS) {
    throw new TimeoutTooLargeError(
      `requested timeout ${timeoutSeconds}s exceeds the ceiling of ${MAX_EXEC_TIMEOUT_SECONDS}s`,
      {
        code: "timeout_too_large",
        cause: `requested timeout ${timeoutSeconds}s exceeds the ceiling ${MAX_EXEC_TIMEOUT_SECONDS}s`,
        remediation: `Request a timeout at or below the ceiling (${MAX_EXEC_TIMEOUT_SECONDS}s); the timeout is rejected, never silently reduced.`,
        context: {
          requested_s: timeoutSeconds,
          max_timeout_s: MAX_EXEC_TIMEOUT_SECONDS,
        },
      },
    );
  }
}

/**
 * Replaces every occurrence of a non-empty token in `text` with "[REDACTED]".
 * An empty or undefined token is a no-op. Mirrors internal/mcp redact.
 */
export function redact(text: string, token?: string): string {
  if (!token) {
    return text;
  }
  return text.split(token).join("[REDACTED]");
}

function codeForStatus(status: number): string {
  switch (status) {
    case 400:
      return "bad_request";
    case 401:
      return "unauthorized";
    case 403:
      return "forbidden";
    case 404:
      return "not_found";
    case 409:
      return "conflict";
    case 413:
      return "request_too_large";
    case 429:
      return "rate_limited";
    case 500:
      return "internal_error";
    case 503:
      return "unavailable";
    default:
      if (status >= 500) {
        return "server_error";
      }
      return "request_failed";
  }
}

function remediationForStatus(status: number): string {
  switch (status) {
    case 401:
    case 403:
      return "Check the sandbox bearer token is set and authorizes this sandbox.";
    case 404:
      return "Confirm the sandbox id exists and is Ready before calling.";
    case 413:
      return "Reduce the request payload size (file content is hex-encoded and bounded by the server).";
    case 429:
      return "Back off and retry the request after a short delay.";
    default:
      if (status >= 500) {
        return "Retry the request; if it persists, inspect the forkd or sandbox-server logs.";
      }
      return "Inspect the request fields against the sandbox API contract.";
  }
}
