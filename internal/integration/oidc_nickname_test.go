package integration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/identitystore"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/sessiontoken"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

type oidcNicknameIssuerFunc func(context.Context, uint, time.Time, time.Time) (string, error)

func (f oidcNicknameIssuerFunc) Issue(ctx context.Context, id uint, now, expiry time.Time) (string, error) {
	return f(ctx, id, now, expiry)
}

func oidcNicknameUser(t *testing.T, db *gorm.DB, config auth.ProviderConfig, subject string) model.User {
	t.Helper()
	admin := true
	user := model.User{Username: "local-" + subject, Email: "local@example.test", Password: "local-hash",
		Avatar: "local-avatar", Remark: "local-remark", GroupId: 1, IsAdmin: &admin, Status: model.COMMON_STATUS_ENABLE}
	transactionCreate(t, db, &user)
	transactionCreate(t, db, &model.OIDCIdentity{Issuer: config.Issuer, Subject: subject, UserID: user.Id})
	return oidcNicknameRead(t, db, user.Id)
}

func oidcNicknameRead(t *testing.T, db *gorm.DB, id uint) model.User {
	t.Helper()
	var user model.User
	if err := db.First(&user, id).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

func oidcNicknameProfileUnchanged(t *testing.T, before, after model.User, nickname string) {
	t.Helper()
	if after.Nickname != nickname {
		t.Fatalf("nickname = %q, want %q", after.Nickname, nickname)
	}
	after.Nickname = before.Nickname
	if !reflect.DeepEqual(before, after) {
		t.Fatal("OIDC name synchronization changed another local profile/role/identity field")
	}
}

func oidcNicknameStore(t *testing.T, db *gorm.DB) *identitystore.Store {
	t.Helper()
	store, err := identitystore.New(db, sessiontoken.NewIssuer(nil), time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func oidcNicknameIdentity(config auth.ProviderConfig, subject, name string) auth.ExternalIdentity {
	return auth.ExternalIdentity{Issuer: config.Issuer, Subject: subject, Name: "Ignored OIDC name",
		Username: name, Email: "external@example.test", Picture: "external-avatar"}
}

func TestOIDCNicknameRealFailuresRollbackAndRetry(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		upgrade(t, f.DB, 281)
		_, config := identityConfig(t, f.DB)
		user := oidcNicknameUser(t, f.DB, config, "rollback")
		claimed := oidcNicknameIdentity(config, "rollback", " \tTrusted Real Name\n")
		for _, tc := range []struct{ name, table, event, mode string }{
			{"token-signing", "", "", "issuer"},
			{"session-insert", "user_tokens", "INSERT", "sql"},
			{"login-log-insert", "login_logs", "INSERT", "sql"},
			{"nickname-update", "users", "UPDATE", "sql"},
			{"nickname-commit", "users", "UPDATE", "commit"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				// Existing successful sessions are part of the rollback assertion,
				// rather than testing only a newly empty database.
				if err := f.DB.Model(&model.User{}).Where("id = ?", user.Id).UpdateColumn("nickname", "").Error; err != nil {
					t.Fatal(err)
				}
				before := oidcNicknameRead(t, f.DB, user.Id)
				sessions := transactionCount(t, f.DB, &model.UserToken{})
				logs := transactionCount(t, f.DB, &model.LoginLog{})
				observed, native := observeTransactions(t, f.DB)
				store := oidcNicknameStore(t, observed)
				remove := func() {}
				if tc.mode == "issuer" {
					var err error
					store, err = identitystore.New(observed, oidcNicknameIssuerFunc(func(context.Context, uint, time.Time, time.Time) (string, error) {
						return "", errors.New("test signing unavailable")
					}), time.Hour, nil)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					// Real engine triggers reject INSERT/UPDATE. For COMMIT, PG
					// defers a failing FK until COMMIT; MySQL kills the actual
					// connection only after the final nickname UPDATE succeeded.
					remove = realUserNativeFault(t, f, tc.table, tc.event, tc.mode)
				}
				principal, err := store.Login(context.Background(), claimed, config, auth.IssueRequest{Method: "oauth"})
				remove()
				if !errors.Is(err, auth.ErrUnavailable) || !reflect.DeepEqual(principal, auth.Principal{}) {
					t.Fatalf("failed login exposed principal or wrong classification: %v", err)
				}
				if tc.mode != "issuer" && !realUserObservedNativeFault(native, f.Engine, tc.mode) {
					t.Fatal("real SQL/COMMIT failure was not observed by the driver")
				}
				fresh := f.Reopen(t)
				oidcNicknameProfileUnchanged(t, before, oidcNicknameRead(t, fresh, user.Id), "")
				if transactionCount(t, fresh, &model.UserToken{}) != sessions || transactionCount(t, fresh, &model.LoginLog{}) != logs || transactionCount(t, fresh, &model.OIDCIdentity{}) != 1 {
					t.Fatal("failed transaction partially committed nickname/session/log/identity")
				}
				principal, err = oidcNicknameStore(t, fresh).Login(context.Background(), claimed, config, auth.IssueRequest{Method: "oauth"})
				if err != nil || principal.User.Nickname != "Trusted Real Name" || principal.User.ID != user.Id || !principal.User.IsAdmin {
					t.Fatal("retry did not atomically return the completed nickname and original identity/role", err)
				}
				oidcNicknameProfileUnchanged(t, before, oidcNicknameRead(t, f.Reopen(t), user.Id), "Trusted Real Name")
				if transactionCount(t, fresh, &model.UserToken{}) != sessions+1 || transactionCount(t, fresh, &model.LoginLog{}) != logs+1 {
					t.Fatal("successful retry did not persist exactly one session and login log")
				}
			})
		}
	})
}

