package integration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/authstore"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionadmin"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionguard"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/service"
	logrus "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UserService captures its DB. Constructor mutation is synchronous, before any
// worker starts; no test switches service globals while an operation runs.
func legacyLifecycleUsers(t *testing.T, db *gorm.DB) *service.UserService {
	t.Helper()
	oldConfig, oldDB, oldLogger, oldJWT, oldLock, oldServices := service.Config, service.DB, service.Logger, service.Jwt, service.Lock, service.AllService
	built := service.New(nil, db, logrus.New(), nil, oldLock)
	service.Config, service.DB, service.Logger, service.Jwt, service.Lock, service.AllService = oldConfig, oldDB, oldLogger, oldJWT, oldLock, oldServices
	return built.UserService
}

type legacyLifecycleState struct {
	Collections realCollectionState
	Users       []model.User
	Identities  []model.OIDCIdentity
	Thirds      []model.UserThird
	Sessions    []model.UserToken
}

func legacyLifecycleSnapshot(t *testing.T, db *gorm.DB) legacyLifecycleState {
	t.Helper()
	s := legacyLifecycleState{Collections: realCollectionSnapshot(t, db)}
	for _, row := range []any{&s.Users, &s.Identities, &s.Thirds, &s.Sessions} {
		if err := db.Order("id").Find(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	return s
}
func legacyLifecycleOwnedFacts(t *testing.T, f *fixture, owner, other auth.Principal, book collectionadmin.Collection) {
	t.Helper()
	transactionCreate(t, f.DB, &model.OIDCIdentity{Issuer: "https://integration.invalid", Subject: "collection-owner", UserID: owner.User.ID})
	transactionCreate(t, f.DB, &model.UserThird{UserId: owner.User.ID, UnionId: "legacy-owner"})
	for _, uid := range []uint{owner.User.ID, other.User.ID} {
		transactionCreate(t, f.DB, &model.AddressBook{Id: "owned-book-device", UserId: uid, CollectionId: book.ID, Note: "by-cid"})
		transactionCreate(t, f.DB, &model.Tag{UserId: uid, CollectionId: book.ID, Name: "by-cid"})
		transactionCreate(t, f.DB, &model.AddressBookCollectionRule{UserId: uid, CollectionId: book.ID, Type: 1, Rule: 3, ToId: other.User.ID})
	}
	transactionCreate(t, f.DB, &model.AddressBook{Id: "personal-device", UserId: owner.User.ID, CollectionId: 0})
	transactionCreate(t, f.DB, &model.Tag{UserId: owner.User.ID, CollectionId: 0, Name: "personal"})
	transactionCreate(t, f.DB, &model.Tag{UserId: owner.User.ID, CollectionId: 999999, Name: "legacy-orphan"})
}

func TestRealCollectionReferencesUserDeleteReferencesAndScopedAssociations(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, owner, other := realCollectionsSetup(t, f)
		book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "owned")
		sentinel := realCollectionCreate(t, f.DB, other, collectionadmin.My, 0, "other")
		legacyLifecycleOwnedFacts(t, f, owner, other, book)
		transactionCreate(t, f.DB, &model.AddressBook{Id: "sentinel", UserId: other.User.ID, CollectionId: sentinel.ID})
		transactionCreate(t, f.DB, &model.Tag{UserId: other.User.ID, CollectionId: sentinel.ID, Name: "sentinel"})
		ownedPeer := model.Peer{Id: "owned-peer", Uuid: "owned-uuid", UserId: owner.User.ID, Note: "retain-metadata"}
		transactionCreate(t, f.DB, &ownedPeer)
		crossOwner := model.Peer{Id: "cross-owner-reference", Uuid: "other-uuid", UserId: other.User.ID, AbId: book.ID}
		transactionCreate(t, f.DB, &crossOwner)
		users := legacyLifecycleUsers(t, f.DB)
		victim := model.User{IdModel: model.IdModel{Id: owner.User.ID}}
		before := legacyLifecycleSnapshot(t, f.DB)
		if err := users.DeleteContext(context.Background(), &victim); !errors.Is(err, collectionguard.ErrReferenced) {
			t.Fatal("foreign peer owner escaped primary reference rejection", err)
		}
		if !reflect.DeepEqual(before, legacyLifecycleSnapshot(t, f.Reopen(t))) {
			t.Fatal("rejected user deletion partially removed facts")
		}
		if err := f.DB.Model(&crossOwner).Updates(map[string]any{"ab_id": 0, "preset_ab_id": book.ID}).Error; err != nil {
			t.Fatal(err)
		}
		if err := users.DeleteContext(context.Background(), &victim); err != nil {
			t.Fatal("unreferenced user cleanup failed", err)
		}
		after := legacyLifecycleSnapshot(t, f.Reopen(t))
		if len(after.Collections.Books) != 1 || after.Collections.Books[0].Id != sentinel.ID || len(after.Collections.Entries) != 1 || len(after.Collections.Tags) != 1 || len(after.Collections.Rules) != 0 {
			t.Fatal("user cleanup missed wrong-owner cid children or personal/orphan tags")
		}
		for _, identity := range after.Identities {
			if identity.UserID == owner.User.ID {
				t.Fatal("identity survived deleted user")
			}
		}
		for _, row := range after.Thirds {
			if row.UserId == owner.User.ID {
				t.Fatal("legacy identity survived deleted user")
			}
		}
		for _, row := range after.Sessions {
			if row.UserId == owner.User.ID {
				t.Fatal("session survived deleted user")
			}
		}
		for _, row := range after.Users {
			if row.Id == owner.User.ID {
				t.Fatal("victim survived cleanup")
			}
		}
		for _, peer := range after.Collections.Peers {
			if peer.RowId == ownedPeer.RowId && (peer.UserId != 0 || peer.Note != "retain-metadata") {
				t.Fatal("owned peer was not atomically unbound")
			}
			if peer.RowId == crossOwner.RowId && (peer.UserId != other.User.ID || peer.PresetAbId != book.ID) {
				t.Fatal("cleanup guessed foreign owner or preset repair")
			}
		}
	})
}

