// A snapshot of a cluster-mode sandbox's status, returned by
// ClusterSandbox.info(). Mirrors the Python SandboxInfo fields.
package run.mitos.sdk;

/**
 * A snapshot of a cluster-mode sandbox's observed status.
 *
 * @param name      the Sandbox object name
 * @param phase     the lifecycle phase
 * @param endpoint  the serving endpoint (host:port), empty until Ready
 * @param node      the node the sandbox is scheduled on
 * @param sandboxId the runtime sandbox id reported by forkd
 * @param pool      the SandboxPool the sandbox was claimed from
 */
public record SandboxInfo(
        String name,
        SandboxPhase phase,
        String endpoint,
        String node,
        String sandboxId,
        String pool) {
}
