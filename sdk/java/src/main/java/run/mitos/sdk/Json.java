// A tiny, dependency-free JSON encoder and parser for the few flat wire shapes
// the mitos sandbox-server speaks (templates, fork, sandboxes, exec, and the
// error envelope). It is deliberately minimal: it covers objects, arrays,
// strings, numbers, booleans, and null, which is all the sandbox-server returns.
// The SDK ships with no runtime dependencies, so it does not pull in a JSON
// library; this helper stands in for one for the small surface we need.
package run.mitos.sdk;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Minimal JSON support: {@link #encode(Object)} serializes a Map / List / String
 * / Number / Boolean / null tree, and {@link #parse(String)} returns the same
 * shape. Object keys are preserved in insertion order via LinkedHashMap.
 */
final class Json {

    private Json() {
    }

    // ---- encoding ----

    /** Serializes a Map / List / String / Number / Boolean / null tree to JSON. */
    static String encode(Object value) {
        StringBuilder sb = new StringBuilder();
        encodeValue(value, sb);
        return sb.toString();
    }

    private static void encodeValue(Object value, StringBuilder sb) {
        if (value == null) {
            sb.append("null");
        } else if (value instanceof String s) {
            encodeString(s, sb);
        } else if (value instanceof Boolean || value instanceof Number) {
            sb.append(value.toString());
        } else if (value instanceof Map<?, ?> map) {
            encodeObject(map, sb);
        } else if (value instanceof List<?> list) {
            encodeArray(list, sb);
        } else {
            // Anything else is encoded by its string form, quoted, so an
            // unexpected type never produces invalid JSON.
            encodeString(value.toString(), sb);
        }
    }

    private static void encodeObject(Map<?, ?> map, StringBuilder sb) {
        sb.append('{');
        boolean first = true;
        for (Map.Entry<?, ?> e : map.entrySet()) {
            if (!first) {
                sb.append(',');
            }
            first = false;
            encodeString(String.valueOf(e.getKey()), sb);
            sb.append(':');
            encodeValue(e.getValue(), sb);
        }
        sb.append('}');
    }

    private static void encodeArray(List<?> list, StringBuilder sb) {
        sb.append('[');
        boolean first = true;
        for (Object item : list) {
            if (!first) {
                sb.append(',');
            }
            first = false;
            encodeValue(item, sb);
        }
        sb.append(']');
    }

    private static void encodeString(String s, StringBuilder sb) {
        sb.append('"');
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"' -> sb.append("\\\"");
                case '\\' -> sb.append("\\\\");
                case '\n' -> sb.append("\\n");
                case '\r' -> sb.append("\\r");
                case '\t' -> sb.append("\\t");
                case '\b' -> sb.append("\\b");
                case '\f' -> sb.append("\\f");
                default -> {
                    if (c < 0x20) {
                        sb.append(String.format("\\u%04x", (int) c));
                    } else {
                        sb.append(c);
                    }
                }
            }
        }
        sb.append('"');
    }

    // ---- parsing ----

    /**
     * Parses a JSON document into a Map / List / String / Double / Boolean / null
     * tree. Throws {@link IllegalArgumentException} on malformed input.
     */
    static Object parse(String text) {
        Parser p = new Parser(text);
        p.skipWhitespace();
        Object v = p.parseValue();
        p.skipWhitespace();
        if (!p.atEnd()) {
            throw new IllegalArgumentException("trailing content after JSON value");
        }
        return v;
    }

    /** Parses a document expected to be a JSON object, returning a string-keyed map. */
    @SuppressWarnings("unchecked")
    static Map<String, Object> parseObject(String text) {
        Object v = parse(text);
        if (v instanceof Map<?, ?> m) {
            return (Map<String, Object>) m;
        }
        throw new IllegalArgumentException("expected a JSON object");
    }

    /** Parses a document expected to be a JSON array. */
    @SuppressWarnings("unchecked")
    static List<Object> parseArray(String text) {
        Object v = parse(text);
        if (v instanceof List<?> l) {
            return (List<Object>) l;
        }
        throw new IllegalArgumentException("expected a JSON array");
    }

    private static final class Parser {
        private final String s;
        private int i;

        Parser(String s) {
            this.s = s;
        }

        boolean atEnd() {
            return i >= s.length();
        }

        void skipWhitespace() {
            while (i < s.length()) {
                char c = s.charAt(i);
                if (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
                    i++;
                } else {
                    break;
                }
            }
        }

        Object parseValue() {
            skipWhitespace();
            if (atEnd()) {
                throw new IllegalArgumentException("unexpected end of JSON");
            }
            char c = s.charAt(i);
            return switch (c) {
                case '{' -> parseObjectInternal();
                case '[' -> parseArrayInternal();
                case '"' -> parseString();
                case 't', 'f' -> parseBoolean();
                case 'n' -> parseNull();
                default -> parseNumber();
            };
        }

        private Map<String, Object> parseObjectInternal() {
            Map<String, Object> out = new LinkedHashMap<>();
            expect('{');
            skipWhitespace();
            if (peek() == '}') {
                i++;
                return out;
            }
            while (true) {
                skipWhitespace();
                String key = parseString();
                skipWhitespace();
                expect(':');
                Object value = parseValue();
                out.put(key, value);
                skipWhitespace();
                char c = next();
                if (c == '}') {
                    return out;
                }
                if (c != ',') {
                    throw new IllegalArgumentException("expected ',' or '}' in object");
                }
            }
        }

        private List<Object> parseArrayInternal() {
            List<Object> out = new ArrayList<>();
            expect('[');
            skipWhitespace();
            if (peek() == ']') {
                i++;
                return out;
            }
            while (true) {
                Object value = parseValue();
                out.add(value);
                skipWhitespace();
                char c = next();
                if (c == ']') {
                    return out;
                }
                if (c != ',') {
                    throw new IllegalArgumentException("expected ',' or ']' in array");
                }
            }
        }

        private String parseString() {
            expect('"');
            StringBuilder sb = new StringBuilder();
            while (true) {
                if (atEnd()) {
                    throw new IllegalArgumentException("unterminated string");
                }
                char c = next();
                if (c == '"') {
                    return sb.toString();
                }
                if (c == '\\') {
                    char esc = next();
                    switch (esc) {
                        case '"' -> sb.append('"');
                        case '\\' -> sb.append('\\');
                        case '/' -> sb.append('/');
                        case 'n' -> sb.append('\n');
                        case 'r' -> sb.append('\r');
                        case 't' -> sb.append('\t');
                        case 'b' -> sb.append('\b');
                        case 'f' -> sb.append('\f');
                        case 'u' -> {
                            String hex = s.substring(i, i + 4);
                            i += 4;
                            sb.append((char) Integer.parseInt(hex, 16));
                        }
                        default -> throw new IllegalArgumentException("invalid escape: \\" + esc);
                    }
                } else {
                    sb.append(c);
                }
            }
        }

        private Boolean parseBoolean() {
            if (s.startsWith("true", i)) {
                i += 4;
                return Boolean.TRUE;
            }
            if (s.startsWith("false", i)) {
                i += 5;
                return Boolean.FALSE;
            }
            throw new IllegalArgumentException("invalid literal at position " + i);
        }

        private Object parseNull() {
            if (s.startsWith("null", i)) {
                i += 4;
                return null;
            }
            throw new IllegalArgumentException("invalid literal at position " + i);
        }

        private Double parseNumber() {
            int start = i;
            while (i < s.length()) {
                char c = s.charAt(i);
                if ((c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.'
                        || c == 'e' || c == 'E') {
                    i++;
                } else {
                    break;
                }
            }
            if (i == start) {
                throw new IllegalArgumentException("invalid number at position " + start);
            }
            return Double.parseDouble(s.substring(start, i));
        }

        private char peek() {
            if (atEnd()) {
                throw new IllegalArgumentException("unexpected end of JSON");
            }
            return s.charAt(i);
        }

        private char next() {
            if (atEnd()) {
                throw new IllegalArgumentException("unexpected end of JSON");
            }
            return s.charAt(i++);
        }

        private void expect(char c) {
            char got = next();
            if (got != c) {
                throw new IllegalArgumentException("expected '" + c + "' but found '" + got + "'");
            }
        }
    }
}
