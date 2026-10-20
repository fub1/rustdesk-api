package identitystore_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/identitystore"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/sessiontoken"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func fixture(t *testing.T, auto bool) (*gorm.DB, *identitystore.Store, auth.ProviderConfig) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "identity.db")+"?_foreign_keys=on&_txlock=immediate&_busy_timeout=5000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	t.Cleanup(func() { pool.Close() })
	if err := db.AutoMigrate(&model.User{}, &model.UserToken{}, &model.LoginLog{}, &model.Peer{}, &model.Oauth{}, &model.OIDCIdentity{}); err != nil {
		t.Fatal(err)
	}
	// Use the runtime schema for current-model peer writes.
	if err := database.Update(db, 281, nil); err != nil {
		t.Fatal(err)
	}
	enabled := true
	config := model.Oauth{Op: "ours", OauthType: "oidc", Issuer: "https://idp.test", ClientId: "rustdesk", AutoRegister: &auto, PkceEnable: &enabled, PkceMethod: "S256"}
	if err := db.Create(&config).Error; err != nil {
		t.Fatal(err)
	}
	store, err := identitystore.New(db, sessiontoken.NewIssuer(nil), time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := store.Provider(context.Background(), "ours")
	if err != nil {
		t.Fatal(err)
	}
	return db, store, c
}

func identity(config auth.ProviderConfig) auth.ExternalIdentity {
	return auth.ExternalIdentity{Issuer: config.Issuer, Subject: "stable-sub", Username: "Alice", Name: "Display", Email: "admin@example.test"}
}

func count(t *testing.T, db *gorm.DB, record any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(record).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIdentityAdmissionNeverLinksByEmailOrName(t *testing.T) {
	db, s, c := fixture(t, true)
	admin := true
	existing := model.User{Username: "alice", Email: "admin@example.test", IsAdmin: &admin, Status: model.COMMON_STATUS_ENABLE}
	if err := db.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	p, err := s.Login(context.Background(), identity(c), c, auth.IssueRequest{Client: "app", Method: "oauth"})
	if err != nil {
		t.Fatal(err)
	}
	if p.User.ID == existing.Id || p.User.IsAdmin || p.User.PasswordHash != "" || p.User.Username == existing.Username {
		t.Fatal("external profile selected or elevated an existing account")
	}
	second, err := s.Login(context.Background(), identity(c), c, auth.IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if p.User.ID != second.User.ID || p.Session.Token == second.Session.Token || count(t, db, &model.OIDCIdentity{}) != 1 || count(t, db, &model.User{}) != 2 {
		t.Fatal("identity did not resolve consistently")
	}
}

func TestIdentityLoginRollsBackAllAdmissionWrites(t *testing.T) {
	for _, target := range []string{"users", "oidc_identities", "user_tokens", "login_logs"} {
		t.Run(target, func(t *testing.T) {
			db, s, c := fixture(t, true)
			db.Callback().Create().Before("gorm:create").Register("fault", func(tx *gorm.DB) {
				if tx.Statement.Table == target {
					tx.AddError(errors.New("simulated database fault"))
				}
			})
			p, err := s.Login(context.Background(), identity(c), c, auth.IssueRequest{})
			if !errors.Is(err, auth.ErrUnavailable) || p.Session.Token != "" {
				t.Fatal("admission failure exposed credential")
			}
			for _, row := range []any{&model.User{}, &model.OIDCIdentity{}, &model.UserToken{}, &model.LoginLog{}} {
				if count(t, db, row) != 0 {
					t.Fatal("partial identity/login persisted")
				}
			}
		})
	}
}

func TestExplicitBindingUsesLiveActorAndUniqueIdentity(t *testing.T) {
	db, s, c := fixture(t, false)
	u := model.User{Username: "existing", Status: model.COMMON_STATUS_ENABLE, Password: "hash"}
	db.Create(&u)
	session := model.UserToken{UserId: u.Id, Token: "actor-token", ExpiredAt: time.Now().Add(time.Hour).Unix()}
	db.Create(&session)
	actor := auth.Principal{User: auth.User{ID: u.Id}, Session: auth.Session{ID: session.Id, UserID: u.Id, Token: session.Token, ExpiresAt: time.Unix(session.ExpiredAt, 0)}}
	expired := actor
	expired.Session.ExpiresAt = time.Now().Add(-time.Minute)
	if err := s.Bind(context.Background(), identity(c), c, expired); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("expired signed actor could bind through longer DB lifetime")
	}
	if _, err := s.Login(context.Background(), identity(c), c, auth.IssueRequest{}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("registration closed admitted identity")
	}
	if err := s.Bind(context.Background(), identity(c), c, actor); err != nil {
		t.Fatal(err)
	}
	if err := s.Bind(context.Background(), identity(c), c, actor); err != nil {
		t.Fatal("same binding must be idempotent")
	}
	p, err := s.Login(context.Background(), identity(c), c, auth.IssueRequest{})
	if err != nil || p.User.ID != u.Id {
		t.Fatal("explicit identity did not resolve")
	}
	// A revoked initiating browser session may not bind another external identity.
	db.Delete(&session)
	other := identity(c)
	other.Subject = "other"
	if err := s.Bind(context.Background(), other, c, actor); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("revoked actor could bind")
	}
	if count(t, db, &model.OIDCIdentity{}) != 1 {
		t.Fatal("revoked actor persisted a link")
	}
	duplicate := model.OIDCIdentity{Issuer: c.Issuer, Subject: "stable-sub", UserID: u.Id}
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("issuer/sub unique constraint absent")
	}
	orphan := model.OIDCIdentity{Issuer: c.Issuer, Subject: "orphan", UserID: 9999}
	if err := db.Create(&orphan).Error; err == nil {
		t.Fatal("user FK absent")
	}
}

