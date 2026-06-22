// AuthResolver applies the unified base-URL and bearer-token precedence shared
// with the Python, TypeScript, Ruby, and Rust direct-mode SDKs.
//
// Base URL precedence: the explicit argument, then MITOS_BASE_URL, then the
// hosted production endpoint https://mitos.run.
//
// Bearer precedence: the explicit argument, then MITOS_API_KEY, then the CLI
// login credential file written by `mitos auth login`
// (~/.config/mitos/credentials.json, honoring MITOS_CONFIG_DIR, the "token"
// field), then none (tokenless). A missing, unreadable, or non-JSON credential
// file is NOT an error: it just yields no token so the SDK stays usable
// tokenless. The token VALUE is never logged. The path rule is the single
// source of truth shared with the CLI's credentialsPath (internal/credfile).
package run.mitos.sdk;

import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.Map;

/** Resolves the base URL and bearer token from arguments, environment, and the
 * CLI credential file. */
final class AuthResolver {

    /** The hosted production control plane, used when no base URL is configured. */
    static final String DEFAULT_BASE_URL = "https://mitos.run";

    static final String ENV_API_KEY = "MITOS_API_KEY";
    static final String ENV_BASE_URL = "MITOS_BASE_URL";
    static final String ENV_CONFIG_DIR = "MITOS_CONFIG_DIR";

    private AuthResolver() {
    }

    /**
     * Resolves the base URL: explicit argument, then MITOS_BASE_URL, then the
     * hosted endpoint. Any trailing slashes are stripped.
     */
    static String resolveBaseUrl(String url) {
        String chosen = url;
        if (isBlank(chosen)) {
            chosen = System.getenv(ENV_BASE_URL);
        }
        if (isBlank(chosen)) {
            chosen = DEFAULT_BASE_URL;
        }
        return chosen.replaceAll("/+$", "");
    }

    /**
     * Resolves the bearer token: explicit argument, then MITOS_API_KEY, then the
     * CLI login credential file, then null (tokenless). The token VALUE is never
     * logged.
     */
    static String resolveToken(String apiKey) {
        if (!isBlank(apiKey)) {
            return apiKey;
        }
        String env = System.getenv(ENV_API_KEY);
        if (!isBlank(env)) {
            return env;
        }
        return tokenFromCredentialFile();
    }

    /**
     * Returns the location of the CLI login profile, honoring MITOS_CONFIG_DIR,
     * else $HOME/.config/mitos/credentials.json. Returns null when no home
     * directory can be resolved, in which case there is simply no credential-file
     * fallback.
     */
    static Path credentialsPath() {
        String dir = System.getenv(ENV_CONFIG_DIR);
        if (!isBlank(dir)) {
            return Paths.get(dir, "credentials.json");
        }
        String home = System.getProperty("user.home");
        if (isBlank(home)) {
            return null;
        }
        return Paths.get(home, ".config", "mitos", "credentials.json");
    }

    /**
     * Reads the "token" field from the CLI login profile, or null. A missing,
     * unreadable, or non-JSON file (or one without a non-empty "token") is NOT an
     * error: it yields no token so the SDK stays usable tokenless.
     */
    static String tokenFromCredentialFile() {
        try {
            Path path = credentialsPath();
            if (path == null || !Files.isRegularFile(path)) {
                return null;
            }
            String raw = Files.readString(path);
            Object parsed = Json.parse(raw);
            if (!(parsed instanceof Map<?, ?> map)) {
                return null;
            }
            Object token = map.get("token");
            if (token instanceof String s && !s.isEmpty()) {
                return s;
            }
            return null;
        } catch (Exception e) {
            // Missing / unreadable / non-JSON: no token, no error.
            return null;
        }
    }

    private static boolean isBlank(String s) {
        return s == null || s.isEmpty();
    }
}
