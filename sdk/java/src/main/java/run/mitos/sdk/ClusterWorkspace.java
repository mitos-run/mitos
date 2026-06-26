// A durable, forkable agent workspace handle in cluster mode. Lazy: it does not
// touch the cluster until a verb is called. Mirrors the Python Workspace
// (sdk/python/mitos/workspace.py): git-shaped verbs (head, resumable, log).
package run.mitos.sdk;

import java.security.SecureRandom;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.regex.Pattern;

/**
 * A durable, forkable workspace ({@code mitos.run/v1} Workspace). It is lazy: no
 * cluster call happens until a verb is called. Verbs are git-shaped, mirroring
 * the Python {@code Workspace}.
 */
public final class ClusterWorkspace {

    private final String name;
    private final String namespace;
    private final K8s k8s;

    // serveWaitIntervalMs is the polling interval, in milliseconds, while waiting
    // for the sandbox to reach Ready. It is a per-instance field so two workspaces
    // never share mutable wait state; tests set it on their own instance via
    // setServeWaitIntervalMs to avoid sleeping. Defaults to 500ms.
    private long serveWaitIntervalMs = 500;

    // serveMaxAttempts bounds the readiness poll so serve() cannot loop forever
    // when a sandbox never reaches Ready or Failed (an unknown phase maps to
    // Pending). With the 500ms default interval this is about a 5 minute ceiling;
    // tests set the interval to 0, so the cap is reached almost instantly when a
    // fixture never returns Ready. Per-instance so workspaces never share state.
    private int serveMaxAttempts = 600;

    ClusterWorkspace(String name, String namespace, K8s k8s) {
        this.name = name;
        this.namespace = namespace;
        this.k8s = k8s;
    }

    // setServeWaitIntervalMs overrides the per-instance Ready poll interval. It is
    // package-private so a test can set it to 0 on its specific workspace without
    // sharing state with any other instance.
    void setServeWaitIntervalMs(long ms) {
        this.serveWaitIntervalMs = ms;
    }

    // setServeMaxAttempts overrides the per-instance readiness poll cap. Package
    // private so a test can lower it on its own instance.
    void setServeMaxAttempts(int attempts) {
        this.serveMaxAttempts = attempts;
    }

    /** The Workspace object name. */
    public String name() {
        return name;
    }

    /** The Workspace object namespace. */
    public String namespace() {
        return namespace;
    }

    // get reads the Workspace object, mapping a 404 to a typed
    // workspace_not_found error.
    Map<String, Object> get() {
        try {
            return k8s.getObject(namespace, "workspaces", name);
        } catch (MitosException e) {
            if (e.getStatus() == 404) {
                throw new MitosException(
                        "workspace " + name + " not found",
                        "workspace_not_found",
                        e.getCauseDetail(),
                        "Create it with AgentRun.createWorkspace(name) first.",
                        404);
            }
            throw e;
        }
    }

    /** The workspace head revision name (status.head). */
    public String head() {
        return K8s.nestedString(get(), "status", "head");
    }

    /** Whether the workspace head is resumable (status.resumable). */
    public boolean resumable() {
        Object v = K8s.nested(get(), "status", "resumable");
        return v instanceof Boolean b && b;
    }

    /** Lists the workspace's revisions, newest first by creation timestamp. */
    public List<RevisionInfo> log() {
        List<Object> items = k8s.listObjects(namespace, "workspacerevisions");
        List<RevisionInfo> revs = new ArrayList<>();
        for (Object item : items) {
            Map<String, Object> obj = K8s.asMap(item);
            if (!K8s.nestedString(obj, "spec", "workspaceRef", "name").equals(name)) {
                continue;
            }
            boolean hasSnap = K8s.nested(obj, "spec", "memorySnapshotRef") != null;
            revs.add(new RevisionInfo(
                    K8s.nestedString(obj, "metadata", "name"),
                    K8s.nestedString(obj, "status", "phase"),
                    lineage(obj),
                    hasSnap,
                    K8s.nestedString(obj, "metadata", "creationTimestamp")));
        }
        // Newest first by creation timestamp (RFC3339 strings sort lexically).
        revs.sort((a, b) -> b.created().compareTo(a.created()));
        return revs;
    }

    // ---- serve ----

