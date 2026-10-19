package sessiontoken

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type brokenRandom struct{}

func (brokenRandom) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestSessionIssuerUniquenessAndVerification(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, key := range [][]byte{nil, []byte("test-key")} {
		issuer := NewIssuer(key)
		seen := map[string]bool{}
		for n := 0; n < 100; n++ {
			token, err := issuer.Issue(context.Background(), 7, now, now.Add(time.Hour))
			if err != nil || seen[token] || token == "" {
				t.Fatal("credential generation failed or repeated")
			}
			seen[token] = true
			if len(key) == 0 {
				if len(token) != 43 || strings.Contains(token, "=") {
					t.Fatal("invalid opaque token")
				}
			} else {
				verifier, _ := NewHS256(key)
				claims, err := verifier.Verify(context.Background(), token, now)
				if err != nil || claims.UserID != 7 || !claims.ExpiresAt.Equal(now.Add(time.Hour)) {
					t.Fatal("issued JWT is not accepted by session verifier")
				}
			}
		}
	}
}

func TestIssuerPropagatesRandomFailure(t *testing.T) {
	issuer := NewIssuer(nil)
	issuer.random = brokenRandom{}
	token, err := issuer.Issue(context.Background(), 1, time.Now(), time.Now().Add(time.Hour))
	if token != "" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("random failure accepted")
	}
}