func legacyLifecycleFailDeleteCommit(t *testing.T, f *fixture) func() {
	t.Helper()
	if f.Engine == "postgres" {
		transactionExec(t, f.DB, "CREATE TABLE legacy_delete_commit_parent(id bigint PRIMARY KEY)")
		transactionExec(t, f.DB, "CREATE TABLE legacy_delete_commit_guard(parent_id bigint REFERENCES legacy_delete_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)")
		transactionExec(t, f.DB, "CREATE FUNCTION legacy_delete_commit_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO legacy_delete_commit_guard(parent_id) VALUES(999999); RETURN OLD; END $$")
		transactionExec(t, f.DB, "CREATE TRIGGER legacy_delete_commit_fault AFTER DELETE ON users FOR EACH ROW EXECUTE FUNCTION legacy_delete_commit_fault()")
		return func() {
			transactionExec(t, f.DB, "DROP TRIGGER legacy_delete_commit_fault ON users")
			transactionExec(t, f.DB, "DROP FUNCTION legacy_delete_commit_fault()")
			transactionExec(t, f.DB, "DROP TABLE legacy_delete_commit_guard")
			transactionExec(t, f.DB, "DROP TABLE legacy_delete_commit_parent")
		}
	}
	control := f.Reopen(t)
	var fired, killed atomic.Bool
	if err := f.DB.Callback().Delete().After("gorm:delete").Register("legacy:user-delete-kill", func(tx *gorm.DB) {
		if tx.Statement.Table != "users" || tx.Error != nil || !fired.CompareAndSwap(false, true) {
			return
		}
		var id uint64
		if err := tx.Statement.ConnPool.QueryRowContext(tx.Statement.Context, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
			tx.AddError(err)
			return
		}
		if err := control.Exec(fmt.Sprintf("KILL CONNECTION %d", id)).Error; err != nil {
			tx.AddError(err)
			return
		}
		killed.Store(true)
	}); err != nil {
		t.Fatal(err)
	}
	return func() {
		if !fired.Load() || !killed.Load() {
			t.Error("real final DELETE connection failure never reached")
		}
		if err := f.DB.Callback().Delete().Remove("legacy:user-delete-kill"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRealCollectionReferencesUserDeleteSQLAndCommitRollbackRetry(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, owner, other := realCollectionsSetup(t, f)
		book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "owned")
		legacyLifecycleOwnedFacts(t, f, owner, other, book)
		transactionCreate(t, f.DB, &model.Peer{Id: "owned", Uuid: "uuid", UserId: owner.User.ID, Note: "keep"})
		victim := model.User{IdModel: model.IdModel{Id: owner.User.ID}}
		users := legacyLifecycleUsers(t, f.DB)
		before := legacyLifecycleSnapshot(t, f.DB)
		if f.Engine == "mysql" {
			transactionExec(t, f.DB, "CREATE TRIGGER legacy_reject_user BEFORE DELETE ON users FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='private delete cause'")
		} else {
			transactionExec(t, f.DB, "CREATE FUNCTION legacy_reject_user() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'private delete cause'; END $$")
			transactionExec(t, f.DB, "CREATE TRIGGER legacy_reject_user BEFORE DELETE ON users FOR EACH ROW EXECUTE FUNCTION legacy_reject_user()")
		}
		if err := users.DeleteContext(context.Background(), &victim); err == nil {
			t.Fatal("final DELETE SQL error acknowledged")
		}
		if !reflect.DeepEqual(before, legacyLifecycleSnapshot(t, f.Reopen(t))) {
			t.Fatal("final DELETE error retained identity/book/session/peer changes")
		}
		if f.Engine == "mysql" {
			transactionExec(t, f.DB, "DROP TRIGGER legacy_reject_user")
		} else {
			transactionExec(t, f.DB, "DROP TRIGGER legacy_reject_user ON users")
			transactionExec(t, f.DB, "DROP FUNCTION legacy_reject_user()")
		}
		remove := legacyLifecycleFailDeleteCommit(t, f)
		if err := users.DeleteContext(context.Background(), &victim); err == nil {
			t.Fatal("real failed user DELETE COMMIT acknowledged")
		}
		remove()
		if !reflect.DeepEqual(before, legacyLifecycleSnapshot(t, f.Reopen(t))) {
			t.Fatal("real failed COMMIT retained cleanup or peer unbind")
		}
		if err := users.DeleteContext(context.Background(), &victim); err != nil {
			t.Fatal("user delete retry failed", err)
		}
		if transactionCount(t, f.DB, &model.OIDCIdentity{}) != 0 || transactionCount(t, f.DB, &model.AddressBookCollection{}) != 0 || transactionCount(t, f.DB, &model.Tag{}) != 0 {
			t.Fatal("retry did not clean user scope")
		}
	})
}

