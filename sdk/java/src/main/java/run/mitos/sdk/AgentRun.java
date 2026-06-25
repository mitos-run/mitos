// Kubernetes cluster mode for the Java SDK: the AgentRun client drives the
// mitos.run/v1 CRDs (SandboxPool, Sandbox, Workspace) directly, the same surface
// the Python AgentRun (sdk/python/mitos/client.py) and the TypeScript AgentRun
// expose. It is the operator path: a Sandbox is born from a pool (or forked from
// another sandbox) and the controller drives it to Ready.
//
// Cluster mode is built on the minimal stdlib Kubernetes client in K8s.java; it
// pulls NO third-party dependency (no fabric8, no official client, no YAML lib)
// and leaves direct mode (SandboxServer) entirely untouched.
package run.mitos.sdk;

import java.security.SecureRandom;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.regex.Pattern;

/**
 * The Kubernetes cluster-mode client. It reconciles with the {@code mitos.run/v1}
 * CRDs directly, the Java analogue of the Python {@code AgentRun}. Construct it
 * with {@link #inCluster()} inside a pod or {@link #fromKubeconfig(String)} from
 * a kubeconfig file, then call {@link #sandbox(String)} for the one-liner path.
 *
 * <p>This is a separate surface from the direct-mode {@link SandboxServer}; the
 * two never interact. The per-sandbox bearer token is read from the
 * {@code <name>-sandbox-token} Secret, held in memory only, and never logged.
 */
public final class AgentRun {

    static final String API_GROUP = K8s.API_GROUP;
    static final String API_VERSION = K8s.API_VERSION;

    private static final String DEFAULT_POOL_PREFIX = "mitos-default-";

    // Collapses any run of characters outside [a-z0-9.-] into a single "-".
    // Mirrors the Python _SLUG_RE and the TypeScript slug regex byte-for-byte.
    private static final Pattern SLUG_RE = Pattern.compile("[^a-z0-9.-]+");

    private static final SecureRandom RANDOM = new SecureRandom();
    private static final char[] HEX = "0123456789abcdef".toCharArray();

    private final K8s k8s;
    private final String namespace;
    private final boolean allowDefaultPool;

    private AgentRun(K8s k8s, String namespace, boolean allowDefaultPool) {
        this.k8s = k8s;
        this.namespace = namespace;
        this.allowDefaultPool = allowDefaultPool;
    }

    // ---- construction ----

    /**
     * Builds a cluster-mode client from the in-cluster service-account mount
     * (KUBERNETES_SERVICE_HOST/PORT, the projected token, and the mounted CA),
     * operating in the {@code default} namespace. Use it inside a Kubernetes pod.
     */
    public static AgentRun inCluster() {
        return inCluster("default");
    }

    /** As {@link #inCluster()} but operating in {@code namespace}. */
    public static AgentRun inCluster(String namespace) {
        return new AgentRun(K8s.inCluster(), namespace, true);
    }

    /**
     * Builds a cluster-mode client from a kubeconfig file (the current context),
     * operating in the {@code default} namespace. A null or empty path falls
     * back to {@code $KUBECONFIG} then {@code $HOME/.kube/config}. The SDK parses
     * a common kubeconfig subset (server, CA, bearer token or inline client
     * cert/key); it does not support exec credential plugins.
     */
    public static AgentRun fromKubeconfig(String path) {
        return fromKubeconfig(path, "default");
    }

    /** As {@link #fromKubeconfig(String)} but operating in {@code namespace}. */
    public static AgentRun fromKubeconfig(String path, String namespace) {
        return new AgentRun(K8s.fromKubeconfig(path), namespace, true);
    }

    /**
     * Builds a cluster-mode client over an already-resolved {@link K8s}
     * connection, for example one pointed at a local test server. The namespace
     * and the default-pool toggle are explicit.
     */
    static AgentRun of(K8s k8s, String namespace, boolean allowDefaultPool) {
        return new AgentRun(k8s, namespace, allowDefaultPool);
    }

    /** Returns this client with the lazy default-pool convenience toggled. When
     * disabled, {@link #sandbox(String)} requires an explicit pool. */
    public AgentRun allowDefaultPool(boolean allow) {
        return new AgentRun(k8s, namespace, allow);
    }

    /** The namespace this client operates in. */
    public String namespace() {
        return namespace;
    }

    // ---- default-pool naming ----

