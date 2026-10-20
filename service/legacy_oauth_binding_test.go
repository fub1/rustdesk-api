package service

import (
	"reflect"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/model"
)

// Legacy op/open_id data is historical only. The old service boundary must not
// create a new identity even when its provider and local user both still exist.
func TestLegacyOAuthBindingCannotWriteIdentities(t *testing.T) {
	db := issuanceDB(t)
	issuanceService(t, db, 0)
	if err := db.AutoMigrate(&model.Oauth{}); err != nil {
		t.Fatal(err)
	}
	user := issuanceUser(t, db)
	issuanceCreate(t, db, &model.Oauth{Op: "legacy-provider", OauthType: model.OauthTypeOidc, Issuer: "https://issuer.test", ClientId: "client"})
	issuanceCreate(t, db, &model.UserThird{UserId: user.Id, Op: "legacy-provider", OauthUser: model.OauthUser{OpenId: "historical-sub", Email: "historical@example.test"}})
	issuanceCreate(t, db, &model.OIDCIdentity{UserID: user.Id, Issuer: "https://issuer.test", Subject: "verified-sub"})
	issuanceCreate(t, db, &model.UserToken{UserId: user.Id, Token: "existing-session", ExpiredAt: 2000000000})
	type state struct {
		Users      []model.User
		Thirds     []model.UserThird
		Identities []model.OIDCIdentity
		Tokens     []model.UserToken
	}
	read := func() state {
		t.Helper()
		var s state
		for _, rows := range []any{&s.Users, &s.Thirds, &s.Identities, &s.Tokens} {
			if err := db.Order("id ASC").Find(rows).Error; err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	before := read()
	for _, tc := range []struct {
		name string
		id   uint
		user *model.OauthUser
		op   string
	}{
		{"existing-user-and-provider", user.Id, &model.OauthUser{OpenId: "new-sub", Email: user.Email}, "legacy-provider"},
		{"missing-user", user.Id + 1000, &model.OauthUser{OpenId: "late-sub"}, "legacy-provider"},
		{"historical-identity", user.Id, &model.OauthUser{OpenId: "historical-sub"}, "legacy-provider"},
		{"nil-profile", user.Id, nil, "legacy-provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := (&OauthService{}).BindOauthUser(tc.id, tc.user, tc.op); err == nil {
				t.Fatal("legacy identity admission succeeded")
			}
			if after := read(); !reflect.DeepEqual(before, after) {
				t.Fatal("legacy binding changed persisted users, historical identities, verified identities or sessions")
			}
		})
	}
	// Historical records remain readable; denying new admission is not migration
	// or deletion of an already-stored provider-name association.
	if old := (&OauthService{}).UserThirdInfo("legacy-provider", "historical-sub"); old.UserId != user.Id {
		t.Fatal("historical identity read changed")
	}
}

func TestLegacyOAuthBindingRejectsWithoutDependencies(t *testing.T) {
	old := DB
	DB = nil
	t.Cleanup(func() { DB = old })
	var service *OauthService
	if err := service.BindOauthUser(0, nil, ""); err == nil {
		t.Fatal("legacy binding accepted empty inputs without dependencies")
	}
}
