package main

import (
	"os"
	"strings"

	"mitos.run/mitos/internal/saas/console"
)

// capabilitiesFromEnv builds the console capabilities document from the
// server-controlled environment the Helm chart sets. This is the single source
// of edition behavior: the SAME binary and SPA bundle serve both editions, and
// nothing here can be set by a browser. Unset values default to the self-hosted
// community edition, with signup and billing OFF (the #208 gate).
func capabilitiesFromEnv() console.Capabilities {
	edition := envOr("MITOS_CONSOLE_EDITION", "community")
	providers := splitNonEmpty(envOr("MITOS_CONSOLE_SECRET_PROVIDERS", "kube"))
	if len(providers) == 0 {
		providers = []string{"kube"}
	}
	ownership := "self-hosted"
	orgSwitcher := false
	if edition == "hosted" {
		ownership = "hosted"
		orgSwitcher = true
	}
	return console.Capabilities{
		Edition:     edition,
		Billing:     envBool("MITOS_CONSOLE_BILLING"),
		Signup:      envBool("MITOS_CONSOLE_SIGNUP"),
		Teams:       true,
		IDP:         "oidc",
		OrgSwitcher: orgSwitcher,
		Secrets:     console.SecretsCapability{Providers: providers},
		Proof:       true,
		Ownership:   ownership,
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "true" || v == "1" || v == "yes"
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
