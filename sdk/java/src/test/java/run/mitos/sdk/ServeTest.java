// Tests for ClusterWorkspace.serve(ServeOptions). They follow the same pattern
// as ClusterTest: in-process com.sun.net.httpserver.HttpServer stubs, K8s.of
// seam, and SdkTest assertion helpers. ServeTest.run() is called from
// ClusterTest.run() so it participates in the same pass/fail counters.
package run.mitos.sdk;

import java.net.http.HttpClient;
import java.util.Map;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;

import static run.mitos.sdk.SdkTest.assertEquals;
import static run.mitos.sdk.SdkTest.assertTrue;
import static run.mitos.sdk.SdkTest.ok;

final class ServeTest {

    private ServeTest() {
    }

    static void run() throws Exception {
        testServeReturnsUrl();
        testServePopulatesExposeSpec();
        testServePopulatesWorkspaceRef();
        testServeMissingPool();
        testServeMissingExposeDomain();
        testServeReservedLabel();
        testServeInvalidPort();
        testServeInvalidLabelFormat();
        testServeLabelTooLong();
        testServeSandboxFailed();
        testServeTimeout();
        testServeWaitsUntilReady();
    }

    // serve() returns a ServedWorkspace whose url is
    // "https://<label>.<exposeDomain>/".
    private static void testServeReturnsUrl() throws Exception {
        SdkTest.Stub stub2 = stubWithReadyHandler();
        try (stub2) {
            ClusterWorkspace ws = workspaceFor(stub2, "ws-1");

            ServeOptions opts = ServeOptions.builder()
                    .pool("my-pool")
                    .exposeDomain("mitos.app")
                    .build();
            ServedWorkspace sw = ws.serve(opts);
            assertTrue(sw.url().startsWith("https://"), "url starts with https://");
            assertTrue(sw.url().endsWith(".mitos.app/"), "url ends with .mitos.app/");
            assertTrue(sw.sandboxName().startsWith("sandbox-"), "sandboxName has sandbox- prefix");
            assertEquals("private", sw.sharing(), "default sharing is private");
        }
        ok("serve() returns url https://<label>.mitos.app/");
    }