// Observe the actual token-group access path, never its unlocked enumeration
// or the superseded session-PK lock. Bind values remain internal to this test.
func legacyLifecycleTokenGroupLock(tx *gorm.DB, token string, nowait bool) bool {
	if tx.Statement.Table != "user_tokens" {
		return false
	}
	if _, ok := tx.Statement.Dest.(*[]model.UserToken); !ok {
		return false
	}
	locking, ok := tx.Statement.Clauses["FOR"].Expression.(clause.Locking)
	if !ok || locking.Strength != "UPDATE" || (nowait && locking.Options != "NOWAIT") {
		return false
	}
	where, ok := tx.Statement.Clauses["WHERE"].Expression.(clause.Where)
	if !ok {
		return false
	}
	for _, expression := range where.Exprs {
		switch condition := expression.(type) {
		case clause.Expr:
			if condition.SQL == "token = ?" && len(condition.Vars) == 1 && condition.Vars[0] == token {
				return true
			}
		case clause.Eq:
			if column, ok := condition.Column.(clause.Column); ok && column.Name == "token" && condition.Value == token {
				return true
			}
		}
	}
	return false
}

func TestRealCollectionReferencesUserDeleteSessionPeerOrderWithRevoke(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, owner, _ := realCollectionsSetup(t, f)
		peer := model.Peer{Id: "revoke-peer", Uuid: "revoke-uuid", UserId: owner.User.ID}
		transactionCreate(t, f.DB, &peer)
		if err := f.DB.Model(&model.UserToken{}).Where("id=?", owner.Session.ID).UpdateColumn("device_uuid", peer.Uuid).Error; err != nil {
			t.Fatal(err)
		}
		revokeDB := f.Reopen(t)
		deleteDB, observed := observeTransactions(t, f.Reopen(t))
		locked, release, firstRelated := make(chan struct{}), make(chan struct{}), make(chan string, 1)
		var once, relatedOnce sync.Once
		if err := revokeDB.Callback().Query().After("gorm:query").Register("legacy:hold-revoke-session", func(tx *gorm.DB) {
			if _, ok := tx.Statement.Clauses["FOR"]; ok && tx.Statement.Table == "user_tokens" && tx.Error == nil {
				once.Do(func() {
					close(locked)
					select {
					case <-release:
					case <-tx.Statement.Context.Done():
						tx.AddError(tx.Statement.Context.Err())
					}
				})
			}
		}); err != nil {
			t.Fatal(err)
		}
		if err := deleteDB.Callback().Query().Before("gorm:query").Register("legacy:delete-next-lock", func(tx *gorm.DB) {
			_, locking := tx.Statement.Clauses["FOR"]
			if legacyLifecycleTokenGroupLock(tx, owner.Session.Token, true) || (locking && tx.Statement.Table == "peers") {
				relatedOnce.Do(func() { firstRelated <- tx.Statement.Table })
			}
		}); err != nil {
			t.Fatal(err)
		}
		revoking, err := authstore.New(revokeDB)
		if err != nil {
			t.Fatal(err)
		}
		users := legacyLifecycleUsers(t, deleteDB)
		before := legacyLifecycleSnapshot(t, f.DB)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		revoked, deleted := make(chan error, 1), make(chan error, 1)
		go func() { revoked <- revoking.Revoke(ctx, owner.Session) }()
		select {
		case <-locked:
		case <-ctx.Done():
			close(release)
			t.Fatal("Revoke did not hold session")
		}
		go func() { deleted <- users.DeleteContext(ctx, &model.User{IdModel: model.IdModel{Id: owner.User.ID}}) }()
		select {
		case table := <-firstRelated:
			if table != "user_tokens" {
				close(release)
				t.Fatal("user delete attempted peer before session", table)
			}
		case <-ctx.Done():
			close(release)
			t.Fatal("user delete did not reach related locks")
		}
		// Busy deletion must finish while Revoke still holds its session. It
		// neither waits into a secondary-index cycle nor silently retries.
		select {
		case err := <-deleted:
			if !errors.Is(err, collectionguard.ErrUnavailable) {
				close(release)
				t.Fatal("busy token group was not safe unavailable", err)
			}
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("busy token group did not fail NOWAIT")
		}
		if !legacyLifecycleObservedNowait(observed, f.Engine) {
			close(release)
			t.Fatal("busy deletion did not produce native NOWAIT category")
		}
		if !reflect.DeepEqual(before, legacyLifecycleSnapshot(t, f.Reopen(t))) {
			close(release)
			t.Fatal("busy deletion changed identity/session/peer/book facts")
		}
		close(release)
		select {
		case err := <-revoked:
			if err != nil {
				t.Fatal("Revoke/user-delete session-peer cycle", err)
			}
		case <-ctx.Done():
			t.Fatal("Revoke deadlocked")
		}
		if err := users.DeleteContext(ctx, &model.User{IdModel: model.IdModel{Id: owner.User.ID}}); err != nil {
			t.Fatal("explicit user-delete retry after Revoke failed", err)
		}
		final := assignmentRead(t, f.Reopen(t), peer.RowId)
		if final.UserId != 0 {
			t.Fatal("final revoke/delete did not unbind peer")
		}
	})
}

