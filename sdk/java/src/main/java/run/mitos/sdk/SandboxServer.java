// Client for the standalone / hosted sandbox-server REST API (direct mode, no
// Kubernetes). Mirrors the Python SandboxServer (sdk/python/mitos/direct.py),
// the TypeScript SandboxServer (sdk/typescript/src/server.ts), the Ruby
// Mitos::SandboxServer, and the Rust direct-mode client.
//
// fork() returns a Sandbox bound to this server: exec round-trips through the
// server URL and terminate issues DELETE /v1/sandboxes/{id}.
//
// Base URL precedence: the constructor argument, then MITOS_BASE_URL, then the
// hosted production endpoint https://mitos.run. The bearer token (constructor
// argument, then MITOS_API_KEY, then the CLI login credential file) is optional;
// when present it rides on the Authorization: Bearer header. The standalone
// server is tokenless and ignores it; the hosted front door verifies it. The
// token VALUE is never logged and is redacted from any error body.
package run.mitos.sdk;

import java.security.SecureRandom;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.regex.Pattern;

/** Thin direct-mode client for the mitos sandbox-server REST API. */
public final class SandboxServer {

    /** The sandbox id allowlist: start with an alphanumeric, then up to 63
     * alphanumeric, underscore, or hyphen characters. Mirrors daemon/validate.go,
     * the TypeScript validSandboxId, and the Ruby SANDBOX_ID_RE. */
    static final Pattern SANDBOX_ID_RE =
            Pattern.compile("^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$");

    /** The default template build wait in seconds, matching the other SDKs. */
    public static final int DEFAULT_INIT_WAIT_SECONDS = 5;

    private static final SecureRandom RANDOM = new SecureRandom();
    private static final char[] HEX = "0123456789abcdef".toCharArray();

    private final String url;
    private final HttpTransport transport;

    /**
     * Builds a client using the resolved base URL and bearer token. With no
     * arguments the base URL is MITOS_BASE_URL or the hosted endpoint and the
     * token is MITOS_API_KEY or the CLI login credential file.
     */
    public SandboxServer() {
        this(null, null);
    }

    /**
     * Builds a client.
     *
     * @param baseUrl the base URL, or null to use MITOS_BASE_URL then the hosted
     *                endpoint
     * @param apiKey  the bearer token, or null to use MITOS_API_KEY then the CLI
     *                login credential file then tokenless
     */
    public SandboxServer(String baseUrl, String apiKey) {
        this.url = AuthResolver.resolveBaseUrl(baseUrl);
        this.transport = new HttpTransport(this.url, AuthResolver.resolveToken(apiKey));
    }

    /** The resolved base URL this client targets. */
    public String url() {
        return url;
    }

    HttpTransport transport() {
        return transport;
    }

    /** Lists the templates known to the server. */
    public List<Template> listTemplates() {
        Object out = transport.get("/v1/templates");
        List<Template> result = new ArrayList<>();
        if (out instanceof List<?> list) {
            for (Object item : list) {
                result.add(toTemplate(asObject(item)));
            }
        }
        return result;
    }

    /** Creates the template named {@code id} with the default build wait. */
    public Template createTemplate(String id) {
        return createTemplate(id, DEFAULT_INIT_WAIT_SECONDS, null);
    }

    /**
     * Creates (or builds) the template named {@code id}. Sends a fresh
     * Idempotency-Key so a retried create returns the same template rather than a
     * duplicate (matching the other SDKs).
     *
     * @param id              the template id
     * @param initWaitSeconds how long the server waits for the init to settle
     * @param idempotencyKey  an explicit key, or null to generate a fresh one
     */
    public Template createTemplate(String id, int initWaitSeconds, String idempotencyKey) {
        Map<String, Object> body = new LinkedHashMap<>();
        body.put("id", id);
        body.put("init_wait_seconds", initWaitSeconds);
        Map<String, String> headers = creatingHeaders(idempotencyKey);
        Object out = transport.post("/v1/templates", body, headers);
        return toTemplate(asObject(out));
    }

    /** Forks a sandbox from {@code template} with a generated id. */
    public Sandbox fork(String template) {
        return fork(template, null, null);
    }

    /** Forks a sandbox from {@code template} with an explicit id. */
    public Sandbox fork(String template, String id) {
        return fork(template, id, null);
    }

