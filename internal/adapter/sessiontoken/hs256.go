// Package sessiontoken verifies signed legacy session tokens. It never makes a
// signed token authoritative without a matching database session.
package sessiontoken

import (
	"context"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
)

type Verifier struct{ key []byte }

var _ auth.TokenVerifier = (*Verifier)(nil)

func NewHS256(key []byte) (*Verifier, error) {
	if len(key) == 0 {
		return nil, errors.New("HS256 session key is required")
	}
	return &Verifier{key: append([]byte(nil), key...)}, nil
}

type claims struct {
	UserID uint `json:"user_id"`
	jwt.RegisteredClaims
}

func (v *Verifier) Verify(ctx context.Context, raw string, now time.Time) (auth.TokenClaims, error) {
	if err := ctx.Err(); err != nil {
		return auth.TokenClaims{}, err
	}
	c := &claims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	token, err := parser.ParseWithClaims(raw, c, func(*jwt.Token) (interface{}, error) { return v.key, nil })
	if err != nil || !token.Valid || c.UserID == 0 || c.ExpiresAt == nil {
		return auth.TokenClaims{}, auth.ErrUnauthenticated
	}
	if err := ctx.Err(); err != nil {
		return auth.TokenClaims{}, err
	}
	return auth.TokenClaims{UserID: c.UserID, ExpiresAt: c.ExpiresAt.Time.UTC().Truncate(time.Second)}, nil
}
