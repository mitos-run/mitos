package onboarding

import "strings"

// canonicalEmail folds an address to a stable identity used for dedup and the
// once-per-person signup credit. It lowercases (via normalizeEmail), drops a
// "+tag" suffix from the local part for ALL providers, and for gmail.com /
// googlemail.com also removes dots from the local part and folds the domain to
// gmail.com. The DELIVERY address is NOT this value; callers keep the original
// for sending mail. Returns ("", false) for a malformed address.
func canonicalEmail(addr string) (string, bool) {
	// Step 1: delegate parsing, validation, and lowercasing to normalizeEmail.
	norm, ok := normalizeEmail(addr)
	if !ok {
		return "", false
	}

	// Step 2: split at the LAST '@' (normalizeEmail guarantees at least one '@'
	// with non-empty local and domain, both already lowercased; a quoted local
	// part may contain more, so LastIndex is the correct split point).
	at := strings.LastIndex(norm, "@")
	local := norm[:at]
	domain := norm[at+1:]

	// Step 3: drop a plus-tag from the local part for ALL providers.
	if i := strings.IndexByte(local, '+'); i >= 0 {
		local = local[:i]
	}
	if local == "" {
		return "", false
	}

	// Step 4: Gmail-specific folding - remove dots and normalise domain.
	if domain == "gmail.com" || domain == "googlemail.com" {
		local = strings.ReplaceAll(local, ".", "")
		if local == "" {
			return "", false
		}
		domain = "gmail.com"
	}

	// Step 5: reassemble.
	return local + "@" + domain, true
}
