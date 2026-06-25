// A plain main-based test runner for the mitos Java SDK. No JUnit or build tool
// is required: it spins up an in-process com.sun.net.httpserver.HttpServer that
// reproduces the sandbox-server wire shapes (mirroring the Rust and Ruby stub
// tests) and asserts the client behavior. Compile with:
//   javac --release 17 -d out $(find sdk/java/src -name '*.java')
// and run with:
//   java -cp out run.mitos.sdk.SdkTest
//
// It exits non-zero on the first failed assertion so it is CI-friendly.
package run.mitos.sdk;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicInteger;

public final class SdkTest {

    private static int passed = 0;
    private static int failed = 0;

    public static void main(String[] args) throws Exception {
        testCreateTemplate();
        testListTemplates();
        testForkSendsIdempotencyKeyAndId();
        testForkGeneratesId();
        testInvalidIdThrowsBeforeRequest();
        testExecRoundTrip();
        testExecSendsSandboxIdAndBearer();
        testExecErrorEndStream();
        testTerminateIssuesDelete();
        testDefaultBaseUrl();
        testBearerPrecedenceCredentialFile();
        testErrorEnvelopeParsed();
        testTokenNeverInExceptionMessage();

        // Cluster mode (AgentRun) tests run against the same in-process HttpServer
        // stub, reproducing the Kubernetes custom-resource REST wire shapes. They
        // share the assert helpers and Stub harness below via package-private hooks.
        ClusterTest.run();

        System.out.println();
        System.out.println("RESULT: " + passed + " passed, " + failed + " failed");
        if (failed > 0) {
            System.exit(1);
        }
        System.out.println("ALL GREEN");
    }

    // ---- tests ----

    private static void testCreateTemplate() throws Exception {
        Stub stub = new Stub();
        stub.handle("POST", "/v1/templates", ex -> json(ex, 200,
                "{\"id\":\"python\",\"ready\":true,"
                        + "\"created_at\":\"2026-06-21T00:00:00Z\",\"creation_time_ms\":12.5}"));
        try (stub) {
            SandboxServer server = new SandboxServer(stub.url(), null);
            Template t = server.createTemplate("python");
            assertEquals("python", t.id(), "createTemplate id");
            assertTrue(t.ready(), "createTemplate ready");
            assertEquals(12.5, t.creationTimeMs(), "createTemplate creation_time_ms");
        }
        ok("createTemplate returns {id, ready}");
    }

    private static void testListTemplates() throws Exception {
        Stub stub = new Stub();
        stub.handle("GET", "/v1/templates", ex -> json(ex, 200,
                "[{\"id\":\"python\",\"ready\":true,\"created_at\":\"t\",\"creation_time_ms\":1},"
                        + "{\"id\":\"node\",\"ready\":false,\"created_at\":\"t\",\"creation_time_ms\":2}]"));
        try (stub) {
            SandboxServer server = new SandboxServer(stub.url(), null);
            List<Template> ts = server.listTemplates();
            assertEquals(2, ts.size(), "listTemplates size");
            assertEquals("python", ts.get(0).id(), "listTemplates[0] id");
            assertEquals("node", ts.get(1).id(), "listTemplates[1] id");
            assertTrue(!ts.get(1).ready(), "listTemplates[1] not ready");
        }
        ok("listTemplates returns the array");
    }

    private static void testForkSendsIdempotencyKeyAndId() throws Exception {
        Stub stub = new Stub();
        stub.handle("POST", "/v1/fork", ex -> json(ex, 200,
                "{\"id\":\"my-sb\",\"template_id\":\"python\","
                        + "\"endpoint\":\"http://x\",\"fork_time_ms\":3.0}"));
        try (stub) {
            SandboxServer server = new SandboxServer(stub.url(), null);
            Sandbox sb = server.fork("python", "my-sb");
            assertEquals("my-sb", sb.id(), "fork returns sandbox id");
            String key = stub.lastHeader("/v1/fork", "Idempotency-Key");
            assertTrue(key != null && !key.isEmpty(), "fork sent Idempotency-Key header");
        }
        ok("fork returns a Sandbox with the right id and sends an Idempotency-Key");
    }

