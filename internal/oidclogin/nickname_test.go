package oidclogin_test

import (
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/model"
)

func TestPreferredUsernameNicknameThroughCallbackAndPoll(t *testing.T) {
	f := newFixture(t, nil)
	user := model.User{Username: "unchanged-local-login", Status: model.COMMON_STATUS_ENABLE}
	if err := f.db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	link := model.OIDCIdentity{Issuer: f.protocol.identity.Issuer, Subject: f.protocol.identity.Subject, UserID: user.Id}
	if err := f.db.Create(&link).Error; err != nil {
		t.Fatal(err)
	}
	// Match the existing IdP contract: preferred_username, without a name claim.
	f.protocol.identity.Username, f.protocol.identity.Name = " \tPreferred Display\n", ""
	first := f.poll(t, f.authorize(t))
	if first.User.ID != user.Id || first.User.Username != user.Username || first.User.Nickname != "Preferred Display" || first.User.IsAdmin {
		t.Fatal("first login response lost the preferred display or local identity")
	}
	f.protocol.identity.Username, f.protocol.identity.Name = "Later preferred value", "Different name"
	second := f.poll(t, f.authorize(t))
	if second.User.ID != user.Id || second.User.Username != user.Username || second.User.Nickname != "Preferred Display" {
		t.Fatal("later login changed the internal username or existing nickname")
	}
	var stored model.OIDCIdentity
	if err := f.db.First(&stored, link.Id).Error; err != nil || stored != link {
		t.Fatal("login changed the issuer/sub association", err)
	}
	assertCounts(t, f, 2, 2, 1)
	if count(t, f.db, &model.User{}) != 1 {
		t.Fatal("display mapping created a second local account")
	}
}
