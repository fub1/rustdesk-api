package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/authstore"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionadmin"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionguard"
	"github.com/lejianwen/rustdesk-api/v2/internal/useradmin"
	"github.com/lejianwen/rustdesk-api/v2/internal/userguard"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"github.com/lejianwen/rustdesk-api/v2/utils"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func realUserStore(t *testing.T, db *gorm.DB) *useradmin.Store {
	t.Helper()
	s, err := useradmin.New(db, useradmin.Options{})
	realBookMust(t, err)
	return s
}

func realUserSetup(t *testing.T, f *fixture) (auth.Principal, auth.Principal) {
	t.Helper()
	upgrade(t, f.DB, 281)
	// The seed administrator must not accidentally satisfy a two-admin test.
	realBookMust(t, f.DB.Model(&model.User{}).Where("id > 0").UpdateColumn("is_admin", false).Error)
	a := definitionAdmin(t, f.DB, "user-a")
	b := definitionAdmin(t, f.DB, "user-b")
	return a, b
}

// Capture physical columns, including nullable/raw JSON fields and timestamps.
// Sequence counters are not transactional facts and are intentionally excluded.
func realUserRawSnapshot(t *testing.T, db *gorm.DB) map[string][]map[string]any {
	t.Helper()
	result := map[string][]map[string]any{}
	for _, table := range []string{"users", "user_tokens", "oidc_identities", "user_thirds", "login_logs", "peers", "address_book_collections", "address_books", "tags", "address_book_collection_rules"} {
		key := "id"
		if table == "peers" || table == "address_books" {
			key = "row_id"
		}
		rows := []map[string]any{}
		realBookMust(t, db.Table(table).Order(key).Find(&rows).Error)
		result[table] = rows
	}
	return result
}
func realUserUnchanged(t *testing.T, f *fixture, before map[string][]map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(before, realUserRawSnapshot(t, f.Reopen(t))) {
		t.Fatal("failed user mutation changed physical persisted facts")
	}
}

func realUserRead(t *testing.T, db *gorm.DB, id uint) model.User {
	t.Helper()
	var row model.User
	realBookMust(t, db.First(&row, id).Error)
	return row
}
func realUserSession(t *testing.T, db *gorm.DB, id uint) model.UserToken {
	t.Helper()
	var row model.UserToken
	realBookMust(t, db.First(&row, id).Error)
	return row
}
func realUserOperation(s *useradmin.Store, kind string, actor, target auth.Principal) func(context.Context) error {
	return func(ctx context.Context) error {
		switch kind {
		case "create":
			return s.Create(ctx, actor, useradmin.UserInput{Username: "created-user", Nickname: "created", Status: model.COMMON_STATUS_ENABLE})
		case "update":
			return s.Update(ctx, actor, useradmin.UserInput{ID: target.User.ID, Nickname: "updated-user"})
		case "delete":
			return s.Delete(ctx, actor, target.User.ID)
		case "disable":
			return s.Update(ctx, actor, useradmin.UserInput{ID: target.User.ID, Status: model.COMMON_STATUS_DISABLED})
		case "demote":
			no := false
			return s.Update(ctx, actor, useradmin.UserInput{ID: target.User.ID, IsAdmin: &no})
		case "reset":
			return s.ResetPassword(ctx, actor, useradmin.PasswordInput{ID: target.User.ID, Password: "replacement-password"})
		case "self-password":
			return s.ChangePassword(ctx, actor, "", "replacement-password")
		}
		return errors.New("unknown integration operation")
	}
}
func realUserPositive(t *testing.T, db *gorm.DB, kind string, actor, target auth.Principal) {
	t.Helper()
	switch kind {
	case "create":
		var row model.User
		realBookMust(t, db.Where("username=?", "created-user").First(&row).Error)
		if row.Nickname != "created" || row.Password == "" {
			t.Fatal("valid create did not commit intended account")
		}
	case "delete":
		var n int64
		realBookMust(t, db.Model(&model.User{}).Where("id=?", target.User.ID).Count(&n).Error)
		if n != 0 {
			t.Fatal("successful delete retained target")
		}
	case "update":
		if realUserRead(t, db, target.User.ID).Nickname != "updated-user" {
			t.Fatal("valid update did not change intended account")
		}
	case "disable", "demote":
		row := realUserRead(t, db, target.User.ID)
		if kind == "disable" && row.Status != model.COMMON_STATUS_DISABLED || kind == "demote" && (row.IsAdmin == nil || *row.IsAdmin) {
			t.Fatal("successful role/status operation did not commit")
		}
	case "reset", "self-password":
		id := target.User.ID
		if kind == "self-password" {
			id = actor.User.ID
		}
		row := realUserRead(t, db, id)
		ok, _, err := utils.VerifyPassword(row.Password, "replacement-password")
		if err != nil || !ok {
			t.Fatal("password did not commit expected hash")
		}
	}
	if kind == "disable" || kind == "reset" || kind == "self-password" || kind == "delete" {
		id := target.User.ID
		if kind == "self-password" {
			id = actor.User.ID
		}
		var n int64
		realBookMust(t, db.Model(&model.UserToken{}).Where("user_id=?", id).Count(&n).Error)
		if n != 0 {
			t.Fatal("successful removal/disable/password operation retained target sessions")
		}
	}
}

