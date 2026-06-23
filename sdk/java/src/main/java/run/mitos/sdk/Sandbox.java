// A sandbox handle returned by SandboxServer.fork. exec round-trips through the
// server URL (POST /v1/exec) and terminate issues DELETE /v1/sandboxes/{id}.
// The handle holds the SandboxServer it was forked from so requests carry the
// same base URL and optional bearer header. Mirrors the Ruby Mitos::Sandbox and
// the Python DirectSandbox exec / terminate surface.
package run.mitos.sdk;

import java.util.LinkedHashMap;
import java.util.Map;

/** A running sandbox in direct (sandbox-server) mode. */
public final class Sandbox {

    /** The default exec timeout in seconds, matching the other SDKs. */
    public static final int DEFAULT_EXEC_TIMEOUT_SECONDS = 30;

    private final String id;
    private final String endpoint;
    private final SandboxServer server;

    Sandbox(String id, String endpoint, SandboxServer server) {
        this.id = id;
        this.endpoint = endpoint;
        this.server = server;
    }

    /** The sandbox id. */
    public String id() {
        return id;
    }

    /** The server endpoint serving this sandbox. */
    public String endpoint() {
        return endpoint;
    }

    /** Runs a command with the default timeout and returns an {@link ExecResult}. */
    public ExecResult exec(String command) {
        return exec(command, DEFAULT_EXEC_TIMEOUT_SECONDS);
    }

    /**
     * Runs {@code command} in the sandbox and returns an {@link ExecResult}.
     * Requires a Ready sandbox: the sandbox-server routes exec through the guest
     * agent over vsock, so a sandbox that is not yet up returns a typed error.
     *
     * @param command        the shell command to run
     * @param timeoutSeconds the per-command timeout in seconds
     */
    public ExecResult exec(String command, int timeoutSeconds) {
        Map<String, Object> body = new LinkedHashMap<>();
        body.put("sandbox", id);
        body.put("command", command);
        body.put("timeout", timeoutSeconds);
        Object data = server.transport().post("/v1/exec", body, null);
        Map<String, Object> m = SandboxServer.asObject(data);
        return new ExecResult(
                SandboxServer.asInt(m.get("exit_code")),
                SandboxServer.asString(m.get("stdout")),
                SandboxServer.asString(m.get("stderr")),
                SandboxServer.asDouble(m.get("exec_time_ms")));
    }

    /** Terminates the sandbox via DELETE /v1/sandboxes/{id}. */
    public void terminate() {
        server.terminate(id);
    }

    @Override
    public String toString() {
        return "Sandbox{id=" + id + ", endpoint=" + endpoint + "}";
    }
}
