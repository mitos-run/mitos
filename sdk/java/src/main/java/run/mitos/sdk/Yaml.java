// A tiny, dependency-free YAML reader for the kubeconfig subset cluster mode
// needs. It is deliberately minimal: it covers the block-style maps, block-style
// lists, and scalar values a kubeconfig uses (clusters/contexts/users lists of
// nested maps, plus current-context and the server/CA/token/cert scalars). It is
// NOT a general YAML implementation: it does not handle flow style ({}/[]),
// anchors, multi-document streams, or block scalars, none of which appear in a
// kubeconfig. Keeping the SDK dependency-free means no SnakeYAML; this stands in
// for the small surface we read.
package run.mitos.sdk;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Minimal block-style YAML parsing: {@link #parse(String)} returns a Map / List
 * / String tree, the same shape {@link Json} produces, so the kubeconfig
 * resolver can read it with the same accessors. Scalars are returned as Strings;
 * the resolver coerces where needed.
 */
final class Yaml {

    private Yaml() {
    }

    /** Parses a block-style YAML document into a String-keyed map tree. */
    static Map<String, Object> parse(String text) {
        List<Line> lines = new ArrayList<>();
        for (String raw : text.split("\\R", -1)) {
            String noComment = stripComment(raw);
            if (noComment.strip().isEmpty()) {
                continue;
            }
            if (noComment.strip().equals("---") || noComment.strip().equals("...")) {
                // A document marker; the first document is all a kubeconfig has.
                continue;
            }
            int indent = indentOf(noComment);
            lines.add(new Line(indent, noComment.strip()));
        }
        int[] pos = {0};
        Object root = parseBlock(lines, pos, 0);
        if (root instanceof Map<?, ?>) {
            return K8s.asMap(root);
        }
        return new LinkedHashMap<>();
    }

    // parseBlock parses a run of sibling lines at minIndent (or deeper) into a
    // Map or a List, advancing pos past the consumed lines.
    private static Object parseBlock(List<Line> lines, int[] pos, int minIndent) {
        if (pos[0] >= lines.size()) {
            return new LinkedHashMap<String, Object>();
        }
        int indent = lines.get(pos[0]).indent;
        if (lines.get(pos[0]).text.startsWith("- ") || lines.get(pos[0]).text.equals("-")) {
            return parseList(lines, pos, indent);
        }
        return parseMap(lines, pos, indent);
    }

    private static Map<String, Object> parseMap(List<Line> lines, int[] pos, int indent) {
        Map<String, Object> map = new LinkedHashMap<>();
        while (pos[0] < lines.size()) {
            Line line = lines.get(pos[0]);
            if (line.indent < indent) {
                break;
            }
            if (line.indent > indent) {
                // Shouldn't happen at a well-formed map start; skip defensively.
                pos[0]++;
                continue;
            }
            String content = line.text;
            int colon = keyColon(content);
            if (colon < 0) {
                // Not a key: value line at this level; stop the map.
                break;
            }
            String key = unquote(content.substring(0, colon).strip());
            String rest = content.substring(colon + 1).strip();
            pos[0]++;
            if (!rest.isEmpty()) {
                map.put(key, scalar(rest));
            } else {
                // The value is a nested block on the following deeper lines.
                if (pos[0] < lines.size() && lines.get(pos[0]).indent > indent) {
                    map.put(key, parseBlock(lines, pos, lines.get(pos[0]).indent));
                } else if (pos[0] < lines.size()
                        && lines.get(pos[0]).indent == indent
                        && (lines.get(pos[0]).text.startsWith("- ")
                            || lines.get(pos[0]).text.equals("-"))) {
                    // A list whose items sit at the same indent as the key (the
                    // common kubeconfig style for clusters/contexts/users).
                    map.put(key, parseList(lines, pos, indent));
                } else {
                    map.put(key, "");
                }
            }
        }
        return map;
    }

    private static List<Object> parseList(List<Line> lines, int[] pos, int indent) {
        List<Object> list = new ArrayList<>();
        while (pos[0] < lines.size()) {
            Line line = lines.get(pos[0]);
            if (line.indent != indent
                    || !(line.text.startsWith("- ") || line.text.equals("-"))) {
                break;
            }
            String afterDash = line.text.equals("-") ? "" : line.text.substring(2).strip();
            if (afterDash.isEmpty()) {
                // The item is a nested block on the following deeper lines.
                pos[0]++;
                if (pos[0] < lines.size() && lines.get(pos[0]).indent > indent) {
                    list.add(parseBlock(lines, pos, lines.get(pos[0]).indent));
                } else {
                    list.add("");
                }
                continue;
            }
            int colon = keyColon(afterDash);
            if (colon >= 0) {
                // An inline "- key: value" begins a map item. Rewrite the line so
                // its first key sits at indent+2 and parse the item as a map.
                int itemIndent = indent + 2;
                List<Line> sub = new ArrayList<>();
                sub.add(new Line(itemIndent, afterDash));
                pos[0]++;
                // Pull in the following lines that belong to this item (deeper
                // than the dash indent).
                while (pos[0] < lines.size() && lines.get(pos[0]).indent > indent) {
                    sub.add(lines.get(pos[0]));
                    pos[0]++;
                }
                int[] subPos = {0};
                list.add(parseMap(sub, subPos, itemIndent));
            } else {
                // A scalar list item.
                list.add(scalar(afterDash));
                pos[0]++;
            }
        }
        return list;
    }

    // keyColon returns the index of the colon that separates a map key from its
    // value, or -1 when the line is not a "key:" line. The colon must be followed
    // by a space or end-of-line (so a URL "https://x" is not mistaken for a key).
    private static int keyColon(String s) {
        boolean inSingle = false;
        boolean inDouble = false;
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            if (c == '\'' && !inDouble) {
                inSingle = !inSingle;
            } else if (c == '"' && !inSingle) {
                inDouble = !inDouble;
            } else if (c == ':' && !inSingle && !inDouble) {
                if (i + 1 >= s.length() || s.charAt(i + 1) == ' ') {
                    return i;
                }
            }
        }
        return -1;
    }

    private static Object scalar(String s) {
        return unquote(s);
    }

    private static String unquote(String s) {
        if (s.length() >= 2) {
            char f = s.charAt(0);
            char l = s.charAt(s.length() - 1);
            if ((f == '"' && l == '"') || (f == '\'' && l == '\'')) {
                return s.substring(1, s.length() - 1);
            }
        }
        return s;
    }

    private static String stripComment(String line) {
        boolean inSingle = false;
        boolean inDouble = false;
        for (int i = 0; i < line.length(); i++) {
            char c = line.charAt(i);
            if (c == '\'' && !inDouble) {
                inSingle = !inSingle;
            } else if (c == '"' && !inSingle) {
                inDouble = !inDouble;
            } else if (c == '#' && !inSingle && !inDouble) {
                // A comment starts at # only when preceded by whitespace or at the
                // line start, matching YAML.
                if (i == 0 || line.charAt(i - 1) == ' ' || line.charAt(i - 1) == '\t') {
                    return line.substring(0, i);
                }
            }
        }
        return line;
    }

    private static int indentOf(String line) {
        int n = 0;
        while (n < line.length() && line.charAt(n) == ' ') {
            n++;
        }
        return n;
    }

    private record Line(int indent, String text) {
    }
}