    // Reserved expose labels that tenants may not use. Mirrors the Go SDK
    // reservedExposeLabels (sdk/go/serve.go) and internal/preview/route.go.
    // Keep this list consistent with the proxy reservedLabels map.
    private static final Set<String> RESERVED_EXPOSE_LABELS = Set.of(
            "www", "app", "api", "console", "gateway",
            "admin", "auth", "login", "account", "mail",
            "static", "assets", "cdn", "status");

    // Matches a valid single DNS label: starts and ends with alphanumeric, may
    // contain hyphens in the middle, max 63 characters. Lowercase only.
    private static final Pattern EXPOSE_LABEL_RE =
            Pattern.compile("^[a-z0-9]([a-z0-9-]*[a-z0-9])?$");

    private static final SecureRandom SERVE_RANDOM = new SecureRandom();
    private static final char[] SERVE_HEX = "0123456789abcdef".toCharArray();

    /**
     * Creates a Sandbox bound to this workspace with {@code spec.expose} set,
     * polls until the sandbox reaches Ready, and returns a {@link ServedWorkspace}
     * carrying the public HTTPS URL.
     *
     * <p>Options: pool is required; port defaults to 8080; sharing defaults to
     * "private"; label defaults to the generated sandbox name; exposeDomain
     * defaults to the MITOS_EXPOSE_DOMAIN environment variable.
     *
     * <p>Note: link-token minting is a follow-up. The proxy enforces the sharing
     * tier independently without a per-sandbox bearer token at this layer.
     */
    public ServedWorkspace serve(ServeOptions opts) {
        if (opts.pool() == null || opts.pool().isEmpty()) {
            throw new MitosException(
                    "serve() needs a pool",
                    "missing_serve_pool",
                    "ServeOptions.pool() was not set",
                    "Pass ServeOptions.builder().pool(name).build() to select the SandboxPool to claim from.",
                    0);
        }
        int port = opts.port();
        if (port < 1 || port > 65535) {
            throw new MitosException(
                    "serve port out of range",
                    "invalid_serve_port",
                    "port " + port + " is not in 1-65535",
                    "Pass ServeOptions.builder().port(n).build() with a port in the range 1-65535.",
                    0);
        }

        // Resolve expose domain: option first, then env var.
        String exposeDomain = opts.exposeDomain();
        if (exposeDomain == null || exposeDomain.isEmpty()) {
            exposeDomain = System.getenv("MITOS_EXPOSE_DOMAIN");
        }
        if (exposeDomain == null || exposeDomain.isEmpty()) {
            throw new MitosException(
                    "expose domain is required",
                    "missing_expose_domain",
                    "no expose domain was provided and MITOS_EXPOSE_DOMAIN is not set",
                    "Pass ServeOptions.builder().exposeDomain(domain).build() or set the MITOS_EXPOSE_DOMAIN environment variable.",
                    0);
        }

        // Generate the sandbox name up front so it can serve as the default label
        // before the server assigns one (we control the name).
        String sbName = "sandbox-" + serveRandomHex(4);

        // Determine the effective label; fall back to the sandbox name when not set.
        String rawLabel = opts.label();
        String label = (rawLabel == null || rawLabel.isEmpty()) ? sbName : rawLabel;

        // Normalize to lowercase before validation.
        label = label.toLowerCase();

        // Validate label and build the URL before sending anything to the cluster
        // so a bad label fails fast without leaving a partially configured sandbox.
        validateExposeLabel(label);
        String url = "https://" + label + "." + exposeDomain + "/";

        // Build the Sandbox CRD body with spec.expose in the initial POST.
        // This matches the api/v1 SandboxExpose JSON shape: port, label, sharing.
        // The new policy fields are optional and omitted here.
        Map<String, Object> expose = new LinkedHashMap<>();
        expose.put("port", port);
        expose.put("label", label);
        expose.put("sharing", opts.sharing());

        Map<String, Object> spec = new LinkedHashMap<>();
        spec.put("source", Map.of("poolRef", Map.of("name", opts.pool())));
        spec.put("workspaceRef", Map.of("name", name));
        spec.put("expose", expose);

        Map<String, Object> body = new LinkedHashMap<>();
        body.put("apiVersion", K8s.API_GROUP + "/" + K8s.API_VERSION);
        body.put("kind", "Sandbox");
        Map<String, Object> meta = new LinkedHashMap<>();
        meta.put("name", sbName);
        meta.put("namespace", namespace);
        body.put("metadata", meta);
        body.put("spec", spec);

        k8s.createObject(namespace, "sandboxes", body);

        // Wait until the sandbox reaches Ready (or a typed error is thrown).
        waitSandboxReady(sbName);

        return new ServedWorkspace(url, sbName, label, opts.sharing());
    }