func TestProviderChangesAndDisabledUsersDenyIssuance(t *testing.T) {
	db, s, c := fixture(t, true)
	p, err := s.Login(context.Background(), identity(c), c, auth.IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	db.Model(&model.User{}).Where("id = ?", p.User.ID).Update("status", model.COMMON_STATUS_DISABLED)
	if _, err := s.Login(context.Background(), identity(c), c, auth.IssueRequest{}); !errors.Is(err, auth.ErrDisabled) {
		t.Fatal("disabled user admitted")
	}
	db.Model(&model.Oauth{}).Where("id = ?", c.ID).Update("issuer", "https://changed.test")
	if _, err := s.Login(context.Background(), identity(c), c, auth.IssueRequest{}); !errors.Is(err, auth.ErrInvalidInput) {
		t.Fatal("stale provider admitted")
	}
	if count(t, db, &model.UserToken{}) != 1 {
		t.Fatal("denied login created session")
	}
}

func TestIssuerAndSubjectAreCaseSensitive(t *testing.T) {
	db, s, c := fixture(t, true)
	a := identity(c)
	if _, err := s.Login(context.Background(), a, c, auth.IssueRequest{}); err != nil {
		t.Fatal(err)
	}
	a.Subject = "STABLE-SUB"
	if _, err := s.Login(context.Background(), a, c, auth.IssueRequest{}); err != nil {
		t.Fatal(err)
	}
	if count(t, db, &model.OIDCIdentity{}) != 2 {
		t.Fatal("subject case folded")
	}
}

func TestUnbindDoesNotCountAnUnconfiguredIdentityAsRecovery(t *testing.T) {
	db, s, c := fixture(t, true)
	actor, err := s.Login(context.Background(), identity(c), c, auth.IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	stale := model.OIDCIdentity{Issuer: "https://old-provider.test", Subject: "other-sub", UserID: actor.User.ID}
	if err := db.Create(&stale).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Unbind(context.Background(), c, actor); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("unconfigured issuer counted as a usable login method")
	}
	enabled := true
	other := model.Oauth{Op: "other", OauthType: "oidc", Issuer: stale.Issuer, ClientId: "other", PkceEnable: &enabled, PkceMethod: "S256"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Unbind(context.Background(), c, actor); err != nil {
		t.Fatal(err)
	}
	if count(t, db, &model.OIDCIdentity{}) != 1 {
		t.Fatal("wrong identity removed")
	}
}
