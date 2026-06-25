// Minimal Kubernetes REST client for cluster mode. The Java SDK stays
// dependency-free: direct mode (SandboxServer) is untouched and pulls nothing,
// and cluster mode here is built on the JDK standard library alone
// (java.net.http.HttpClient, javax.net.ssl/SSLContext for the cluster CA and
// optional client-cert mTLS, java.util.Base64, and the hand-rolled Json
// helper). It does NOT pull fabric8, the official client, or any YAML library,
// which would drag a transitive dependency tree into every consumer.
//
// The client speaks the custom-resource REST paths directly:
//
//   /apis/mitos.run/v1/namespaces/{ns}/{plural}            (list, create)
//   /apis/mitos.run/v1/namespaces/{ns}/{plural}/{name}     (get, delete, patch)
//
// and reads core Secrets at /api/v1/namespaces/{ns}/secrets/{name}. Auth is a
// bearer token; TLS trusts the cluster CA. The token VALUE is never logged.
package run.mitos.sdk;

import java.io.ByteArrayInputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.KeyFactory;
import java.security.KeyStore;
import java.security.cert.Certificate;
import java.security.cert.CertificateFactory;
import java.security.spec.PKCS8EncodedKeySpec;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

import javax.net.ssl.KeyManagerFactory;
import javax.net.ssl.SSLContext;
import javax.net.ssl.TrustManagerFactory;

/**
 * A minimal Kubernetes API client used by cluster mode. It resolves a
 * connection (in-cluster or from a kubeconfig), trusts the cluster CA, attaches
 * the bearer token, and drives the namespaced custom-resource and Secret REST
 * endpoints. The token value is held in memory only and is never logged.
 */
final class K8s {

    static final String API_GROUP = "mitos.run";
    static final String API_VERSION = "v1";

    // The in-cluster service-account mount paths (the standard Kubernetes
    // locations). These are file paths, not secret values.
    private static final String IN_CLUSTER_TOKEN_PATH =
            "/var/run/secrets/kubernetes.io/serviceaccount/token";
    private static final String IN_CLUSTER_CA_PATH =
            "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt";

    private final String server;
    private final String token; // bearer token; in memory only, never logged
    private final HttpClient http;

    private K8s(String server, String token, HttpClient http) {
        this.server = server.replaceAll("/+$", "");
        this.token = (token == null || token.isEmpty()) ? null : token;
        this.http = http;
    }

    /** The resolved API server base URL. Package-private for tests/inspection. */
    String server() {
        return server;
    }

    /** The resolved bearer token, or null. Package-private for redaction reuse. */
    String token() {
        return token;
    }

    // ---- connection construction ----

    /**
     * Builds a client over an already-resolved server URL, bearer token, and
     * an HttpClient (whose SSLContext trusts the right CA). Used by the loaders
     * below and injected directly by tests pointing at a local HTTP stub.
     */
    static K8s of(String server, String token, HttpClient http) {
        return new K8s(server, token, http);
    }

    /**
     * Builds a client from the in-cluster service-account mount: the API server
     * from KUBERNETES_SERVICE_HOST/PORT, the bearer token from the projected
     * token file, and the CA from the mounted ca.crt. Raises an actionable
     * MitosException when run outside a cluster.
     */
    static K8s inCluster() {
        String host = System.getenv("KUBERNETES_SERVICE_HOST");
        String port = System.getenv("KUBERNETES_SERVICE_PORT");
        if (host == null || host.isEmpty() || port == null || port.isEmpty()) {
            throw new MitosException(
                    "in-cluster config requested but KUBERNETES_SERVICE_HOST/PORT are not set",
                    "not_in_cluster",
                    "the in-cluster service-account environment is not present",
                    "Run inside a Kubernetes pod, or pass a kubeconfig path to AgentRun.fromKubeconfig(path).",
                    0);
        }
        String tokenValue;
        byte[] caPem;
        try {
            tokenValue = Files.readString(Path.of(IN_CLUSTER_TOKEN_PATH)).strip();
        } catch (Exception e) {
            throw new MitosException(
                    "could not read the in-cluster service-account token",
                    "incluster_token_unreadable",
                    String.valueOf(e),
                    "Ensure the pod mounts a service-account token at " + IN_CLUSTER_TOKEN_PATH + ".",
                    0);
        }
        try {
            caPem = Files.readAllBytes(Path.of(IN_CLUSTER_CA_PATH));
        } catch (Exception e) {
            throw new MitosException(
                    "could not read the in-cluster CA certificate",
                    "incluster_ca_unreadable",
                    String.valueOf(e),
                    "Ensure the pod mounts the cluster CA at " + IN_CLUSTER_CA_PATH + ".",
                    0);
        }
        HttpClient client = httpClientFor(caPem, null, null);
        String serverUrl = "https://" + hostPort(host, port);
        return new K8s(serverUrl, tokenValue, client);
    }

