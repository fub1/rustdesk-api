package useradmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/utils"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type fixture struct {
	db           *gorm.DB
	s            *Store
	now          time.Time
	actor, other auth.Principal
	target       model.User
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func newFixture(t *testing.T) fixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "users.db")+"?_txlock=immediate&_busy_timeout=5000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	must(t, err)
	sqlDB, err := db.DB()
	must(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	must(t, database.Update(db, 281, nil))
	now := time.Now().UTC().Truncate(time.Second)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	must(t, err)
	hash, err := utils.EncryptPassword("old-password")
	must(t, err)
	create := func(name string, admin bool) auth.Principal {
		user := model.User{Username: name, Password: hash, IsAdmin: &admin, Status: model.COMMON_STATUS_ENABLE, Nickname: "original", GroupId: 1}
		must(t, db.Create(&user).Error)
		token := model.UserToken{UserId: user.Id, Token: name + "-session", ExpiredAt: now.Add(time.Hour).Unix()}
		must(t, db.Create(&token).Error)
		return auth.Principal{User: auth.User{ID: user.Id, IsAdmin: admin, Enabled: true, PasswordHash: hash}, Session: auth.Session{ID: token.Id, UserID: user.Id, Token: token.Token, ExpiresAt: time.Unix(token.ExpiredAt, 0).UTC()}}
	}
	actor, other := create("actor", true), create("other-admin", true)
	target := model.User{Username: "target", Password: hash, Status: model.COMMON_STATUS_ENABLE, GroupId: 1}
	must(t, db.Create(&target).Error)
	for _, exp := range []int64{now.Add(time.Hour).Unix(), now.Add(-time.Hour).Unix()} {
		token := model.UserToken{UserId: target.Id, Token: fmt.Sprintf("target-%d", exp), ExpiredAt: exp}
		must(t, db.Create(&token).Error)
	}
	return fixture{db, s, now, actor, other, target}
}
func user(t *testing.T, f fixture, id uint) model.User {
	t.Helper()
	var row model.User
	must(t, f.db.First(&row, id).Error)
	return row
}
func tokenCount(t *testing.T, f fixture, id uint) int64 {
	t.Helper()
	var n int64
	must(t, f.db.Model(&model.UserToken{}).Where("user_id = ?", id).Count(&n).Error)
	return n
}
func snapshot(t *testing.T, f fixture) []any {
	t.Helper()
	var users []model.User
	var tokens []model.UserToken
	for _, out := range []any{&users, &tokens} {
		must(t, f.db.Order("id ASC").Find(out).Error)
	}
	return []any{users, tokens}
}

func TestCreateRejectsUsernameThatNormalizesToEmpty(t *testing.T) {
	f := newFixture(t)
	before := snapshot(t, f)
	if err := f.s.Create(context.Background(), f.actor, UserInput{Username: "   ", GroupID: 1, Status: model.COMMON_STATUS_ENABLE}); !errors.Is(err, ErrInput) {
		t.Fatal("empty normalized account name accepted", err)
	}
	if !reflect.DeepEqual(before, snapshot(t, f)) {
		t.Fatal("invalid account name changed persisted users or sessions")
	}
}

