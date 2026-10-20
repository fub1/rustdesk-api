package integration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/config"
	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionguard"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	"github.com/lejianwen/rustdesk-api/v2/internal/useradmin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"github.com/lejianwen/rustdesk-api/v2/utils"
	logrus "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// The old password lookup and LDAP's zero-value service wrapper read globals.
// Install them synchronously, restore on test cleanup, and never switch them
// from a worker or callback while another credential operation is running.
func credentialLegacy(t *testing.T, db *gorm.DB) *service.UserService {
	t.Helper()
	oldConfig, oldDB, oldLogger, oldJWT, oldLock, oldAll := service.Config, service.DB, service.Logger, service.Jwt, service.Lock, service.AllService
	// The native error observer intentionally returns an initialized GORM
	// statement for pool instrumentation. Legacy global query chains require a
	// fresh cloneable statement, as a normal production gorm.Open DB supplies.
	// Keep its wrapped pool and callbacks without sharing per-query clauses.
	stable := db.Session(&gorm.Session{NewDB: true, Context: context.Background()})
	built := service.New(&config.Config{App: config.App{TokenExpire: time.Hour}}, stable, logrus.New(), nil, oldLock)
	t.Cleanup(func() {
		service.Config, service.DB, service.Logger, service.Jwt, service.Lock, service.AllService = oldConfig, oldDB, oldLogger, oldJWT, oldLock, oldAll
	})
	return built.UserService
}

