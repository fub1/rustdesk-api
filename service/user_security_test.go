package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/userguard"
	"github.com/lejianwen/rustdesk-api/v2/model"
)

func TestUserUpdateUsesBoundStorageAndLegacyLDAPFallback(t *testing.T) {
	firstDB := issuanceDB(t)
	firstService := issuanceService(t, firstDB, time.Hour)
	first := issuanceUser(t, firstDB)
	secondDB := issuanceDB(t)
	_ = issuanceService(t, secondDB, time.Hour)
	second := issuanceUser(t, secondDB)
	for _, entry := range []struct {
		dbToken string
		which   bool
	}{{"first-session", true}, {"second-session", false}} {
		db := secondDB
		owner := second.Id
		if entry.which {
			db = firstDB
			owner = first.Id
		}
		issuanceCreate(t, db, &model.UserToken{UserId: owner, Token: entry.dbToken, ExpiredAt: time.Now().Add(time.Hour).Unix()})
	}
	firstInput := first
	firstInput.Nickname = "first-disabled"
	firstInput.Status = model.COMMON_STATUS_DISABLED
	firstInput.Password = "must-not-write-via-metadata"
	if err := firstService.UpdateContext(context.Background(), &firstInput); err != nil {
		t.Fatal(err)
	}
	var currentFirst, currentSecond model.User
	if err := firstDB.First(&currentFirst, first.Id).Error; err != nil {
		t.Fatal(err)
	}
	if err := secondDB.First(&currentSecond, second.Id).Error; err != nil {
		t.Fatal(err)
	}
	if currentFirst.Nickname != "first-disabled" || currentFirst.Status != model.COMMON_STATUS_DISABLED || currentFirst.Password != first.Password || currentSecond.Status != model.COMMON_STATUS_ENABLE {
		t.Fatal("bound metadata update leaked to global storage or overwrote password")
	}
	if issuanceCount(t, firstDB, &model.UserToken{}) != 0 || issuanceCount(t, secondDB, &model.UserToken{}) != 1 {
		t.Fatal("bound disable revoked wrong database")
	}
	currentSecond.Nickname = "ldap-disabled"
	currentSecond.Status = model.COMMON_STATUS_DISABLED
	if err := (&UserService{}).Update(&currentSecond); err != nil {
		t.Fatal("zero-value legacy LDAP adapter broke", err)
	}
	if err := secondDB.First(&currentSecond, second.Id).Error; err != nil {
		t.Fatal(err)
	}
	if currentSecond.Nickname != "ldap-disabled" || currentSecond.Status != model.COMMON_STATUS_DISABLED || issuanceCount(t, secondDB, &model.UserToken{}) != 0 {
		t.Fatal("legacy fallback did not commit disable and revoke")
	}
}

func TestLegacyUpdateDeleteProtectLastEnabledAdminAndNilRole(t *testing.T) {
	for _, action := range []string{"disable", "demote", "delete"} {
		t.Run(action, func(t *testing.T) {
			db := issuanceDB(t)
			us := issuanceService(t, db, time.Hour)
			yes := true
			enabled := model.User{Username: "enabled-admin", IsAdmin: &yes, Status: model.COMMON_STATUS_ENABLE}
			disabled := model.User{Username: "disabled-admin", IsAdmin: &yes, Status: model.COMMON_STATUS_DISABLED}
			issuanceCreate(t, db, &enabled)
			issuanceCreate(t, db, &disabled)
			input := model.User{IdModel: model.IdModel{Id: enabled.Id}, Nickname: "allowed-edit"}
			if err := us.Update(&input); err != nil {
				t.Fatal("nil role metadata update rejected", err)
			}
			var err error
			switch action {
			case "delete":
				err = us.DeleteContext(context.Background(), &enabled)
				if !errors.Is(err, errLastAdminDelete) {
					t.Fatal("delete removed last enabled administrator", err)
				}
			case "disable":
				input.Status = model.COMMON_STATUS_DISABLED
				err = us.Update(&input)
			case "demote":
				no := false
				input.IsAdmin = &no
				err = us.Update(&input)
			}
			if action != "delete" && !errors.Is(err, userguard.ErrLastEnabledAdmin) {
				t.Fatal("update removed last enabled administrator", err)
			}
			if err := db.First(&enabled, enabled.Id).Error; err != nil || !userguard.EnabledAdmin(enabled) || enabled.Nickname != "allowed-edit" {
				t.Fatal("last admin changed despite rejection", err)
			}
		})
	}
}

func TestLegacyAndLDAPReenableRevokeHistoricalSessionsAndAllowFreshLogin(t *testing.T) {
	for _, legacyLDAP := range []bool{false, true} {
		for _, status := range []model.StatusCode{model.COMMON_STATUS_DISABLED, model.StatusCode(3)} {
			t.Run(fmt.Sprintf("ldap-%t/status-%d", legacyLDAP, status), func(t *testing.T) {
				db := issuanceDB(t)
				us := issuanceService(t, db, time.Hour)
				u := issuanceUser(t, db)
				issuanceCreate(t, db, &model.UserToken{UserId: u.Id, Token: "historical-local-token", ExpiredAt: time.Now().Add(time.Hour).Unix()})
				if err := db.Model(&model.User{}).Where("id=?", u.Id).UpdateColumn("status", status).Error; err != nil {
					t.Fatal(err)
				}
				sessions, err := app.NewSessions(db, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := sessions.Authenticate(context.Background(), "historical-local-token"); !errors.Is(err, auth.ErrDisabled) {
					t.Fatal("historical fixture did not retain inactive account token", err)
				}
				input := model.User{IdModel: model.IdModel{Id: u.Id}, Status: model.COMMON_STATUS_ENABLE, Nickname: "reenabled"}
				writer := us
				if legacyLDAP {
					writer = &UserService{} // Same compatibility path used by LDAP sync.
				}
				if err := writer.Update(&input); err != nil {
					t.Fatal(err)
				}
				if _, err := sessions.Authenticate(context.Background(), "historical-local-token"); !errors.Is(err, auth.ErrUnauthenticated) {
					t.Fatal("legacy/LDAP reenable revived historical token", err)
				}
				if issuanceCount(t, db, &model.UserToken{}) != 0 {
					t.Fatal("legacy/LDAP reenable left old sessions")
				}
				fresh, err := us.Login(context.Background(), &u, &model.LoginLog{UserId: u.Id, Client: model.LoginLogClientWebAdmin, Type: model.LoginLogTypeAccount})
				if err != nil || fresh == nil {
					t.Fatal("reenabled user could not log in again", err)
				}
				if authenticated, err := sessions.Authenticate(context.Background(), fresh.Token); err != nil || authenticated.User.ID != u.Id {
					t.Fatal("fresh legacy-issued session did not authenticate", err)
				}
				if err := writer.Update(&model.User{IdModel: model.IdModel{Id: u.Id}, Nickname: "enabled-metadata"}); err != nil {
					t.Fatal(err)
				}
				if _, err := sessions.Authenticate(context.Background(), fresh.Token); err != nil {
					t.Fatal("enabled metadata update revoked fresh legacy session", err)
				}
			})
		}
	}
}
