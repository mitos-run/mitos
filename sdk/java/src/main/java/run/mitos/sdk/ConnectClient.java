// ConnectClient speaks the Connect sandbox.v1.Sandbox runtime protocol (issue
// #358/#24) directly over the SDK's existing java.net.http.HttpClient, so the
// Java SDK gains streaming exec WITHOUT a gRPC runtime or a codegen step: it
// stays dependency-free (JDK stdlib only, the SDK's Json helper, java.util.Base64).
// It mirrors the Go sdk/go/connect.go and the Python sdk/python/mitos/_connect.py
// references; the proto-JSON message shapes come straight from
// proto/sandbox/v1/sandbox.proto (camelCase field names; bytes fields are base64
// strings).
//
// Only the server-streaming shape is implemented here (ExecStream): the SDK
// sends ONE opening enveloped frame (the request message) and then reads a
// stream of response frames. A frame is a 5-byte prefix (1 flag byte + 4-byte
// big-endian length) then the JSON payload. The terminal server frame sets the
// end-stream flag (0x02); its payload carries trailers and, on failure, an
// {"error":{code,message}} object.
//
// The bearer token rides on Authorization and is never logged; it is redacted
// from any error cause via the shared transport redactor.
package run.mitos.sdk;

import java.io.IOException;
import java.io.InputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

/** A dependency-free client for the Connect sandbox.v1.Sandbox runtime service. */
final class ConnectClient {

    static final String SERVICE_NAME = "sandbox.v1.Sandbox";
    static final String STREAM_CONTENT_TYPE = "application/connect+json";
    static final String SANDBOX_ID_HEADER = "X-Sandbox-Id";

    /** The end-stream flag (bit 1). The terminal server frame sets it; its
     * payload carries trailers and an optional error object. */
    static final int FLAG_END_STREAM = 0x02;
    /** The compressed flag (bit 0). The SDK negotiates identity encoding and
     * never sends or accepts a compressed frame, so this is only used to refuse
     * an unexpected one. */
    static final int FLAG_COMPRESSED = 0x01;

    /** Guards the frame-length prefix so a malformed or hostile length cannot
     * make the SDK allocate unbounded memory. */
    static final long MAX_FRAME_BYTES = 64L << 20; // 64 MiB

    // Maps the Connect textual error codes to the HTTP-ish status the SDK's
    // typed-error layer keys remediation on. An unmapped code falls back to 500.
    // Mirrors the Go and Python maps.
    private static final Map<String, Integer> CONNECT_CODE_STATUS = Map.ofEntries(
            Map.entry("canceled", 499),
            Map.entry("unknown", 500),
            Map.entry("invalid_argument", 400),
            Map.entry("deadline_exceeded", 504),
            Map.entry("not_found", 404),
            Map.entry("already_exists", 409),
            Map.entry("permission_denied", 403),
            Map.entry("resource_exhausted", 429),
            Map.entry("failed_precondition", 400),
            Map.entry("aborted", 409),
            Map.entry("out_of_range", 400),
            Map.entry("unimplemented", 501),
            Map.entry("internal", 500),
            Map.entry("unavailable", 503),
            Map.entry("data_loss", 500),
            Map.entry("unauthenticated", 401));

    private final HttpClient client;
    private final String baseUrl;
    private final String token;
    private final HttpTransport transport;

    ConnectClient(HttpTransport transport) {
        this.transport = transport;
        // Reuse the SDK's one HttpClient (and its connection pool); the request is
        // pinned to HTTP/1.1 below.
        this.client = transport.client();
        this.baseUrl = transport.baseUrl();
        this.token = transport.token();
    }

    /** The Connect RPC path for a Sandbox method name (e.g. "ExecStream"). */
    private static String path(String method) {
        return "/" + SERVICE_NAME + "/" + method;
    }