func TestRealUserManagementCurrentActorAfterNativeWait(t *testing.T) {
	for _, kind := range []string{"create", "update", "delete", "reset", "self-password"} {
		t.Run(kind, func(t *testing.T) {
			for _, change := range []string{"revoke", "disable", "demote", "expire"} {
				if kind == "self-password" && change == "demote" {
					continue // Ordinary users retain the existing self-password right.
				}
				t.Run(change, func(t *testing.T) {
					forEngines(t, func(t *testing.T, f *fixture) {
						a, b := realUserSetup(t, f)
						originalUser, originalSession := realUserRead(t, f.DB, a.User.ID), realUserSession(t, f.DB, a.Session.ID)
						ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
						defer cancel()
						hold := f.Reopen(t).WithContext(ctx).Begin(&sql.TxOptions{Isolation: sql.LevelReadCommitted})
						realBookMust(t, hold.Error)
						defer hold.Rollback()
						var user model.User
						realBookMust(t, hold.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, a.User.ID).Error)
						done := make(chan error, 1)
						op := realUserOperation(realUserStore(t, f.Reopen(t)), kind, a, b)
						go func() { done <- op(ctx) }()
						realBookNativeWait(t, f, ctx, "users")
						switch change {
						case "revoke":
							realBookMust(t, hold.Delete(&model.UserToken{}, a.Session.ID).Error)
						case "disable":
							realBookMust(t, hold.Model(&user).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error)
						case "demote":
							realBookMust(t, hold.Model(&user).UpdateColumn("is_admin", false).Error)
						case "expire":
							realBookMust(t, hold.Model(&model.UserToken{}).Where("id=?", a.Session.ID).UpdateColumn("expired_at", time.Now().Add(-time.Minute).Unix()).Error)
						}
						before := realUserRawSnapshot(t, hold)
						realBookMust(t, hold.Commit().Error)
						err := realBookResult(t, ctx, done)
						want := auth.ErrUnauthenticated
						if change == "demote" {
							want = auth.ErrForbidden
						}
						if !errors.Is(err, want) {
							t.Fatal("stale actor succeeded after real user-row wait", err)
						}
						realUserUnchanged(t, f, before)
						// Restore the same signed principal and replay the same valid input.
						realBookMust(t, f.DB.Model(&model.User{}).Where("id=?", a.User.ID).Updates(map[string]any{"status": originalUser.Status, "is_admin": *originalUser.IsAdmin}).Error)
						if change == "revoke" {
							realBookMust(t, f.DB.Create(&originalSession).Error)
						} else {
							realBookMust(t, f.DB.Model(&model.UserToken{}).Where("id=?", a.Session.ID).UpdateColumn("expired_at", originalSession.ExpiredAt).Error)
						}
						realBookMust(t, op(ctx))
						realUserPositive(t, f.Reopen(t), kind, a, b)
					})
				})
			}
		})
	}
}

