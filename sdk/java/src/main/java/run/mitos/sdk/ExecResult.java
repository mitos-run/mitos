// The result of a Sandbox.exec call. Mirrors the Python ExecResult and the
// sandbox-server /v1/exec response {exit_code, stdout, stderr, exec_time_ms}.
package run.mitos.sdk;

/**
 * The outcome of a command run in a sandbox.
 *
 * @param exitCode   the command exit code (0 on success)
 * @param stdout     the captured standard output
 * @param stderr     the captured standard error
 * @param execTimeMs the wall-clock execution time in milliseconds
 */
public record ExecResult(int exitCode, String stdout, String stderr, double execTimeMs) {

    /** Whether the command exited successfully (exit code 0). */
    public boolean success() {
        return exitCode == 0;
    }
}