func TestUserWritesRejectStaleActorAndAllowSameRequestAfterRestore(t *testing.T) {
	for _, action := range []string{"create", "update", "delete", "reset", "self-password"} {
		for _, drift := range []string{"revoke", "disabled", "demoted", "stored-expiry", "signed-expiry", "duplicate"} {
			if action == "self-password" && drift == "demoted" {
				continue
			} // self password requires current user, not admin.
			t.Run(action+"/"+drift, func(t *testing.T) {
				f := newFixture(t)
				actor := f.actor
				call := func() error {
					switch action {
					case "create":
						return f.s.Create(context.Background(), actor, UserInput{Username: "New User", Status: model.COMMON_STATUS_ENABLE, GroupID: 1})
					case "update":
						return f.s.Update(context.Background(), actor, UserInput{ID: f.target.Id, Nickname: "changed"})
					case "delete":
						return f.s.Delete(context.Background(), actor, f.target.Id)
					case "reset":
						return f.s.ResetPassword(context.Background(), actor, PasswordInput{ID: f.target.Id, Password: "new-password"})
					default:
						return f.s.ChangePassword(context.Background(), actor, "old-password", "new-password")
					}
				}
				var restore func()
				switch drift {
				case "revoke":
					var row model.UserToken
					must(t, f.db.First(&row, actor.Session.ID).Error)
					must(t, f.db.Delete(&row).Error)
					restore = func() { must(t, f.db.Create(&row).Error) }
				case "disabled":
					must(t, f.db.Model(&model.User{}).Where("id=?", actor.User.ID).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error)
					restore = func() {
						must(t, f.db.Model(&model.User{}).Where("id=?", actor.User.ID).UpdateColumn("status", model.COMMON_STATUS_ENABLE).Error)
					}
				case "demoted":
					must(t, f.db.Model(&model.User{}).Where("id=?", actor.User.ID).UpdateColumn("is_admin", false).Error)
					restore = func() {
						must(t, f.db.Model(&model.User{}).Where("id=?", actor.User.ID).UpdateColumn("is_admin", true).Error)
					}
				case "stored-expiry":
					must(t, f.db.Model(&model.UserToken{}).Where("id=?", actor.Session.ID).UpdateColumn("expired_at", f.now.Unix()).Error)
					restore = func() {
						must(t, f.db.Model(&model.UserToken{}).Where("id=?", actor.Session.ID).UpdateColumn("expired_at", actor.Session.ExpiresAt.Unix()).Error)
					}
				case "signed-expiry":
					actor.Session.ExpiresAt = f.now
					restore = func() { actor = f.actor }
				case "duplicate":
					dup := model.UserToken{UserId: f.other.User.ID, Token: actor.Session.Token, ExpiredAt: actor.Session.ExpiresAt.Unix()}
					must(t, f.db.Create(&dup).Error)
					restore = func() { must(t, f.db.Delete(&dup).Error) }
				}
				before := snapshot(t, f)
				err := call()
				want := auth.ErrUnauthenticated
				if drift == "demoted" {
					want = auth.ErrForbidden
				}
				if !errors.Is(err, want) {
					t.Fatalf("unexpected stale actor classification %v", err)
				}
				if !reflect.DeepEqual(before, snapshot(t, f)) {
					t.Fatal("stale actor changed business state")
				}
				restore()
				must(t, call())
				switch action {
				case "create":
					var created model.User
					must(t, f.db.Where("username=?", "newuser").First(&created).Error)
					ok, _, err := utils.VerifyPassword(created.Password, utils.DefaultInitPassword)
					if err != nil || !ok {
						t.Fatal("positive create missing valid initial hash")
					}
				case "update":
					if user(t, f, f.target.Id).Nickname != "changed" {
						t.Fatal("positive update missing")
					}
				case "delete":
					var n int64
					must(t, f.db.Model(&model.User{}).Where("id=?", f.target.Id).Count(&n).Error)
					if n != 0 || tokenCount(t, f, f.target.Id) != 0 {
						t.Fatal("positive delete incomplete")
					}
				case "reset", "self-password":
					id := f.target.Id
					if action == "self-password" {
						id = actor.User.ID
					}
					updated := user(t, f, id)
					ok, _, err := utils.VerifyPassword(updated.Password, "new-password")
					if err != nil || !ok || tokenCount(t, f, id) != 0 {
						t.Fatal("positive password/reset did not update hash and revoke")
					}
				}
			})
		}
	}
}

func TestLastEnabledAdminAndNullablePatch(t *testing.T) {
	for _, action := range []string{"delete", "disable", "status3", "demote"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			must(t, f.db.Model(&model.User{}).Where("id=?", f.other.User.ID).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error)
			before := snapshot(t, f)
			var err error
			if action == "delete" {
				err = f.s.Delete(context.Background(), f.actor, f.actor.User.ID)
			} else {
				in := UserInput{ID: f.actor.User.ID}
				switch action {
				case "disable":
					in.Status = model.COMMON_STATUS_DISABLED
				case "status3":
					in.Status = model.StatusCode(3)
				case "demote":
					value := false
					in.IsAdmin = &value
				}
				err = f.s.Update(context.Background(), f.actor, in)
			}
			if !errors.Is(err, ErrLastEnabledAdmin) || !reflect.DeepEqual(before, snapshot(t, f)) {
				t.Fatal("last enabled admin removal accepted or changed rows", err)
			}
			must(t, f.s.Update(context.Background(), f.actor, UserInput{ID: f.actor.User.ID, Nickname: "metadata-only"}))
			row := user(t, f, f.actor.User.ID)
			if row.IsAdmin == nil || !*row.IsAdmin || row.Nickname != "metadata-only" {
				t.Fatal("nil role patch changed role or refused metadata")
			}
		})
	}
}

