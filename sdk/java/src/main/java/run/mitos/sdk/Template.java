// A template as reported by the sandbox-server. Mirrors the wire shape
// {id, ready, created_at, creation_time_ms} from cmd/sandbox-server.
package run.mitos.sdk;

/**
 * A template known to the sandbox-server.
 *
 * @param id             the template id
 * @param ready          whether the template is built and ready to fork
 * @param createdAt      the server-reported creation timestamp
 * @param creationTimeMs the build time in milliseconds
 */
public record Template(String id, boolean ready, String createdAt, double creationTimeMs) {
}
