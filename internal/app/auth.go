package app

import (
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/authstore"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/sessiontoken"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"gorm.io/gorm"
)

// NewSessions composes one local-session installation. It opens no resources,
// changes no legacy globals and performs no OIDC network calls.
func NewSessions(db *gorm.DB, ttl time.Duration, jwtKey []byte) (*auth.Service, error) {
	store, err := authstore.New(db)
	if err != nil {
		return nil, err
	}
	var verifier auth.TokenVerifier
	if len(jwtKey) > 0 {
		verifier, err = sessiontoken.NewHS256(jwtKey)
		if err != nil {
			return nil, err
		}
	}
	return auth.New(auth.Dependencies{Store: store, Verifier: verifier, TTL: ttl})
}

func NewIssuer(db *gorm.DB, ttl time.Duration, jwtKey []byte) (*auth.Issuer, error) {
	store, err := authstore.New(db)
	if err != nil {
		return nil, err
	}
	return auth.NewIssuer(store, sessiontoken.NewIssuer(jwtKey), ttl, nil)
}
