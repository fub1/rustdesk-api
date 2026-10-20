package service

import (
	"context"
	"errors"
	"testing"
	"time"

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
