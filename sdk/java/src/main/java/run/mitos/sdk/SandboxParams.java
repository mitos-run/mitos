// A small builder for the optional fields of AgentRun.sandbox and
// AgentRun.create: pool, name, env, secrets, ttl, workspace, and replicas. It
// mirrors the Python keyword arguments (pool=, name=, env=, secrets=, timeout=,
// workspace=) as an idiomatic Java builder so the common one-liner path stays
// argument-free while the full surface is still reachable.
package run.mitos.sdk;

import java.util.LinkedHashMap;
import java.util.Map;

/**
 * Optional parameters for creating a sandbox. Build it fluently and pass it to
 * {@link AgentRun#sandbox(String, SandboxParams)} or
 * {@link AgentRun#create(SandboxParams)}:
 *
 * <pre>{@code
 * var params = SandboxParams.builder()
 *         .name("worker-1")
 *         .env("MODE", "fast")
 *         .ttl("30m")
 *         .build();
 * var sb = agent.sandbox("python:3.12", params);
 * }</pre>
 */
public final class SandboxParams {

    private final String pool;
    private final String name;
    private final Map<String, String> env;
    private final Map<String, SecretRef> secrets;
    private final String ttl;
    private final String workspace;
    private final int replicas;

    private SandboxParams(Builder b) {
        this.pool = b.pool;
        this.name = b.name;
        this.env = b.env;
        this.secrets = b.secrets;
        this.ttl = b.ttl;
        this.workspace = b.workspace;
        this.replicas = b.replicas;
    }

    /** An empty parameter set (all defaults). */
    public static SandboxParams none() {
        return builder().build();
    }

    /** A new builder. */
    public static Builder builder() {
        return new Builder();
    }

    String pool() {
        return pool;
    }

    String name() {
        return name;
    }

    Map<String, String> env() {
        return env;
    }

    Map<String, SecretRef> secrets() {
        return secrets;
    }

    String ttl() {
        return ttl;
    }

    String workspace() {
        return workspace;
    }

    int replicas() {
        return replicas;
    }

    /** Fluent builder for {@link SandboxParams}. */
    public static final class Builder {
        private String pool;
        private String name;
        private Map<String, String> env;
        private Map<String, SecretRef> secrets;
        private String ttl;
        private String workspace;
        private int replicas;

        private Builder() {
        }

        /** Selects an existing SandboxPool to claim from. With a pool set, the
         * lazy default-pool path is never taken. */
        public Builder pool(String pool) {
            this.pool = pool;
            return this;
        }

        /** Sets an explicit sandbox name. When omitted a sandbox-&lt;hex&gt; name
         * is generated. */
        public Builder name(String name) {
            this.name = name;
            return this;
        }

        /** Replaces the environment map injected into the sandbox (spec.env). */
        public Builder env(Map<String, String> env) {
            this.env = env;
            return this;
        }

        /** Adds a single environment variable injected into the sandbox. */
        public Builder env(String key, String value) {
            if (this.env == null) {
                this.env = new LinkedHashMap<>();
            }
            this.env.put(key, value);
            return this;
        }

        /** Replaces the Secret-backed environment map (spec.secrets): env-var
         * name to the {@link SecretRef} it reads. */
        public Builder secrets(Map<String, SecretRef> secrets) {
            this.secrets = secrets;
            return this;
        }

        /** Adds a single Secret-backed environment variable. */
        public Builder secret(String envVar, String secretName, String key) {
            if (this.secrets == null) {
                this.secrets = new LinkedHashMap<>();
            }
            this.secrets.put(envVar, new SecretRef(secretName, key));
            return this;
        }

        /** Bounds the sandbox lifetime (spec.lifetime.ttl), for example "30m". */
        public Builder ttl(String ttl) {
            this.ttl = ttl;
            return this;
        }

        /** Binds the sandbox to a durable Workspace by name (spec.workspaceRef). */
        public Builder workspace(String workspace) {
            this.workspace = workspace;
            return this;
        }

        /** Sets spec.replicas on the created Sandbox. */
        public Builder replicas(int replicas) {
            this.replicas = replicas;
            return this;
        }

        /** Builds the immutable parameter set. */
        public SandboxParams build() {
            return new SandboxParams(this);
        }
    }
}