    /**
     * Derives the deterministic default-pool name for an image. The image is
     * lowercased, "/" and ":" become "-", any other unsafe character collapses
     * to "-", the slug is bounded to 40 characters, and leading/trailing "-" and
     * "." are stripped (a trailing "." is an invalid object-name tail). The
     * result is prefixed with "mitos-default-". It is kept byte-for-byte
     * identical to the Python {@code default_pool_name} and the TypeScript
     * {@code defaultPoolName}.
     */
    public static String defaultPoolName(String image) {
        String slug = image.toLowerCase().replace("/", "-").replace(":", "-");
        slug = SLUG_RE.matcher(slug).replaceAll("-");
        // Bound first, then strip trailing/leading "-" and "." so truncation can
        // never leave a name ending in "." or "-" (both invalid object-name tails).
        if (slug.length() > 40) {
            slug = slug.substring(0, 40);
        }
        slug = stripChars(slug, "-.");
        return DEFAULT_POOL_PREFIX + slug;
    }

    // stripChars removes any leading and trailing characters that appear in
    // chars, mirroring Python's str.strip(chars).
    private static String stripChars(String s, String chars) {
        int start = 0;
        int end = s.length();
        while (start < end && chars.indexOf(s.charAt(start)) >= 0) {
            start++;
        }
        while (end > start && chars.indexOf(s.charAt(end - 1)) >= 0) {
            end--;
        }
        return s.substring(start, end);
    }

    // ---- the one-liner entry point ----

    /** The one-liner entry point: ensure the default pool for {@code image}
     * exists, then create a Sandbox from it. */
    public ClusterSandbox sandbox(String image) {
        return sandbox(image, SandboxParams.none());
    }

    /**
     * The one-liner entry point. With {@link SandboxParams#pool()} set it claims
     * from that existing pool and creates nothing else. Otherwise it ensures the
     * default pool for {@code image} exists (creating it with an inline template
     * when absent and allowed), then creates a Sandbox from it. Exactly one of
     * {@code image} or {@code params.pool()} is required.
     */
    public ClusterSandbox sandbox(String image, SandboxParams params) {
        String pool = params.pool();
        if ((pool == null || pool.isEmpty()) && (image == null || image.isEmpty())) {
            throw new MitosException(
                    "sandbox() needs an image or a pool",
                    "missing_image_or_pool",
                    "neither image nor a pool was provided",
                    "Pass an image like \"python\" for a lazy default pool, or a pool name for an existing pool.",
                    0);
        }
        if (pool == null || pool.isEmpty()) {
            if (!allowDefaultPool) {
                throw new MitosException(
                        "default pools are disabled on this client",
                        "no_default_pool",
                        "allowDefaultPool(false) was set",
                        "Pass a pool name for an existing pool, or construct AgentRun without allowDefaultPool(false).",
                        0);
            }
            pool = ensureDefaultPool(image);
        }
        SandboxParams.Builder b = SandboxParams.builder().pool(pool);
        if (params.name() != null) {
            b.name(params.name());
        }
        if (params.env() != null) {
            b.env(params.env());
        }
        if (params.secrets() != null) {
            b.secrets(params.secrets());
        }
        if (params.ttl() != null) {
            b.ttl(params.ttl());
        }
        if (params.workspace() != null) {
            b.workspace(params.workspace());
        }
        if (params.replicas() != 0) {
            b.replicas(params.replicas());
        }
        return create(b.build());
    }

    // ensureDefaultPool get-or-creates the default SandboxPool for an image and
    // returns its name. A pre-existing pool is reused untouched (its inline image
    // is verified against the requested image to guard a slug collision); a
    // missing one is created as a single SandboxPool with inline spec.template.
    // A 409 from a concurrent creator is tolerated.
    private String ensureDefaultPool(String image) {
        String name = defaultPoolName(image);
        try {
            Map<String, Object> existing = k8s.getObject(namespace, "sandboxpools", name);
            verifyPoolImage(existing, name, image);
            return name;
        } catch (MitosException e) {
            if (e.getStatus() != 404) {
                throw e;
            }
        }

        Map<String, Object> pool = new LinkedHashMap<>();
        pool.put("apiVersion", API_GROUP + "/" + API_VERSION);
        pool.put("kind", "SandboxPool");
        pool.put("metadata", metadata(name));
        Map<String, Object> spec = new LinkedHashMap<>();
        spec.put("template", Map.of("image", image));
        spec.put("replicas", 1);
        pool.put("spec", spec);

        try {
            k8s.createObject(namespace, "sandboxpools", pool);
        } catch (MitosException e) {
            if (e.getStatus() != 409) { // raced another creator; reuse it
                throw e;
            }
        }
        return name;
    }