    /**
     * Builds a client from a kubeconfig file (the current context). An empty
     * path falls back to $KUBECONFIG, then $HOME/.kube/config. The SDK parses a
     * common kubeconfig subset (server, CA, bearer token or inline client
     * cert/key); it does not support exec credential plugins or auth-provider
     * blocks.
     */
    static K8s fromKubeconfig(String path) {
        String resolved = path;
        if (resolved == null || resolved.isEmpty()) {
            resolved = System.getenv("KUBECONFIG");
        }
        if (resolved == null || resolved.isEmpty()) {
            String home = System.getProperty("user.home");
            if (home != null && !home.isEmpty()) {
                resolved = Path.of(home, ".kube", "config").toString();
            }
        }
        if (resolved == null || resolved.isEmpty()) {
            throw new MitosException(
                    "no kubeconfig path and no home directory to derive ~/.kube/config",
                    "kubeconfig_not_found",
                    "neither an explicit path nor $KUBECONFIG nor a home directory is set",
                    "Pass a path to AgentRun.fromKubeconfig(path), set $KUBECONFIG, or use AgentRun.inCluster() inside a pod.",
                    0);
        }
        String raw;
        Path file = Path.of(resolved);
        try {
            raw = Files.readString(file);
        } catch (Exception e) {
            throw new MitosException(
                    "could not read the kubeconfig file",
                    "kubeconfig_unreadable",
                    String.valueOf(e),
                    "Check the kubeconfig path is correct and readable: " + resolved,
                    0);
        }
        Map<String, Object> doc;
        try {
            doc = Yaml.parse(raw);
        } catch (RuntimeException e) {
            throw new MitosException(
                    "could not parse the kubeconfig file",
                    "kubeconfig_parse_failed",
                    String.valueOf(e),
                    "The SDK parses a common kubeconfig subset (no exec/auth-provider plugins); check the file or use AgentRun.inCluster().",
                    0);
        }
        Path baseDir = file.toAbsolutePath().getParent();
        return resolveKubeconfig(doc, baseDir);
    }