func TestApprovedSelfActionsAndDisableReenable(t *testing.T) {
	for _, action := range []string{"delete", "disable", "status3", "demote"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			switch action {
			case "delete":
				must(t, f.s.Delete(context.Background(), f.actor, f.actor.User.ID))
			case "disable", "status3":
				status := model.COMMON_STATUS_DISABLED
				if action == "status3" {
					status = model.StatusCode(3)
				}
				must(t, f.s.Update(context.Background(), f.actor, UserInput{ID: f.actor.User.ID, Status: status}))
				if tokenCount(t, f, f.actor.User.ID) != 0 {
					t.Fatal("self disable did not revoke")
				}
				must(t, f.s.Update(context.Background(), f.other, UserInput{ID: f.actor.User.ID, Status: model.COMMON_STATUS_ENABLE}))
				if tokenCount(t, f, f.actor.User.ID) != 0 {
					t.Fatal("re-enable restored old session")
				}
				if err := f.s.Update(context.Background(), f.actor, UserInput{ID: f.target.Id, Nickname: "stale"}); !errors.Is(err, auth.ErrUnauthenticated) {
					t.Fatal("old actor regained session after re-enable", err)
				}
			case "demote":
				no := false
				must(t, f.s.Update(context.Background(), f.actor, UserInput{ID: f.actor.User.ID, IsAdmin: &no}))
				if row := user(t, f, f.actor.User.ID); row.IsAdmin == nil || *row.IsAdmin {
					t.Fatal("self demote did not persist")
				}
			}
		})
	}
}

func TestHistoricalInactiveReenableRevokesOldSessionsAndAllowsFreshLogin(t *testing.T) {
	for _, status := range []model.StatusCode{model.COMMON_STATUS_DISABLED, model.StatusCode(3)} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFixture(t)
			// Model an upgrade-era inactive account whose old session was never
			// revoked, rather than relying on the new disable write to clean it.
			must(t, f.db.Model(&model.User{}).Where("id=?", f.target.Id).UpdateColumn("status", status).Error)
			sessions, err := app.NewSessions(f.db, time.Hour, nil)
			must(t, err)
			oldToken := fmt.Sprintf("target-%d", f.now.Add(time.Hour).Unix())
			if _, err := sessions.Authenticate(context.Background(), oldToken); !errors.Is(err, auth.ErrDisabled) {
				t.Fatal("historical fixture did not retain a disabled account's session", err)
			}
			must(t, f.s.Update(context.Background(), f.actor, UserInput{ID: f.target.Id, Status: model.COMMON_STATUS_ENABLE, Nickname: "enabled-again"}))
			if row := user(t, f, f.target.Id); row.Status != model.COMMON_STATUS_ENABLE || row.Nickname != "enabled-again" {
				t.Fatal("reenable did not commit account metadata")
			}
			if _, err := sessions.Authenticate(context.Background(), oldToken); !errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatal("reenable revived an upgrade-era old session", err)
			}
			if tokenCount(t, f, f.target.Id) != 0 {
				t.Fatal("reenable retained historical active or expired sessions")
			}
			issuer, err := app.NewIssuer(f.db, time.Hour, nil)
			must(t, err)
			expected := f.target.Password
			fresh, err := issuer.Issue(context.Background(), auth.IssueRequest{UserID: f.target.Id, ExpectedPassword: &expected, Client: model.LoginLogClientWebAdmin, Method: model.LoginLogTypeAccount})
			must(t, err)
			if authenticated, err := sessions.Authenticate(context.Background(), fresh.Session.Token); err != nil || authenticated.User.ID != f.target.Id || !authenticated.User.Enabled {
				t.Fatal("reenabled account could not use a freshly issued session", err)
			}
			must(t, f.s.Update(context.Background(), f.actor, UserInput{ID: f.target.Id, Nickname: "enabled-metadata"}))
			if _, err := sessions.Authenticate(context.Background(), fresh.Session.Token); err != nil || tokenCount(t, f, f.target.Id) != 1 {
				t.Fatal("ordinary enabled account metadata update revoked a valid fresh session", err)
			}
		})
	}
}

func TestDisableAndPasswordLateFailureRollBackAndRetry(t *testing.T) {
	for _, action := range []string{"disable", "reenable", "reset", "self-password"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			if action == "reenable" {
				must(t, f.db.Model(&model.User{}).Where("id=?", f.target.Id).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error)
			}
			must(t, f.db.Exec("CREATE TRIGGER fail_user_session BEFORE DELETE ON user_tokens BEGIN SELECT RAISE(ABORT, 'private session fault'); END").Error)
			call := func() error {
				switch action {
				case "disable":
					return f.s.Update(context.Background(), f.actor, UserInput{ID: f.target.Id, Status: model.COMMON_STATUS_DISABLED, Nickname: "new"})
				case "reenable":
					return f.s.Update(context.Background(), f.actor, UserInput{ID: f.target.Id, Status: model.COMMON_STATUS_ENABLE, Nickname: "new"})
				case "reset":
					return f.s.ResetPassword(context.Background(), f.actor, PasswordInput{ID: f.target.Id, Password: "new-password"})
				default:
					return f.s.ChangePassword(context.Background(), f.actor, "old-password", "new-password")
				}
			}
			before := snapshot(t, f)
			err := call()
			if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "private") || !reflect.DeepEqual(before, snapshot(t, f)) {
				t.Fatal("late revoke failure retained partial write or leaked cause", err)
			}
			must(t, f.db.Exec("DROP TRIGGER fail_user_session").Error)
			must(t, call())
			id := f.target.Id
			if action == "self-password" {
				id = f.actor.User.ID
			}
			if tokenCount(t, f, id) != 0 {
				t.Fatal("explicit retry did not revoke")
			}
		})
	}
}

