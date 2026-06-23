// A sandbox summary as reported by GET /v1/sandboxes. Mirrors the wire shape
// {id, template_id, endpoint, created_at, fork_time_ms} from cmd/sandbox-server.
package run.mitos.sdk;

/**
 * A sandbox summary as listed by the sandbox-server.
 *
 * @param id         the sandbox id
 * @param templateId the id of the template it was forked from
 * @param endpoint   the server endpoint serving the sandbox
 * @param createdAt  the server-reported creation timestamp
 * @param forkTimeMs the fork time in milliseconds
 */
public record ServerSandbox(String id, String templateId, String endpoint,
                            String createdAt, double forkTimeMs) {
}