    @SuppressWarnings("unchecked")
    private static K8s resolveKubeconfig(Map<String, Object> doc, Path baseDir) {
        String currentContext = asStr(doc.get("current-context"));
        if (currentContext.isEmpty()) {
            throw new MitosException(
                    "the kubeconfig has no current-context",
                    "kubeconfig_no_context",
                    "current-context is empty",
                    "Set a current context with kubectl config use-context <name>.",
                    0);
        }
        // Resolve the current context to its cluster + user names.
        String clusterName = "";
        String userName = "";
        for (Object c : asList(doc.get("contexts"))) {
            Map<String, Object> ctx = asMap(c);
            if (currentContext.equals(asStr(ctx.get("name")))) {
                Map<String, Object> inner = asMap(ctx.get("context"));
                clusterName = asStr(inner.get("cluster"));
                userName = asStr(inner.get("user"));
                break;
            }
        }
        if (clusterName.isEmpty()) {
            throw new MitosException(
                    "the current-context is not defined in the kubeconfig",
                    "kubeconfig_context_missing",
                    "no contexts entry matches current-context " + currentContext,
                    "Check the kubeconfig contexts list includes " + currentContext + ".",
                    0);
        }

        String server = "";
        byte[] caPem = null;
        for (Object c : asList(doc.get("clusters"))) {
            Map<String, Object> cl = asMap(c);
            if (!clusterName.equals(asStr(cl.get("name")))) {
                continue;
            }
            Map<String, Object> inner = asMap(cl.get("cluster"));
            server = asStr(inner.get("server"));
            String caData = asStr(inner.get("certificate-authority-data"));
            String caFile = asStr(inner.get("certificate-authority"));
            if (!caData.isEmpty()) {
                try {
                    caPem = Base64.getDecoder().decode(caData);
                } catch (IllegalArgumentException e) {
                    throw new MitosException(
                            "certificate-authority-data is not valid base64",
                            "kubeconfig_ca_invalid",
                            String.valueOf(e),
                            "Regenerate the kubeconfig or check the certificate-authority-data field.",
                            0);
                }
            } else if (!caFile.isEmpty()) {
                Path p = Path.of(caFile);
                if (!p.isAbsolute() && baseDir != null) {
                    p = baseDir.resolve(caFile);
                }
                try {
                    caPem = Files.readAllBytes(p);
                } catch (Exception e) {
                    throw new MitosException(
                            "could not read the certificate-authority file",
                            "kubeconfig_ca_unreadable",
                            String.valueOf(e),
                            "Check the certificate-authority path in the kubeconfig: " + p,
                            0);
                }
            }
            break;
        }
        if (server.isEmpty()) {
            throw new MitosException(
                    "the current cluster has no server URL",
                    "kubeconfig_no_server",
                    "cluster " + clusterName + " has an empty server field",
                    "Check the kubeconfig clusters entry for " + clusterName + ".",
                    0);
        }

        String token = "";
        byte[] clientCertPem = null;
        byte[] clientKeyPem = null;
        for (Object u : asList(doc.get("users"))) {
            Map<String, Object> user = asMap(u);
            if (!userName.equals(asStr(user.get("name")))) {
                continue;
            }
            Map<String, Object> inner = asMap(user.get("user"));
            token = asStr(inner.get("token"));
            String certData = asStr(inner.get("client-certificate-data"));
            String keyData = asStr(inner.get("client-key-data"));
            if (!certData.isEmpty() && !keyData.isEmpty()) {
                try {
                    clientCertPem = Base64.getDecoder().decode(certData);
                    clientKeyPem = Base64.getDecoder().decode(keyData);
                } catch (IllegalArgumentException e) {
                    throw new MitosException(
                            "the kubeconfig client certificate or key is not valid base64",
                            "kubeconfig_client_cert_invalid",
                            String.valueOf(e),
                            "Check client-certificate-data and client-key-data in the kubeconfig.",
                            0);
                }
            }
            break;
        }

        HttpClient client = httpClientFor(caPem, clientCertPem, clientKeyPem);
        return new K8s(server, token, client);
    }

    /**
     * Builds an HttpClient whose SSLContext trusts caPem (the system default
     * trust store when caPem is null/empty) and, when clientCertPem/clientKeyPem
     * are present, presents a client certificate for mutual-TLS clusters (kind,
     * minikube). A CA or client cert that fails to load is a typed error so the
     * misconfiguration is legible.
     */
    private static HttpClient httpClientFor(byte[] caPem, byte[] clientCertPem, byte[] clientKeyPem) {
        HttpClient.Builder builder = HttpClient.newBuilder()
                .connectTimeout(Duration.ofSeconds(30));
        boolean wantTls = (caPem != null && caPem.length > 0)
                || (clientCertPem != null && clientCertPem.length > 0);
        if (wantTls) {
            builder.sslContext(buildSslContext(caPem, clientCertPem, clientKeyPem));
        }
        return builder.build();
    }