    // serve() with an explicit label uses that label in the URL.
    private static void testServePopulatesExposeSpec() throws Exception {
        SdkTest.Stub stub = stubWithReadyHandler();
        AtomicReference<String> postedBody = new AtomicReference<>();
        // Override the POST handler to capture the body.
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/sandboxes", ex -> {
            postedBody.set(SdkTest.readBody(ex));
            Map<String, Object> body = Json.parseObject(postedBody.get());
            String sbName = K8s.nestedString(body, "metadata", "name");
            // Register the GET handler for this specific sandbox.
            stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/sandboxes/" + sbName,
                    e2 -> SdkTest.json(e2, 200, "{\"status\":{\"phase\":\"Ready\"}}"));
            SdkTest.json(ex, 201, "{\"metadata\":{\"name\":\"" + sbName + "\"}}");
        });
        try (stub) {
            ClusterWorkspace ws = workspaceFor(stub, "ws-2");
            ServeOptions opts = ServeOptions.builder()
                    .pool("my-pool")
                    .port(3000)
                    .sharing("link")
                    .label("myapp")
                    .exposeDomain("mitos.app")
                    .build();
            ServedWorkspace sw = ws.serve(opts);
            assertEquals("https://myapp.mitos.app/", sw.url(), "explicit label in url");
            assertEquals("myapp", sw.label(), "label in ServedWorkspace");
            assertEquals("link", sw.sharing(), "sharing in ServedWorkspace");

            Map<String, Object> created = Json.parseObject(postedBody.get());
            Object portObj = K8s.nested(created, "spec", "expose", "port");
            assertEquals(3000, K8s.asInt(portObj), "spec.expose.port");
            assertEquals("myapp", K8s.nestedString(created, "spec", "expose", "label"),
                    "spec.expose.label");
            assertEquals("link", K8s.nestedString(created, "spec", "expose", "sharing"),
                    "spec.expose.sharing");
        }
        ok("serve() populates spec.expose with port, label, and sharing");
    }

    // serve() sets spec.workspaceRef to the workspace name.
    private static void testServePopulatesWorkspaceRef() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        AtomicReference<String> postedBody = new AtomicReference<>();
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/sandboxes", ex -> {
            postedBody.set(SdkTest.readBody(ex));
            Map<String, Object> body = Json.parseObject(postedBody.get());
            String sbName = K8s.nestedString(body, "metadata", "name");
            stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/sandboxes/" + sbName,
                    e2 -> SdkTest.json(e2, 200, "{\"status\":{\"phase\":\"Ready\"}}"));
            SdkTest.json(ex, 201, "{\"metadata\":{\"name\":\"" + sbName + "\"}}");
        });
        try (stub) {
            ClusterWorkspace ws = workspaceFor(stub, "ws-ref");
            ServeOptions opts = ServeOptions.builder()
                    .pool("pool-x")
                    .exposeDomain("mitos.app")
                    .build();
            ws.serve(opts);
            Map<String, Object> created = Json.parseObject(postedBody.get());
            assertEquals("ws-ref",
                    K8s.nestedString(created, "spec", "workspaceRef", "name"),
                    "spec.workspaceRef.name is the workspace name");
            assertEquals("pool-x",
                    K8s.nestedString(created, "spec", "source", "poolRef", "name"),
                    "spec.source.poolRef.name is the pool");
        }
        ok("serve() sets spec.workspaceRef.name and spec.source.poolRef.name");
    }

    // serve() throws missing_serve_pool when pool is not set.
    private static void testServeMissingPool() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        try (stub) {
            ClusterWorkspace ws = workspaceFor(stub, "ws-3");
            MitosException thrown = null;
            try {
                ws.serve(ServeOptions.builder().exposeDomain("mitos.app").build());
            } catch (MitosException e) {
                thrown = e;
            }
            assertTrue(thrown != null, "missing pool throws");
            assertEquals("missing_serve_pool", thrown.getCode(), "missing pool code");
        }
        ok("serve() throws missing_serve_pool when pool is not set");
    }

    // serve() throws missing_expose_domain when neither option nor env var is set.
    private static void testServeMissingExposeDomain() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        try (stub) {
            ClusterWorkspace ws = workspaceFor(stub, "ws-4");
            // Ensure MITOS_EXPOSE_DOMAIN is not set in this process for a clean test.
            // (The test process does not set this env var by default.)
            if (System.getenv("MITOS_EXPOSE_DOMAIN") != null) {
                ok("serve() missing expose domain skipped (MITOS_EXPOSE_DOMAIN set in env)");
                return;
            }
            MitosException thrown = null;
            try {
                ws.serve(ServeOptions.builder().pool("p").build());
            } catch (MitosException e) {
                thrown = e;
            }
            assertTrue(thrown != null, "missing expose domain throws");
            assertEquals("missing_expose_domain", thrown.getCode(), "missing domain code");
        }
        ok("serve() throws missing_expose_domain when neither option nor env is set");
    }

    // serve() throws reserved_expose_label when a reserved label is used.
    private static void testServeReservedLabel() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        try (stub) {
            ClusterWorkspace ws = workspaceFor(stub, "ws-5");
            for (String reserved : new String[]{"www", "app", "api", "console", "admin",
                    "auth", "login", "account", "mail", "static", "assets", "cdn",
                    "status", "gateway"}) {
                MitosException thrown = null;
                try {
                    ws.serve(ServeOptions.builder()
                            .pool("p")
                            .label(reserved)
                            .exposeDomain("mitos.app")
                            .build());
                } catch (MitosException e) {
                    thrown = e;
                }
                assertTrue(thrown != null, "reserved label " + reserved + " throws");
                assertEquals("reserved_expose_label", thrown.getCode(),
                        "reserved label code for " + reserved);
            }
        }
        ok("serve() throws reserved_expose_label for all 14 reserved labels");
    }

    // serve() throws invalid_serve_port for out-of-range ports.
    private static void testServeInvalidPort() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        try (stub) {
            ClusterWorkspace ws = workspaceFor(stub, "ws-6");
            for (int bad : new int[]{0, -1, 65536, 100000}) {
                MitosException thrown = null;
                try {
                    ws.serve(ServeOptions.builder()
                            .pool("p")
                            .port(bad)
                            .exposeDomain("mitos.app")
                            .build());
                } catch (MitosException e) {
                    thrown = e;
                }
                assertTrue(thrown != null, "invalid port " + bad + " throws");
                assertEquals("invalid_serve_port", thrown.getCode(),
                        "invalid port code for " + bad);
            }
        }
        ok("serve() throws invalid_serve_port for ports outside 1-65535");
    }

    // serve() throws invalid_expose_label for labels that fail the DNS-label RE.
    private static void testServeInvalidLabelFormat() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        try (stub) {
            ClusterWorkspace ws = workspaceFor(stub, "ws-7");
            for (String bad : new String[]{"-starts-with-hyphen", "ends-with-hyphen-",
                    "has spaces", "HAS_UPPER", "dot.in.label"}) {
                MitosException thrown = null;
                try {
                    ws.serve(ServeOptions.builder()
                            .pool("p")
                            .label(bad)
                            .exposeDomain("mitos.app")
                            .build());
                } catch (MitosException e) {
                    thrown = e;
                }
                assertTrue(thrown != null, "bad label \"" + bad + "\" throws");
                assertEquals("invalid_expose_label", thrown.getCode(),
                        "invalid label code for \"" + bad + "\"");
            }
        }
        ok("serve() throws invalid_expose_label for labels that fail the DNS RE");
    }

    // serve() throws invalid_expose_label for a label exceeding 63 characters.
    private static void testServeLabelTooLong() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        try (stub) {
            ClusterWorkspace ws = workspaceFor(stub, "ws-8");
            String long64 = "a".repeat(64);
            MitosException thrown = null;
            try {
                ws.serve(ServeOptions.builder()
                        .pool("p")
                        .label(long64)
                        .exposeDomain("mitos.app")
                        .build());
            } catch (MitosException e) {
                thrown = e;
            }
            assertTrue(thrown != null, "64-char label throws");
            assertEquals("invalid_expose_label", thrown.getCode(), "too-long label code");
        }
        ok("serve() throws invalid_expose_label for a label exceeding 63 characters");
    }

    // serve() throws sandbox_failed when the sandbox reaches Failed phase.
    private static void testServeSandboxFailed() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/sandboxes", ex -> {
            Map<String, Object> body = Json.parseObject(SdkTest.readBody(ex));
            String sbName = K8s.nestedString(body, "metadata", "name");
            stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/sandboxes/" + sbName,
                    e2 -> SdkTest.json(e2, 200, "{\"status\":{\"phase\":\"Failed\"}}"));
            SdkTest.json(ex, 201, "{\"metadata\":{\"name\":\"" + sbName + "\"}}");
        });
        try (stub) {
            ClusterWorkspace ws = workspaceFor(stub, "ws-9");
            MitosException thrown = null;
            try {
                ws.serve(ServeOptions.builder()
                        .pool("p")
                        .exposeDomain("mitos.app")
                        .build());
            } catch (MitosException e) {
                thrown = e;
            }
            assertTrue(thrown != null, "Failed phase throws");
            assertEquals("sandbox_failed", thrown.getCode(), "sandbox_failed code");
        }
        ok("serve() throws sandbox_failed when the sandbox reaches Failed phase");
    }

    // serve() throws serve_timeout (it does not hang) when the sandbox never
    // reaches Ready within the attempt cap.
    private static void testServeTimeout() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/sandboxes", ex -> {
            Map<String, Object> body = Json.parseObject(SdkTest.readBody(ex));
            String sbName = K8s.nestedString(body, "metadata", "name");
            stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/sandboxes/" + sbName,
                    e2 -> SdkTest.json(e2, 200, "{\"status\":{\"phase\":\"Pending\"}}"));
            SdkTest.json(ex, 201, "{\"metadata\":{\"name\":\"" + sbName + "\"}}");
        });
        try (stub) {
            ClusterWorkspace ws = workspaceFor(stub, "ws-timeout");
            ws.setServeMaxAttempts(3);
            MitosException thrown = null;
            try {
                ws.serve(ServeOptions.builder()
                        .pool("p")
                        .exposeDomain("mitos.app")
                        .build());
            } catch (MitosException e) {
                thrown = e;
            }
            assertTrue(thrown != null, "never-Ready throws instead of hanging");
            assertEquals("serve_timeout", thrown.getCode(), "serve_timeout code");
        }
        ok("serve() throws serve_timeout when the sandbox never becomes Ready");
    }

    // serve() polls until the sandbox becomes Ready (transitions through Pending).
    private static void testServeWaitsUntilReady() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        AtomicInteger pollCount = new AtomicInteger();
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/sandboxes", ex -> {
            Map<String, Object> body = Json.parseObject(SdkTest.readBody(ex));
            String sbName = K8s.nestedString(body, "metadata", "name");
            // First two GET calls return Pending; the third returns Ready.
            stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/sandboxes/" + sbName,
                    e2 -> {
                        int n = pollCount.incrementAndGet();
                        String phase = n < 3 ? "Pending" : "Ready";
                        SdkTest.json(e2, 200, "{\"status\":{\"phase\":\"" + phase + "\"}}");
                    });
            SdkTest.json(ex, 201, "{\"metadata\":{\"name\":\"" + sbName + "\"}}");
        });
        try (stub) {
            ClusterWorkspace ws = workspaceFor(stub, "ws-10");
            ServedWorkspace sw = ws.serve(ServeOptions.builder()
                    .pool("p")
                    .exposeDomain("mitos.app")
                    .build());
            assertTrue(sw.url() != null && !sw.url().isEmpty(), "url is non-empty after polling");
            assertTrue(pollCount.get() >= 3, "polled at least 3 times: " + pollCount.get());
        }
        ok("serve() polls through Pending before returning a ServedWorkspace on Ready");
    }

    // workspaceFor wires a ClusterWorkspace to the in-process stub. The Ready poll
    // interval is set to 0 on this specific instance so the wait never sleeps;
    // because it is a per-instance field, no mutable state is shared across tests.
    private static ClusterWorkspace workspaceFor(SdkTest.Stub stub, String wsName) {
        K8s k8s = K8s.of(stub.url(), null, HttpClient.newHttpClient());
        ClusterWorkspace ws = new ClusterWorkspace(wsName, "default", k8s);
        ws.setServeWaitIntervalMs(0);
        return ws;
    }

    // stubWithReadyHandler returns a Stub that returns Ready on any sandbox GET
    // and echoes the name on any sandbox POST.
    private static SdkTest.Stub stubWithReadyHandler() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/sandboxes", ex -> {
            Map<String, Object> body = Json.parseObject(SdkTest.readBody(ex));
            String sbName = K8s.nestedString(body, "metadata", "name");
            // Register a GET handler for the specific sandbox name.
            stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/sandboxes/" + sbName,
                    e2 -> SdkTest.json(e2, 200, "{\"status\":{\"phase\":\"Ready\"}}"));
            SdkTest.json(ex, 201, "{\"metadata\":{\"name\":\"" + sbName + "\"}}");
        });
        return stub;
    }
}
