// A sandbox handle returned by SandboxServer.fork. exec runs over the Connect
// sandbox.v1.Sandbox runtime protocol (the ExecStream server-streaming RPC,
// issue #358/#24) and terminate issues DELETE /v1/sandboxes/{id}. The handle
// holds the SandboxServer it was forked from so requests carry the same base URL
// and optional bearer header. Mirrors the Ruby Mitos::Sandbox and the Python
// DirectSandbox exec / terminate surface.
package run.mitos.sdk;

import java.nio.charset.StandardCharsets;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.List;
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
     * <p>The call uses the Connect {@code sandbox.v1.Sandbox/ExecStream}
     * server-streaming RPC: it sends one request message and drains the response
     * stream of stdout, stderr, and the terminal exit frame, returning the same
     * {@link ExecResult} shape the legacy path did.
     *
     * @param command        the shell command to run
     * @param timeoutSeconds the per-command timeout in seconds
     */
    public ExecResult exec(String command, int timeoutSeconds) {
        Map<String, Object> request = new LinkedHashMap<>();
        request.put("command", command);
        // Proto-JSON omits the field entirely when there is no timeout; a positive
        // value rides as camelCase timeoutSeconds.
        if (timeoutSeconds > 0) {
            request.put("timeoutSeconds", timeoutSeconds);
        }

        ConnectClient connect = new ConnectClient(server.transport());
        List<Map<String, Object>> messages = connect.serverStream("ExecStream", id, request);

        StringBuilder stdout = new StringBuilder();
        StringBuilder stderr = new StringBuilder();
        int exitCode = 0;
        double execTimeMs = 0.0;
        for (Map<String, Object> m : messages) {
            if (m.containsKey("stdout")) {
                stdout.append(decodeBase64(SandboxServer.asString(m.get("stdout"))));
            } else if (m.containsKey("stderr")) {
                stderr.append(decodeBase64(SandboxServer.asString(m.get("stderr"))));
            } else if (m.get("exit") instanceof Map<?, ?>) {
                Map<String, Object> exit = SandboxServer.asObject(m.get("exit"));
                exitCode = SandboxServer.asInt(exit.get("exitCode"));
                execTimeMs = SandboxServer.asDouble(exit.get("execTimeMs"));
            }
        }
        return new ExecResult(exitCode, stdout.toString(), stderr.toString(), execTimeMs);
    }

    /** Decodes a base64 proto-JSON bytes field to a UTF-8 string. An empty or
     * null value decodes to the empty string. */
    private static String decodeBase64(String value) {
        if (value == null || value.isEmpty()) {
            return "";
        }
        return new String(Base64.getDecoder().decode(value), StandardCharsets.UTF_8);
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
