// Cluster-mode (AgentRun) tests for the Java SDK. They run inside the same
// main-based runner as the direct-mode SdkTest: SdkTest.main calls
// ClusterTest.run(), and the assertions share SdkTest's pass/fail counters and
// its in-process com.sun.net.httpserver.HttpServer stub, here reproducing the
// Kubernetes custom-resource REST wire shapes:
//
//   /apis/mitos.run/v1/namespaces/{ns}/{plural}            (list, create)
//   /apis/mitos.run/v1/namespaces/{ns}/{plural}/{name}     (get, delete)
//
// No real cluster, kubeconfig, or in-cluster mount is needed: AgentRun is wired
// to the stub with K8s.of(url, token, HttpClient), the same seam the direct-mode
// SandboxServer uses. defaultPoolName is a pure function and is asserted against
// the exact vectors the issue specifies, byte-for-byte with the Python slug.
package run.mitos.sdk;

import java.net.http.HttpClient;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicReference;

import static run.mitos.sdk.SdkTest.assertEquals;
import static run.mitos.sdk.SdkTest.assertTrue;
import static run.mitos.sdk.SdkTest.ok;

final class ClusterTest {

    private ClusterTest() {
    }

    static void run() throws Exception {
        testDefaultPoolNameVectors();
        testSandboxGetOrCreatesDefaultPool();
        testSandboxReusesExistingPool();
        testSandboxToleratesPoolConflict();
        testCreateWritesPoolRef();
        testGetReadsPoolRefAndPhase();
        testListFiltersByPool();
        testPoolStatusReadsStatus();
        testReadyTokenFromSecretNeverLogged();
        testWorkspaceCreateAndNotFound();
        testKubeconfigBearerTokenParse();
    }

    // The minimal kubeconfig parser resolves the current context's server and
    // bearer token, selecting by context name (not position). No CA is set, so
    // the system trust store is used. This exercises the Yaml block parser and
    // the K8s.fromKubeconfig resolver without needing a live cluster.
    private static void testKubeconfigBearerTokenParse() throws Exception {
        String kube = "apiVersion: v1\n"
                + "kind: Config\n"
                + "current-context: prod\n"
                + "clusters:\n"
                + "- name: dev\n"
                + "  cluster:\n"
                + "    server: https://dev.example:6443\n"
                + "- name: prod\n"
                + "  cluster:\n"
                + "    server: https://prod.example:6443\n"
                + "contexts:\n"
                + "- name: dev\n"
                + "  context:\n"
                + "    cluster: dev\n"
                + "    user: dev-user\n"
                + "- name: prod\n"
                + "  context:\n"
                + "    cluster: prod\n"
                + "    user: prod-user\n"
                + "    namespace: agents\n"
                + "users:\n"
                + "- name: dev-user\n"
                + "  user:\n"
                + "    token: dev-token\n"
                + "- name: prod-user\n"
                + "  user:\n"
                + "    token: prod-token\n";
        java.nio.file.Path tmp = java.nio.file.Files.createTempFile("kubeconfig", ".yaml");
        java.nio.file.Files.writeString(tmp, kube);
        try {
            K8s k8s = K8s.fromKubeconfig(tmp.toString());
            assertEquals("https://prod.example:6443", k8s.server(),
                    "kubeconfig resolves the current context's server by name");
            assertEquals("prod-token", k8s.token(),
                    "kubeconfig resolves the current context's bearer token by name");
        } finally {
            java.nio.file.Files.deleteIfExists(tmp);
        }
        ok("a minimal kubeconfig parses to the current context's server and token");
    }