    // verifyPoolImage guards the default-pool reuse path against a slug collision
    // serving the wrong image. It reads the reused pool's inline
    // spec.template.image and fails closed when it is absent or does not match.
    private static void verifyPoolImage(Map<String, Object> pool, String name, String image) {
        String existing = K8s.nestedString(pool, "spec", "template", "image");
        if (existing.isEmpty()) {
            throw new MitosException(
                    "default pool " + name + " has no readable inline template image",
                    "pool_image_mismatch",
                    "pool " + name + " spec.template.image is absent or unreadable",
                    "Pass pool=\"" + name + "\" explicitly to reuse this pool, or use a distinct image that maps to a different default pool.",
                    0);
        }
        if (!existing.equals(image)) {
            throw new MitosException(
                    "default pool " + name + " already exists for a different image",
                    "pool_image_mismatch",
                    "pool " + name + " runs image \"" + existing + "\", not the requested \"" + image
                            + "\" (the image slug collides)",
                    "Pass pool=\"" + name + "\" explicitly to reuse this pool, or use a distinct image that maps to a different default pool.",
                    0);
        }
    }

    // ---- create / get / list ----

    /** Creates a Sandbox from a pool ({@link SandboxParams#pool()} required). */
    public ClusterSandbox create(SandboxParams params) {
        String pool = params.pool();
        if (pool == null || pool.isEmpty()) {
            throw new MitosException(
                    "create() needs a pool",
                    "missing_pool",
                    "no pool was provided",
                    "Pass a pool via SandboxParams.builder().pool(name); or use sandbox(image) for the lazy default-pool path.",
                    0);
        }
        String name = params.name();
        if (name == null || name.isEmpty()) {
            name = "sandbox-" + randomHex(4);
        }

        Map<String, Object> spec = new LinkedHashMap<>();
        spec.put("source", Map.of("poolRef", Map.of("name", pool)));
        if (params.replicas() != 0) {
            spec.put("replicas", params.replicas());
        }
        if (params.env() != null && !params.env().isEmpty()) {
            List<Object> envList = new ArrayList<>();
            for (Map.Entry<String, String> e : params.env().entrySet()) {
                Map<String, Object> entry = new LinkedHashMap<>();
                entry.put("name", e.getKey());
                entry.put("value", e.getValue());
                envList.add(entry);
            }
            spec.put("env", envList);
        }
        if (params.secrets() != null && !params.secrets().isEmpty()) {
            List<Object> secretList = new ArrayList<>();
            for (Map.Entry<String, SecretRef> e : params.secrets().entrySet()) {
                String envVar = e.getKey();
                SecretRef ref = e.getValue();
                Map<String, Object> entry = new LinkedHashMap<>();
                entry.put("name", envVar);
                entry.put("secretRef", Map.of("name", ref.secretName(), "key", ref.key()));
                entry.put("envVar", envVar);
                secretList.add(entry);
            }
            spec.put("secrets", secretList);
        }
        if (params.ttl() != null && !params.ttl().isEmpty()) {
            spec.put("lifetime", new LinkedHashMap<>(Map.of("ttl", params.ttl())));
        }
        if (params.workspace() != null && !params.workspace().isEmpty()) {
            spec.put("workspaceRef", Map.of("name", params.workspace()));
        }

        Map<String, Object> body = new LinkedHashMap<>();
        body.put("apiVersion", API_GROUP + "/" + API_VERSION);
        body.put("kind", "Sandbox");
        body.put("metadata", metadata(name));
        body.put("spec", spec);

        k8s.createObject(namespace, "sandboxes", body);
        return new ClusterSandbox(name, namespace, pool, SandboxPhase.PENDING, "", k8s);
    }

    /** Reconnects to an existing sandbox by name, returning a live handle. An
     * alias for {@link #get(String)}, named for the reconnect use case. */
    public ClusterSandbox fromName(String name) {
        return get(name);
    }

