package sessiontoken

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
)

type Issuer struct {
	key    []byte
	random io.Reader
}

func NewIssuer(key []byte) *Issuer {
	return &Issuer{key: append([]byte(nil), key...), random: rand.Reader}
}

func (i *Issuer) Issue(ctx context.Context, userID uint, now, expiry time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if userID == 0 || !expiry.After(now) {
		return "", auth.ErrInvalidInput
	}
	var b [32]byte
	if _, err := io.ReadFull(i.random, b[:]); err != nil {
		return "", err
	}
	value := base64.RawURLEncoding.EncodeToString(b[:])
	if len(i.key) == 0 {
		return value, nil
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims{UserID: userID, RegisteredClaims: jwt.RegisteredClaims{
		ID: value, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(expiry),
	}}).SignedString(i.key)
}