    private static void testForkGeneratesId() throws Exception {
        Stub stub = new Stub();
        // Echo back an empty id so the SDK falls back to the id it sent.
        List<String> sentIds = new ArrayList<>();
        stub.handle("POST", "/v1/fork", ex -> {
            String body = readBody(ex);
            Map<String, Object> m = Json.parseObject(body);
            sentIds.add(String.valueOf(m.get("id")));
            json(ex, 200, "{\"id\":\"\",\"template_id\":\"python\","
                    + "\"endpoint\":\"http://x\",\"fork_time_ms\":1.0}");
        });
        try (stub) {
            SandboxServer server = new SandboxServer(stub.url(), null);
            Sandbox sb = server.fork("python");
            assertTrue(sb.id().startsWith("sandbox-"), "generated id has sandbox- prefix: " + sb.id());
            assertTrue(SandboxServer.validSandboxId(sb.id()), "generated id is valid");
            assertEquals(sb.id(), sentIds.get(0), "generated id was the one sent");
        }
        ok("fork generates a sandbox-<hex> id when none is given");
    }

    private static void testInvalidIdThrowsBeforeRequest() throws Exception {
        Stub stub = new Stub();
        AtomicInteger hits = new AtomicInteger();
        stub.handle("POST", "/v1/fork", ex -> {
            hits.incrementAndGet();
            json(ex, 200, "{}");
        });
        try (stub) {
            SandboxServer server = new SandboxServer(stub.url(), null);
            MitosException thrown = null;
            try {
                server.fork("python", "bad id with spaces");
            } catch (MitosException e) {
                thrown = e;
            }
            assertTrue(thrown != null, "invalid id throws MitosException");
            assertEquals("invalid_sandbox_id", thrown.getCode(), "invalid id code");
            assertEquals(0, hits.get(), "no request was sent for an invalid id");
        }
        ok("an invalid id throws before any request is sent");
    }

    private static void testExecRoundTrip() throws Exception {
        Stub stub = new Stub();
        stub.handle("POST", "/v1/fork", ex -> json(ex, 200,
                "{\"id\":\"sb1\",\"template_id\":\"python\","
                        + "\"endpoint\":\"http://x\",\"fork_time_ms\":1.0}"));
        // The Connect ExecStream RPC: the server replies with a stream of
        // enveloped frames (a stdout frame, an exit frame, then a clean
        // end-stream frame). stdout/stderr are base64 in proto-JSON.
        stub.handle("POST", "/sandbox.v1.Sandbox/ExecStream", ex -> {
            // The request rides as ONE opening enveloped frame; the payload is the
            // proto-JSON ExecStreamRequest. (Read the body exactly once.)
            Map<String, Object> req = Json.parseObject(firstFramePayload(ex));
            assertEquals("echo hi", String.valueOf(req.get("command")), "exec command");
            String b64Hi = java.util.Base64.getEncoder()
                    .encodeToString("hi\n".getBytes(StandardCharsets.UTF_8));
            byte[] frames = concat(
                    frame(0x00, "{\"stdout\":\"" + b64Hi + "\"}"),
                    frame(0x00, "{\"exit\":{\"exitCode\":0,\"execTimeMs\":2.0}}"),
                    frame(0x02, "{}"));
            connectStream(ex, frames);
        });
        try (stub) {
            SandboxServer server = new SandboxServer(stub.url(), null);
            Sandbox sb = server.fork("python", "sb1");
            ExecResult r = sb.exec("echo hi");
            assertEquals(0, r.exitCode(), "exec exit_code");
            assertEquals("hi\n", r.stdout(), "exec stdout");
            assertEquals(2.0, r.execTimeMs(), "exec exec_time_ms");
            assertTrue(r.success(), "exec success()");
        }
        ok("exec round-trips stdout and exit_code over Connect ExecStream");
    }

    private static void testExecSendsSandboxIdAndBearer() throws Exception {
        String token = "sk-exec-token-abc";
        Stub stub = new Stub();
        stub.handle("POST", "/v1/fork", ex -> json(ex, 200,
                "{\"id\":\"sb-hdr\",\"template_id\":\"python\","
                        + "\"endpoint\":\"http://x\",\"fork_time_ms\":1.0}"));
        stub.handle("POST", "/sandbox.v1.Sandbox/ExecStream", ex -> {
            byte[] frames = concat(
                    frame(0x00, "{\"exit\":{\"exitCode\":0,\"execTimeMs\":1.0}}"),
                    frame(0x02, "{}"));
            connectStream(ex, frames);
        });
        try (stub) {
            SandboxServer server = new SandboxServer(stub.url(), token);
            Sandbox sb = server.fork("python", "sb-hdr");
            sb.exec("true");
            String sid = stub.lastHeader("/sandbox.v1.Sandbox/ExecStream", "X-Sandbox-Id");
            assertEquals("sb-hdr", sid, "exec sent X-Sandbox-Id header");
            String auth = stub.lastHeader("/sandbox.v1.Sandbox/ExecStream", "Authorization");
            assertEquals("Bearer " + token, auth, "exec sent the bearer header when a token is set");
            String proto = stub.lastHeader("/sandbox.v1.Sandbox/ExecStream", "Connect-Protocol-Version");
            assertEquals("1", proto, "exec sent Connect-Protocol-Version: 1");
        }
        ok("exec carries X-Sandbox-Id and the bearer header");
    }