    // The exact vectors from issue #306: the Java defaultPoolName MUST match the
    // Python default_pool_name byte-for-byte, including applying the 40-char
    // bound BEFORE stripping "-." (Python slug[:40].strip("-.")).
    private static void testDefaultPoolNameVectors() {
        assertEquals("mitos-default-python-3.12",
                AgentRun.defaultPoolName("python:3.12"), "defaultPoolName python:3.12");
        assertEquals("mitos-default-ghcr.io-acme-foo-bar-latest",
                AgentRun.defaultPoolName("ghcr.io/Acme/Foo-Bar:latest"),
                "defaultPoolName ghcr.io/Acme/Foo-Bar:latest");
        assertEquals("mitos-default-busybox",
                AgentRun.defaultPoolName("busybox"), "defaultPoolName busybox");
        assertEquals("mitos-default-upper-case-tag",
                AgentRun.defaultPoolName("UPPER/Case:TAG"), "defaultPoolName UPPER/Case:TAG");
        assertEquals("mitos-default-" + "a".repeat(40),
                AgentRun.defaultPoolName("a".repeat(60) + ":x"),
                "defaultPoolName 60a:x bounds to 40 before stripping");
        assertEquals("mitos-default-registry.io-x-sha256-abc",
                AgentRun.defaultPoolName("registry.io/x@sha256:abc"),
                "defaultPoolName registry.io/x@sha256:abc");
        assertEquals("mitos-default-node-18",
                AgentRun.defaultPoolName("node_18"), "defaultPoolName node_18");
        ok("defaultPoolName matches the 7 Python vectors byte-for-byte");
    }

    // sandbox(image) with no pool get-or-creates the default pool: a 404 on GET
    // sandboxpools/<name> leads to a POST sandboxpools with the inline template,
    // then a POST sandboxes with the resolved poolRef.
    private static void testSandboxGetOrCreatesDefaultPool() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        AtomicReference<String> poolBody = new AtomicReference<>();
        AtomicReference<String> sandboxBody = new AtomicReference<>();
        String pool = AgentRun.defaultPoolName("python:3.12");

