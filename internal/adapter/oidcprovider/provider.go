// Package oidcprovider implements a public OIDC relying party. Login-attempt
// state and account binding belong to the calling application.
package oidcprovider

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

var (
	ErrConfiguration   = errors.New("OIDC provider configuration is invalid")
	ErrInput           = errors.New("OIDC login input is invalid")
	ErrUnavailable     = errors.New("OIDC provider is unavailable")
	ErrInvalidIdentity = errors.New("OIDC identity validation failed")
)

type Config struct {
	Issuer, ClientID, RedirectURL string
	HTTPClient                    *http.Client
}

type Identity struct {
	Issuer, Subject, Username, Name, Email, Picture string
}

type Provider struct {
	config Config
	client *http.Client
	mu     sync.RWMutex
	ready  *preparedProvider
}

type metadata struct {
	Issuer            string   `json:"issuer"`
	AuthorizationURL  string   `json:"authorization_endpoint"`
	TokenURL          string   `json:"token_endpoint"`
	UserInfoURL       string   `json:"userinfo_endpoint"`
	JWKSURL           string   `json:"jwks_uri"`
	SigningAlgorithms []string `json:"id_token_signing_alg_values_supported"`
	AuthMethods       []string `json:"token_endpoint_auth_methods_supported"`
	ChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

type preparedProvider struct {
	metadata metadata
	oauth    oauth2.Config
	algs     []string
}

const maxResponseBytes = 1 << 20

// New validates local configuration without contacting the provider. The client
// is copied, so its timeout and redirect policy do not mutate the caller's client.
func New(config Config) (*Provider, error) {
	if !httpsURL(config.Issuer, false) || !httpsURL(config.RedirectURL, true) || strings.TrimSpace(config.ClientID) == "" {
		return nil, ErrConfiguration
	}
	client := http.Client{Timeout: 10 * time.Second}
	if config.HTTPClient != nil {
		client = *config.HTTPClient
		if client.Timeout <= 0 || client.Timeout > 10*time.Second {
			client.Timeout = 10 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Provider{config: config, client: &client}, nil
}

func httpsURL(raw string, queryAllowed bool) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" &&
		(queryAllowed || (u.RawQuery == "" && !u.ForceQuery))
}

func (p *Provider) Prepare(ctx context.Context) error {
	_, err := p.prepare(ctx)
	return err
}

func (p *Provider) prepare(ctx context.Context) (*preparedProvider, error) {
	if ctx.Err() != nil {
		return nil, unavailable(ctx, ctx.Err())
	}
	p.mu.RLock()
	ready := p.ready
	p.mu.RUnlock()
	if ready != nil {
		return ready, nil
	}
	discovered, err := oidc.NewProvider(oidc.ClientContext(ctx, p.client), p.config.Issuer)
	if err != nil {
		return nil, unavailable(ctx, err)
	}
	var info metadata
	if err := discovered.Claims(&info); err != nil || info.Issuer != p.config.Issuer {
		return nil, ErrConfiguration
	}
	for _, endpoint := range []string{info.AuthorizationURL, info.TokenURL, info.UserInfoURL, info.JWKSURL} {
		if !httpsURL(endpoint, true) {
			return nil, ErrConfiguration
		}
	}
	if (len(info.AuthMethods) != 0 && !contains(info.AuthMethods, "none")) ||
		(len(info.ChallengeMethods) != 0 && !contains(info.ChallengeMethods, "S256")) {
		return nil, ErrConfiguration
	}
	var algorithms []string
	for _, alg := range info.SigningAlgorithms {
		switch alg {
		case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA":
			algorithms = append(algorithms, alg)
		}
	}
	if len(algorithms) == 0 {
		return nil, ErrConfiguration
	}
	ready = &preparedProvider{
		metadata: info, algs: algorithms,
		oauth: oauth2.Config{
			ClientID: p.config.ClientID, RedirectURL: p.config.RedirectURL,
			Scopes:   []string{oidc.ScopeOpenID, "profile"},
			Endpoint: oauth2.Endpoint{AuthURL: info.AuthorizationURL, TokenURL: info.TokenURL, AuthStyle: oauth2.AuthStyleInParams},
		},
	}
	// Concurrent initial calls may discover independently; no request waits on a
	// mutex held by another request's network operation.
	p.mu.Lock()
	if p.ready == nil {
		p.ready = ready
	}
	ready = p.ready
	p.mu.Unlock()
	return ready, nil
}

func (p *Provider) AuthorizationURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	if state == "" || nonce == "" || !validVerifier(verifier) {
		return "", ErrInput
	}
	ready, err := p.prepare(ctx)
	if err != nil {
		return "", err
	}
	return ready.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), nil
}

