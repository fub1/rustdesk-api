package identitystore_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/identitystore"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

type nicknameIssuerFunc func(context.Context, uint, time.Time, time.Time) (string, error)

func (f nicknameIssuerFunc) Issue(ctx context.Context, id uint, now, expiry time.Time) (string, error) {
	return f(ctx, id, now, expiry)
}

func nicknameExisting(t *testing.T, db *gorm.DB, config auth.ProviderConfig, nickname string) model.User {
	t.Helper()
	admin := true
	user := model.User{Username: "local-existing", Nickname: nickname, Password: "local-password-hash",
		Email: "local@example.test", Avatar: "local-avatar", GroupId: 7, IsAdmin: &admin,
		Status: model.COMMON_STATUS_ENABLE, Remark: "local-remark"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.OIDCIdentity{Issuer: config.Issuer, Subject: "stable-sub", UserID: user.Id}).Error; err != nil {
		t.Fatal(err)
	}
	return nicknameRead(t, db, user.Id)
}

func nicknameRead(t *testing.T, db *gorm.DB, id uint) model.User {
	t.Helper()
	var user model.User
	if err := db.First(&user, id).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

func nicknameOnlyChange(t *testing.T, before, after model.User, want string) {
	t.Helper()
	if after.Nickname != want {
		t.Fatalf("nickname = %q, want %q", after.Nickname, want)
	}
	after.Nickname = before.Nickname
	if !reflect.DeepEqual(before, after) {
		t.Fatal("nickname synchronization changed another local user field")
	}
}

func TestOIDCExistingNicknameFillOnlyAndPreserveLocalProfile(t *testing.T) {
	for _, tc := range []struct{ label, before, username, name, want string }{
		{"preferred-without-name", "", "Preferred User", "", "Preferred User"},
		{"preferred-wins-over-name", "", "Preferred User", "Different Name", "Preferred User"},
		{"trim-preferred-username", "", " \t显示 用户\n", "Ignored Name", "显示 用户"},
		{"existing", "Personal nickname", "Changed Preferred User", "Changed Name", "Personal nickname"},
		{"existing-whitespace-is-not-empty", " \t ", "Preferred User", "", " \t "},
		{"missing-preferred-no-name-fallback", "", "", "Must not fill", ""},
		{"blank-preferred-no-name-fallback", "", " \t\r\n\u3000", "Must not fill", ""},
	} {
		t.Run(tc.label, func(t *testing.T) {
			db, store, config := fixture(t, false)
			before := nicknameExisting(t, db, config, tc.before)
			var updates int
			if err := db.Callback().Update().Before("gorm:update").Register("nickname:observe-user-write", func(tx *gorm.DB) {
				if tx.Statement.Table == "users" {
					updates++
				}
			}); err != nil {
				t.Fatal(err)
			}
			claimed := identity(config)
			claimed.Name, claimed.Username, claimed.Email, claimed.Picture = tc.name, tc.username, "external@example.test", "external-avatar"
			principal, err := store.Login(context.Background(), claimed, config, auth.IssueRequest{Method: "oauth"})
			if err != nil {
				t.Fatal(err)
			}
			if principal.User.ID != before.Id || principal.User.Username != before.Username || !principal.User.IsAdmin ||
				principal.User.GroupID != before.GroupId || principal.User.Nickname != tc.want || principal.Session.Token == "" {
				t.Fatal("login response lost local identity/role or returned a stale nickname")
			}
			nicknameOnlyChange(t, before, nicknameRead(t, db, before.Id), tc.want)
			if tc.before == tc.want && updates != 0 {
				t.Fatal("a preserved/blank nickname caused an unnecessary user write")
			}
			var link model.OIDCIdentity
			if err := db.First(&link).Error; err != nil || link.UserID != before.Id || link.Issuer != config.Issuer || link.Subject != claimed.Subject || count(t, db, &model.OIDCIdentity{}) != 1 || count(t, db, &model.User{}) != 1 {
				t.Fatal("nickname completion changed the issuer/sub identity association")
			}
		})
	}
}

func TestOIDCExistingNicknameRepeatedLoginAndManualEdit(t *testing.T) {
	db, store, config := fixture(t, false)
	before := nicknameExisting(t, db, config, "")
	claimed := identity(config)
	for _, name := range []string{"First trusted name", "Later OIDC name"} {
		claimed.Username, claimed.Name = name, "Ignored Name"
		principal, err := store.Login(context.Background(), claimed, config, auth.IssueRequest{})
		if err != nil || principal.User.Nickname != "First trusted name" {
			t.Fatal("a repeated login overwrote the first committed nickname", err)
		}
	}
	if err := db.Model(&model.User{}).Where("id = ?", before.Id).UpdateColumn("nickname", "Manual choice").Error; err != nil {
		t.Fatal(err)
	}
	principal, err := store.Login(context.Background(), claimed, config, auth.IssueRequest{})
	if err != nil || principal.User.Nickname != "Manual choice" {
		t.Fatal("OIDC login overwrote a later manual nickname", err)
	}
	nicknameOnlyChange(t, before, nicknameRead(t, db, before.Id), "Manual choice")
}

func TestOIDCExistingNicknameFailuresLeaveNoPartialLogin(t *testing.T) {
	for _, target := range []string{"token-issuer", "user_tokens", "login_logs", "nickname-update", "disabled-user"} {
		t.Run(target, func(t *testing.T) {
			db, store, config := fixture(t, false)
			before := nicknameExisting(t, db, config, "")
			wantErr := auth.ErrUnavailable
			if target == "token-issuer" {
				var err error
				store, err = identitystore.New(db, nicknameIssuerFunc(func(context.Context, uint, time.Time, time.Time) (string, error) {
					return "", errors.New("test signing failure")
				}), time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
			} else if target == "disabled-user" {
				if err := db.Model(&model.User{}).Where("id = ?", before.Id).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
					t.Fatal(err)
				}
				before = nicknameRead(t, db, before.Id)
				wantErr = auth.ErrDisabled
			} else if target == "nickname-update" {
				if err := db.Callback().Update().Before("gorm:update").Register("nickname:fail-update", func(tx *gorm.DB) {
					if tx.Statement.Table == "users" {
						tx.AddError(errors.New("test nickname write failure"))
					}
				}); err != nil {
					t.Fatal(err)
				}
			} else if err := db.Callback().Create().Before("gorm:create").Register("nickname:fail-session-log", func(tx *gorm.DB) {
				if tx.Statement.Table == target {
					tx.AddError(errors.New("test login persistence failure"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			principal, err := store.Login(context.Background(), identity(config), config, auth.IssueRequest{})
			if !errors.Is(err, wantErr) || !reflect.DeepEqual(principal, auth.Principal{}) {
				t.Fatalf("failed login returned a principal or wrong error: %v", err)
			}
			nicknameOnlyChange(t, before, nicknameRead(t, db, before.Id), "")
			if count(t, db, &model.UserToken{}) != 0 || count(t, db, &model.LoginLog{}) != 0 || count(t, db, &model.User{}) != 1 || count(t, db, &model.OIDCIdentity{}) != 1 {
				t.Fatal("failed login partially committed nickname/session/log/identity")
			}
		})
	}
}

func TestOIDCNewAccountNicknameNormalizesPreferredUsername(t *testing.T) {
	for _, tc := range []struct{ username, name, want string }{
		{" \t\n\u3000", "Must not fill", ""},
		{"", "Must not fill", ""},
		{" \tPreferred User\n", "Different Name", "Preferred User"},
		{"Preferred User", "", "Preferred User"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			db, store, config := fixture(t, true)
			claimed := identity(config)
			claimed.Username, claimed.Name = tc.username, tc.name
			principal, err := store.Login(context.Background(), claimed, config, auth.IssueRequest{})
			if err != nil || principal.User.Nickname != tc.want || principal.User.IsAdmin {
				t.Fatal("new OIDC nickname normalization or ordinary-user admission failed", err)
			}
			if nicknameRead(t, db, principal.User.ID).Nickname != tc.want {
				t.Fatal("persisted new nickname was not normalized")
			}
		})
	}
}
