// ServedWorkspace is the handle returned by ClusterWorkspace.serve. It carries
// the public HTTPS URL and the identity of the backing sandbox. Mirrors the Go
// SDK ServedWorkspace (sdk/go/serve.go).
package run.mitos.sdk;

/**
 * The result of {@link ClusterWorkspace#serve(ServeOptions)}: a handle carrying
 * the public HTTPS URL and the identity of the backing Sandbox.
 */
public final class ServedWorkspace {

    private final String url;
    private final String sandboxName;
    private final String label;
    private final String sharing;

    ServedWorkspace(String url, String sandboxName, String label, String sharing) {
        this.url = url;
        this.sandboxName = sandboxName;
        this.label = label;
        this.sharing = sharing;
    }

    /** The public HTTPS URL: {@code https://<label>.<exposeDomain>/}. */
    public String url() {
        return url;
    }

    /** The name of the Sandbox CRD that backs this serve session. */
    public String sandboxName() {
        return sandboxName;
    }

    /** The single DNS label used as the URL subdomain. */
    public String label() {
        return label;
    }

    /** The effective access tier ("private", "link", etc.). */
    public String sharing() {
        return sharing;
    }

    @Override
    public String toString() {
        return "ServedWorkspace{url=" + url + ", sandboxName=" + sandboxName
                + ", label=" + label + ", sharing=" + sharing + "}";
    }
}
