// A cluster-mode Sandbox handle (a CRD-backed sandbox), as opposed to the
// direct-mode Sandbox. It carries the cluster identity (name, namespace, pool)
// and the last-observed phase and endpoint, and reads the per-sandbox bearer
// token from the <name>-sandbox-token Secret. Mirrors the Python cluster
// Sandbox surface (sdk/python/mitos/sandbox.py): waitUntilReady, info,
// terminate, and the token for callers driving the sandbox HTTP API themselves.
package run.mitos.sdk;

import java.util.LinkedHashMap;
import java.util.Map;

/**
 * A running cluster-mode sandbox (a {@code mitos.run/v1} Sandbox object). Unlike
 * the direct-mode {@link Sandbox}, exec and file traffic flow through the
 * controller-assigned endpoint gated by the per-sandbox token; this handle owns
 * the cluster lifecycle (wait for Ready, read status, terminate) and exposes the
 * resolved endpoint and token so a caller can drive the sandbox HTTP API. The
 * token value is held in memory only and is never logged.
 */
public final class ClusterSandbox {

    /** The default wait for a sandbox to reach Ready, in milliseconds. */
    public static final long DEFAULT_READY_TIMEOUT_MS = 30_000;

    // The poll interval while waiting for a phase transition, in milliseconds.
    private static final long POLL_INTERVAL_MS = 50;

    private final String name;
    private final String namespace;
    private final String pool;
    private final K8s k8s;

    private SandboxPhase phase;
    private String endpoint;
    private String token; // per-sandbox bearer token; in memory only, never logged

    ClusterSandbox(String name, String namespace, String pool, SandboxPhase phase,
                   String endpoint, K8s k8s) {
        this.name = name;
        this.namespace = namespace;
        this.pool = pool;
        this.phase = phase;
        this.endpoint = endpoint;
        this.k8s = k8s;
    }

    /** The Sandbox object name. */
    public String name() {
        return name;
    }

    /** The Sandbox object namespace. */
    public String namespace() {
        return namespace;
    }

    /** The SandboxPool this sandbox was claimed from. */
    public String pool() {
        return pool;
    }

    /** The last-observed lifecycle phase. */
    public SandboxPhase phase() {
        return phase;
    }

    /** The last-observed serving endpoint (host:port), empty until Ready. */
    public String endpoint() {
        return endpoint;
    }

    /**
     * The per-sandbox bearer token, if one has been loaded, for callers that
     * drive the sandbox HTTP API themselves. It is empty when the sandbox is not
     * Ready or has no token Secret. The token value is never logged.
     */
    public String token() {
        return token == null ? "" : token;
    }

    /**
     * Blocks until the sandbox is Ready (then returns this so it chains), or
     * raises {@link MitosException} with code {@code sandbox_failed} or
     * {@code ready_timeout}. Idempotent: returns immediately when already Ready
     * with an endpoint. Uses the default {@value #DEFAULT_READY_TIMEOUT_MS} ms
     * timeout.
     */
    public ClusterSandbox waitUntilReady() {
        return waitUntilReady(DEFAULT_READY_TIMEOUT_MS);
    }

    /** As {@link #waitUntilReady()} but waits up to {@code timeoutMs}. */
    public ClusterSandbox waitUntilReady(long timeoutMs) {
        if (phase == SandboxPhase.READY && !endpoint.isEmpty()) {
            return this;
        }
        long deadline = System.currentTimeMillis() + timeoutMs;
        while (System.currentTimeMillis() < deadline) {
            Map<String, Object> obj = k8s.getObject(namespace, "sandboxes", name);
            phase = SandboxPhase.fromWire(K8s.nestedString(obj, "status", "phase"));
            endpoint = K8s.nestedString(obj, "status", "endpoint");

            if (phase == SandboxPhase.READY && !endpoint.isEmpty()) {
                loadToken();
                return this;
            }
            if (phase == SandboxPhase.FAILED) {
                throw new MitosException(
                        "sandbox " + name + " failed",
                        "sandbox_failed",
                        "sandbox " + name + " reached the Failed phase",
                        "Inspect the Sandbox status conditions and the pool capacity.",
                        0);
            }
            sleep();
        }
        throw new MitosException(
                "sandbox " + name + " not ready after " + timeoutMs + "ms",
                "ready_timeout",
                "sandbox " + name + " did not reach Ready within " + timeoutMs + "ms",
                "Raise the timeout, or check the controller is reconciling and the pool has capacity.",
                0);
    }

    /** Reads the current sandbox status from the cluster as a {@link SandboxInfo}. */
    public SandboxInfo info() {
        Map<String, Object> obj = k8s.getObject(namespace, "sandboxes", name);
        phase = SandboxPhase.fromWire(K8s.nestedString(obj, "status", "phase"));
        endpoint = K8s.nestedString(obj, "status", "endpoint");
        return new SandboxInfo(
                name,
                phase,
                endpoint,
                K8s.nestedString(obj, "status", "node"),
                K8s.nestedString(obj, "status", "sandboxID"),
                pool);
    }

    /**
     * Terminates the sandbox by deleting the Sandbox object. The controller
     * drives teardown (and, for a workspace-bound sandbox, the dehydrate on the
     * way out). Returns the bound workspace name, or empty when unbound.
     */
    public String terminate() {
        String wsRef = "";
        try {
            Map<String, Object> obj = k8s.getObject(namespace, "sandboxes", name);
            wsRef = K8s.nestedString(obj, "spec", "workspaceRef", "name");
        } catch (MitosException e) {
            if (e.getStatus() != 404) {
                throw e;
            }
        }
        k8s.deleteObject(namespace, "sandboxes", name);
        phase = SandboxPhase.TERMINATING;
        return wsRef;
    }

    // loadToken reads the per-sandbox bearer token from the <name>-sandbox-token
    // Secret. A missing Secret is tolerated (the sandbox stays tokenless). The
    // token VALUE is held in memory only and is never logged.
    void loadToken() {
        Map<String, String> data;
        try {
            data = k8s.readSecret(namespace, name + "-sandbox-token");
        } catch (MitosException e) {
            return; // a missing token Secret is tolerated
        }
        String value = data.get("token");
        if (value != null && !value.isEmpty()) {
            token = value;
        }
    }

    private static void sleep() {
        try {
            Thread.sleep(POLL_INTERVAL_MS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new MitosException(
                    "interrupted while waiting for the sandbox",
                    "interrupted",
                    "the waiting thread was interrupted",
                    "Retry the wait from a thread that is not interrupted.",
                    0);
        }
    }

    @Override
    public String toString() {
        // The token is never included in the string form.
        Map<String, Object> repr = new LinkedHashMap<>();
        repr.put("name", name);
        repr.put("phase", phase.wire());
        repr.put("endpoint", endpoint);
        return "ClusterSandbox" + repr;
    }
}
