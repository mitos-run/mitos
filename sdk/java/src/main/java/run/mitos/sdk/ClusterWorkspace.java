// A durable, forkable agent workspace handle in cluster mode. Lazy: it does not
// touch the cluster until a verb is called. Mirrors the Python Workspace
// (sdk/python/mitos/workspace.py): git-shaped verbs (head, resumable, log).
package run.mitos.sdk;

import java.util.ArrayList;
import java.util.List;
import java.util.Map;

/**
 * A durable, forkable workspace ({@code mitos.run/v1} Workspace). It is lazy: no
 * cluster call happens until a verb is called. Verbs are git-shaped, mirroring
 * the Python {@code Workspace}.
 */
public final class ClusterWorkspace {

    private final String name;
    private final String namespace;
    private final K8s k8s;

    ClusterWorkspace(String name, String namespace, K8s k8s) {
        this.name = name;
        this.namespace = namespace;
        this.k8s = k8s;
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