func TestOIDCNicknameIndependentPoolsAndProvidersKeepFirstCommittedName(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		upgrade(t, f.DB, 281)
		_, config := identityConfig(t, f.DB)
		user := oidcNicknameUser(t, f.DB, config, "concurrent")
		enabled := true
		other := model.Oauth{Op: "other", OauthType: "oidc", Issuer: "https://other-identity.test", ClientId: "other-client",
			AutoRegister: &enabled, PkceEnable: &enabled, PkceMethod: "S256"}
		transactionCreate(t, f.DB, &other)
		otherConfig, err := oidcNicknameStore(t, f.Reopen(t)).Provider(context.Background(), other.Op)
		if err != nil {
			t.Fatal(err)
		}
		transactionCreate(t, f.DB, &model.OIDCIdentity{Issuer: otherConfig.Issuer, Subject: "concurrent", UserID: user.Id})
		var beforeLinks []model.OIDCIdentity
		if err := f.DB.Order("id").Find(&beforeLinks).Error; err != nil {
			t.Fatal(err)
		}
		const writers = 8
		type outcome struct {
			principal auth.Principal
			err       error
		}
		start := make(chan struct{})
		results := make(chan outcome, writers)
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			store := oidcNicknameStore(t, f.Reopen(t))
			selected := config
			if i%2 != 0 {
				selected = otherConfig
			}
			claimed := oidcNicknameIdentity(selected, "concurrent", fmt.Sprintf("OIDC name %d", i))
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				principal, err := store.Login(ctx, claimed, selected, auth.IssueRequest{Method: "oauth"})
				results <- outcome{principal, err}
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		persisted := oidcNicknameRead(t, f.Reopen(t), user.Id)
		if persisted.Nickname == "" {
			t.Fatal("no concurrent login completed the empty nickname")
		}
		tokens := map[string]bool{}
		for result := range results {
			if result.err != nil || result.principal.User.ID != user.Id || result.principal.User.Nickname != persisted.Nickname ||
				!result.principal.User.IsAdmin || result.principal.Session.Token == "" || tokens[result.principal.Session.Token] {
				t.Fatal("independent logins disagreed on the first committed nickname/identity or failed", result.err)
			}
			tokens[result.principal.Session.Token] = true
		}
		oidcNicknameProfileUnchanged(t, user, persisted, persisted.Nickname)
		var afterLinks []model.OIDCIdentity
		if err := f.Reopen(t).Order("id").Find(&afterLinks).Error; err != nil || !reflect.DeepEqual(beforeLinks, afterLinks) {
			t.Fatal("concurrent name completion changed external identity ownership", err)
		}
		if transactionCount(t, f.DB, &model.UserToken{}) != writers || transactionCount(t, f.DB, &model.LoginLog{}) != writers {
			t.Fatal("concurrent successful logins lost or duplicated sessions/logs")
		}
	})
}

