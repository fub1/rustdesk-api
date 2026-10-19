package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/identitystore"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/oidcprovider"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/sessiontoken"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/loginattempt"
	"github.com/lejianwen/rustdesk-api/v2/internal/oidclogin"
	"gorm.io/gorm"
)

func OIDCHTTPClient(proxyEnabled bool, proxyAddress string) (*http.Client, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client.Transport = transport
	if proxyEnabled {
		proxy, err := url.Parse(proxyAddress)
		if err != nil || proxy.Hostname() == "" || proxy.Scheme != "http" && proxy.Scheme != "https" {
			return nil, errors.New("invalid OIDC proxy configuration")
		}
		transport.Proxy = http.ProxyURL(proxy)
		client.Transport = transport
	}
	return client, nil
}

type oidcProtocol struct{ provider *oidcprovider.Provider }

func (p oidcProtocol) AuthorizationURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	url, err := p.provider.AuthorizationURL(ctx, state, nonce, verifier)
	return url, protocolError(err)
}
func (p oidcProtocol) Exchange(ctx context.Context, code, nonce, verifier string) (auth.ExternalIdentity, error) {
	identity, err := p.provider.Exchange(ctx, code, nonce, verifier)
	return auth.ExternalIdentity{Issuer: identity.Issuer, Subject: identity.Subject, Username: identity.Username, Name: identity.Name, Email: identity.Email, Picture: identity.Picture}, protocolError(err)
}

func protocolError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, oidcprovider.ErrInvalidIdentity):
		return auth.ErrUnauthenticated
	case errors.Is(err, oidcprovider.ErrConfiguration), errors.Is(err, oidcprovider.ErrInput):
		return auth.ErrInvalidInput
	default:
		return auth.ErrUnavailable
	}
}

// NewOIDC opens no resources and performs no discovery during construction.
func NewOIDC(db *gorm.DB, ttl time.Duration, key []byte, sessions *auth.Service, apiBase string, webSSO bool, client *http.Client) (*oidclogin.Service, error) {
	store, err := identitystore.New(db, sessiontoken.NewIssuer(key), ttl, nil)
	if err != nil {
		return nil, err
	}
	attempts, err := loginattempt.New(loginattempt.Options{})
	if err != nil {
		return nil, err
	}
	issuer, err := NewIssuer(db, ttl, key)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(apiBase, "/")
	webURL := ""
	if webSSO {
		webURL = base + "/_admin/#/oauth/"
	}
	return oidclogin.New(store, attempts, sessions, issuer, func(config auth.ProviderConfig) (oidclogin.Protocol, error) {
		p, err := oidcprovider.New(oidcprovider.Config{Issuer: config.Issuer, ClientID: config.ClientID, RedirectURL: base + "/api/oidc/callback", HTTPClient: client})
		return oidcProtocol{provider: p}, protocolError(err)
	}, webURL)
}