func TestRealUserManagementLastEnabledAdminBothOrders(t *testing.T) {
	for _, pair := range [][2]string{{"delete", "delete"}, {"demote", "demote"}, {"disable", "demote"}, {"delete", "demote"}} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-%s/reverse-%t", pair[0], pair[1], reverse), func(t *testing.T) {
				forEngines(t, func(t *testing.T, f *fixture) {
					a, b := realUserSetup(t, f)
					disabled := definitionAdmin(t, f.DB, "disabled-not-usable")
					realBookMust(t, f.DB.Model(&model.User{}).Where("id=?", disabled.User.ID).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error)
					firstKind, secondKind := pair[0], pair[1]
					if reverse {
						firstKind, secondKind = secondKind, firstKind
						a, b = b, a
					}
					lead, follow := f.Reopen(t), f.Reopen(t)
					held, release := make(chan struct{}), make(chan struct{})
					var once sync.Once
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					defer func() { once.Do(func() { close(release) }) }()
					var heldOnce sync.Once
					realBookMust(t, lead.Callback().Query().After("gorm:query").Register("user:hold-first-admin", func(tx *gorm.DB) {
						if realBookIsLockedTable(tx, "users") && tx.Error == nil {
							heldOnce.Do(func() {
								close(held)
								select {
								case <-release:
								case <-ctx.Done():
									tx.AddError(ctx.Err())
								}
							})
						}
					}))
					first, second := make(chan error, 1), make(chan error, 1)
					firstOp := realUserOperation(realUserStore(t, lead), firstKind, a, a)
					secondOp := realUserOperation(realUserStore(t, follow), secondKind, b, b)
					go func() { first <- firstOp(ctx) }()
					realBookWait(t, ctx, held, "leading account mutation did not hold administrator anchor")
					go func() { second <- secondOp(ctx) }()
					realBookNativeWait(t, f, ctx, "users")
					once.Do(func() { close(release) })
					realBookMust(t, realBookResult(t, ctx, first))
					if err := realBookResult(t, ctx, second); !errors.Is(err, useradmin.ErrLastEnabledAdmin) {
						t.Fatal("concurrent last enabled administrator removal did not fail closed", err)
					}
					realUserPositive(t, f.Reopen(t), firstKind, a, a)
					var count int64
					realBookMust(t, f.DB.Model(&model.User{}).Where("is_admin=? AND status=?", true, model.COMMON_STATUS_ENABLE).Count(&count).Error)
					if count != 1 || !userguard.EnabledAdmin(realUserRead(t, f.DB, b.User.ID)) {
						t.Fatal("disabled administrator counted as usable or protected loser changed")
					}
					if realUserSession(t, f.DB, b.Session.ID).Token != b.Session.Token {
						t.Fatal("last administrator rejection revoked protected session")
					}
				})
			})
		}
	}
}