        // GET the pool: not found, so the lazy path must create it.
        stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/sandboxpools/" + pool,
                ex -> SdkTest.json(ex, 404,
                        "{\"kind\":\"Status\",\"reason\":\"NotFound\","
                                + "\"message\":\"sandboxpools \\\"" + pool + "\\\" not found\",\"code\":404}"));
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/sandboxpools", ex -> {
            poolBody.set(SdkTest.readBody(ex));
            SdkTest.json(ex, 201, "{\"metadata\":{\"name\":\"" + pool + "\"}}");
        });
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/sandboxes", ex -> {
            sandboxBody.set(SdkTest.readBody(ex));
            SdkTest.json(ex, 201, "{\"metadata\":{\"name\":\"sandbox-x\"}}");
        });

        try (stub) {
            AgentRun agent = agentFor(stub);
            ClusterSandbox sb = agent.sandbox("python:3.12");
            assertTrue(sb.name() != null && sb.name().startsWith("sandbox-"),
                    "created sandbox has a generated name: " + sb.name());
            assertEquals(pool, sb.pool(), "sandbox pool is the default pool");

            Map<String, Object> createdPool = Json.parseObject(poolBody.get());
            assertEquals("SandboxPool", createdPool.get("kind"), "created object is a SandboxPool");
            assertEquals("python:3.12",
                    K8s.nestedString(createdPool, "spec", "template", "image"),
                    "default pool carries the inline template image");
            assertEquals(1, K8s.asInt(K8s.nested(createdPool, "spec", "replicas")),
                    "default pool replicas is 1");
            assertEquals(pool, K8s.nestedString(createdPool, "metadata", "name"),
                    "default pool name is the slug");

            Map<String, Object> createdSb = Json.parseObject(sandboxBody.get());
            assertEquals(pool,
                    K8s.nestedString(createdSb, "spec", "source", "poolRef", "name"),
                    "sandbox spec.source.poolRef.name is the pool");
        }
        ok("sandbox(image) get-or-creates the default pool, then creates a Sandbox from it");
    }

    // sandbox(image) reuses a pre-existing default pool untouched when its inline
    // image matches: only a GET (200) on the pool and a POST on the sandbox.
    private static void testSandboxReusesExistingPool() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        String pool = AgentRun.defaultPoolName("busybox");
        java.util.concurrent.atomic.AtomicInteger poolCreates = new java.util.concurrent.atomic.AtomicInteger();

        stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/sandboxpools/" + pool,
                ex -> SdkTest.json(ex, 200,
                        "{\"metadata\":{\"name\":\"" + pool + "\"},"
                                + "\"spec\":{\"template\":{\"image\":\"busybox\"},\"replicas\":1}}"));
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/sandboxpools", ex -> {
            poolCreates.incrementAndGet();
            SdkTest.json(ex, 201, "{}");
        });
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/sandboxes",
                ex -> SdkTest.json(ex, 201, "{\"metadata\":{\"name\":\"sb-reuse\"}}"));

        try (stub) {
            AgentRun agent = agentFor(stub);
            ClusterSandbox sb = agent.sandbox("busybox");
            assertEquals(pool, sb.pool(), "reused pool name");
            assertEquals(0, poolCreates.get(), "an existing matching pool is not re-created");
        }
        ok("sandbox(image) reuses a matching pre-existing default pool untouched");
    }

    // A 409 on the pool create (a concurrent creator won the race) is tolerated:
    // the pool is reused and the sandbox is still created.
    private static void testSandboxToleratesPoolConflict() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        String pool = AgentRun.defaultPoolName("alpine");

        stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/sandboxpools/" + pool,
                ex -> SdkTest.json(ex, 404,
                        "{\"reason\":\"NotFound\",\"message\":\"not found\",\"code\":404}"));
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/sandboxpools",
                ex -> SdkTest.json(ex, 409,
                        "{\"reason\":\"AlreadyExists\",\"message\":\"already exists\",\"code\":409}"));
        java.util.concurrent.atomic.AtomicInteger sbCreates = new java.util.concurrent.atomic.AtomicInteger();
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/sandboxes", ex -> {
            sbCreates.incrementAndGet();
            SdkTest.json(ex, 201, "{\"metadata\":{\"name\":\"sb-409\"}}");
        });

        try (stub) {
            AgentRun agent = agentFor(stub);
            ClusterSandbox sb = agent.sandbox("alpine");
            assertEquals(pool, sb.pool(), "pool reused after 409");
            assertEquals(1, sbCreates.get(), "the sandbox is still created after a tolerated 409");
        }
        ok("a 409 on the default-pool create is tolerated and the pool is reused");
    }

    // create() with an explicit pool, env, secret, ttl, and workspace writes the
    // expected Sandbox spec shape.
    private static void testCreateWritesPoolRef() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        AtomicReference<String> body = new AtomicReference<>();
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/sandboxes", ex -> {
            body.set(SdkTest.readBody(ex));
            SdkTest.json(ex, 201, "{\"metadata\":{\"name\":\"worker-1\"}}");
        });
        try (stub) {
            AgentRun agent = agentFor(stub);
            ClusterSandbox sb = agent.create(SandboxParams.builder()
                    .pool("my-pool")
                    .name("worker-1")
                    .env("MODE", "fast")
                    .secret("API_KEY", "creds", "key")
                    .ttl("30m")
                    .workspace("ws-main")
                    .build());
            assertEquals("worker-1", sb.name(), "explicit sandbox name");
            assertEquals("my-pool", sb.pool(), "explicit pool");

            Map<String, Object> created = Json.parseObject(body.get());
            assertEquals("Sandbox", created.get("kind"), "kind is Sandbox");
            assertEquals("my-pool",
                    K8s.nestedString(created, "spec", "source", "poolRef", "name"),
                    "spec.source.poolRef.name");
            assertEquals("30m", K8s.nestedString(created, "spec", "lifetime", "ttl"),
                    "spec.lifetime.ttl");
            assertEquals("ws-main", K8s.nestedString(created, "spec", "workspaceRef", "name"),
                    "spec.workspaceRef.name");
            List<Object> env = K8s.asList(K8s.nested(created, "spec", "env"));
            assertEquals(1, env.size(), "one env entry");
            assertEquals("MODE", K8s.nestedString(K8s.asMap(env.get(0)), "name"), "env name");
            assertEquals("fast", K8s.nestedString(K8s.asMap(env.get(0)), "value"), "env value");
            List<Object> secrets = K8s.asList(K8s.nested(created, "spec", "secrets"));
            assertEquals(1, secrets.size(), "one secret entry");
            assertEquals("creds",
                    K8s.nestedString(K8s.asMap(secrets.get(0)), "secretRef", "name"),
                    "secret references the secret name");
            assertEquals("key",
                    K8s.nestedString(K8s.asMap(secrets.get(0)), "secretRef", "key"),
                    "secret references the secret key");
        }
        ok("create() writes spec.source.poolRef plus env/secrets/ttl/workspace");
    }

    // get(name) reads spec.source.poolRef and status.phase.
    private static void testGetReadsPoolRefAndPhase() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/sandboxes/sb-get",
                ex -> SdkTest.json(ex, 200,
                        "{\"metadata\":{\"name\":\"sb-get\"},"
                                + "\"spec\":{\"source\":{\"poolRef\":{\"name\":\"pool-a\"}}},"
                                + "\"status\":{\"phase\":\"Pending\",\"endpoint\":\"\"}}"));
        try (stub) {
            AgentRun agent = agentFor(stub);
            ClusterSandbox sb = agent.get("sb-get");
            assertEquals("sb-get", sb.name(), "get name");
            assertEquals("pool-a", sb.pool(), "get reads spec.source.poolRef.name");
            assertEquals(SandboxPhase.PENDING, sb.phase(), "get reads status.phase");

            // fromName is an alias for get.
            ClusterSandbox same = agent.fromName("sb-get");
            assertEquals("pool-a", same.pool(), "fromName aliases get");
        }
        ok("get/fromName read spec.source.poolRef and status.phase");
    }

    // list(pool) returns only the sandboxes whose poolRef matches the filter.
    private static void testListFiltersByPool() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/sandboxes",
                ex -> SdkTest.json(ex, 200,
                        "{\"items\":["
                                + "{\"metadata\":{\"name\":\"a\"},\"spec\":{\"source\":{\"poolRef\":{\"name\":\"p1\"}}},"
                                + "\"status\":{\"phase\":\"Ready\",\"endpoint\":\"10.0.0.1:9091\"}},"
                                + "{\"metadata\":{\"name\":\"b\"},\"spec\":{\"source\":{\"poolRef\":{\"name\":\"p2\"}}},"
                                + "\"status\":{\"phase\":\"Pending\"}}"
                                + "]}"));
        // 'a' is Ready, so list() loads its token Secret; serve an empty 404 so
        // the tokenless path is exercised without leaking anything.
        stub.handle("GET", "/api/v1/namespaces/default/secrets/a-sandbox-token",
                ex -> SdkTest.json(ex, 404, "{\"reason\":\"NotFound\",\"code\":404}"));
        try (stub) {
            AgentRun agent = agentFor(stub);
            List<ClusterSandbox> all = agent.list(null);
            assertEquals(2, all.size(), "list(null) returns every sandbox");
            List<ClusterSandbox> filtered = agent.list("p2");
            assertEquals(1, filtered.size(), "list(pool) filters by pool");
            assertEquals("b", filtered.get(0).name(), "filtered sandbox is the p2 one");
        }
        ok("list(pool) filters by spec.source.poolRef.name");
    }

    // poolStatus reads status.readySnapshots, spec.replicas, and
    // status.nodeDistribution.
    private static void testPoolStatusReadsStatus() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/sandboxpools/p-stat",
                ex -> SdkTest.json(ex, 200,
                        "{\"metadata\":{\"name\":\"p-stat\"},"
                                + "\"spec\":{\"replicas\":3},"
                                + "\"status\":{\"readySnapshots\":2,"
                                + "\"nodeDistribution\":{\"node-a\":1,\"node-b\":1}}}"));
        try (stub) {
            AgentRun agent = agentFor(stub);
            PoolStatus status = agent.poolStatus("p-stat");
            assertEquals("p-stat", status.name(), "pool status name");
            assertEquals(2, status.readySnapshots(), "pool status readySnapshots");
            assertEquals(3, status.desired(), "pool status desired from spec.replicas");
            assertEquals(2, status.nodeDistribution().size(), "node distribution size");
            assertEquals(1, status.nodeDistribution().get("node-a"), "node-a count");
        }
        ok("poolStatus reads readySnapshots, replicas, and nodeDistribution");
    }

    // A Ready sandbox loads its per-sandbox token from <name>-sandbox-token, and
    // the token value is reachable via token() but never appears in toString().
    private static void testReadyTokenFromSecretNeverLogged() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        String secret = "tok-abc-123-never-logged";
        String tokenB64 = java.util.Base64.getEncoder()
                .encodeToString(secret.getBytes(java.nio.charset.StandardCharsets.UTF_8));
        stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/sandboxes/sb-ready",
                ex -> SdkTest.json(ex, 200,
                        "{\"metadata\":{\"name\":\"sb-ready\"},"
                                + "\"spec\":{\"source\":{\"poolRef\":{\"name\":\"p\"}}},"
                                + "\"status\":{\"phase\":\"Ready\",\"endpoint\":\"10.0.0.9:9091\"}}"));
        stub.handle("GET", "/api/v1/namespaces/default/secrets/sb-ready-sandbox-token",
                ex -> SdkTest.json(ex, 200,
                        "{\"data\":{\"token\":\"" + tokenB64 + "\"}}"));
        try (stub) {
            AgentRun agent = agentFor(stub);
            ClusterSandbox sb = agent.get("sb-ready");
            assertEquals(SandboxPhase.READY, sb.phase(), "sandbox is Ready");
            assertEquals(secret, sb.token(), "token is decoded from the Secret");
            assertTrue(!sb.toString().contains(secret),
                    "the token value never appears in toString(): " + sb.toString());
        }
        ok("a Ready sandbox loads its token Secret; the token never appears in toString()");
    }

    // createWorkspace POSTs a Workspace; getWorkspace on an absent name raises a
    // typed workspace_not_found.
    private static void testWorkspaceCreateAndNotFound() throws Exception {
        SdkTest.Stub stub = new SdkTest.Stub();
        AtomicReference<String> body = new AtomicReference<>();
        stub.handle("POST", "/apis/mitos.run/v1/namespaces/default/workspaces", ex -> {
            body.set(SdkTest.readBody(ex));
            SdkTest.json(ex, 201, "{\"metadata\":{\"name\":\"ws1\"}}");
        });
        stub.handle("GET", "/apis/mitos.run/v1/namespaces/default/workspaces/missing",
                ex -> SdkTest.json(ex, 404,
                        "{\"reason\":\"NotFound\",\"message\":\"not found\",\"code\":404}"));
        try (stub) {
            AgentRun agent = agentFor(stub);
            ClusterWorkspace ws = agent.createWorkspace("ws1");
            assertEquals("ws1", ws.name(), "created workspace name");
            Map<String, Object> created = Json.parseObject(body.get());
            assertEquals("Workspace", created.get("kind"), "kind is Workspace");

            MitosException thrown = null;
            try {
                agent.getWorkspace("missing");
            } catch (MitosException e) {
                thrown = e;
            }
            assertTrue(thrown != null, "getWorkspace on an absent name throws");
            assertEquals("workspace_not_found", thrown.getCode(), "typed workspace_not_found code");
            assertEquals(404, thrown.getStatus(), "workspace_not_found carries status 404");
        }
        ok("createWorkspace POSTs a Workspace; getWorkspace raises workspace_not_found");
    }

    // agentFor wires an AgentRun at the in-process stub via the K8s.of seam,
    // tokenless, in the default namespace, with the default-pool path enabled.
    private static AgentRun agentFor(SdkTest.Stub stub) {
        K8s k8s = K8s.of(stub.url(), null, HttpClient.newHttpClient());
        return AgentRun.of(k8s, "default", true);
    }
}
