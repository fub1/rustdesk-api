package service

import (
	"github.com/lejianwen/rustdesk-api/v2/model"
	"testing"
)

func TestPublicOIDCUpdateClearsLegacySecret(t *testing.T) {
	db := issuanceDB(t)
	if err := db.AutoMigrate(&model.Oauth{}); err != nil {
		t.Fatal(err)
	}
	old := DB
	DB = db
	t.Cleanup(func() { DB = old })
	config := model.Oauth{Op: "ours", OauthType: "oidc", Issuer: "https://idp.test", ClientId: "client", ClientSecret: "legacy-secret"}
	if err := db.Create(&config).Error; err != nil {
		t.Fatal(err)
	}
	config.ClientSecret = ""
	config.PkceEnable = nil
	if err := (&OauthService{}).Update(&config); err != nil {
		t.Fatal(err)
	}
	var stored model.Oauth
	if err := db.First(&stored, config.Id).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ClientSecret != "" || stored.PkceEnable == nil || !*stored.PkceEnable || stored.PkceMethod != "S256" {
		t.Fatal("public configuration did not clear legacy secret")
	}
}