func TestRealUserManagementLegacyWrappersAndCanonicalUserLocks(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		a, b := realUserSetup(t, f)
		// LDAP sync uses a zero-value UserService. Capture its fallback DB before
		// workers start, never switch globals while a worker is running.
		old := service.DB
		service.DB = f.DB
		defer func() { service.DB = old }()
		legacy := &service.UserService{}
		realBookMust(t, f.DB.Model(&model.User{}).Where("id=?", b.User.ID).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error)
		before := realUserRawSnapshot(t, f.DB)
		no := false
		if err := legacy.Update(&model.User{IdModel: model.IdModel{Id: a.User.ID}, IsAdmin: &no}); !errors.Is(err, userguard.ErrLastEnabledAdmin) {
			t.Fatal("LDAP-compatible zero-value update bypassed last enabled administrator", err)
		}
		realUserUnchanged(t, f, before)
		if err := legacy.Delete(&model.User{IdModel: model.IdModel{Id: a.User.ID}}); err == nil {
			t.Fatal("legacy deletion bypassed last enabled administrator")
		}
		realUserUnchanged(t, f, before)
		// Omitted is_admin preserves sole administrator and permits metadata edits.
		realBookMust(t, legacy.Update(&model.User{IdModel: model.IdModel{Id: a.User.ID}, Nickname: "safe-metadata"}))
		if got := realUserRead(t, f.DB, a.User.ID); !userguard.EnabledAdmin(got) || got.Nickname != "safe-metadata" {
			t.Fatal("legacy effective patch lost administrator role")
		}
		var locked []uint
		seenSession := false
		realBookMust(t, f.DB.Callback().Query().After("gorm:query").Register("user:canonical-locks", func(tx *gorm.DB) {
			if tx.Error != nil {
				return
			}
			if realBookIsLockedTable(tx, "users") {
				if row, ok := tx.Statement.Dest.(*model.User); ok && row.Id != 0 {
					if seenSession || len(locked) > 0 && locked[len(locked)-1] >= row.Id {
						tx.AddError(errors.New("non-canonical user lock"))
					}
					locked = append(locked, row.Id)
				}
			}
			if realBookIsLockedTable(tx, "user_tokens") {
				seenSession = true
			}
		}))
		realBookMust(t, realUserStore(t, f.DB).Update(context.Background(), a, useradmin.UserInput{ID: b.User.ID, Nickname: "target-updated"}))
		realBookMust(t, f.DB.Callback().Query().Remove("user:canonical-locks"))
		if len(locked) != 2 || !seenSession {
			t.Fatal("account mutation did not prove all administrator/actor/target user locks before actor session")
		}
		// Re-enabling a disabled account never resurrects sessions removed by a
		// guarded disable, including when using the legacy wrapper.
		realBookMust(t, legacy.Update(&model.User{IdModel: model.IdModel{Id: b.User.ID}, Status: model.COMMON_STATUS_ENABLE}))
		// The earlier metadata update of an already-disabled target correctly
		// revoked its pre-existing session. Seed three fresh legacy facts so this
		// observer measures group ordering without assuming that session survived.
		for _, token := range []string{"z-target-extra", "middle-target-extra", "a-target-extra"} {
			transactionCreate(t, f.DB, &model.UserToken{UserId: b.User.ID, Token: token, ExpiredAt: time.Now().Add(time.Hour).Unix()})
		}
		var tokenGroups []string
		realBookMust(t, f.DB.Callback().Query().After("gorm:query").Register("user:sorted-token-groups", func(tx *gorm.DB) {
			if !realBookIsLockedTable(tx, "user_tokens") || tx.Error != nil {
				return
			}
			if rows, ok := tx.Statement.Dest.(*[]model.UserToken); ok && len(*rows) > 0 {
				tokenGroups = append(tokenGroups, (*rows)[0].Token)
			}
		}))
		realBookMust(t, legacy.Update(&model.User{IdModel: model.IdModel{Id: b.User.ID}, Status: model.COMMON_STATUS_DISABLED}))
		realBookMust(t, f.DB.Callback().Query().Remove("user:sorted-token-groups"))
		if len(tokenGroups) != 3 || !sort.StringsAreSorted(tokenGroups) {
			t.Fatal("target token groups were skipped or not locked in byte order")
		}
		realUserPositive(t, f.DB, "disable", a, b)
		realBookMust(t, legacy.Update(&model.User{IdModel: model.IdModel{Id: b.User.ID}, Status: model.COMMON_STATUS_ENABLE}))
		var n int64
		realBookMust(t, f.DB.Model(&model.UserToken{}).Where("user_id=?", b.User.ID).Count(&n).Error)
		if n != 0 {
			t.Fatal("re-enable resurrected old local sessions")
		}
	})
}