func credentialHTTPStatus(t *testing.T, sessions *auth.Service, token string) int {
	t.Helper()
	h, err := httpapi.New(sessions, httpapi.Options{Translate: func(_, message string) string { return message }})
	realBookMust(t, err)
	r := gin.New()
	r.GET("/credential-transition-probe", h.ClientAuth(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodGet, "/credential-transition-probe", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}
func credentialAuth(t *testing.T, db *gorm.DB) *auth.Service {
	t.Helper()
	s, err := app.NewSessions(db, time.Hour, nil)
	realBookMust(t, err)
	return s
}
func credentialNewIssue(t *testing.T, f *fixture, userID uint) auth.Principal {
	t.Helper()
	issuer, err := app.NewIssuer(f.Reopen(t), time.Hour, nil)
	realBookMust(t, err)
	current := realUserRead(t, f.DB, userID)
	p, err := issuer.Issue(context.Background(), auth.IssueRequest{UserID: userID, ExpectedPassword: &current.Password, Client: "credential-transition"})
	realBookMust(t, err)
	credentialWorks(t, f, p)
	return p
}
func credentialWorks(t *testing.T, f *fixture, p auth.Principal) {
	t.Helper()
	s := credentialAuth(t, f.Reopen(t))
	got, err := s.Authenticate(context.Background(), p.Session.Token)
	if err != nil || got.User.ID != p.User.ID || got.Session.ID != p.Session.ID {
		t.Fatal("freshly issued local session did not authenticate", err)
	}
	if status := credentialHTTPStatus(t, s, p.Session.Token); status != http.StatusNoContent {
		t.Fatal("fresh local session did not pass the client middleware", status)
	}
}
func credentialOldRejected(t *testing.T, f *fixture, token string) {
	t.Helper()
	s := credentialAuth(t, f.Reopen(t))
	if _, err := s.Authenticate(context.Background(), token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("old inactive session revived after enable", err)
	}
	if status := credentialHTTPStatus(t, s, token); status != http.StatusUnauthorized {
		t.Fatal("old inactive session was not rejected with HTTP401", status)
	}
}

func TestRealUserCredentialReenableHistoricalStatusesRevokeOldSessions(t *testing.T) {
	for _, path := range []string{"store", "ldap-wrapper"} {
		t.Run(path, func(t *testing.T) {
			for _, status := range []model.StatusCode{2, 3} {
				t.Run(map[model.StatusCode]string{2: "disabled-2", 3: "historical-3"}[status], func(t *testing.T) {
					forEngines(t, func(t *testing.T, f *fixture) {
						a, b := realUserSetup(t, f)
						// Simulate an upgrade-era inactive account whose old sessions
						// predate the new guarded disable/re-enable policy.
						realBookMust(t, f.DB.Model(&model.User{}).Where("id=?", b.User.ID).UpdateColumn("status", status).Error)
						store := realUserStore(t, f.DB)
						credentialLegacy(t, f.DB)
						update := func(in useradmin.UserInput) error {
							if path == "store" {
								return store.Update(context.Background(), a, in)
							}
							return (&service.UserService{}).Update(&model.User{IdModel: model.IdModel{Id: in.ID}, Status: in.Status, Nickname: in.Nickname})
						}
						realBookMust(t, update(useradmin.UserInput{ID: b.User.ID, Status: model.COMMON_STATUS_ENABLE}))
						credentialOldRejected(t, f, b.Session.Token)
						fresh := credentialNewIssue(t, f, b.User.ID)
						before := realUserRawSnapshot(t, f.DB)
						realBookMust(t, update(useradmin.UserInput{ID: b.User.ID, Nickname: "enabled-metadata-edit"}))
						after := realUserRawSnapshot(t, f.Reopen(t))
						if !reflect.DeepEqual(before["user_tokens"], after["user_tokens"]) {
							t.Fatal("enabled-to-enabled metadata edit revoked or changed a fresh session")
						}
						credentialWorks(t, f, fresh)
					})
				})
			}
		})
	}
}

func TestRealUserCredentialReenableNativeSQLAndCommitRollback(t *testing.T) {
	for _, path := range []string{"store", "ldap-wrapper"} {
		for _, mode := range []string{"sql", "commit"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				forEngines(t, func(t *testing.T, f *fixture) {
					a, b := realUserSetup(t, f)
					realBookMust(t, f.DB.Model(&model.User{}).Where("id=?", b.User.ID).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error)
					before := realUserRawSnapshot(t, f.DB)
					observed, native := observeTransactions(t, f.DB)
					credentialLegacy(t, observed)
					var first atomic.Bool
					realBookMust(t, observed.Callback().Update().After("gorm:update").Register("credential:reenable-first-update", func(tx *gorm.DB) {
						if tx.Statement.Table == "users" && tx.Error == nil && tx.RowsAffected == 1 {
							first.Store(true)
						}
					}))
					cleanup := realUserNativeFault(t, f, "user_tokens", "DELETE", mode)
					store := realUserStore(t, observed)
					change := func() error {
						if path == "store" {
							return store.Update(context.Background(), a, useradmin.UserInput{ID: b.User.ID, Status: model.COMMON_STATUS_ENABLE})
						}
						return (&service.UserService{}).Update(&model.User{IdModel: model.IdModel{Id: b.User.ID}, Status: model.COMMON_STATUS_ENABLE})
					}
					want := useradmin.ErrUnavailable
					if path == "ldap-wrapper" {
						want = collectionguard.ErrUnavailable
					}
					if err := change(); !errors.Is(err, want) || strings.Contains(err.Error(), "private") {
						t.Fatal("failed re-enable did not return sanitized unavailable", err)
					}
					cleanup()
					realBookMust(t, observed.Callback().Update().Remove("credential:reenable-first-update"))
					if !first.Load() || !realUserObservedNativeFault(native, f.Engine, mode) {
						t.Fatal("fault did not follow successful status UPDATE and intended final native DELETE/COMMIT")
					}
					realUserUnchanged(t, f, before)
					realBookMust(t, change())
					if realUserRead(t, f.DB, b.User.ID).Status != model.COMMON_STATUS_ENABLE {
						t.Fatal("explicit retry did not enable target")
					}
					credentialOldRejected(t, f, b.Session.Token)
					credentialNewIssue(t, f, b.User.ID)
				})
			})
		}
	}
}

