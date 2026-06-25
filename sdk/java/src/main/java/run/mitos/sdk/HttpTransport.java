// HttpTransport is a thin wrapper over java.net.http.HttpClient that attaches the
// optional bearer token, sends and parses JSON, and turns any non-2xx response
// into a MitosException with the token redacted from the body. The token is held
// in memory only and is never logged. Mirrors the TypeScript HttpClient and the
// Ruby SandboxServer request path.
package run.mitos.sdk;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.Map;

final class HttpTransport {

    private final String baseUrl;
    private final String token;
    private final HttpClient client;

    HttpTransport(String baseUrl, String token) {
        this.baseUrl = baseUrl.replaceAll("/+$", "");
        // An empty token is treated as no token, so the standalone (tokenless)
        // server gets no Authorization header at all.
        this.token = (token == null || token.isEmpty()) ? null : token;
        this.client = HttpClient.newBuilder()
                .connectTimeout(Duration.ofSeconds(30))
                .build();
    }

    /** The configured bearer token, or null. Package-private for redaction reuse. */
    String token() {
        return token;
    }

    /** The resolved base URL (trailing slashes stripped). Package-private so the
     * Connect runtime client targets the same server as the REST transport. */
    String baseUrl() {
        return baseUrl;
    }

    /** The shared HttpClient. Package-private so the Connect runtime client
     * reuses one client (and its connection pool) for the whole SDK. */
    HttpClient client() {
        return client;
    }

    /** GETs path and returns the parsed JSON tree (Map / List / scalar), or null
     * on an empty body. Throws MitosException on a non-2xx status. */
    Object get(String path) {
        HttpRequest.Builder b = HttpRequest.newBuilder()
                .uri(URI.create(baseUrl + path))
                .GET();
        return send(b);
    }

    /** POSTs a JSON body and returns the parsed JSON tree, or null on an empty
     * body. extraHeaders carries the Idempotency-Key on creating calls. Throws
     * MitosException on a non-2xx status. */
    Object post(String path, Object body, Map<String, String> extraHeaders) {
        String json = Json.encode(body);
        HttpRequest.Builder b = HttpRequest.newBuilder()
                .uri(URI.create(baseUrl + path))
                .header("Content-Type", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(json));
        if (extraHeaders != null) {
            for (Map.Entry<String, String> e : extraHeaders.entrySet()) {
                b.header(e.getKey(), e.getValue());
            }
        }
        return send(b);
    }

    /** Issues a DELETE. Throws MitosException on a non-2xx status. */
    void delete(String path) {
        HttpRequest.Builder b = HttpRequest.newBuilder()
                .uri(URI.create(baseUrl + path))
                .DELETE();
        send(b);
    }

    private Object send(HttpRequest.Builder builder) {
        if (token != null) {
            builder.header("Authorization", "Bearer " + token);
        }
        HttpResponse<String> resp;
        try {
            resp = client.send(builder.build(), HttpResponse.BodyHandlers.ofString());
        } catch (Exception e) {
            // Network-level failure: redact the token from the message defensively,
            // though a JDK exception message does not contain it.
            throw new MitosException(
                    "sandbox API request failed: " + redact(e.getMessage()),
                    "transport_error",
                    redact(String.valueOf(e)),
                    "Check the base URL is reachable and the network allows the request.",
                    0);
        }
        int status = resp.statusCode();
        if (status < 200 || status >= 300) {
            throw errorFromResponse(status, resp.body());
        }
        String text = resp.body();
        if (text == null || text.isEmpty()) {
            return null;
        }
        return Json.parse(text);
    }

    // errorFromResponse builds a MitosException from a non-2xx response. It prefers
    // the structured envelope {error:{code,message,cause,remediation}} and falls
    // back to status-derived defaults for an older or non-mitos server. Any bearer
    // token echoed in the body is redacted before it becomes a cause.
    private MitosException errorFromResponse(int status, String rawBody) {
        String body = redact(rawBody == null ? "" : rawBody);
        String code = statusCode(status);
        String message = "sandbox API request failed: HTTP " + status + " (" + code + ")";
        String cause = body.trim().isEmpty() ? "HTTP " + status : body.trim();
        String remediation = statusRemediation(status);

        try {
            Object parsed = Json.parse(body);
            if (parsed instanceof Map<?, ?> map) {
                Object err = map.get("error");
                if (err instanceof Map<?, ?> e) {
                    code = nonEmpty(asString(e.get("code")), code);
                    message = nonEmpty(asString(e.get("message")), message);
                    cause = nonEmpty(redact(asString(e.get("cause"))), cause);
                    remediation = nonEmpty(asString(e.get("remediation")), remediation);
                } else if (err instanceof String s) {
                    cause = nonEmpty(redact(s), cause);
                }
            }
        } catch (RuntimeException ignored) {
            // Not JSON: keep the status-derived defaults with the text body as cause.
        }

        return new MitosException(message, code, cause, remediation, status);
    }

    private static String asString(Object o) {
        return o == null ? null : o.toString();
    }

    private static String nonEmpty(String value, String fallback) {
        return (value == null || value.isEmpty()) ? fallback : value;
    }

    /** Replaces every occurrence of the configured token with [REDACTED]. A null
     * or empty token is a no-op. Mirrors internal/mcp redact. */
    String redact(String text) {
        if (text == null) {
            return "";
        }
        if (token == null || token.isEmpty()) {
            return text;
        }
        return text.replace(token, "[REDACTED]");
    }

    private static final Map<Integer, String> STATUS_CODE = Map.ofEntries(
            Map.entry(400, "bad_request"),
            Map.entry(401, "unauthorized"),
            Map.entry(403, "forbidden"),
            Map.entry(404, "not_found"),
            Map.entry(409, "conflict"),
            Map.entry(413, "request_too_large"),
            Map.entry(429, "rate_limited"),
            Map.entry(500, "internal_error"),
            Map.entry(503, "unavailable"));

    private static final Map<Integer, String> STATUS_REMEDIATION = Map.of(
            401, "Check the API key is set and authorizes this request.",
            403, "Check the API key is set and authorizes this request.",
            404, "Confirm the sandbox id exists and is Ready before calling.",
            413, "Reduce the request payload size.",
            429, "Back off and retry the request after a short delay.");

    private static String statusCode(int status) {
        String c = STATUS_CODE.get(status);
        if (c != null) {
            return c;
        }
        return status >= 500 ? "server_error" : "request_failed";
    }

    private static String statusRemediation(int status) {
        String r = STATUS_REMEDIATION.get(status);
        if (r != null) {
            return r;
        }
        if (status >= 500) {
            return "Retry the request; if it persists, inspect the sandbox-server logs.";
        }
        return "Inspect the request fields against the sandbox API contract.";
    }
}
