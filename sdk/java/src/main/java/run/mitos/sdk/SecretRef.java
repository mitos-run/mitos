// A reference to a key in a Kubernetes Secret to inject as an environment
// variable, mirroring the Python secrets={env: (secret, key)} mapping. Secret
// VALUES never pass through the SDK; only these references do.
package run.mitos.sdk;

/**
 * A reference to a Kubernetes Secret key, injected into a sandbox as an
 * environment variable. The SDK never reads or carries the secret value; the
 * controller resolves the reference inside the cluster.
 *
 * @param secretName the name of the Secret object
 * @param key        the key within the Secret's data
 */
public record SecretRef(String secretName, String key) {
}