    /**
     * Opens a server-streaming Connect call: sends {@code message} as the single
     * opening enveloped frame and returns the list of response message payloads
     * (each a parsed JSON object), draining the stream. The terminal end-stream
     * frame is consumed here: a clean end ends the list, an error end throws a
     * typed {@link MitosException}.
     *
     * @param method    the Sandbox method name (e.g. "ExecStream")
     * @param sandboxId the sandbox id, sent as the X-Sandbox-Id header
     * @param message   the proto-JSON request message (Map tree)
     */
    List<Map<String, Object>> serverStream(String method, String sandboxId, Map<String, Object> message) {
        byte[] body = encodeFrame(Json.encode(message).getBytes(StandardCharsets.UTF_8), false);

        HttpRequest.Builder b = HttpRequest.newBuilder()
                .uri(URI.create(baseUrl + path(method)))
                // Pin HTTP/1.1: the Connect server-stream is delivered as a plain
                // chunked body that the SDK reads frame by frame, so we read it as
                // a 1.1 stream rather than attempt an HTTP/2 upgrade.
                .version(HttpClient.Version.HTTP_1_1)
                .header("Content-Type", STREAM_CONTENT_TYPE)
                .header("Connect-Protocol-Version", "1")
                .header(SANDBOX_ID_HEADER, sandboxId)
                .POST(HttpRequest.BodyPublishers.ofByteArray(body));
        if (token != null) {
            b.header("Authorization", "Bearer " + token);
        }

        HttpResponse<InputStream> resp;
        try {
            resp = client.send(b.build(), HttpResponse.BodyHandlers.ofInputStream());
        } catch (IOException | InterruptedException e) {
            if (e instanceof InterruptedException) {
                Thread.currentThread().interrupt();
            }
            throw new MitosException(
                    "sandbox RPC request failed: " + transport.redact(e.getMessage()),
                    "transport_error",
                    transport.redact(String.valueOf(e)),
                    "Check the base URL is reachable and the network allows the request.",
                    0);
        }

        int status = resp.statusCode();
        if (status < 200 || status >= 300) {
            // A streaming RPC that fails before the first frame returns a normal
            // HTTP error body (the Connect error envelope), not an end-stream frame.
            byte[] errBody = drain(resp.body());
            throw connectErrorFromBody(status, new String(errBody, StandardCharsets.UTF_8));
        }

        List<Map<String, Object>> messages = new ArrayList<>();
        try (InputStream in = resp.body()) {
            FrameReader reader = new FrameReader(in);
            Frame frame;
            while ((frame = reader.next()) != null) {
                if ((frame.flag & FLAG_COMPRESSED) != 0) {
                    throw new MitosException(
                            "sandbox RPC returned a compressed frame the SDK did not negotiate",
                            "internal_error",
                            "unexpected compressed Connect frame",
                            "Report this; the SDK negotiates identity encoding.",
                            500);
                }
                if ((frame.flag & FLAG_END_STREAM) != 0) {
                    handleEndStream(frame.payload);
                    return messages;
                }
                if (frame.payload.length == 0) {
                    continue;
                }
                messages.add(SandboxServer.asObject(
                        Json.parse(new String(frame.payload, StandardCharsets.UTF_8))));
            }
        } catch (IOException e) {
            throw new MitosException(
                    "sandbox RPC stream read failed: " + transport.redact(e.getMessage()),
                    "transport_error",
                    transport.redact(String.valueOf(e)),
                    "Retry the request; if it persists, inspect the sandbox-server logs.",
                    0);
        }
        // A clean transport EOF without an explicit end-stream frame ends the stream.
        return messages;
    }

    // ---- frame parsing ----

    /** Wraps one message payload in the Connect 5-byte envelope prefix: a flag
     * byte, a 4-byte big-endian length, then the payload. */
    static byte[] encodeFrame(byte[] payload, boolean endStream) {
        byte flag = (byte) (endStream ? FLAG_END_STREAM : 0);
        int n = payload.length;
        byte[] out = new byte[5 + n];
        out[0] = flag;
        out[1] = (byte) ((n >>> 24) & 0xff);
        out[2] = (byte) ((n >>> 16) & 0xff);
        out[3] = (byte) ((n >>> 8) & 0xff);
        out[4] = (byte) (n & 0xff);
        System.arraycopy(payload, 0, out, 5, n);
        return out;
    }

    /** One decoded Connect frame: the flag byte and the JSON payload bytes. */
    private static final class Frame {
        final int flag;
        final byte[] payload;

        Frame(int flag, byte[] payload) {
            this.flag = flag;
            this.payload = payload;
        }
    }

    /** Reads Connect enveloped frames off an InputStream, reassembling each frame
     * across read boundaries. {@link #next()} returns the next frame or null at a
     * clean end of stream. */
    private static final class FrameReader {
        private final InputStream in;