// Native fault helpers use test-only triggers; runtime schema remains version281.
func realUserNativeFault(t *testing.T, f *fixture, table, event, mode string) func() {
	t.Helper()
	callbackName := "user:final-native-fault"
	var reached, killed atomic.Bool
	callback := func(tx *gorm.DB) {
		if tx.Statement.Table != table || tx.Error != nil || tx.RowsAffected != 1 {
			return
		}
		reached.Store(true)
		if f.Engine != "mysql" || mode != "commit" || !killed.CompareAndSwap(false, true) {
			return
		}
		var id uint64
		if err := tx.Statement.ConnPool.QueryRowContext(tx.Statement.Context, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
			tx.AddError(err)
			return
		}
		if err := f.Reopen(t).Exec(fmt.Sprintf("KILL CONNECTION %d", id)).Error; err != nil {
			tx.AddError(err)
		}
	}
	removeCallback := func() {}
	if mode == "commit" {
		if event == "UPDATE" {
			realBookMust(t, f.DB.Callback().Update().After("gorm:update").Register(callbackName, callback))
			removeCallback = func() { realBookMust(t, f.DB.Callback().Update().Remove(callbackName)) }
		} else {
			realBookMust(t, f.DB.Callback().Delete().After("gorm:delete").Register(callbackName, callback))
			removeCallback = func() { realBookMust(t, f.DB.Callback().Delete().Remove(callbackName)) }
		}
	}
	if f.Engine == "mysql" {
		if mode == "sql" {
			transactionExec(t, f.DB, "CREATE TRIGGER user_reject_final BEFORE "+event+" ON "+table+" FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='private user fixture cause'")
		}
		return func() {
			removeCallback()
			if mode == "sql" {
				transactionExec(t, f.DB, "DROP TRIGGER user_reject_final")
			} else if !reached.Load() || !killed.Load() {
				t.Error("native final successful statement was never followed by connection kill")
			}
		}
	}
	returnRow := "NEW"
	if event == "DELETE" {
		returnRow = "OLD"
	}
	if mode == "commit" {
		transactionExec(t, f.DB, "CREATE TABLE user_commit_parent(id bigint PRIMARY KEY)")
		transactionExec(t, f.DB, "CREATE TABLE user_commit_guard(parent_id bigint REFERENCES user_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)")
		transactionExec(t, f.DB, "CREATE FUNCTION user_reject_final() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO user_commit_guard(parent_id) VALUES(999999); RETURN "+returnRow+"; END $$")
	} else {
		transactionExec(t, f.DB, "CREATE FUNCTION user_reject_final() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'private user fixture cause'; END $$")
	}
	timing := "BEFORE"
	if mode == "commit" {
		timing = "AFTER"
	}
	transactionExec(t, f.DB, "CREATE TRIGGER user_reject_final "+timing+" "+event+" ON "+table+" FOR EACH ROW EXECUTE FUNCTION user_reject_final()")
	return func() {
		removeCallback()
		if mode == "commit" && !reached.Load() {
			t.Error("deferred COMMIT failure did not follow a successful final statement")
		}
		transactionExec(t, f.DB, "DROP TRIGGER user_reject_final ON "+table)
		transactionExec(t, f.DB, "DROP FUNCTION user_reject_final()")
		if mode == "commit" {
			transactionExec(t, f.DB, "DROP TABLE user_commit_guard")
			transactionExec(t, f.DB, "DROP TABLE user_commit_parent")
		}
	}
}

func realUserObservedNativeFault(observed *transactionErrors, engine, mode string) bool {
	observed.mu.Lock()
	defer observed.mu.Unlock()
	for _, err := range observed.values {
		if engine == "mysql" {
			var native *mysqldriver.MySQLError
			if errors.As(err, &native) && mode == "sql" && native.Number == 1644 {
				return true
			}
			// The successful final statement and KILL are independently asserted
			// by the helper. The wrapped real Commit must report a native failure.
			if mode == "commit" && err != nil {
				return true
			}
		} else {
			var native interface{ SQLState() string }
			if errors.As(err, &native) && (mode == "sql" && native.SQLState() == "P0001" || mode == "commit" && native.SQLState() == "23503") {
				return true
			}
		}
	}
	return false
}