    private static void testExecErrorEndStream() throws Exception {
        Stub stub = new Stub();
        stub.handle("POST", "/v1/fork", ex -> json(ex, 200,
                "{\"id\":\"sb-err\",\"template_id\":\"python\","
                        + "\"endpoint\":\"http://x\",\"fork_time_ms\":1.0}"));
        // The terminal end-stream frame carries an error object: the SDK must turn
        // it into a typed MitosException with the mapped code and status.
        stub.handle("POST", "/sandbox.v1.Sandbox/ExecStream", ex -> {
            byte[] frames = frame(0x02,
                    "{\"error\":{\"code\":\"not_found\",\"message\":\"no such sandbox\"}}");
            connectStream(ex, frames);
        });
        try (stub) {
            SandboxServer server = new SandboxServer(stub.url(), null);
            Sandbox sb = server.fork("python", "sb-err");
            MitosException thrown = null;
            try {
                sb.exec("true");
            } catch (MitosException e) {
                thrown = e;
            }
            assertTrue(thrown != null, "error end-stream throws MitosException");
            assertEquals("not_found", thrown.getCode(), "error end-stream code");
            assertEquals(404, thrown.getStatus(), "error end-stream status (mapped from code)");
            assertTrue(thrown.getMessage().contains("no such sandbox"),
                    "error end-stream message: " + thrown.getMessage());
        }
        ok("an error end-stream frame throws a typed MitosException");
    }

    private static void testTerminateIssuesDelete() throws Exception {
        Stub stub = new Stub();
        List<String> deleted = new ArrayList<>();
        stub.handle("POST", "/v1/fork", ex -> json(ex, 200,
                "{\"id\":\"sb-del\",\"template_id\":\"python\","
                        + "\"endpoint\":\"http://x\",\"fork_time_ms\":1.0}"));
        stub.handle("DELETE", "/v1/sandboxes/sb-del", ex -> {
            deleted.add(ex.getRequestURI().getPath());
            ex.sendResponseHeaders(204, -1);
            ex.close();
        });
        try (stub) {
            SandboxServer server = new SandboxServer(stub.url(), null);
            Sandbox sb = server.fork("python", "sb-del");
            sb.terminate();
            assertEquals(1, deleted.size(), "terminate issued one DELETE");
            assertEquals("/v1/sandboxes/sb-del", deleted.get(0), "terminate DELETE path");
        }
        ok("terminate issues a DELETE for the sandbox id");
    }

    private static void testDefaultBaseUrl() {
        // Ensure no env override is present in this process for the assertion to
        // mean what we think. The test harness runs without MITOS_BASE_URL set.
        if (System.getenv("MITOS_BASE_URL") == null) {
            SandboxServer server = new SandboxServer();
            assertEquals("https://mitos.run", server.url(), "default base URL");
            ok("base URL defaults to https://mitos.run when unset");
        } else {
            ok("base URL default skipped (MITOS_BASE_URL set in env)");
        }
    }

    private static void testBearerPrecedenceCredentialFile() throws Exception {
        Path tmp = Files.createTempDirectory("mitos-cred");
        Files.writeString(tmp.resolve("credentials.json"),
                "{\"token\":\"file-tok\",\"email\":\"a@b.c\"}");

        // The credential file is read via MITOS_CONFIG_DIR; we point a child JVM
        // at it so we can set the env var (the JDK has no in-process setenv).
        String out = runChild(BearerCheck.class.getName(), tmp.toString());
        // Expected lines: arg=arg-tok env=env-tok file=file-tok
        assertTrue(out.contains("arg=arg-tok"),
                "explicit arg wins over env and file: " + out);
        assertTrue(out.contains("env=env-tok"),
                "env wins over file when no arg: " + out);
        assertTrue(out.contains("file=file-tok"),
                "credential file is the last fallback: " + out);
        ok("bearer precedence: arg > env > credential file");
    }

