package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/saas/placement"
)

// knownAuthConnectors is the closed set of social-login provider identifiers
// the console recognises. Values outside this set are silently dropped so a
// misconfigured env var does not expose unexpected connector URLs.
var knownAuthConnectors = map[string]bool{
	"github": true,
	"google": true,
}

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
		Edition:        edition,
		Billing:        envBool("MITOS_CONSOLE_BILLING"),
		Signup:         envBool("MITOS_CONSOLE_SIGNUP"),
		Teams:          true,
		IDP:            "oidc",
		OrgSwitcher:    orgSwitcher,
		Secrets:        console.SecretsCapability{Providers: providers},
		Proof:          true,
		Ownership:      ownership,
		AuthConnectors: parseAuthConnectors(os.Getenv("MITOS_CONSOLE_AUTH_CONNECTORS")),
		Feedback:       feedbackCapabilityFromEnv(edition),
		Version:        version,
		Placement:      placementFromEnv(edition),
	}
}

// placementFromEnv builds the deployment's placement registry (issue #712
// phase 0): the operator-defined key and the values it advertises. Hosted
// defaults to key "region" with the single Frankfurt value; self-hosted
// community defaults to key "cluster" with a single "default" value. Either
// can be overridden by MITOS_CONSOLE_PLACEMENT_KEY / MITOS_CONSOLE_PLACEMENT_VALUES
// regardless of edition, so a self-host operator can rename the key to
// whatever fits their fleet (cluster, zone, dc) without a code change, and a
// hosted deployment can add a second region ahead of phase 1 actually
// routing to it.
func placementFromEnv(edition string) placement.Registry {
	defaultKey := "cluster"
	defaultValues := "default"
	if edition == "hosted" {
		defaultKey = "region"
		defaultValues = "fra:Frankfurt (EU)"
	}
	key := envOr("MITOS_CONSOLE_PLACEMENT_KEY", defaultKey)
	values := envOr("MITOS_CONSOLE_PLACEMENT_VALUES", defaultValues)
	return placement.New(key, values)
}

// feedbackCapabilityFromEnv resolves the console's one-click feedback
// channel. Hosted deployments default to email support (target overridable
// via MITOS_CONSOLE_FEEDBACK_EMAIL); self-hosted community deployments
// default to a GitHub new-issue link against mitos-run/mitos (target
// overridable via MITOS_CONSOLE_FEEDBACK_GITHUB_REPO, e.g. for a fork). This
// is server-controlled like the rest of Capabilities: the SPA only renders
// what it is told, and there is no server write path for feedback in v1.
func feedbackCapabilityFromEnv(edition string) console.FeedbackCapability {
	if edition == "hosted" {
		return console.FeedbackCapability{
			Channel: "email",
			Target:  envOr("MITOS_CONSOLE_FEEDBACK_EMAIL", "feedback@mitos.run"),
		}
	}
	return console.FeedbackCapability{
		Channel: "github",
		Target:  envOr("MITOS_CONSOLE_FEEDBACK_GITHUB_REPO", "mitos-run/mitos"),
	}
}

// parseAuthConnectors splits a comma-separated connector list, filters to
// known providers, deduplicates, and sorts. Unknown values are silently
// dropped. The result is always non-nil (empty slice, not nil) so the JSON
// field serialises as [] rather than null when no connectors are configured.
func parseAuthConnectors(raw string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, v := range splitNonEmpty(raw) {
		if knownAuthConnectors[v] && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// newAuthConnectorsHandler returns a PUBLIC http.HandlerFunc for
// GET /auth/connectors. It responds with the connector list from caps so the
// Login/Signup SPA pages can render only the social-login buttons for
// providers that are actually wired up, and with the server-controlled signup
// flag so those pages can hide self-serve signup when the deployment disables
// it (in production /console/capabilities sits behind the session middleware,
// so this is the pre-auth pages' only capability source). No auth cookie is
// required: the response carries no org data, only server-controlled
// deployment configuration.
func newAuthConnectorsHandler(caps console.Capabilities) http.HandlerFunc {
	// Capture the values at startup; they are immutable for the lifetime of the
	// process (capabilities change only on redeploy).
	connectors := caps.AuthConnectors
	if connectors == nil {
		connectors = []string{}
	}
	type response struct {
		Connectors []string `json:"connectors"`
		Signup     bool     `json:"signup"`
	}
	body, err := json.Marshal(response{Connectors: connectors, Signup: caps.Signup})
	if err != nil {
		// json.Marshal on a []string never errors; this branch is unreachable
		// in practice, but keeps the compiler and errcheck satisfied.
		panic(fmt.Sprintf("newAuthConnectorsHandler: marshal: %v", err))
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// planSourceFromEnv builds the console's org -> plan lookup from
// MITOS_CONSOLE_TEAM_ORGS, a comma-separated list of org ids manually granted
// PlanTeam. This is an early manual-grant mechanism, standing in for a real
// subscription/payment integration; every org not listed resolves to
// PlanFree. Unset or empty grants no org Team.
func planSourceFromEnv() *billing.StaticPlanSource {
	return billing.NewStaticPlanSource(splitNonEmpty(os.Getenv("MITOS_CONSOLE_TEAM_ORGS")))
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

// instanceAdminEmailsFromEnv parses MITOS_CONSOLE_INSTANCE_ADMINS, a
// comma-separated list of account emails granted the instance-operator
// capability (GET/POST /console/admin/...). console.New normalizes case and
// whitespace, so this just splits. Unset or empty grants none via this path;
// the community-edition single-org-owner fallback (console.Console's
// isInstanceAdmin) still applies regardless.
func instanceAdminEmailsFromEnv() []string {
	return splitNonEmpty(os.Getenv("MITOS_CONSOLE_INSTANCE_ADMINS"))
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