// The regular-owner cases block on their common user row. Legacy owner zero
// intentionally skips that row and must serialize at the collection itself.
func TestRealCollectionReferencesUpdateAliasDeleteBothOrders(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		admin, owner, other := realCollectionsSetup(t, f)
		for _, zeroOwner := range []bool{false, true} {
			for _, aliasFirst := range []bool{true, false} {
				name := fmt.Sprintf("owner0-%t-aliasfirst-%t", zeroOwner, aliasFirst)
				t.Run(name, func(t *testing.T) {
					book := model.AddressBookCollection{UserId: owner.User.ID, Name: name}
					mode, actor := collectionadmin.My, owner
					if zeroOwner {
						book.UserId = 0
						mode, actor = collectionadmin.Admin, admin
					}
					transactionCreate(t, f.DB, &book)
					peer := model.Peer{Id: name, Uuid: name, UserId: other.User.ID, Note: "preserve"}
					transactionCreate(t, f.DB, &peer)
					aliasDB, deleteDB := f.Reopen(t), f.Reopen(t)
					oldDB := service.DB
					service.DB = aliasDB
					t.Cleanup(func() { service.DB = oldDB })
					leading, following := aliasDB, deleteDB
					if !aliasFirst {
						leading, following = deleteDB, aliasDB
					}
					locked, release, attempted, acquired := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
					var holdOnce, attemptOnce, acquireOnce sync.Once
					if err := leading.Callback().Query().After("gorm:query").Register("legacy:hold-parent", func(tx *gorm.DB) {
						if _, ok := tx.Statement.Clauses["FOR"]; ok && tx.Statement.Table == "address_book_collections" && tx.Error == nil {
							holdOnce.Do(func() {
								close(locked)
								select {
								case <-release:
								case <-tx.Statement.Context.Done():
									tx.AddError(tx.Statement.Context.Err())
								}
							})
						}
					}); err != nil {
						t.Fatal(err)
					}
					if err := following.Callback().Query().Before("gorm:query").Register("legacy:attempt-shared", func(tx *gorm.DB) {
						attempt := realCollectionsAttemptsOwner(tx, owner.User.ID)
						if zeroOwner {
							_, locking := tx.Statement.Clauses["FOR"]
							attempt = locking && tx.Statement.Table == "address_book_collections"
						}
						if attempt {
							attemptOnce.Do(func() { close(attempted) })
						}
					}); err != nil {
						t.Fatal(err)
					}
					if err := following.Callback().Query().After("gorm:query").Register("legacy:acquire-shared", func(tx *gorm.DB) {
						_, locking := tx.Statement.Clauses["FOR"]
						if !locking || tx.Error != nil {
							return
						}
						done := false
						if zeroOwner {
							row, ok := tx.Statement.Dest.(*model.AddressBookCollection)
							done = ok && row.Id == book.Id
						} else {
							row, ok := tx.Statement.Dest.(*model.User)
							done = ok && row.Id == owner.User.ID
						}
						if done {
							acquireOnce.Do(func() { close(acquired) })
						}
					}); err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					deleting := realCollectionsStore(t, deleteDB)
					aliasDone, deleteDone := make(chan error, 1), make(chan error, 1)
					alias := func() {
						aliasDone <- (&service.PeerService{}).UpdateAlias(peer.RowId, book.Id, "assigned", "user", "host")
					}
					del := func() { deleteDone <- deleting.Delete(ctx, actor, mode, collectionadmin.Input{ID: book.Id}) }
					if aliasFirst {
						go alias()
					} else {
						go del()
					}
					select {
					case <-locked:
					case <-ctx.Done():
						close(release)
						t.Fatal("leader failed to hold parent")
					}
					if aliasFirst {
						go del()
					} else {
						go alias()
					}
					select {
					case <-attempted:
					case <-ctx.Done():
						close(release)
						t.Fatal("follower failed to attempt shared row")
					}
					select {
					case <-acquired:
						close(release)
						t.Fatal("follower acquired shared row before leader release")
					case <-time.After(100 * time.Millisecond):
					}
					close(release)
					var aerr, derr error
					select {
					case aerr = <-aliasDone:
					case <-ctx.Done():
						t.Fatal("alias deadlocked")
					}
					select {
					case derr = <-deleteDone:
					case <-ctx.Done():
						t.Fatal("delete deadlocked")
					}
					if aliasFirst {
						if aerr != nil || !errors.Is(derr, collectionadmin.ErrReferenced) {
							t.Fatal("committed alias did not block deletion", aerr, derr)
						}
					} else {
						if derr != nil || !errors.Is(aerr, collectionguard.ErrInput) {
							t.Fatal("deleted collection allowed new alias binding", aerr, derr)
						}
					}
					final := assignmentRead(t, f.Reopen(t), peer.RowId)
					want := uint(0)
					if aliasFirst {
						want = book.Id
					}
					if final.AbId != want || final.Note != "preserve" || final.UserId != other.User.ID {
						t.Fatal("alias/delete partial state or unauthorized owner change")
					}
					service.DB = oldDB
				})
			}
		}
	})
}