func TestRealUserManagementNativeSQLAndCommitRollback(t *testing.T) {
	for _, kind := range []string{"update", "delete", "disable", "reset", "self-password"} {
		for _, mode := range []string{"sql", "commit"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				forEngines(t, func(t *testing.T, f *fixture) {
					a, b := realUserSetup(t, f)
					if kind == "delete" {
						book := realCollectionCreate(t, f.DB, b, collectionadmin.My, 0, "cleanup")
						legacyLifecycleOwnedFacts(t, f, b, a, book)
						transactionCreate(t, f.DB, &model.Peer{Id: "cleanup-peer", Uuid: "cleanup-uuid", UserId: b.User.ID, Note: "preserve"})
					}
					table, event := "users", "UPDATE"
					if kind == "delete" {
						event = "DELETE"
					}
					if kind == "disable" || kind == "reset" || kind == "self-password" {
						table, event = "user_tokens", "DELETE"
					}
					before := realUserRawSnapshot(t, f.DB)
					// For session DELETE faults prove the password/status UPDATE really
					// succeeded first, rather than failing before mutation was attempted.
					var firstWrite atomic.Bool
					realBookMust(t, f.DB.Callback().Update().After("gorm:update").Register("user:prove-first-write", func(tx *gorm.DB) {
						if tx.Statement.Table == "users" && tx.Error == nil && tx.RowsAffected == 1 {
							firstWrite.Store(true)
						}
					}))
					cleanup := realUserNativeFault(t, f, table, event, mode)
					observedDB, nativeErrors := observeTransactions(t, f.DB)
					op := realUserOperation(realUserStore(t, observedDB), kind, a, b)
					if err := op(context.Background()); !errors.Is(err, useradmin.ErrUnavailable) || strings.Contains(err.Error(), "private") {
						t.Fatal("native failure not sanitized unavailable", err)
					}
					cleanup()
					if !realUserObservedNativeFault(nativeErrors, f.Engine, mode) {
						t.Fatal("intended native SQL/COMMIT error category was never reached")
					}
					realBookMust(t, f.DB.Callback().Update().Remove("user:prove-first-write"))
					if table == "user_tokens" && !firstWrite.Load() {
						t.Fatal("session failure never followed successful account mutation")
					}
					realUserUnchanged(t, f, before)
					// Explicit retry after native fault removal must perform the actual
					// intended operation and cleanup, not merely return a nil error.
					realBookMust(t, op(context.Background()))
					realUserPositive(t, f.Reopen(t), kind, a, b)
				})
			})
		}
	}
}