type failedCommitPool struct {
	gorm.ConnPool
	db      *sql.DB
	reached *bool
}

func (p failedCommitPool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.db.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &failedCommitTx{Tx: tx, reached: p.reached}, nil
}

type failedCommitTx struct {
	*sql.Tx
	reached *bool
}

func (t failedCommitTx) Commit() error {
	*t.reached = true
	_ = t.Tx.Rollback()
	return errors.New("private actual COMMIT failure")
}

func TestAllUserWritesActualCommitFailureRollsBackAndRetry(t *testing.T) {
	for _, action := range []string{"create", "disable", "reenable", "delete", "reset", "self-password"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			if action == "reenable" {
				must(t, f.db.Model(&model.User{}).Where("id=?", f.target.Id).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error)
			}
			pool, err := f.db.DB()
			must(t, err)
			reached := false
			fault, err := gorm.Open(sqlite.Dialector{Conn: failedCommitPool{ConnPool: pool, db: pool, reached: &reached}}, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			must(t, err)
			f.s, err = New(fault, Options{Now: func() time.Time { return f.now }})
			must(t, err)
			call := func() error {
				switch action {
				case "create":
					return f.s.Create(context.Background(), f.actor, UserInput{Username: "commit-user", Status: model.COMMON_STATUS_ENABLE, GroupID: 1})
				case "disable":
					return f.s.Update(context.Background(), f.actor, UserInput{ID: f.target.Id, Status: model.COMMON_STATUS_DISABLED, Nickname: "committed"})
				case "reenable":
					return f.s.Update(context.Background(), f.actor, UserInput{ID: f.target.Id, Status: model.COMMON_STATUS_ENABLE, Nickname: "committed"})
				case "delete":
					return f.s.Delete(context.Background(), f.actor, f.target.Id)
				case "reset":
					return f.s.ResetPassword(context.Background(), f.actor, PasswordInput{ID: f.target.Id, Password: "new-password"})
				default:
					return f.s.ChangePassword(context.Background(), f.actor, "old-password", "new-password")
				}
			}
			before := snapshot(t, f)
			err = call()
			if !reached || !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "private") || !reflect.DeepEqual(before, snapshot(t, f)) {
				t.Fatal("actual COMMIT fault not reached, not rolled back or cause leaked", err, reached)
			}
			f.s, err = New(f.db, Options{Now: func() time.Time { return f.now }})
			must(t, err)
			must(t, call())
			switch action {
			case "create":
				var row model.User
				must(t, f.db.Where("username=?", "commit-user").First(&row).Error)
				if row.Password == "" {
					t.Fatal("retry did not create hash")
				}
			case "disable":
				row := user(t, f, f.target.Id)
				if row.Status != model.COMMON_STATUS_DISABLED || row.Nickname != "committed" || tokenCount(t, f, f.target.Id) != 0 {
					t.Fatal("retry did not commit disable+revocation")
				}
			case "reenable":
				row := user(t, f, f.target.Id)
				if row.Status != model.COMMON_STATUS_ENABLE || row.Nickname != "committed" || tokenCount(t, f, f.target.Id) != 0 {
					t.Fatal("retry did not commit reenable+revocation")
				}
			case "delete":
				var n int64
				must(t, f.db.Model(&model.User{}).Where("id=?", f.target.Id).Count(&n).Error)
				if n != 0 || tokenCount(t, f, f.target.Id) != 0 {
					t.Fatal("retry did not commit deletion")
				}
			case "reset", "self-password":
				id := f.target.Id
				if action == "self-password" {
					id = f.actor.User.ID
				}
				row := user(t, f, id)
				ok, _, err := utils.VerifyPassword(row.Password, "new-password")
				if err != nil || !ok || tokenCount(t, f, id) != 0 {
					t.Fatal("retry did not commit password+revocation")
				}
			}
		})
	}
}
