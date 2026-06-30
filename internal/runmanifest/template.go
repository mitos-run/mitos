package runmanifest

import "strings"

// PublicURLEnvVar is the environment variable carrying a sandbox's resolved public
// URL (its Mitos Expose address, for example "https://app.mitos.run"). It is
// injected into the golden template env at build time and into each fork's env at
// provision time so an exposed app can self-configure surfaces that cannot be
// hardcoded against the dynamically assigned subdomain: CORS and origin
// allowlists, OAuth redirect URIs, and base URLs (issue #476, part 1).
//
// The value is a non-secret URL, never a credential; it is safe to bake into the
// shareable golden snapshot.
const PublicURLEnvVar = "MITOS_PUBLIC_URL"

// publicURLRef is the single template token expandPublicURL substitutes.
const publicURLRef = "${" + PublicURLEnvVar + "}"

// expandPublicURL substitutes the ${MITOS_PUBLIC_URL} token in s with url. It is a
// single, well-defined substitution, NOT a shell or general template engine: only
// this one braced token is expanded, and every other ${...}, $name, command,
// arithmetic, or path expression is left exactly as written, so there is no
// injection surface. When url is empty the token is left literal, so a missing URL
// never silently blanks a configured value (the caller decides whether to inject).
func expandPublicURL(s, url string) string {
	if url == "" {
		return s
	}
	return strings.ReplaceAll(s, publicURLRef, url)
}

// referencesPublicURL reports whether s contains the ${MITOS_PUBLIC_URL} token.
func referencesPublicURL(s string) bool {
	return strings.Contains(s, publicURLRef)
}
