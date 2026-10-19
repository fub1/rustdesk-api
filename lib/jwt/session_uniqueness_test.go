package jwt

import (
	"testing"
	"time"
)

func TestIndependentSessionsHaveDistinctTokens(t *testing.T) {
	issuer := NewJwt("test-only-session-key", time.Hour)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		token := issuer.GenerateToken(1)
		if token == "" || seen[token] {
			t.Fatal("independent logins produced an empty or duplicate token")
		}
		seen[token] = true
		if id, err := issuer.ParseToken(token); err != nil || id != 1 {
			t.Fatalf("new token failed existing issuer contract: user=%d error=%v", id, err)
		}
	}
}
