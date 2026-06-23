package main

import (
	"log/slog"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	sandboxv2 "mitos.run/mitos/api/v1alpha2"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/saas/console/baosecrets"
	"mitos.run/mitos/internal/saas/console/kubesecrets"
	"mitos.run/mitos/internal/tenant"
)

// kubeClient builds a controller-runtime client over the ambient kube config
// (in-cluster service account, or KUBECONFIG in dev) with the scheme the console
// needs: core types (for org Secrets) plus the mitos.run/v1alpha2 Sandbox types
// (for the org-scoped live-sandbox query). Shared by the kube secret store and
// the cluster sandbox control.
func kubeClient() (ctrlclient.Client, error) {
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, err
	}
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := sandboxv2.AddToScheme(s); err != nil {
		return nil, err
	}
	return ctrlclient.New(cfg, ctrlclient.Options{Scheme: s})
}

// primarySecretProvider returns the provider the binary should construct: the
// first recognized entry in the advertised list, defaulting to kube. Capability
// advertisement (what the SPA shows) and the active backend stay in sync because
// both derive from MITOS_CONSOLE_SECRET_PROVIDERS.
func primarySecretProvider(providers []string) string {
	for _, p := range providers {
		switch p {
		case "kube", "openbao", "aws_secrets_manager":
			return p
		}
	}
	return "kube"
}

// buildSecretStore constructs the active SecretStore from configuration. It
// falls back to the in-memory store (with a warning) when the configured
// provider cannot be built — e.g. running outside a cluster in dev — so the
// console still starts.
func buildSecretStore(logger *slog.Logger, caps console.Capabilities) console.SecretStore {
	switch primarySecretProvider(caps.Secrets.Providers) {
	case "openbao":
		addr := os.Getenv("MITOS_CONSOLE_OPENBAO_ADDR")
		token := openbaoToken()
		if addr != "" && token != "" {
			logger.Info("secret store: openbao", "addr", addr)
			return baosecrets.New(baosecrets.Config{Address: addr, Token: token, Mount: envOr("MITOS_CONSOLE_OPENBAO_MOUNT", "secret")})
		}
		logger.Warn("openbao is the primary secret provider but MITOS_CONSOLE_OPENBAO_ADDR/TOKEN are unset; using in-memory store")
	case "kube":
		if store, err := buildKubeSecretStore(); err == nil {
			logger.Info("secret store: kube (org-namespaced Secrets)")
			return store
		} else {
			logger.Warn("kube secret store unavailable (not in cluster?); using in-memory store", "err", err.Error())
		}
	}
	return console.NewMemSecretStore()
}

// buildKubeSecretStore builds the kube provider over the shared kube client,
// placing each org's Secrets in its hard-isolation namespace (tenant.NamespaceForOrg)
// — the same boundary the sandbox control queries.
func buildKubeSecretStore() (console.SecretStore, error) {
	c, err := kubeClient()
	if err != nil {
		return nil, err
	}
	return kubesecrets.New(c, tenant.NamespaceForOrg), nil
}

// openbaoToken reads the OpenBao token from the env or a token file.
func openbaoToken() string {
	if t := os.Getenv("MITOS_CONSOLE_OPENBAO_TOKEN"); t != "" {
		return t
	}
	if f := os.Getenv("MITOS_CONSOLE_OPENBAO_TOKEN_FILE"); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}