        FrameReader(InputStream in) {
            this.in = in;
        }

        Frame next() throws IOException {
            byte[] header = readN(5, true);
            if (header == null) {
                // A clean EOF at a frame boundary is the end of the stream.
                return null;
            }
            int flag = header[0] & 0xff;
            long len = ((long) (header[1] & 0xff) << 24)
                    | ((long) (header[2] & 0xff) << 16)
                    | ((long) (header[3] & 0xff) << 8)
                    | (header[4] & 0xff);
            if (len > MAX_FRAME_BYTES) {
                throw new IOException("connect: response frame too large (" + len + " bytes)");
            }
            byte[] payload = readN((int) len, false);
            if (payload == null) {
                // Header read but payload truncated: a short frame is a hard error.
                throw new IOException("connect: short frame payload");
            }
            return new Frame(flag, payload);
        }

        // readN reads exactly n bytes, looping until filled or EOF. When
        // eofAtStartOk is true a clean EOF before the first byte returns null
        // (the stream ended at a frame boundary); a truncated read still throws.
        private byte[] readN(int n, boolean eofAtStartOk) throws IOException {
            byte[] buf = new byte[n];
            int off = 0;
            while (off < n) {
                int r = in.read(buf, off, n - off);
                if (r < 0) {
                    if (off == 0 && eofAtStartOk) {
                        return null;
                    }
                    if (off == 0) {
                        return null;
                    }
                    throw new IOException("connect: unexpected EOF mid-frame");
                }
                off += r;
            }
            return buf;
        }
    }

    private static byte[] drain(InputStream in) {
        try (in) {
            return in.readAllBytes();
        } catch (IOException e) {
            return new byte[0];
        }
    }

    // ---- error handling ----

    /** Processes the terminal end-stream frame. A payload carrying an
     * {"error":{code,message}} object throws a typed MitosException; a clean end
     * (empty, trailers only, or non-JSON) returns normally. */
    private void handleEndStream(byte[] payload) {
        if (payload.length == 0) {
            return;
        }
        Object parsed;
        try {
            parsed = Json.parse(new String(payload, StandardCharsets.UTF_8));
        } catch (RuntimeException e) {
            // Malformed trailer: treat as a clean end.
            return;
        }
        if (!(parsed instanceof Map<?, ?> map)) {
            return;
        }
        Object err = map.get("error");
        if (err instanceof Map<?, ?> e) {
            String code = SandboxServer.asString(e.get("code"));
            String message = SandboxServer.asString(e.get("message"));
            throw connectError(code, message, statusForCode(code));
        }
    }

    /** Turns a non-2xx Connect response body into a typed MitosException. Prefers
     * the Connect error envelope {code,message}; falls back to the raw redacted
     * body and the HTTP status when the body is not the envelope. */
    private MitosException connectErrorFromBody(int status, String rawBody) {
        String body = rawBody == null ? "" : rawBody;
        try {
            Object parsed = Json.parse(body);
            if (parsed instanceof Map<?, ?> map) {
                String code = SandboxServer.asString(map.get("code"));
                String message = SandboxServer.asString(map.get("message"));
                if (code != null && !code.isEmpty()) {
                    return connectError(code, message, status);
                }
            }
        } catch (RuntimeException ignored) {
            // Not the envelope: fall through to the raw body.
        }
        return new MitosException(
                "sandbox RPC failed: HTTP " + status,
                "http_error",
                transport.redact(body.trim()),
                "Inspect the request against the sandbox.v1.Sandbox contract.",
                status);
    }

    /** Builds a typed MitosException from a Connect error code and message. The
     * Connect textual code is the stable code; the message is redacted of any
     * token before it becomes the cause. */
    private MitosException connectError(String code, String message, int status) {
        String stable = (code == null || code.isEmpty()) ? "internal" : code;
        String cause = transport.redact(message == null ? "" : message);
        if (cause.isEmpty()) {
            cause = "connect error " + stable;
        }
        return new MitosException(
                "sandbox RPC failed: " + stable,
                stable,
                cause,
                "Inspect the request against the sandbox.v1.Sandbox contract.",
                status);
    }

    private static int statusForCode(String code) {
        Integer s = CONNECT_CODE_STATUS.get(code);
        return s == null ? 500 : s;
    }
}
