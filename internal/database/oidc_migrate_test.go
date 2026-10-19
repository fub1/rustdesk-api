package database

import (
	"github.com/lejianwen/rustdesk-api/v2/model"
	"testing"
)

func TestUpgrade274AddsIdentityWithoutInferringLegacyLinks(t *testing.T) {
	db := openTestDB(t)
	if err := Update(db, 274, seedTestData); err != nil {
		t.Fatal(err)
	}
	// Emulate the actual old schema rather than keeping the new table created
	// by this checkout's schema function in the historical setup.
	if err := db.Migrator().DropTable(&model.OIDCIdentity{}); err != nil {
		t.Fatal(err)
	}
	legacy := model.UserThird{UserId: 1, Op: "ours", OauthType: "oidc", OauthUser: model.OauthUser{OpenId: "legacy-sub", Email: "admin@example.test"}}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if err := Update(db, testVersion, seedTestData); err != nil {
		t.Fatal(err)
	}
	assertCurrentVersion(t, db, testVersion)
	if rowCount(t, db, &model.OIDCIdentity{}) != 0 || rowCount(t, db, &model.UserThird{}) != 1 {
		t.Fatal("migration inferred external identity")
	}
	if !db.Migrator().HasConstraint(&model.OIDCIdentity{}, "User") {
		t.Fatal("identity user FK not installed")
	}
}
