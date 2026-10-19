package service

import (
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/config"
)

func TestLegacyIssuanceDefaultTTLMatchesSessionValidation(t *testing.T) {
	previous := Config
	Config = &config.Config{}
	t.Cleanup(func() { Config = previous })
	before := time.Now().Add(168 * time.Hour).Unix()
	expiry := (&UserService{}).UserTokenExpireTimestamp()
	if expiry < before || expiry > before+1 {
		t.Fatalf("zero TTL expiration=%d, want seven days near %d", expiry, before)
	}
}
