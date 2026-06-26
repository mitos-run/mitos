// ServeOptions configures ClusterWorkspace.serve. It mirrors the Go SDK
// WithServe* functional options (sdk/go/serve.go) as an idiomatic Java builder
// so the common path stays concise while the full surface is reachable.
package run.mitos.sdk;

/**
 * Options for {@link ClusterWorkspace#serve(ServeOptions)}.
 *
 * <pre>{@code
 * var opts = ServeOptions.builder()
 *         .pool("my-pool")
 *         .port(3000)
 *         .sharing("link")
 *         .exposeDomain("mitos.app")
 *         .build();
 * var served = workspace.serve(opts);
 * }</pre>
 */
public final class ServeOptions {

    private final String pool;
    private final int port;
    private final String sharing;
    private final String label;
    private final String exposeDomain;

    private ServeOptions(Builder b) {
        this.pool = b.pool;
        this.port = b.port;
        this.sharing = b.sharing;
        this.label = b.label;
        this.exposeDomain = b.exposeDomain;
    }

    /** Returns a new builder. */
    public static Builder builder() {
        return new Builder();
    }

    /** The SandboxPool to claim from (required). */
    public String pool() {
        return pool;
    }

    /** The guest TCP port to expose (default 8080). */
    public int port() {
        return port;
    }

    /** The access tier: "private", "link", "org", "authenticated", or "public".
     * Defaults to "private". */
    public String sharing() {
        return sharing;
    }

    /** An explicit subdomain label. When null the sandbox name is used. */
    public String label() {
        return label;
    }

    /** The base expose domain, for example "mitos.app". When null the
     * MITOS_EXPOSE_DOMAIN environment variable is consulted. */
    public String exposeDomain() {
        return exposeDomain;
    }

    /** Fluent builder for {@link ServeOptions}. */
    public static final class Builder {

        private String pool;
        private int port = 8080;
        private String sharing = "private";
        private String label;
        private String exposeDomain;

        private Builder() {
        }

        /** Sets the SandboxPool to claim from. Required. */
        public Builder pool(String pool) {
            this.pool = pool;
            return this;
        }

        /** Sets the guest TCP port to expose (default 8080). */
        public Builder port(int port) {
            this.port = port;
            return this;
        }

        /** Sets the access tier (default "private"). Valid values: "private",
         * "link", "org", "authenticated", "public". */
        public Builder sharing(String sharing) {
            this.sharing = sharing;
            return this;
        }

        /** Sets an explicit subdomain label. When omitted the sandbox name
         * is used as the label. */
        public Builder label(String label) {
            this.label = label;
            return this;
        }

        /** Sets the base expose domain, for example "mitos.app". When omitted
         * the MITOS_EXPOSE_DOMAIN environment variable is consulted. */
        public Builder exposeDomain(String exposeDomain) {
            this.exposeDomain = exposeDomain;
            return this;
        }

        /** Builds the immutable options. */
        public ServeOptions build() {
            return new ServeOptions(this);
        }
    }
}