func TestOIDCNicknameManualUpdateCompetitionAcrossIndependentConnections(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		upgrade(t, f.DB, 281)
		_, config := identityConfig(t, f.DB)
		for _, order := range []string{"manual-first", "oidc-first"} {
			t.Run(order, func(t *testing.T) {
				user := oidcNicknameUser(t, f.DB, config, order)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				loginDB, manualDB := f.Reopen(t), f.Reopen(t)
				claimed := oidcNicknameIdentity(config, order, "OIDC name")
				type outcome struct {
					principal auth.Principal
					err       error
				}
				loginResult := make(chan outcome, 1)
				startLogin := func() {
					store := oidcNicknameStore(t, loginDB)
					go func() {
						principal, err := store.Login(ctx, claimed, config, auth.IssueRequest{Method: "oauth"})
						loginResult <- outcome{principal, err}
					}()
				}
				wait := func(ch <-chan struct{}) {
					t.Helper()
					select {
					case <-ch:
					case <-ctx.Done():
						t.Fatal("deterministic race rendezvous timed out")
					}
				}
				wantResponse := "Manual nickname"
				if order == "manual-first" {
					manual := manualDB.WithContext(ctx).Begin()
					defer manual.Rollback()
					if manual.Error != nil || manual.Model(&model.User{}).Where("id = ?", user.Id).UpdateColumn("nickname", "Manual nickname").Error != nil {
						t.Fatal("manual transaction could not take the user row lock")
					}
					attempted := make(chan struct{})
					var once sync.Once
					if err := loginDB.Callback().Query().Before("gorm:query").Register("nickname:login-user-attempt", func(tx *gorm.DB) {
						if tx.Statement.Table == "users" {
							once.Do(func() { close(attempted) })
						}
					}); err != nil {
						t.Fatal(err)
					}
					defer loginDB.Callback().Query().Remove("nickname:login-user-attempt")
					startLogin()
					wait(attempted)
					if err := manual.Commit().Error; err != nil {
						t.Fatal(err)
					}
				} else {
					// At this successful login-log INSERT the production issuance
					// transaction already owns the user row lock, regardless of
					// whether name completion precedes or follows session writes.
					locked, release := make(chan struct{}), make(chan struct{})
					var once, releaseOnce sync.Once
					releaseLogin := func() { releaseOnce.Do(func() { close(release) }) }
					defer releaseLogin()
					if err := loginDB.Callback().Create().After("gorm:create").Register("nickname:hold-login-transaction", func(tx *gorm.DB) {
						if tx.Statement.Table == "login_logs" && tx.Error == nil {
							once.Do(func() {
								close(locked)
								select {
								case <-release:
								case <-ctx.Done():
									tx.AddError(ctx.Err())
								}
							})
						}
					}); err != nil {
						t.Fatal(err)
					}
					defer loginDB.Callback().Create().Remove("nickname:hold-login-transaction")
					attempted := make(chan struct{})
					if err := manualDB.Callback().Update().Before("gorm:update").Register("nickname:manual-update-attempt", func(tx *gorm.DB) { close(attempted) }); err != nil {
						t.Fatal(err)
					}
					defer manualDB.Callback().Update().Remove("nickname:manual-update-attempt")
					startLogin()
					wait(locked)
					manualResult := make(chan error, 1)
					go func() {
						manualResult <- manualDB.WithContext(ctx).Model(&model.User{}).Where("id = ?", user.Id).UpdateColumn("nickname", "Manual nickname").Error
					}()
					wait(attempted)
					releaseLogin()
					if err := <-manualResult; err != nil {
						t.Fatal(err)
					}
					wantResponse = "OIDC name"
				}
				result := <-loginResult
				if result.err != nil || result.principal.User.ID != user.Id || result.principal.User.Nickname != wantResponse {
					t.Fatal("login lost the nickname visible at its commit ordering", result.err)
				}
				oidcNicknameProfileUnchanged(t, user, oidcNicknameRead(t, f.Reopen(t), user.Id), "Manual nickname")
				// A later OIDC login must also preserve the winning manual edit.
				again, err := oidcNicknameStore(t, f.Reopen(t)).Login(ctx, claimed, config, auth.IssueRequest{})
				if err != nil || again.User.Nickname != "Manual nickname" {
					t.Fatal("subsequent login replaced the committed manual nickname", err)
				}
			})
		}
	})
}
