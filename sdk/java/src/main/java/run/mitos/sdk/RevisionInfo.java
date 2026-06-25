// One Workspace revision as listed in a workspace log. Mirrors the Python
// RevisionInfo dataclass.
package run.mitos.sdk;

/**
 * One revision in a workspace's history.
 *
 * @param name      the WorkspaceRevision object name
 * @param phase     the revision phase (for example "Committed")
 * @param lineage   a short description of the revision's source
 * @param resumable whether the revision carries a memory snapshot
 * @param created   the creation timestamp (RFC3339)
 */
public record RevisionInfo(
        String name,
        String phase,
        String lineage,
        boolean resumable,
        String created) {
}