func (p *Provider) Exchange(ctx context.Context, code, nonce, verifier string) (Identity, error) {
	if code == "" || nonce == "" || !validVerifier(verifier) {
		return Identity{}, ErrInput
	}
	ready, err := p.prepare(ctx)
	if err != nil {
		return Identity{}, err
	}
	token, err := ready.oauth.Exchange(oidc.ClientContext(ctx, p.client), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, unavailable(ctx, err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" || token.AccessToken == "" {
		return Identity{}, ErrInvalidIdentity
	}
	keys := &requestKeySet{client: p.client, url: ready.metadata.JWKSURL, algorithms: ready.algs}
	verified, err := oidc.NewVerifier(p.config.Issuer, keys, &oidc.Config{
		ClientID: p.config.ClientID, SupportedSigningAlgs: ready.algs,
	}).Verify(ctx, rawIDToken)
	if err != nil {
		if keys.failure != nil {
			return Identity{}, keys.failure
		}
		if ctx.Err() != nil {
			return Identity{}, unavailable(ctx, ctx.Err())
		}
		return Identity{}, ErrInvalidIdentity
	}
	var claims struct {
		AuthorizedParty string  `json:"azp"`
		IssuedAt        float64 `json:"iat"`
		ExpiresAt       float64 `json:"exp"`
	}
	now := time.Now()
	if verified.Claims(&claims) != nil || claims.IssuedAt <= 0 || claims.ExpiresAt <= 0 ||
		verified.Issuer != p.config.Issuer || verified.Subject == "" ||
		verified.IssuedAt.IsZero() || verified.IssuedAt.Unix() <= 0 || verified.IssuedAt.After(now) ||
		!verified.Expiry.After(now) || verified.IssuedAt.After(verified.Expiry) ||
		subtle.ConstantTimeCompare([]byte(verified.Nonce), []byte(nonce)) != 1 ||
		(len(verified.Audience) > 1 && claims.AuthorizedParty != p.config.ClientID) ||
		(claims.AuthorizedParty != "" && claims.AuthorizedParty != p.config.ClientID) {
		return Identity{}, ErrInvalidIdentity
	}
	return p.userInfo(ctx, ready.metadata.UserInfoURL, token.AccessToken, verified.Subject)
}

func (p *Provider) userInfo(ctx context.Context, endpoint, accessToken, subject string) (Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Identity{}, ErrConfiguration
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return Identity{}, unavailable(ctx, err)
	}
	defer resp.Body.Close()
	var profile struct {
		Subject  string `json:"sub"`
		Username string `json:"preferred_username"`
		Name     string `json:"name"`
		Email    string `json:"email"`
		Picture  string `json:"picture"`
	}
	if resp.StatusCode != http.StatusOK {
		return Identity{}, unavailable(ctx, nil)
	}
	if err := decodeResponse(resp.Body, &profile); err != nil {
		return Identity{}, unavailable(ctx, err)
	}
	if profile.Subject == "" || profile.Subject != subject {
		return Identity{}, ErrInvalidIdentity
	}
	return Identity{Issuer: p.config.Issuer, Subject: subject, Username: profile.Username,
		Name: profile.Name, Email: profile.Email, Picture: profile.Picture}, nil
}

// go-oidc's RemoteKeySet deliberately detaches its HTTP fetch from request
// cancellation. This per-exchange KeySet keeps the caller's context and exposes
// dependency failure separately from a bad signature, without returning IdP data.
type requestKeySet struct {
	client     *http.Client
	url        string
	algorithms []string
	failure    error
}

func (k *requestKeySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		k.failure = ErrConfiguration
		return nil, k.failure
	}
	resp, err := k.client.Do(req)
	if err != nil {
		k.failure = unavailable(ctx, err)
		return nil, k.failure
	}
	defer resp.Body.Close()
	var keys jose.JSONWebKeySet
	if resp.StatusCode != http.StatusOK {
		k.failure = unavailable(ctx, nil)
		return nil, k.failure
	}
	if err := decodeResponse(resp.Body, &keys); err != nil {
		k.failure = unavailable(ctx, err)
		return nil, k.failure
	}
	algs := make([]jose.SignatureAlgorithm, len(k.algorithms))
	for i, alg := range k.algorithms {
		algs[i] = jose.SignatureAlgorithm(alg)
	}
	signed, err := jose.ParseSigned(raw, algs)
	if err != nil || len(signed.Signatures) != 1 {
		return nil, ErrInvalidIdentity
	}
	header := signed.Signatures[0].Header
	for _, key := range keys.Keys {
		if (header.KeyID != "" && key.KeyID != header.KeyID) || (key.Use != "" && key.Use != "sig") ||
			(key.Algorithm != "" && key.Algorithm != header.Algorithm) {
			continue
		}
		if payload, err := signed.Verify(key.Key); err == nil {
			return payload, nil
		}
	}
	return nil, ErrInvalidIdentity
}

func decodeResponse(body io.Reader, value interface{}) error {
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponseBytes {
		return ErrUnavailable
	}
	return json.Unmarshal(data, value)
}

func unavailable(ctx context.Context, cause error) error {
	if ctx.Err() != nil {
		return errors.Join(ErrUnavailable, ctx.Err())
	}
	var timeout net.Error
	if errors.As(cause, &timeout) && timeout.Timeout() {
		return errors.Join(ErrUnavailable, context.DeadlineExceeded)
	}
	return ErrUnavailable
}

func validVerifier(verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, ch := range verifier {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("-._~", ch)) {
			return false
		}
	}
	return true
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