    private static SSLContext buildSslContext(byte[] caPem, byte[] clientCertPem, byte[] clientKeyPem) {
        try {
            TrustManagerFactory tmf = null;
            if (caPem != null && caPem.length > 0) {
                KeyStore trust = KeyStore.getInstance(KeyStore.getDefaultType());
                trust.load(null, null);
                CertificateFactory cf = CertificateFactory.getInstance("X.509");
                int idx = 0;
                for (Certificate cert : cf.generateCertificates(new ByteArrayInputStream(caPem))) {
                    trust.setCertificateEntry("ca-" + idx, cert);
                    idx++;
                }
                if (idx == 0) {
                    throw new MitosException(
                            "the cluster CA certificate could not be parsed",
                            "ca_parse_failed",
                            "no X.509 certificate was found in the CA PEM",
                            "Check the CA certificate is valid PEM (certificate-authority-data or the mounted ca.crt).",
                            0);
                }
                tmf = TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm());
                tmf.init(trust);
            }

            KeyManagerFactory kmf = null;
            if (clientCertPem != null && clientCertPem.length > 0
                    && clientKeyPem != null && clientKeyPem.length > 0) {
                kmf = clientKeyManager(clientCertPem, clientKeyPem);
            }

            SSLContext ctx = SSLContext.getInstance("TLS");
            ctx.init(
                    kmf == null ? null : kmf.getKeyManagers(),
                    tmf == null ? null : tmf.getTrustManagers(),
                    null);
            return ctx;
        } catch (MitosException e) {
            throw e;
        } catch (Exception e) {
            throw new MitosException(
                    "could not build the cluster TLS context",
                    "tls_context_failed",
                    String.valueOf(e),
                    "Check the cluster CA and any client certificate in the kubeconfig are valid PEM.",
                    0);
        }
    }

    // clientKeyManager assembles a KeyManagerFactory from a PEM client
    // certificate and an unencrypted PKCS#8 private key (the form
    // client-key-data carries in a kubeconfig).
    private static KeyManagerFactory clientKeyManager(byte[] certPem, byte[] keyPem) throws Exception {
        CertificateFactory cf = CertificateFactory.getInstance("X.509");
        List<Certificate> chain = new ArrayList<>(
                cf.generateCertificates(new ByteArrayInputStream(certPem)));
        if (chain.isEmpty()) {
            throw new MitosException(
                    "the kubeconfig client certificate could not be parsed",
                    "kubeconfig_client_cert_invalid",
                    "no X.509 certificate was found in client-certificate-data",
                    "Check client-certificate-data in the kubeconfig is valid PEM.",
                    0);
        }
        PKCS8EncodedKeySpec keySpec = new PKCS8EncodedKeySpec(pemKeyToDer(keyPem));
        java.security.PrivateKey key = loadPrivateKey(keySpec);

        KeyStore ks = KeyStore.getInstance(KeyStore.getDefaultType());
        ks.load(null, null);
        ks.setKeyEntry("client", key, new char[0],
                chain.toArray(new Certificate[0]));
        KeyManagerFactory kmf = KeyManagerFactory.getInstance(
                KeyManagerFactory.getDefaultAlgorithm());
        kmf.init(ks, new char[0]);
        return kmf;
    }

    // pemKeyToDer strips the PEM armor and base64-decodes the DER body of a
    // PKCS#8 private key. The header may be "PRIVATE KEY" (PKCS#8) which is what
    // kubeconfig client-key-data carries; encrypted or PKCS#1 keys are not
    // supported (a clear error surfaces from the KeyFactory).
    private static byte[] pemKeyToDer(byte[] keyPem) {
        String text = new String(keyPem, StandardCharsets.UTF_8);
        StringBuilder body = new StringBuilder();
        boolean inBody = false;
        for (String line : text.split("\\R")) {
            String t = line.trim();
            if (t.startsWith("-----BEGIN")) {
                inBody = true;
                continue;
            }
            if (t.startsWith("-----END")) {
                break;
            }
            if (inBody) {
                body.append(t);
            }
        }
        return Base64.getMimeDecoder().decode(body.toString());
    }

    private static java.security.PrivateKey loadPrivateKey(PKCS8EncodedKeySpec spec) {
        // Try the common algorithms a kubeconfig client key uses. The first that
        // accepts the PKCS#8 encoding wins.
        for (String algo : new String[] {"RSA", "EC"}) {
            try {
                return KeyFactory.getInstance(algo).generatePrivate(spec);
            } catch (Exception ignored) {
                // Try the next algorithm.
            }
        }
        throw new MitosException(
                "the kubeconfig client key could not be loaded",
                "kubeconfig_client_cert_invalid",
                "client-key-data is not an unencrypted PKCS#8 RSA or EC key",
                "Provide an unencrypted PKCS#8 client key, or use a bearer-token kubeconfig.",
                0);
    }

    // ---- REST operations ----

    /** Builds the namespaced custom-resource path; name is omitted for the
     * collection (list, create). */
    static String crdPath(String namespace, String plural, String name) {
        String base = "/apis/" + API_GROUP + "/" + API_VERSION
                + "/namespaces/" + namespace + "/" + plural;
        return (name == null || name.isEmpty()) ? base : base + "/" + name;
    }

    /** Builds the core Secret item path. */
    static String secretPath(String namespace, String name) {
        return "/api/v1/namespaces/" + namespace + "/secrets/" + name;
    }

    /** GETs a single custom object. A 404 surfaces as a MitosException with
     * status 404 so callers can tell absent from a real failure. */
    Map<String, Object> getObject(String namespace, String plural, String name) {
        Object out = send("GET", crdPath(namespace, plural, name), null, null);
        return asMap(out);
    }

    /** GETs the collection of custom objects, returning the items list. */
    List<Object> listObjects(String namespace, String plural) {
        Object out = send("GET", crdPath(namespace, plural, ""), null, null);
        Object items = asMap(out).get("items");
        return items instanceof List<?> ? asList(items) : new ArrayList<>();
    }

    /** POSTs a custom object to the collection. */
    Map<String, Object> createObject(String namespace, String plural, Map<String, Object> body) {
        Object out = send("POST", crdPath(namespace, plural, ""), body, "application/json");
        return asMap(out);
    }

    /** PATCHes a custom object with a strategic-merge patch. */
    Map<String, Object> patchObject(String namespace, String plural, String name,
                                    Map<String, Object> patch) {
        Object out = send("PATCH", crdPath(namespace, plural, name), patch,
                "application/merge-patch+json");
        return asMap(out);
    }

    /** DELETEs a single custom object. */
    void deleteObject(String namespace, String plural, String name) {
        send("DELETE", crdPath(namespace, plural, name), null, null);
    }

    /**
     * Reads a core Secret and returns its data decoded to UTF-8 strings keyed by
     * the Secret key. A 404 surfaces as a MitosException with status 404 so a
     * caller can tolerate a missing token Secret. Secret VALUES are held in
     * memory only and are NEVER logged.
     */
    Map<String, String> readSecret(String namespace, String name) {
        Object out = send("GET", secretPath(namespace, name), null, null);
        Map<String, Object> data = asMap(asMap(out).get("data"));
        Map<String, String> result = new LinkedHashMap<>();
        for (Map.Entry<String, Object> e : data.entrySet()) {
            try {
                byte[] decoded = Base64.getDecoder().decode(asStr(e.getValue()));
                result.put(e.getKey(), new String(decoded, StandardCharsets.UTF_8));
            } catch (IllegalArgumentException ignored) {
                // A non-base64 Secret value is unexpected; skip it rather than
                // surfacing the raw value (which could be a secret).
            }
        }
        return result;
    }

    // send issues an API request with the bearer token and parses a 2xx JSON
    // body. A non-2xx response becomes a typed MitosException carrying the
    // Kubernetes Status reason/code and the HTTP status. The bearer token never
    // appears in an error.
    private Object send(String method, String path, Object body, String contentType) {
        HttpRequest.Builder b = HttpRequest.newBuilder()
                .uri(URI.create(server + path))
                .header("Accept", "application/json");
        if (token != null) {
            b.header("Authorization", "Bearer " + token);
        }
        if (body != null) {
            String json = Json.encode(body);
            if (contentType != null) {
                b.header("Content-Type", contentType);
            }
            b.method(method, HttpRequest.BodyPublishers.ofString(json));
        } else {
            b.method(method, HttpRequest.BodyPublishers.noBody());
        }

        HttpResponse<String> resp;
        try {
            resp = http.send(b.build(), HttpResponse.BodyHandlers.ofString());
        } catch (Exception e) {
            throw new MitosException(
                    "kubernetes API request failed: " + redact(e.getMessage()),
                    "k8s_transport_error",
                    redact(String.valueOf(e)),
                    "Check the API server URL is reachable and the kubeconfig/in-cluster auth is valid.",
                    0);
        }
        int status = resp.statusCode();
        if (status < 200 || status >= 300) {
            throw errorFromStatus(status, resp.body());
        }
        String text = resp.body();
        if (text == null || text.isBlank()) {
            return new LinkedHashMap<String, Object>();
        }
        return Json.parse(text);
    }

    // errorFromStatus turns a non-2xx Kubernetes response into a typed
    // MitosException. The HTTP status is preserved so callers can branch on 404
    // (absent) versus 409 (already exists). A Status body carries no token, so
    // only defensive redaction is applied.
    private MitosException errorFromStatus(int status, String rawBody) {
        String body = redact(rawBody == null ? "" : rawBody);
        String reason = "";
        String message = "kubernetes API returned HTTP " + status;
        try {
            Object parsed = Json.parse(body);
            if (parsed instanceof Map<?, ?> m) {
                Map<String, Object> obj = asMap(parsed);
                reason = asStr(obj.get("reason"));
                String msg = asStr(obj.get("message"));
                if (!msg.isEmpty()) {
                    message = msg;
                } else if (!reason.isEmpty()) {
                    message = reason;
                }
            }
        } catch (RuntimeException ignored) {
            // Not JSON: keep the status-derived message with the body as cause.
        }
        String code = "k8s_" + statusCodeSuffix(status, reason);
        String cause = "kubernetes API returned " + status
                + (reason.isEmpty() ? "" : " " + reason);
        return new MitosException(message, code, cause, statusRemediation(status), status);
    }

    private static String statusCodeSuffix(int status, String reason) {
        switch (status) {
            case 404:
                return "not_found";
            case 409:
                return "conflict";
            case 403:
                return "forbidden";
            case 401:
                return "unauthorized";
            default:
                return reason.isEmpty() ? "error" : reason.toLowerCase();
        }
    }

    private static String statusRemediation(int status) {
        switch (status) {
            case 404:
                return "Check the object name and namespace; create it first if it does not exist.";
            case 401:
            case 403:
                return "Check the service-account RBAC grants access to mitos.run resources in this namespace.";
            default:
                return "";
        }
    }

    /** Replaces every occurrence of the configured token with [REDACTED]. */
    String redact(String text) {
        if (text == null) {
            return "";
        }
        if (token == null || token.isEmpty()) {
            return text;
        }
        return text.replace(token, "[REDACTED]");
    }

    // ---- small coercion helpers, shared with the cluster classes ----

    @SuppressWarnings("unchecked")
    static Map<String, Object> asMap(Object o) {
        if (o instanceof Map<?, ?> m) {
            return (Map<String, Object>) m;
        }
        return new LinkedHashMap<>();
    }

    @SuppressWarnings("unchecked")
    static List<Object> asList(Object o) {
        if (o instanceof List<?> l) {
            return (List<Object>) l;
        }
        return new ArrayList<>();
    }

    static String asStr(Object o) {
        return o == null ? "" : o.toString();
    }

    static int asInt(Object o) {
        if (o instanceof Number n) {
            return n.intValue();
        }
        return 0;
    }

    /** Walks the nested maps along keys and returns the value, or null when any
     * segment is absent or not a map. */
    static Object nested(Map<String, Object> obj, String... keys) {
        Object cur = obj;
        for (String k : keys) {
            if (!(cur instanceof Map<?, ?>)) {
                return null;
            }
            cur = asMap(cur).get(k);
            if (cur == null) {
                return null;
            }
        }
        return cur;
    }

    /** Reads a string at the nested path, or "" when absent or not a string. */
    static String nestedString(Map<String, Object> obj, String... keys) {
        Object v = nested(obj, keys);
        return v == null ? "" : v.toString();
    }

    private static String hostPort(String host, String port) {
        // Bracket a bare IPv6 literal so the URL host parses.
        if (host.indexOf(':') >= 0 && !host.startsWith("[")) {
            return "[" + host + "]:" + port;
        }
        return host + ":" + port;
    }
}