    private static void testErrorEnvelopeParsed() throws Exception {
        Stub stub = new Stub();
        stub.handle("POST", "/v1/fork", ex -> json(ex, 404,
                "{\"error\":{\"code\":\"not_found\",\"message\":\"no such template\","
                        + "\"cause\":\"template python missing\","
                        + "\"remediation\":\"create the template first\"}}"));
        try (stub) {
            SandboxServer server = new SandboxServer(stub.url(), null);
            MitosException thrown = null;
            try {
                server.fork("python", "sb-x");
            } catch (MitosException e) {
                thrown = e;
            }
            assertTrue(thrown != null, "non-2xx envelope throws");
            assertEquals("not_found", thrown.getCode(), "parsed envelope code");
            assertEquals(404, thrown.getStatus(), "parsed envelope status");
            assertTrue(thrown.getMessage().contains("no such template"),
                    "parsed envelope message: " + thrown.getMessage());
        }
        ok("a non-2xx envelope throws MitosException with the parsed code");
    }

    private static void testTokenNeverInExceptionMessage() throws Exception {
        String secret = "sk-super-secret-value-123";
        Stub stub = new Stub();
        // A hostile server reflects the bearer token into its error body.
        stub.handle("POST", "/v1/fork", ex -> json(ex, 500,
                "{\"error\":{\"code\":\"internal_error\",\"message\":\"boom\","
                        + "\"cause\":\"saw token " + secret + " here\"}}"));
        try (stub) {
            SandboxServer server = new SandboxServer(stub.url(), secret);
            MitosException thrown = null;
            try {
                server.fork("python", "sb-y");
            } catch (MitosException e) {
                thrown = e;
            }
            assertTrue(thrown != null, "non-2xx throws");
            String full = thrown.getMessage() + " " + thrown.getCauseDetail();
            assertTrue(!full.contains(secret),
                    "the api_key value must never appear in an exception message/cause");
            assertTrue(full.contains("[REDACTED]"), "the token is redacted: " + full);
        }
        ok("the api_key value never appears in an exception message");
    }

    // ---- a child main used to exercise env-dependent bearer precedence ----

    public static final class BearerCheck {
        public static void main(String[] args) {
            // MITOS_CONFIG_DIR is set by the parent so the credential file is found.
            // arg wins:
            System.out.println("arg=" + AuthResolver.resolveToken("arg-tok"));
            // env wins over file (MITOS_API_KEY is set by the parent):
            System.out.println("env=" + AuthResolver.resolveToken(null));
            // file is the last fallback: the parent runs this twice, the second
            // time without MITOS_API_KEY. We detect that by the env value.
            String token = AuthResolver.resolveToken(null);
            if ("file-tok".equals(token)) {
                System.out.println("file=" + token);
            }
        }
    }

    // ---- tiny assert + stub harness ----

    static void ok(String name) {
        passed++;
        System.out.println("PASS: " + name);
    }

    static void fail(String name) {
        failed++;
        System.out.println("FAIL: " + name);
    }

    static void assertTrue(boolean cond, String name) {
        if (cond) {
            return;
        }
        fail(name);
        throw new AssertionError(name);
    }

    static void assertEquals(Object expected, Object actual, String name) {
        if (expected == null ? actual == null : expected.equals(actual)) {
            return;
        }
        fail(name + " (expected=" + expected + " actual=" + actual + ")");
        throw new AssertionError(name);
    }

    private static void assertEquals(double expected, double actual, String name) {
        if (Math.abs(expected - actual) < 1e-9) {
            return;
        }
        fail(name + " (expected=" + expected + " actual=" + actual + ")");
        throw new AssertionError(name);
    }

    interface Handler {
        void handle(HttpExchange ex) throws IOException;
    }

    /** An in-process HTTP stub that records request headers per path. */
    static final class Stub implements AutoCloseable {
        private final HttpServer http;
        private final Map<String, Handler> routes = new ConcurrentHashMap<>();
        private final Map<String, Map<String, String>> lastHeaders = new ConcurrentHashMap<>();

        Stub() throws IOException {
            http = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
            http.createContext("/", ex -> {
                String key = ex.getRequestMethod() + " " + ex.getRequestURI().getPath();
                Map<String, String> h = new ConcurrentHashMap<>();
                ex.getRequestHeaders().forEach((k, v) -> {
                    if (!v.isEmpty()) {
                        h.put(k, v.get(0));
                    }
                });
                lastHeaders.put(ex.getRequestURI().getPath(), h);
                Handler handler = routes.get(key);
                if (handler == null) {
                    json(ex, 404, "{\"error\":{\"code\":\"not_found\",\"message\":\"no route\"}}");
                    return;
                }
                handler.handle(ex);
            });
            http.start();
        }

        void handle(String method, String path, Handler h) {
            routes.put(method + " " + path, h);
        }

        String url() {
            return "http://127.0.0.1:" + http.getAddress().getPort();
        }

