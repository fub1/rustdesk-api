package service

import (
	"context"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/utils"
	"gorm.io/gorm"
)

func TestLegacyPasswordUpgradeCommitsVerifiedHashAndIssues(t *testing.T) {
	db := issuanceDB(t)
	us := issuanceService(t, db, time.Hour)
	u := issuanceUser(t, db)
	old := utils.Md5("legacy-password" + "rustdesk-api")
	if err := db.Model(&u).UpdateColumn("password", old).Error; err != nil {
		t.Fatal(err)
	}
	result := us.InfoByUsernamePassword(u.Username, "legacy-password")
	if result == nil || result.Id != u.Id || result.Password == old {
		t.Fatal("valid legacy password not upgraded")
	}
	var stored model.User
	if err := db.First(&stored, u.Id).Error; err != nil {
		t.Fatal(err)
	}
	ok, _, err := utils.VerifyPassword(stored.Password, "legacy-password")
	if err != nil || !ok || stored.Password != result.Password {
		t.Fatal("stored upgrade differs from verified result", err)
	}
	token, err := us.Login(context.Background(), result, &model.LoginLog{UserId: u.Id, Client: model.LoginLogClientWebAdmin, Type: model.LoginLogTypeAccount})
	if err != nil || token == nil || token.Token == "" {
		t.Fatal("actual upgraded hash cannot issue", err)
	}
}

func TestLegacyPasswordUpgradeCannotOverwriteConcurrentReset(t *testing.T) {
	db := issuanceDB(t)
	us := issuanceService(t, db, time.Hour)
	u := issuanceUser(t, db)
	old := utils.Md5("legacy-password" + "rustdesk-api")
	if err := db.Model(&u).UpdateColumn("password", old).Error; err != nil {
		t.Fatal(err)
	}
	replacement, err := utils.EncryptPassword("replacement-password")
	if err != nil {
		t.Fatal(err)
	}
	reached := false
	if err := db.Callback().Update().Before("gorm:begin_transaction").Register("password-upgrade:concurrent-reset", func(tx *gorm.DB) {
		if reached || tx.Statement.Table != "users" {
			return
		}
		values, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok || values["password"] == nil {
			return
		}
		reached = true
		// Interleave a committed reset after verified legacy lookup, before upgrade
		// CAS SQL. This is a separate autocommit statement, not a swallowed trigger.
		if err := db.Exec("UPDATE users SET password=? WHERE id=?", replacement, u.Id).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	result := us.InfoByUsernamePassword(u.Username, "legacy-password")
	if !reached || result == nil || result.Id != 0 {
		t.Fatal("lost upgrade CAS returned a stale signable user")
	}
	var stored model.User
	if err := db.First(&stored, u.Id).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Password != replacement || issuanceCount(t, db, &model.UserToken{}) != 0 {
		t.Fatal("legacy upgrade overwrote concurrent reset or issued")
	}
	result = us.InfoByUsernamePassword(u.Username, "replacement-password")
	if result.Id != u.Id || result.Password != replacement {
		t.Fatal("current password could not retry after conflict")
	}
}

func TestLegacyPasswordUpgradeStorageFailureDoesNotReturnSignableUser(t *testing.T) {
	db := issuanceDB(t)
	us := issuanceService(t, db, time.Hour)
	u := issuanceUser(t, db)
	old := utils.Md5("legacy-password" + "rustdesk-api")
	if err := db.Model(&u).UpdateColumn("password", old).Error; err != nil {
		t.Fatal(err)
	}
	issuanceSQL(t, db, "CREATE TRIGGER fail_password_upgrade BEFORE UPDATE OF password ON users BEGIN SELECT RAISE(ABORT,'private hash upgrade fault'); END")
	result := us.InfoByUsernamePassword(u.Username, "legacy-password")
	if result == nil || result.Id != 0 {
		t.Fatal("failed upgrade returned a stale signable user")
	}
	var stored model.User
	if err := db.First(&stored, u.Id).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Password != old || issuanceCount(t, db, &model.UserToken{}) != 0 {
		t.Fatal("failed upgrade changed hash or issued")
	}
	issuanceSQL(t, db, "DROP TRIGGER fail_password_upgrade")
	result = us.InfoByUsernamePassword(u.Username, "legacy-password")
	if result.Id != u.Id || result.Password == old {
		t.Fatal("valid explicit retry did not upgrade")
	}
}
