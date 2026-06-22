package main

import "testing"

// TestCapabilitiesFromEnvDefaultsToCommunity asserts that, with no env set, the
// binary advertises the self-hosted community edition with the #208 gate held
// (signup + billing off) and the kube provider.
func TestCapabilitiesFromEnvDefaultsToCommunity(t *testing.T) {
	for _, k := range []string{"MITOS_CONSOLE_EDITION", "MITOS_CONSOLE_BILLING", "MITOS_CONSOLE_SIGNUP", "MITOS_CONSOLE_SECRET_PROVIDERS"} {
		t.Setenv(k, "")
	}
	c := capabilitiesFromEnv()
	if c.Edition != "community" || c.Billing || c.Signup || c.Ownership != "self-hosted" {
		t.Fatalf("community defaults wrong: %+v", c)
	}
	if len(c.Secrets.Providers) != 1 || c.Secrets.Providers[0] != "kube" {
		t.Fatalf("providers = %v, want [kube]", c.Secrets.Providers)
	}
}

// TestCapabilitiesFromEnvHosted asserts the hosted env flips the
// server-controlled flags and parses a multi-provider list.
func TestCapabilitiesFromEnvHosted(t *testing.T) {
	t.Setenv("MITOS_CONSOLE_EDITION", "hosted")
	t.Setenv("MITOS_CONSOLE_BILLING", "true")
	t.Setenv("MITOS_CONSOLE_SIGNUP", "1")
	t.Setenv("MITOS_CONSOLE_SECRET_PROVIDERS", "kube, openbao")
	c := capabilitiesFromEnv()
	if c.Edition != "hosted" || !c.Billing || !c.Signup || !c.OrgSwitcher || c.Ownership != "hosted" {
		t.Fatalf("hosted flags wrong: %+v", c)
	}
	if len(c.Secrets.Providers) != 2 || c.Secrets.Providers[1] != "openbao" {
		t.Fatalf("providers = %v, want [kube openbao]", c.Secrets.Providers)
	}
}