func TestRealUserCredentialLegacyMD5UpgradeCASAndIssue(t *testing.T) {
	for _, scenario := range []string{"concurrent-reset", "sql-failure", "success"} {
		t.Run(scenario, func(t *testing.T) {
			forEngines(t, func(t *testing.T, f *fixture) {
				a, b := realUserSetup(t, f)
				old := utils.Md5("legacy-credential" + "rustdesk-api")
				realBookMust(t, f.DB.Model(&model.User{}).Where("id=?", b.User.ID).UpdateColumn("password", old).Error)
				current := realUserRead(t, f.DB, b.User.ID)
				observed, native := observeTransactions(t, f.DB)
				us := credentialLegacy(t, observed)
				before := realUserRawSnapshot(t, f.DB)
				var interleaved atomic.Bool
				var afterReset map[string][]map[string]any
				resetDB := f.Reopen(t)
				resetStore := realUserStore(t, resetDB)
				if scenario == "concurrent-reset" {
					realBookMust(t, observed.Callback().Update().Before("gorm:begin_transaction").Register("credential:reset-before-upgrade-cas", func(tx *gorm.DB) {
						values, ok := tx.Statement.Dest.(map[string]any)
						if !ok || tx.Statement.Table != "users" || values["password"] == nil || !interleaved.CompareAndSwap(false, true) {
							return
						}
						// No CAS transaction or row lock has begun. The independent
						// pool commits a real guarded reset in this precise window.
						if err := resetStore.ResetPassword(context.Background(), a, useradmin.PasswordInput{ID: b.User.ID, Password: "reset-credential"}); err != nil {
							tx.AddError(err)
							return
						}
						afterReset = realUserRawSnapshot(t, resetDB)
					}))
				}
				cleanup := func() {}
				if scenario == "sql-failure" {
					cleanup = realUserNativeFault(t, f, "users", "UPDATE", "sql")
				}
				result := us.InfoByUsernamePassword(current.Username, "legacy-credential")
				cleanup()
				if scenario == "concurrent-reset" {
					realBookMust(t, observed.Callback().Update().Remove("credential:reset-before-upgrade-cas"))
					if !interleaved.Load() || afterReset == nil || result == nil || result.Id != 0 {
						t.Fatal("lost MD5 upgrade CAS returned signable stale user or reset did not commit")
					}
					realUserUnchanged(t, f, afterReset)
					if token, err := us.Login(context.Background(), result, &model.LoginLog{}); err == nil || token != nil {
						t.Fatal("failed legacy lookup could issue a local session")
					}
					realUserUnchanged(t, f, afterReset)
					stored := realUserRead(t, f.DB, b.User.ID)
					ok, _, err := utils.VerifyPassword(stored.Password, "reset-credential")
					if err != nil || !ok || stored.Password == old {
						t.Fatal("upgrade overwrote independently committed reset")
					}
					result = us.InfoByUsernamePassword(current.Username, "reset-credential")
				} else if scenario == "sql-failure" {
					if result == nil || result.Id != 0 || !realUserObservedNativeFault(native, f.Engine, "sql") {
						t.Fatal("native failed MD5 upgrade returned signable user or did not reach SQL")
					}
					realUserUnchanged(t, f, before)
					if token, err := us.Login(context.Background(), result, &model.LoginLog{}); err == nil || token != nil {
						t.Fatal("SQL-failed legacy lookup could issue")
					}
					realUserUnchanged(t, f, before)
					result = us.InfoByUsernamePassword(current.Username, "legacy-credential")
				}
				if result == nil || result.Id != b.User.ID || result.Password == old {
					t.Fatal("valid explicit credential check did not return current upgraded/reset user")
				}
				stored := realUserRead(t, f.DB, b.User.ID)
				if stored.Password != result.Password {
					t.Fatal("signable password snapshot differs from committed hash")
				}
				password := "legacy-credential"
				if scenario == "concurrent-reset" {
					password = "reset-credential"
				}
				if ok, _, err := utils.VerifyPassword(stored.Password, password); err != nil || !ok {
					t.Fatal("committed credential is not the verified current password")
				}
				token, err := us.Login(context.Background(), result, &model.LoginLog{Client: model.LoginLogClientWebAdmin, Type: model.LoginLogTypeAccount})
				realBookMust(t, err)
				if token == nil || token.Token == "" {
					t.Fatal("current upgraded/reset credential could not issue")
				}
				credentialWorks(t, f, auth.Principal{User: auth.User{ID: b.User.ID}, Session: auth.Session{ID: token.Id, UserID: b.User.ID, Token: token.Token}})
			})
		})
	}
}
