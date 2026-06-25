// The observed status of a SandboxPool, mirroring the Python PoolStatus.
package run.mitos.sdk;

import java.util.Map;

/**
 * The observed status of a SandboxPool.
 *
 * @param name             the pool name
 * @param readySnapshots   the number of warm snapshots ready to fork from
 * @param desired          the pool's spec.replicas
 * @param nodeDistribution node name to ready-snapshot count on that node
 */
public record PoolStatus(
        String name,
        int readySnapshots,
        int desired,
        Map<String, Integer> nodeDistribution) {
}