func TestRealUserManagementRevokeBothPhysicalOrders(t *testing.T) {
	for _, kind := range []string{"delete", "disable", "reset"} {
		for _, revokeFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/revoke-first-%t", kind, revokeFirst), func(t *testing.T) {
				forEngines(t, func(t *testing.T, f *fixture) {
					a, b := realUserSetup(t, f)
					peer := model.Peer{Id: "revoke-target", Uuid: "revoke-target-uuid", UserId: b.User.ID, Note: "retain"}
					transactionCreate(t, f.DB, &peer)
					realBookMust(t, f.DB.Model(&model.UserToken{}).Where("id=?", b.Session.ID).UpdateColumn("device_uuid", peer.Uuid).Error)
					b.Session.DeviceUUID = peer.Uuid
					revokeDB := f.Reopen(t)
					writeDB, observed := observeTransactions(t, f.Reopen(t))
					r, err := authstore.New(revokeDB)
					realBookMust(t, err)
					op := realUserOperation(realUserStore(t, writeDB), kind, a, b)
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					held, release := make(chan struct{}), make(chan struct{})
					var heldOnce, releaseOnce sync.Once
					defer releaseOnce.Do(func() { close(release) })
					lead := writeDB
					if revokeFirst {
						lead = revokeDB
					}
					realBookMust(t, lead.Callback().Query().After("gorm:query").Register("user:hold-token-group", func(tx *gorm.DB) {
						if tx.Error == nil && legacyLifecycleTokenGroupLock(tx, b.Session.Token, !revokeFirst) {
							heldOnce.Do(func() {
								close(held)
								select {
								case <-release:
								case <-ctx.Done():
									tx.AddError(ctx.Err())
								}
							})
						}
					}))
					first, second := make(chan error, 1), make(chan error, 1)
					before := realUserRawSnapshot(t, f.DB)
					if revokeFirst {
						go func() { first <- r.Revoke(ctx, b.Session) }()
					} else {
						go func() { first <- op(ctx) }()
					}
					realBookWait(t, ctx, held, "leading operation did not lock token group")
					if revokeFirst {
						go func() { second <- op(ctx) }()
						if err := realBookResult(t, ctx, second); !errors.Is(err, useradmin.ErrUnavailable) {
							t.Fatal("target token busy was not whole-write unavailable", err)
						}
						if !legacyLifecycleObservedNowait(observed, f.Engine) {
							t.Fatal("no native NOWAIT code observed")
						}
						realUserUnchanged(t, f, before)
						releaseOnce.Do(func() { close(release) })
						realBookMust(t, realBookResult(t, ctx, first))
						realBookMust(t, lead.Callback().Query().Remove("user:hold-token-group"))
						realBookMust(t, op(ctx)) // Explicit retry; target's revoked session is not the actor session.
					} else {
						go func() { second <- r.Revoke(ctx, b.Session) }()
						legacyLifecycleWaitForNativeSessionLock(t, f, ctx)
						releaseOnce.Do(func() { close(release) })
						realBookMust(t, realBookResult(t, ctx, first))
						if err := realBookResult(t, ctx, second); !errors.Is(err, auth.ErrUnauthenticated) {
							t.Fatal("Revoke resurrected target or hid non-auth failure after completed revocation", err)
						}
						realBookMust(t, lead.Callback().Query().Remove("user:hold-token-group"))
					}
					realUserPositive(t, f.Reopen(t), kind, a, b)
					if observed.retryable() {
						t.Fatal("deadlock/lock-timeout observed in token-index ordering")
					}
				})
			})
		}
	}
}

func TestRealUserManagementIssueBothOrders(t *testing.T) {
	for _, kind := range []string{"disable", "reset", "delete"} {
		for _, issueFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/issue-first-%t", kind, issueFirst), func(t *testing.T) {
				forEngines(t, func(t *testing.T, f *fixture) {
					a, b := realUserSetup(t, f)
					issueDB, mutationDB := f.Reopen(t), f.Reopen(t)
					issued, err := authstore.New(issueDB)
					realBookMust(t, err)
					op := realUserOperation(realUserStore(t, mutationDB), kind, a, b)
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					held, release := make(chan struct{}), make(chan struct{})
					var holdOnce, releaseOnce sync.Once
					defer releaseOnce.Do(func() { close(release) })
					lead := mutationDB
					if issueFirst {
						lead = issueDB
					}
					realBookMust(t, lead.Callback().Query().After("gorm:query").Register("user:hold-before-issue", func(tx *gorm.DB) {
						if row, ok := tx.Statement.Dest.(*model.User); ok && row.Id == b.User.ID && tx.Error == nil && realBookIsLockedTable(tx, "users") {
							holdOnce.Do(func() {
								close(held)
								select {
								case <-release:
								case <-ctx.Done():
									tx.AddError(ctx.Err())
								}
							})
						}
					}))
					now := time.Now().UTC().Truncate(time.Second)
					oldHash := realUserRead(t, f.DB, b.User.ID).Password
					issue := func() error {
						_, err := issued.Issue(ctx, auth.IssuedSession{Request: auth.IssueRequest{UserID: b.User.ID, ExpectedPassword: &oldHash}, Token: "new-issue-race-session", Now: now, ExpiresAt: now.Add(time.Hour)})
						return err
					}
					first, second := make(chan error, 1), make(chan error, 1)
					if issueFirst {
						go func() { first <- issue() }()
					} else {
						go func() { first <- op(ctx) }()
					}
					realBookWait(t, ctx, held, "leading operation did not hold target user")
					if issueFirst {
						go func() { second <- op(ctx) }()
					} else {
						go func() { second <- issue() }()
					}
					realBookNativeWait(t, f, ctx, "users")
					releaseOnce.Do(func() { close(release) })
					realBookMust(t, realBookResult(t, ctx, first))
					err = realBookResult(t, ctx, second)
					if issueFirst {
						realBookMust(t, err)
					} else {
						want := auth.ErrUnauthenticated
						if kind == "disable" {
							want = auth.ErrDisabled
						}
						if !errors.Is(err, want) {
							t.Fatal("stale issuance crossed completed mutation", err)
						}
					}
					realBookMust(t, lead.Callback().Query().Remove("user:hold-before-issue"))
					realUserPositive(t, f.Reopen(t), kind, a, b)
					var n int64
					realBookMust(t, f.DB.Model(&model.UserToken{}).Where("token=?", "new-issue-race-session").Count(&n).Error)
					if n != 0 {
						t.Fatal("racing issuance left a live target session")
					}
				})
			})
		}
	}
}