func legacyLifecycleObservedNowait(observed *transactionErrors, engine string) bool {
	observed.mu.Lock()
	defer observed.mu.Unlock()
	for _, err := range observed.values {
		var mysql *mysqldriver.MySQLError
		if engine == "mysql" && errors.As(err, &mysql) && mysql.Number == 3572 {
			return true
		}
		var pg interface{ SQLState() string }
		if engine == "postgres" && errors.As(err, &pg) && pg.SQLState() == "55P03" {
			return true
		}
	}
	return false
}

func TestRealCollectionReferencesUserDeleteIdentityNowait(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, _, other := realCollectionsSetup(t, f)
		for _, table := range []string{"oidc_identities", "user_thirds"} {
			t.Run(table, func(t *testing.T) {
				owner := definitionAdmin(t, f.DB, "identity-busy-"+table)
				if err := f.DB.Model(&model.User{}).Where("id=?", owner.User.ID).UpdateColumn("is_admin", false).Error; err != nil {
					t.Fatal(err)
				}
				owner.User.IsAdmin = false
				book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "identity-busy-"+table)
				legacyLifecycleOwnedFacts(t, f, owner, other, book)
				before := legacyLifecycleSnapshot(t, f.DB)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				holderDB := f.Reopen(t)
				holder := holderDB.WithContext(ctx).Begin()
				if holder.Error != nil {
					t.Fatal(holder.Error)
				}
				defer holder.Rollback()
				var ids []uint
				if err := holder.Raw("SELECT id FROM "+table+" WHERE user_id=? FOR UPDATE", owner.User.ID).Scan(&ids).Error; err != nil || len(ids) != 1 {
					t.Fatal("identity holder did not lock its existing row", err)
				}
				deleteDB, observed := observeTransactions(t, f.Reopen(t))
				users := legacyLifecycleUsers(t, deleteDB)
				userHeld, releaseDelete, identityUserAttempted := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var userOnce, attemptOnce sync.Once
				if err := deleteDB.Callback().Query().After("gorm:query").Register("legacy:hold-delete-user", func(tx *gorm.DB) {
					row, ok := tx.Statement.Dest.(*model.User)
					_, locking := tx.Statement.Clauses["FOR"]
					if ok && locking && row.Id == owner.User.ID && tx.Error == nil {
						userOnce.Do(func() {
							close(userHeld)
							select {
							case <-releaseDelete:
							case <-tx.Statement.Context.Done():
								tx.AddError(tx.Statement.Context.Err())
							}
						})
					}
				}); err != nil {
					t.Fatal(err)
				}
				if err := holderDB.Callback().Query().Before("gorm:query").Register("legacy:identity-attempt-user", func(tx *gorm.DB) {
					if realCollectionsAttemptsOwner(tx, owner.User.ID) {
						attemptOnce.Do(func() { close(identityUserAttempted) })
					}
				}); err != nil {
					t.Fatal(err)
				}
				deleted, identityFinished := make(chan error, 1), make(chan error, 1)
				go func() { deleted <- users.DeleteContext(ctx, &model.User{IdModel: model.IdModel{Id: owner.User.ID}}) }()
				select {
				case <-userHeld:
				case <-ctx.Done():
					close(releaseDelete)
					t.Fatal("delete did not hold victim user")
				}
				go func() {
					var row model.User
					err := holder.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, owner.User.ID).Error
					if err != nil {
						_ = holder.Rollback()
						identityFinished <- err
						return
					}
					identityFinished <- holder.Commit().Error
				}()
				select {
				case <-identityUserAttempted:
				case <-ctx.Done():
					close(releaseDelete)
					t.Fatal("identity holder did not attempt victim user")
				}
				close(releaseDelete)
				select {
				case err := <-deleted:
					if !errors.Is(err, collectionguard.ErrUnavailable) {
						t.Fatal("busy identity was not safe unavailable", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("NOWAIT user deletion did not fail promptly")
				}
				select {
				case err := <-identityFinished:
					if err != nil {
						t.Fatal("identity -> user transaction could not finish after delete rollback", err)
					}
				case <-ctx.Done():
					t.Fatal("identity holder remained blocked")
				}
				if !legacyLifecycleObservedNowait(observed, f.Engine) {
					t.Fatal("failure was not a native NOWAIT lock-busy category")
				}
				t.Log("observed native NOWAIT lock-busy category; SQL and causes omitted")
				if !reflect.DeepEqual(before, legacyLifecycleSnapshot(t, f.Reopen(t))) {
					t.Fatal("NOWAIT failure partially deleted user facts")
				}
				if err := users.DeleteContext(context.Background(), &model.User{IdModel: model.IdModel{Id: owner.User.ID}}); err != nil {
					t.Fatal("delete retry after identity completion failed", err)
				}
			})
		}
	})
}

