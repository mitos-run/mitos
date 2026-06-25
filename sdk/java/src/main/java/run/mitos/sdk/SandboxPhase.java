// The lifecycle phase of a cluster-mode Sandbox, as reported in status.phase.
// Mirrors the Python SandboxPhase enum and the controller's phase values.
package run.mitos.sdk;

/** A Sandbox lifecycle phase reported by the controller in status.phase. */
public enum SandboxPhase {

    /** The initial phase before the controller schedules and activates it. */
    PENDING("Pending"),
    /** The phase while the snapshot is being restored. */
    RESTORING("Restoring"),
    /** The phase once the sandbox is serving its API. */
    READY("Ready"),
    /** The phase after a terminate is requested. */
    TERMINATING("Terminating"),
    /** The terminal failure phase. */
    FAILED("Failed");

    private final String wire;

    SandboxPhase(String wire) {
        this.wire = wire;
    }

    /** The wire string the controller uses for this phase (for example "Ready"). */
    public String wire() {
        return wire;
    }

    /** Maps a status.phase wire string to a phase, defaulting to PENDING when
     * absent or unrecognized. */
    static SandboxPhase fromWire(String value) {
        if (value == null || value.isEmpty()) {
            return PENDING;
        }
        for (SandboxPhase p : values()) {
            if (p.wire.equals(value)) {
                return p;
            }
        }
        return PENDING;
    }
}
