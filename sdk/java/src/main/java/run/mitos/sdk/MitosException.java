// MitosException is the LLM-legible error raised by the Java SDK. It mirrors the
// server envelope {error:{code, message, cause, remediation}} and the Python
// AgentRunError, the TypeScript AgentRunError, and the Ruby MitosError.
//
// It is an UNCHECKED exception (a RuntimeException subclass) so callers are not
// forced to wrap every SDK call in a try/catch; branch on getCode() to handle a
// specific failure. No bearer token or secret value ever appears in any field:
// the SDK redacts the configured api key from the response body before it
// becomes a cause, so a token a hostile or misconfigured server reflects into
// its error body never surfaces here.
package run.mitos.sdk;

/**
 * The structured error thrown by the mitos SDK. {@code code} is a stable,
 * machine-readable identifier callers branch on (never the message text);
 * {@code causeDetail} is the underlying detail (the server body, redacted);
 * {@code remediation} is a short actionable hint; {@code status} is the HTTP
 * status when the error came from a response (0 otherwise, for example an
 * invalid id rejected before any request).
 *
 * <p>This is unchecked for ergonomics: you may catch it, but the compiler does
 * not require it. The bearer token never appears in the message or any field.
 */
public final class MitosException extends RuntimeException {

    private static final long serialVersionUID = 1L;

    private final String code;
    private final String causeDetail;
    private final String remediation;
    private final int status;

    /**
     * Builds a structured SDK error.
     *
     * @param message     a human-readable summary (no secret values)
     * @param code        a stable machine-readable code callers branch on
     * @param causeDetail the underlying detail, redacted of any token
     * @param remediation a short actionable hint
     * @param status      the HTTP status, or 0 when not from a response
     */
    public MitosException(String message, String code, String causeDetail,
                          String remediation, int status) {
        super(message);
        this.code = code;
        this.causeDetail = causeDetail;
        this.remediation = remediation;
        this.status = status;
    }

    /** The stable, machine-readable error code. Branch on this, not the message. */
    public String getCode() {
        return code;
    }

    /** The underlying detail (the server body, redacted of any bearer token). */
    public String getCauseDetail() {
        return causeDetail;
    }

    /** A short, actionable remediation hint. */
    public String getRemediation() {
        return remediation;
    }

    /** The HTTP status code, or 0 when the error did not come from a response. */
    public int getStatus() {
        return status;
    }

    @Override
    public String getMessage() {
        StringBuilder sb = new StringBuilder("[").append(code).append("] ")
                .append(super.getMessage());
        if (causeDetail != null && !causeDetail.isEmpty()) {
            sb.append(" | cause: ").append(causeDetail);
        }
        if (remediation != null && !remediation.isEmpty()) {
            sb.append(" | remediation: ").append(remediation);
        }
        return sb.toString();
    }
}