func TestRealUserManagementDeleteReferenceAndCompleteCleanup(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		a, b := realUserSetup(t, f)
		book := realCollectionCreate(t, f.DB, b, collectionadmin.My, 0, "owner-book")
		otherBook := realCollectionCreate(t, f.DB, a, collectionadmin.My, 0, "unrelated-book")
		legacyLifecycleOwnedFacts(t, f, b, a, book)
		transactionCreate(t, f.DB, &model.AddressBook{UserId: a.User.ID, CollectionId: otherBook.ID, Id: "unrelated", Note: "sentinel"})
		peer := model.Peer{Id: "blocked", Uuid: "blocked-uuid", UserId: a.User.ID, AbId: book.ID, Alias: "retain", Note: "retain-note"}
		transactionCreate(t, f.DB, &peer)
		owned := model.Peer{Id: "owned", Uuid: "owned-uuid", UserId: b.User.ID, Note: "owned-note", Hostname: "preserve-host"}
		transactionCreate(t, f.DB, &owned)
		s := realUserStore(t, f.DB)
		before := realUserRawSnapshot(t, f.DB)
		if err := s.Delete(context.Background(), a, b.User.ID); !errors.Is(err, collectionguard.ErrReferenced) {
			t.Fatal("foreign-owner primary reference was not protected", err)
		}
		realUserUnchanged(t, f, before)
		realBookMust(t, f.DB.Model(&peer).Updates(map[string]any{"ab_id": 0, "preset_ab_id": book.ID}).Error)
		peerRaw := realEntryPeerRaw(t, f.DB, peer.RowId)
		ownedRaw := realEntryPeerRaw(t, f.DB, owned.RowId)
		realBookMust(t, s.Delete(context.Background(), a, b.User.ID))
		realUserPositive(t, f.Reopen(t), "delete", a, b)
		for _, table := range []string{"oidc_identities", "user_thirds", "address_book_collections", "address_books", "tags", "address_book_collection_rules"} {
			var n int64
			realBookMust(t, f.DB.Table(table).Where("user_id=?", b.User.ID).Count(&n).Error)
			if n != 0 {
				t.Fatal("delete left owned facts in", table)
			}
		}
		for _, table := range []string{"address_books", "tags", "address_book_collection_rules"} {
			var n int64
			realBookMust(t, f.DB.Table(table).Where("collection_id=?", book.ID).Count(&n).Error)
			if n != 0 {
				t.Fatal("delete missed split-owner children in", table)
			}
		}
		if !reflect.DeepEqual(peerRaw, realEntryPeerRaw(t, f.DB, peer.RowId)) {
			t.Fatal("unconsumed preset or other owner's peer changed")
		}
		afterOwned := realEntryPeerRaw(t, f.DB, owned.RowId)
		realEntryNarrowPeer(t, ownedRaw, afterOwned, "user_id")
		if assignmentRead(t, f.DB, owned.RowId).UserId != 0 {
			t.Fatal("owned peer not unbound")
		}
		var sentinel model.AddressBook
		realBookMust(t, f.DB.Where("collection_id=?", otherBook.ID).First(&sentinel).Error)
		if sentinel.Note != "sentinel" {
			t.Fatal("unrelated book changed")
		}
	})
}