    /**
     * Reads an existing sandbox by name and returns a handle carrying its
     * resolved pool, phase, and endpoint. When the sandbox is Ready its
     * per-sandbox bearer token is loaded from the {@code <name>-sandbox-token}
     * Secret.
     */
    public ClusterSandbox get(String name) {
        Map<String, Object> obj = k8s.getObject(namespace, "sandboxes", name);
        String pool = K8s.nestedString(obj, "spec", "source", "poolRef", "name");
        SandboxPhase phase = SandboxPhase.fromWire(K8s.nestedString(obj, "status", "phase"));
        String endpoint = K8s.nestedString(obj, "status", "endpoint");
        ClusterSandbox sb = new ClusterSandbox(name, namespace, pool, phase, endpoint, k8s);
        if (phase == SandboxPhase.READY) {
            sb.loadToken();
        }
        return sb;
    }

    /** Lists sandboxes in the namespace, optionally filtered by pool. A null or
     * empty pool returns every sandbox. */
    public List<ClusterSandbox> list(String pool) {
        List<Object> items = k8s.listObjects(namespace, "sandboxes");
        List<ClusterSandbox> out = new ArrayList<>();
        for (Object item : items) {
            Map<String, Object> obj = K8s.asMap(item);
            String objPool = K8s.nestedString(obj, "spec", "source", "poolRef", "name");
            if (pool != null && !pool.isEmpty() && !objPool.equals(pool)) {
                continue;
            }
            out.add(new ClusterSandbox(
                    K8s.nestedString(obj, "metadata", "name"),
                    namespace,
                    objPool,
                    SandboxPhase.fromWire(K8s.nestedString(obj, "status", "phase")),
                    K8s.nestedString(obj, "status", "endpoint"),
                    k8s));
        }
        return out;
    }

    // ---- pool status ----

    /** Reads the status of a SandboxPool. */
    public PoolStatus poolStatus(String name) {
        Map<String, Object> obj = k8s.getObject(namespace, "sandboxpools", name);
        Map<String, Integer> dist = new LinkedHashMap<>();
        Object raw = K8s.nested(obj, "status", "nodeDistribution");
        if (raw instanceof Map<?, ?>) {
            for (Map.Entry<String, Object> e : K8s.asMap(raw).entrySet()) {
                dist.put(e.getKey(), K8s.asInt(e.getValue()));
            }
        }
        return new PoolStatus(
                name,
                K8s.asInt(K8s.nested(obj, "status", "readySnapshots")),
                K8s.asInt(K8s.nested(obj, "spec", "replicas")),
                dist);
    }

    // ---- workspaces ----

    /** Creates an empty durable Workspace and returns a handle. */
    public ClusterWorkspace createWorkspace(String name) {
        Map<String, Object> body = new LinkedHashMap<>();
        body.put("apiVersion", API_GROUP + "/" + API_VERSION);
        body.put("kind", "Workspace");
        body.put("metadata", metadata(name));
        body.put("spec", new LinkedHashMap<>());
        k8s.createObject(namespace, "workspaces", body);
        return new ClusterWorkspace(name, namespace, k8s);
    }

    /** Returns a lazy handle to a workspace by name. It does not touch the
     * cluster until a verb is called; use {@link #createWorkspace(String)} to
     * create one or {@link #getWorkspace(String)} to reconnect and verify. */
    public ClusterWorkspace workspace(String name) {
        return new ClusterWorkspace(name, namespace, k8s);
    }

    /** Reconnects to an existing workspace, raising {@code workspace_not_found}
     * when it is absent. */
    public ClusterWorkspace getWorkspace(String name) {
        ClusterWorkspace ws = new ClusterWorkspace(name, namespace, k8s);
        ws.get(); // raises workspace_not_found when absent
        return ws;
    }

    /** Lists the workspaces in the client's namespace. */
    public List<ClusterWorkspace> listWorkspaces() {
        List<Object> items = k8s.listObjects(namespace, "workspaces");
        List<ClusterWorkspace> out = new ArrayList<>();
        for (Object item : items) {
            Map<String, Object> obj = K8s.asMap(item);
            out.add(new ClusterWorkspace(
                    K8s.nestedString(obj, "metadata", "name"), namespace, k8s));
        }
        return out;
    }

    // ---- helpers ----

    private Map<String, Object> metadata(String name) {
        Map<String, Object> meta = new LinkedHashMap<>();
        meta.put("name", name);
        meta.put("namespace", namespace);
        return meta;
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
}