        String lastHeader(String path, String name) {
            Map<String, String> h = lastHeaders.get(path);
            if (h == null) {
                return null;
            }
            // The JDK HttpServer canonicalizes header names (for example
            // "Idempotency-key"), so match case-insensitively.
            for (Map.Entry<String, String> e : h.entrySet()) {
                if (e.getKey().equalsIgnoreCase(name)) {
                    return e.getValue();
                }
            }
            return null;
        }

        @Override
        public void close() {
            http.stop(0);
        }
    }

    static void json(HttpExchange ex, int status, String body) throws IOException {
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        ex.getResponseHeaders().add("Content-Type", "application/json");
        ex.sendResponseHeaders(status, bytes.length);
        try (OutputStream os = ex.getResponseBody()) {
            os.write(bytes);
        }
    }

    static String readBody(HttpExchange ex) throws IOException {
        return new String(ex.getRequestBody().readAllBytes(), StandardCharsets.UTF_8);
    }

    // ---- Connect enveloped-frame helpers (mirror the wire the server speaks) ----

    /** Encodes one Connect enveloped frame: a flag byte, a 4-byte big-endian
     * length, then the JSON payload. */
    static byte[] frame(int flag, String json) {
        byte[] payload = json.getBytes(StandardCharsets.UTF_8);
        int n = payload.length;
        byte[] out = new byte[5 + n];
        out[0] = (byte) flag;
        out[1] = (byte) ((n >>> 24) & 0xff);
        out[2] = (byte) ((n >>> 16) & 0xff);
        out[3] = (byte) ((n >>> 8) & 0xff);
        out[4] = (byte) (n & 0xff);
        System.arraycopy(payload, 0, out, 5, n);
        return out;
    }

    static byte[] concat(byte[]... parts) {
        int total = 0;
        for (byte[] p : parts) {
            total += p.length;
        }
        byte[] out = new byte[total];
        int off = 0;
        for (byte[] p : parts) {
            System.arraycopy(p, 0, out, off, p.length);
            off += p.length;
        }
        return out;
    }

    /** Writes a Connect server-stream response (HTTP 200, the connect+json
     * content type, then the enveloped frame bytes). */
    static void connectStream(HttpExchange ex, byte[] frames) throws IOException {
        ex.getResponseHeaders().add("Content-Type", "application/connect+json");
        ex.sendResponseHeaders(200, frames.length);
        try (OutputStream os = ex.getResponseBody()) {
            os.write(frames);
        }
    }

    /** Reads the request body (one or more enveloped frames) and returns the JSON
     * payload of the FIRST frame as a string. */
    static String firstFramePayload(HttpExchange ex) throws IOException {
        byte[] body = ex.getRequestBody().readAllBytes();
        if (body.length < 5) {
            return "";
        }
        int n = ((body[1] & 0xff) << 24) | ((body[2] & 0xff) << 16)
                | ((body[3] & 0xff) << 8) | (body[4] & 0xff);
        return new String(body, 5, n, StandardCharsets.UTF_8);
    }

    // runChild launches a fresh JVM running mainClass with MITOS_CONFIG_DIR set to
    // configDir, capturing the bearer-precedence output. It runs the child twice:
    // once with MITOS_API_KEY set (env wins) and once without (file fallback),
    // concatenating the relevant lines.
    private static String runChild(String mainClass, String configDir) throws Exception {
        String classpath = System.getProperty("java.class.path");
        String javaBin = System.getProperty("java.home") + "/bin/java";

        StringBuilder out = new StringBuilder();

        // Pass 1: env set, so env wins over file; arg still wins over env.
        ProcessBuilder pb1 = new ProcessBuilder(javaBin, "-cp", classpath, mainClass);
        pb1.environment().put("MITOS_CONFIG_DIR", configDir);
        pb1.environment().put("MITOS_API_KEY", "env-tok");
        pb1.redirectErrorStream(true);
        Process p1 = pb1.start();
        out.append(new String(p1.getInputStream().readAllBytes(), StandardCharsets.UTF_8));
        p1.waitFor();

        // Pass 2: no env key, so the credential file is the fallback.
        ProcessBuilder pb2 = new ProcessBuilder(javaBin, "-cp", classpath, mainClass);
        pb2.environment().put("MITOS_CONFIG_DIR", configDir);
        pb2.environment().remove("MITOS_API_KEY");
        pb2.redirectErrorStream(true);
        Process p2 = pb2.start();
        out.append(new String(p2.getInputStream().readAllBytes(), StandardCharsets.UTF_8));
        p2.waitFor();

        return out.toString();
    }
}
