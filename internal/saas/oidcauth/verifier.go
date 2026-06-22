package oidcauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"mitos.run/mitos/internal/saas"
)

// ProviderConfig configures the real OIDC provider. Self-host points IssuerURL
// at the operator's IdP (Dex/Keycloak/Google); hosted points it at our IdP.
type ProviderConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string // openid is always added
}

// NewProvider discovers the OIDC issuer and returns the verifier (for the
// saas.LoginManager) and the matching Exchanger (for the auth Handlers). It dials
// the issuer's discovery endpoint, so it is constructed once at startup; the
// handler flow itself is tested against fakes.
func NewProvider(ctx context.Context, cfg ProviderConfig) (saas.IDTokenVerifier, Exchanger, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, nil, fmt.Errorf("oidc discovery: %w", err)
	}
	oauth := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       append([]string{oidc.ScopeOpenID}, cfg.Scopes...),
	}
	v := &verifier{idtv: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})}
	return v, &exchanger{oauth: oauth, idtv: v}, nil
}

// verifier adapts go-oidc to saas.IDTokenVerifier, mapping the standard claims.
type verifier struct{ idtv *oidc.IDTokenVerifier }

type stdClaims struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

func (v *verifier) Verify(ctx context.Context, rawIDToken string) (saas.OIDCClaims, error) {
	tok, err := v.idtv.Verify(ctx, rawIDToken)
	if err != nil {
		return saas.OIDCClaims{}, err
	}
	var c stdClaims
	if err := tok.Claims(&c); err != nil {
		return saas.OIDCClaims{}, err
	}
	return saas.OIDCClaims{Subject: c.Subject, Email: c.Email, EmailVerified: c.EmailVerified}, nil
}

// exchanger wraps the oauth2 config and pulls the id_token out of the token
// response (it is the LoginManager's verifier that then verifies it).
type exchanger struct {
	oauth *oauth2.Config
	idtv  *verifier
}

func (e *exchanger) AuthCodeURL(state string) string { return e.oauth.AuthCodeURL(state) }

func (e *exchanger) Exchange(ctx context.Context, code string) (string, error) {
	tok, err := e.oauth.Exchange(ctx, code)
	if err != nil {
		return "", err
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return "", errors.New("oidc: token response carried no id_token")
	}
	return raw, nil
}