    /**
     * Forks a sandbox from a named template. When {@code id} is null a
     * "sandbox-&lt;hex&gt;" id is generated. The id is validated against the
     * allowlist; an invalid id throws a {@link MitosException} BEFORE any request
     * is sent. Sends a fresh Idempotency-Key so a retried fork returns the same
     * sandbox rather than a duplicate. Returns a {@link Sandbox} bound to this
     * server.
     *
     * @param template       the template id to fork from
     * @param id             the sandbox id, or null to generate one
     * @param idempotencyKey an explicit key, or null to generate a fresh one
     */
    public Sandbox fork(String template, String id, String idempotencyKey) {
        String sandboxId = (id == null) ? randomSandboxId() : id;
        requireValidSandboxId(sandboxId,
                "Pass a sandbox id of alphanumerics, underscore, or hyphen, up to 64 chars.");
        Map<String, Object> body = new LinkedHashMap<>();
        body.put("template", template);
        body.put("id", sandboxId);
        Map<String, String> headers = creatingHeaders(idempotencyKey);
        Object out = transport.post("/v1/fork", body, headers);
        Map<String, Object> data = asObject(out);
        String resolvedId = asString(data.get("id"));
        if (resolvedId == null || resolvedId.isEmpty()) {
            resolvedId = sandboxId;
        }
        // Exec and files round-trip through the server URL (the returned endpoint
        // is the server's own address); terminate deletes via the server.
        return new Sandbox(resolvedId, url, this);
    }

    /** Lists the live sandboxes known to the server. */
    public List<ServerSandbox> listSandboxes() {
        Object out = transport.get("/v1/sandboxes");
        List<ServerSandbox> result = new ArrayList<>();
        if (out instanceof List<?> list) {
            for (Object item : list) {
                result.add(toServerSandbox(asObject(item)));
            }
        }
        return result;
    }

    /** Issues DELETE /v1/sandboxes/{id}. Called by {@link Sandbox#terminate}. */
    public void terminate(String id) {
        requireValidSandboxId(id,
                "Terminate only ids that match the sandbox id allowlist.");
        transport.delete("/v1/sandboxes/" + id);
    }

    /** Whether {@code id} matches the sandbox id allowlist. */
    public static boolean validSandboxId(String id) {
        return id != null && SANDBOX_ID_RE.matcher(id).matches();
    }

    private static void requireValidSandboxId(String id, String remediation) {
        if (!validSandboxId(id)) {
            throw new MitosException(
                    "invalid sandbox id: " + (id == null ? "null" : "\"" + id + "\""),
                    "invalid_sandbox_id",
                    "id must match " + SANDBOX_ID_RE.pattern(),
                    remediation,
                    0);
        }
    }

    // creatingHeaders returns an Idempotency-Key header for a creating call
    // (template create, fork). A creating call that carries an idempotency key is
    // safe to retry: the server returns the resource the first call created
    // instead of a duplicate. The key VALUE is an opaque caller token, never a
    // secret, so it travels as a plain header.
    private Map<String, String> creatingHeaders(String idempotencyKey) {
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Idempotency-Key",
                (idempotencyKey == null || idempotencyKey.isEmpty())
                        ? newIdempotencyKey() : idempotencyKey);
        return headers;
    }

    private static String newIdempotencyKey() {
        return randomHex(16);
    }

    private static String randomSandboxId() {
        return "sandbox-" + randomHex(4);
    }

    private static String randomHex(int bytes) {
        byte[] buf = new byte[bytes];
        RANDOM.nextBytes(buf);
        char[] out = new char[bytes * 2];
        for (int i = 0; i < bytes; i++) {
            out[i * 2] = HEX[(buf[i] >> 4) & 0xf];
            out[i * 2 + 1] = HEX[buf[i] & 0xf];
        }
        return new String(out);
    }

    private static Template toTemplate(Map<String, Object> data) {
        return new Template(
                asString(data.get("id")),
                asBool(data.get("ready")),
                asString(data.get("created_at")),
                asDouble(data.get("creation_time_ms")));
    }

    private static ServerSandbox toServerSandbox(Map<String, Object> data) {
        return new ServerSandbox(
                asString(data.get("id")),
                asString(data.get("template_id")),
                asString(data.get("endpoint")),
                asString(data.get("created_at")),
                asDouble(data.get("fork_time_ms")));
    }

    // ---- small JSON-tree coercion helpers, shared with Sandbox ----

    @SuppressWarnings("unchecked")
    static Map<String, Object> asObject(Object o) {
        if (o instanceof Map<?, ?> m) {
            return (Map<String, Object>) m;
        }
        return new LinkedHashMap<>();
    }

    static String asString(Object o) {
        return o == null ? "" : o.toString();
    }

    static boolean asBool(Object o) {
        return o instanceof Boolean b && b;
    }

    static int asInt(Object o) {
        if (o instanceof Number n) {
            return n.intValue();
        }
        return 0;
    }

    static double asDouble(Object o) {
        if (o instanceof Number n) {
            return n.doubleValue();
        }
        return 0.0;
    }
}