func TestRealCollectionReferencesUpdateAliasCandidateDriftAndMissing(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, owner, other := realCollectionsSetup(t, f)
		book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "drift")
		peer := model.Peer{Id: "drift-peer", Uuid: "uuid", UserId: other.User.ID, Alias: "before", Note: "keep"}
		transactionCreate(t, f.DB, &peer)
		db := f.Reopen(t)
		oldDB := service.DB
		service.DB = db
		defer func() { service.DB = oldDB }()
		var fired atomic.Bool
		control := f.Reopen(t)
		if err := db.Callback().Query().After("gorm:query").Register("legacy:drift-candidate", func(tx *gorm.DB) {
			if _, locking := tx.Statement.Clauses["FOR"]; locking || tx.Statement.Table != "address_book_collections" || tx.Error != nil || !fired.CompareAndSwap(false, true) {
				return
			}
			if err := control.Model(&model.AddressBookCollection{}).Where("id=?", book.ID).UpdateColumn("user_id", other.User.ID).Error; err != nil {
				tx.AddError(err)
			}
		}); err != nil {
			t.Fatal(err)
		}
		before := assignmentRead(t, f.DB, peer.RowId)
		if err := (&service.PeerService{}).UpdateAlias(peer.RowId, book.ID, "must-not-write", "bad-user", "bad-host"); !errors.Is(err, collectionguard.ErrInput) || !fired.Load() {
			t.Fatal("candidate owner drift accepted", err)
		}
		if err := db.Callback().Query().Remove("legacy:drift-candidate"); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, assignmentRead(t, f.Reopen(t), peer.RowId)) {
			t.Fatal("candidate drift partially changed peer")
		}
		if err := f.DB.Model(&model.AddressBookCollection{}).Where("id=?", book.ID).UpdateColumn("user_id", uint(999999)).Error; err != nil {
			t.Fatal(err)
		}
		if err := (&service.PeerService{}).UpdateAlias(peer.RowId, book.ID, "bad", "bad", "bad"); !errors.Is(err, collectionguard.ErrInput) {
			t.Fatal("missing nonzero owner accepted", err)
		}
		if err := (&service.PeerService{}).UpdateAlias(peer.RowId, book.ID+999999, "bad", "bad", "bad"); !errors.Is(err, collectionguard.ErrInput) {
			t.Fatal("missing collection accepted", err)
		}
		if !reflect.DeepEqual(before, assignmentRead(t, f.Reopen(t), peer.RowId)) {
			t.Fatal("missing owner/parent partially changed peer")
		}
		if err := f.DB.Model(&model.AddressBookCollection{}).Where("id=?", book.ID).UpdateColumn("user_id", owner.User.ID).Error; err != nil {
			t.Fatal(err)
		}
		if err := f.DB.Model(&model.User{}).Where("id=?", owner.User.ID).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
			t.Fatal(err)
		}
		for retry := 0; retry < 2; retry++ {
			if err := (&service.PeerService{}).UpdateAlias(peer.RowId, book.ID, "assigned", "user", "host"); err != nil {
				t.Fatal("legacy disabled owner/same-value alias changed semantics", err)
			}
		}
		if err := (&service.PeerService{}).UpdateAlias(peer.RowId, 0, "", "", ""); err != nil {
			t.Fatal("legacy zero detach failed", err)
		}
		after := assignmentRead(t, f.Reopen(t), peer.RowId)
		if after.AbId != 0 || after.Alias != "" || after.UserId != other.User.ID || after.Note != "keep" {
			t.Fatal("alias detach changed unrelated fields")
		}
	})
}

