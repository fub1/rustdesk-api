package sessiontoken

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
)

func TestHS256RequiresKey(t *testing.T) {
	if _, err := NewHS256(nil); err == nil {
		t.Fatal("missing key accepted")
	}
}

func TestHS256Validation(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	key := []byte("independent-test-signing-key")
	v, err := NewHS256(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		method jwt.SigningMethod
		key    []byte
		claims jwt.MapClaims
		ok     bool
	}{
		{"valid", jwt.SigningMethodHS256, key, jwt.MapClaims{"user_id": 12, "exp": now.Add(time.Hour).Unix()}, true},
		{"wrong_signature", jwt.SigningMethodHS256, []byte("different-key"), jwt.MapClaims{"user_id": 12, "exp": now.Add(time.Hour).Unix()}, false},
		{"wrong_algorithm", jwt.SigningMethodHS384, key, jwt.MapClaims{"user_id": 12, "exp": now.Add(time.Hour).Unix()}, false},
		{"missing_exp", jwt.SigningMethodHS256, key, jwt.MapClaims{"user_id": 12}, false},
		{"expired", jwt.SigningMethodHS256, key, jwt.MapClaims{"user_id": 12, "exp": now.Unix()}, false},
		{"future_nbf", jwt.SigningMethodHS256, key, jwt.MapClaims{"user_id": 12, "exp": now.Add(time.Hour).Unix(), "nbf": now.Add(time.Minute).Unix()}, false},
		{"zero_user", jwt.SigningMethodHS256, key, jwt.MapClaims{"user_id": 0, "exp": now.Add(time.Hour).Unix()}, false},
		{"fractional_user", jwt.SigningMethodHS256, key, jwt.MapClaims{"user_id": 1.5, "exp": now.Add(time.Hour).Unix()}, false},
		{"negative_user", jwt.SigningMethodHS256, key, jwt.MapClaims{"user_id": -1, "exp": now.Add(time.Hour).Unix()}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := jwt.NewWithClaims(test.method, test.claims).SignedString(test.key)
			if err != nil {
				t.Fatal(err)
			}
			claims, err := v.Verify(context.Background(), raw, now)
			if test.ok {
				if err != nil || claims.UserID != 12 || !claims.ExpiresAt.Equal(now.Add(time.Hour)) {
					t.Fatalf("valid claims: %+v, %v", claims, err)
				}
			} else if !errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatalf("invalid JWT accepted: %v", err)
			}
		})
	}
	if _, err := v.Verify(context.Background(), "not-a-token", now); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("malformed JWT: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.Verify(ctx, "anything", now); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestHS256CopiesSigningKey(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	key := []byte("original-key")
	v, err := NewHS256(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"user_id": 1, "exp": now.Add(time.Hour).Unix()}).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	key[0] = 'X'
	if _, err := v.Verify(context.Background(), raw, now); err != nil {
		t.Fatalf("caller mutated verifier key: %v", err)
	}
}