    // validateExposeLabel rejects labels that are empty, too long, do not match
    // the DNS-label pattern, or are in the reserved set.
    private static void validateExposeLabel(String label) {
        if (label.isEmpty()) {
            throw new MitosException(
                    "expose label is required",
                    "invalid_expose_label",
                    "label is empty",
                    "Pass ServeOptions.builder().label(name).build() or use a sandbox name that is a valid single DNS label.",
                    0);
        }
        if (label.length() > 63) {
            throw new MitosException(
                    "expose label \"" + label + "\" exceeds 63 characters",
                    "invalid_expose_label",
                    "label length " + label.length() + " > 63",
                    "Use a shorter label (at most 63 characters).",
                    0);
        }
        if (!EXPOSE_LABEL_RE.matcher(label).matches()) {
            throw new MitosException(
                    "expose label \"" + label + "\" is not a valid single DNS label",
                    "invalid_expose_label",
                    "label must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$",
                    "Use only lowercase letters, digits, and hyphens; do not start or end with a hyphen.",
                    0);
        }
        if (RESERVED_EXPOSE_LABELS.contains(label)) {
            throw new MitosException(
                    "expose label \"" + label + "\" is reserved and may not be used by tenants",
                    "reserved_expose_label",
                    "label \"" + label + "\" is in the reserved set",
                    "Choose a different label that is not a well-known control-plane name.",
                    0);
        }
    }

    // waitSandboxReady polls the Sandbox until it reaches Ready or Failed, or the
    // attempt cap is reached (so an unknown or stuck phase cannot hang serve()).
    private void waitSandboxReady(String sbName) {
        for (int attempt = 0; attempt < serveMaxAttempts; attempt++) {
            Map<String, Object> obj;
            try {
                obj = k8s.getObject(namespace, "sandboxes", sbName);
            } catch (MitosException e) {
                throw new MitosException(
                        "serve: wait ready: " + e.getMessage(),
                        e.getCode(),
                        e.getCauseDetail(),
                        e.getRemediation(),
                        e.getStatus());
            }
            SandboxPhase phase = SandboxPhase.fromWire(
                    K8s.nestedString(obj, "status", "phase"));
            if (phase == SandboxPhase.READY) {
                return;
            }
            if (phase == SandboxPhase.FAILED) {
                throw new MitosException(
                        "sandbox " + sbName + " reached Failed phase",
                        "sandbox_failed",
                        "the controller reported a Failed phase before Ready",
                        "Check the Sandbox status for more detail (kubectl describe sandbox " + sbName + ").",
                        0);
            }
            try {
                Thread.sleep(serveWaitIntervalMs);
            } catch (InterruptedException ie) {
                Thread.currentThread().interrupt();
                throw new MitosException(
                        "interrupted while waiting for sandbox to become ready",
                        "serve_interrupted",
                        "Thread.sleep was interrupted",
                        "Re-run serve() or check whether the sandbox " + sbName + " reached Ready.",
                        0);
            }
        }
        throw new MitosException(
                "sandbox " + sbName + " did not become Ready in time",
                "serve_timeout",
                "the readiness poll reached its attempt cap before the sandbox reported Ready",
                "Check the Sandbox status (kubectl describe sandbox " + sbName + ") and the pool capacity.",
                0);
    }

    private static String serveRandomHex(int bytes) {
        byte[] buf = new byte[bytes];
        SERVE_RANDOM.nextBytes(buf);
        char[] out = new char[bytes * 2];
        for (int i = 0; i < bytes; i++) {
            out[i * 2] = SERVE_HEX[(buf[i] >> 4) & 0xf];
            out[i * 2 + 1] = SERVE_HEX[buf[i] & 0xf];
        }
        return new String(out);
    }

    // ---- revisions ----

    // lineage describes a revision's source, mirroring the Python _lineage.
    private static String lineage(Map<String, Object> obj) {
        String fromClaim = K8s.nestedString(obj, "spec", "source", "fromClaim");
        if (!fromClaim.isEmpty()) {
            return "fromClaim:" + fromClaim;
        }
        Object fwr = K8s.nested(obj, "spec", "source", "fromWorkspaceRevision");
        if (fwr instanceof Map<?, ?>) {
            return "fromWorkspaceRevision:" + K8s.nestedString(K8s.asMap(fwr), "revision");
        }
        return "root";
    }
}