func legacyLifecycleWaitForNativeSessionLock(t *testing.T, f *fixture, ctx context.Context) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		var count int64
		var err error
		if f.Engine == "mysql" {
			var indexes []string
			err = f.DB.WithContext(ctx).Raw("SELECT INDEX_NAME FROM performance_schema.data_locks WHERE OBJECT_SCHEMA=DATABASE() AND OBJECT_NAME='user_tokens' AND LOCK_STATUS='WAITING'").Scan(&indexes).Error
			for _, index := range indexes {
				if index == "PRIMARY" {
					t.Fatal("Revoke waited on PRIMARY after retaining a token secondary lock")
				}
				if index == "idx_user_tokens_token" {
					count++
					t.Log("confirmed native token secondary index wait: idx_user_tokens_token")
				}
			}
		} else {
			err = f.DB.WithContext(ctx).Raw("SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%user_tokens%'").Scan(&count).Error
		}
		if err != nil {
			t.Fatal("could not inspect native session lock wait (details omitted)")
		}
		if count > 0 {
			t.Log("confirmed native session-row lock wait; SQL/bind data omitted")
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("follower did not enter a native session-row lock wait")
}

func TestRealCollectionReferencesUserDeleteFirstRevokePhysicalOrder(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, owner, _ := realCollectionsSetup(t, f)
		peer := model.Peer{Id: "delete-first-revoke", Uuid: "delete-first-uuid", UserId: owner.User.ID}
		transactionCreate(t, f.DB, &peer)
		if err := f.DB.Model(&model.UserToken{}).Where("id=?", owner.Session.ID).UpdateColumn("device_uuid", peer.Uuid).Error; err != nil {
			t.Fatal(err)
		}
		deleteDB, revokeDB := f.Reopen(t), f.Reopen(t)
		locked, release, attempted := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var holdOnce, attemptOnce sync.Once
		if err := deleteDB.Callback().Query().After("gorm:query").Register("legacy:hold-delete-session", func(tx *gorm.DB) {
			rows, ok := tx.Statement.Dest.(*[]model.UserToken)
			hasVictim := false
			if ok {
				for _, row := range *rows {
					if row.Id == owner.Session.ID {
						hasVictim = true
					}
				}
			}
			if legacyLifecycleTokenGroupLock(tx, owner.Session.Token, true) && hasVictim && tx.Error == nil {
				holdOnce.Do(func() {
					close(locked)
					select {
					case <-release:
					case <-tx.Statement.Context.Done():
						tx.AddError(tx.Statement.Context.Err())
					}
				})
			}
		}); err != nil {
			t.Fatal(err)
		}
		if err := revokeDB.Callback().Query().Before("gorm:query").Register("legacy:revoke-attempt-session", func(tx *gorm.DB) {
			if legacyLifecycleTokenGroupLock(tx, owner.Session.Token, false) {
				attemptOnce.Do(func() { close(attempted) })
			}
		}); err != nil {
			t.Fatal(err)
		}
		users := legacyLifecycleUsers(t, deleteDB)
		revoking, err := authstore.New(revokeDB)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		deleted, revoked := make(chan error, 1), make(chan error, 1)
		go func() { deleted <- users.DeleteContext(ctx, &model.User{IdModel: model.IdModel{Id: owner.User.ID}}) }()
		select {
		case <-locked:
		case <-ctx.Done():
			close(release)
			t.Fatal("delete did not hold session")
		}
		go func() { revoked <- revoking.Revoke(ctx, owner.Session) }()
		select {
		case <-attempted:
		case <-ctx.Done():
			close(release)
			t.Fatal("Revoke did not attempt token lock")
		}
		legacyLifecycleWaitForNativeSessionLock(t, f, ctx)
		close(release)
		var derr, rerr error
		select {
		case derr = <-deleted:
		case <-ctx.Done():
			t.Fatal("delete-first operation deadlocked")
		}
		select {
		case rerr = <-revoked:
		case <-ctx.Done():
			t.Fatal("Revoke follower deadlocked")
		}
		if derr != nil {
			t.Fatal("delete-first physical index cycle", derr)
		}
		if !errors.Is(rerr, auth.ErrUnauthenticated) {
			t.Fatal("Revoke did not observe serialized session deletion", rerr)
		}
		if assignmentRead(t, f.Reopen(t), peer.RowId).UserId != 0 {
			t.Fatal("delete-first did not unbind peer")
		}
	})
}
