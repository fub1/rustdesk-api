package integration

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientassign"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionadmin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func realCollectionsStore(t *testing.T, db *gorm.DB) *collectionadmin.Store {
	t.Helper()
	store, err := collectionadmin.New(db, collectionadmin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return store
}
func realCollectionsSetup(t *testing.T, f *fixture) (auth.Principal, auth.Principal, auth.Principal) {
	t.Helper()
	upgrade(t, f.DB, 281)
	admin := definitionAdmin(t, f.DB, "collections-admin")
	owner := definitionAdmin(t, f.DB, "collections-owner")
	other := definitionAdmin(t, f.DB, "collections-other")
	for _, actor := range []auth.Principal{owner, other} {
		if err := f.DB.Model(&model.User{}).Where("id=?", actor.User.ID).UpdateColumn("is_admin", false).Error; err != nil {
			t.Fatal(err)
		}
	}
	owner.User.IsAdmin = false
	other.User.IsAdmin = false
	return admin, owner, other
}
func realCollectionCreate(t *testing.T, db *gorm.DB, actor auth.Principal, mode collectionadmin.Mode, owner uint, name string) collectionadmin.Collection {
	t.Helper()
	row, err := realCollectionsStore(t, db).Create(context.Background(), actor, mode, collectionadmin.Input{UserID: owner, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

type realCollectionState struct {
	Books   []model.AddressBookCollection
	Entries []model.AddressBook
	Tags    []model.Tag
	Rules   []model.AddressBookCollectionRule
	Peers   []model.Peer
}

func realCollectionSnapshot(t *testing.T, db *gorm.DB) realCollectionState {
	t.Helper()
	var s realCollectionState
	for _, row := range []struct {
		target any
		order  string
	}{{&s.Books, "id"}, {&s.Entries, "row_id"}, {&s.Tags, "id"}, {&s.Rules, "id"}, {&s.Peers, "row_id"}} {
		if err := db.Order(row.order).Find(row.target).Error; err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestCollectionsRealImmutableOwnerAndCurrentSession(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		admin, owner, other := realCollectionsSetup(t, f)
		store := realCollectionsStore(t, f.Reopen(t))
		ctx := context.Background()
		row := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, " owner name ")
		original := realCollectionSnapshot(t, f.DB)
		for _, actor := range []auth.Principal{admin, owner} {
			mode := collectionadmin.Admin
			if actor.User.ID == owner.User.ID {
				mode = collectionadmin.My
			}
			if got, err := store.Update(ctx, actor, mode, collectionadmin.Input{ID: row.ID, UserID: other.User.ID, Name: "transfer"}); !errors.Is(err, collectionadmin.ErrInput) || got.ID != 0 {
				t.Fatal("real owner transfer accepted", err)
			}
		}
		if !reflect.DeepEqual(original, realCollectionSnapshot(t, f.Reopen(t))) {
			t.Fatal("owner transfer rejection retained writes")
		}
		for _, uid := range []uint{0, owner.User.ID} {
			got, err := store.Update(ctx, owner, collectionadmin.My, collectionadmin.Input{ID: row.ID, UserID: uid, Name: "renamed"})
			if err != nil || got.UserID != owner.User.ID {
				t.Fatal("owner compatibility rejected", err)
			}
		}
		if _, err := store.Update(ctx, other, collectionadmin.My, collectionadmin.Input{ID: row.ID, Name: "forbidden"}); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("my modified another owner", err)
		}
		for _, change := range []string{"role", "disabled", "db-expiry", "revoke", "jwt-expiry"} {
			actor := definitionAdmin(t, f.DB, "collections-stale-"+change)
			switch change {
			case "role":
				if err := f.DB.Model(&model.User{}).Where("id=?", actor.User.ID).UpdateColumn("is_admin", false).Error; err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := f.DB.Model(&model.User{}).Where("id=?", actor.User.ID).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
					t.Fatal(err)
				}
			case "db-expiry":
				if err := f.DB.Model(&model.UserToken{}).Where("id=?", actor.Session.ID).UpdateColumn("expired_at", time.Now().Add(-time.Hour).Unix()).Error; err != nil {
					t.Fatal(err)
				}
			case "revoke":
				if err := f.DB.Delete(&model.UserToken{}, actor.Session.ID).Error; err != nil {
					t.Fatal(err)
				}
			case "jwt-expiry":
				actor.Session.ExpiresAt = time.Now().Add(-time.Hour)
			}
			before := realCollectionSnapshot(t, f.DB)
			want := auth.ErrUnauthenticated
			if change == "role" {
				want = auth.ErrForbidden
			}
			if got, err := store.Update(ctx, actor, collectionadmin.Admin, collectionadmin.Input{ID: row.ID, Name: "stale"}); !errors.Is(err, want) || got.ID != 0 {
				t.Fatal("real stale actor accepted", change, err)
			}
			if !reflect.DeepEqual(before, realCollectionSnapshot(t, f.Reopen(t))) {
				t.Fatal("stale actor retained business writes")
			}
		}
		if err := f.DB.Model(&model.User{}).Where("id=?", other.User.ID).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(ctx, admin, collectionadmin.Admin, collectionadmin.Input{UserID: other.User.ID, Name: "disabled owner"}); !errors.Is(err, collectionadmin.ErrInput) {
			t.Fatal("real disabled owner creation accepted", err)
		}
	})
}

func TestCollectionsRealReferenceAndCollectionIDChildCleanup(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		admin, owner, other := realCollectionsSetup(t, f)
		store := realCollectionsStore(t, f.DB)
		ctx := context.Background()
		row := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "target")
		sentinel := realCollectionCreate(t, f.DB, other, collectionadmin.My, 0, "sentinel")
		for _, book := range []collectionadmin.Collection{row, sentinel} {
			for _, childOwner := range []uint{book.UserID, admin.User.ID} {
				transactionCreate(t, f.DB, &model.AddressBook{UserId: childOwner, CollectionId: book.ID, Id: "device", Note: "preserve-or-delete-by-cid"})
				transactionCreate(t, f.DB, &model.Tag{UserId: childOwner, CollectionId: book.ID, Name: "tag"})
				transactionCreate(t, f.DB, &model.AddressBookCollectionRule{UserId: childOwner, CollectionId: book.ID, Type: 1, Rule: 3, ToId: other.User.ID})
			}
		}
		peer := model.Peer{Id: "referenced", Uuid: "uuid", AbId: row.ID}
		transactionCreate(t, f.DB, &peer)
		before := realCollectionSnapshot(t, f.DB)
		if err := store.Delete(ctx, owner, collectionadmin.My, collectionadmin.Input{ID: row.ID}); !errors.Is(err, collectionadmin.ErrReferenced) {
			t.Fatal("real peer reference did not block deletion", err)
		}
		if !reflect.DeepEqual(before, realCollectionSnapshot(t, f.Reopen(t))) {
			t.Fatal("referenced deletion retained child writes")
		}
		if err := f.DB.Model(&peer).Updates(map[string]any{"ab_id": 0, "preset_ab_id": row.ID}).Error; err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, owner, collectionadmin.My, collectionadmin.Input{ID: row.ID}); err != nil {
			t.Fatal("real unreferenced deletion failed", err)
		}
		after := realCollectionSnapshot(t, f.Reopen(t))
		if len(after.Books) != 1 || after.Books[0].Id != sentinel.ID || len(after.Entries) != 2 || len(after.Tags) != 2 || len(after.Rules) != 2 || after.Peers[0].PresetAbId != row.ID {
			t.Fatal("cleanup guessed owner/cache or failed to clean all matching cid")
		}
		for _, entry := range after.Entries {
			if entry.CollectionId != sentinel.ID {
				t.Fatal("wrong-owner child survived target cleanup")
			}
		}
	})
}

func TestCollectionsRealSQLAndActualCommitFailureRollback(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, owner, _ := realCollectionsSetup(t, f)
		store := realCollectionsStore(t, f.DB)
		ctx := context.Background()
		row := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "keep")
		transactionCreate(t, f.DB, &model.AddressBook{UserId: row.UserID, CollectionId: row.ID, Id: "keep", Note: "keep"})
		transactionCreate(t, f.DB, &model.Tag{UserId: row.UserID, CollectionId: row.ID, Name: "keep"})
		before := realCollectionSnapshot(t, f.DB)
		if f.Engine == "mysql" {
			transactionExec(t, f.DB, "CREATE TRIGGER collections_reject_tag BEFORE DELETE ON tags FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='private deletion cause'")
		} else {
			transactionExec(t, f.DB, "CREATE FUNCTION collections_reject_tag() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'private deletion cause'; END $$")
			transactionExec(t, f.DB, "CREATE TRIGGER collections_reject_tag BEFORE DELETE ON tags FOR EACH ROW EXECUTE FUNCTION collections_reject_tag()")
		}
		if err := store.Delete(ctx, owner, collectionadmin.My, collectionadmin.Input{ID: row.ID}); !errors.Is(err, collectionadmin.ErrUnavailable) {
			t.Fatal("real SQL child failure acknowledged", err)
		}
		if !reflect.DeepEqual(before, realCollectionSnapshot(t, f.Reopen(t))) {
			t.Fatal("real SQL failure partially deleted scope")
		}
		if f.Engine == "mysql" {
			transactionExec(t, f.DB, "DROP TRIGGER collections_reject_tag")
		} else {
			transactionExec(t, f.DB, "DROP TRIGGER collections_reject_tag ON tags")
			transactionExec(t, f.DB, "DROP FUNCTION collections_reject_tag()")
		}
		remove := failTransactionCommit(t, f, "address_book_collections")
		got, err := store.Create(ctx, owner, collectionadmin.My, collectionadmin.Input{Name: "failed commit"})
		if !errors.Is(err, collectionadmin.ErrUnavailable) || got.ID != 0 {
			t.Fatal("real failed COMMIT published collection", err)
		}
		remove()
		if !reflect.DeepEqual(before, realCollectionSnapshot(t, f.Reopen(t))) {
			t.Fatal("real failed COMMIT retained collection")
		}
		if _, err := store.Create(ctx, owner, collectionadmin.My, collectionadmin.Input{Name: "retry"}); err != nil {
			t.Fatal("real commit retry failed", err)
		}
		if err := store.Delete(ctx, owner, collectionadmin.My, collectionadmin.Input{ID: row.ID}); err != nil {
			t.Fatal("real delete retry failed", err)
		}
	})
}

func TestCollectionsRealOwnerLockWaitRechecksRevocation(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, owner, _ := realCollectionsSetup(t, f)
		row := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "keep")
		control := f.Reopen(t)
		hold := control.Begin()
		if hold.Error != nil {
			t.Fatal(hold.Error)
		}
		defer hold.Rollback()
		var user model.User
		if err := hold.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, owner.User.ID).Error; err != nil {
			t.Fatal(err)
		}
		db := f.Reopen(t)
		attempted := make(chan struct{})
		var once sync.Once
		if err := db.Callback().Query().Before("gorm:query").Register("collections:wait-user", func(tx *gorm.DB) {
			if tx.Statement.Table == "users" {
				once.Do(func() { close(attempted) })
			}
		}); err != nil {
			t.Fatal(err)
		}
		store := realCollectionsStore(t, db)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := store.Update(ctx, owner, collectionadmin.My, collectionadmin.Input{ID: row.ID, Name: "must-not-write"})
			done <- err
		}()
		select {
		case <-attempted:
		case <-ctx.Done():
			t.Fatal("store did not attempt owner lock")
		}
		if err := f.Reopen(t).Delete(&model.UserToken{}, owner.Session.ID).Error; err != nil {
			t.Fatal(err)
		}
		if err := hold.Commit().Error; err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatal("revocation during owner wait was ignored", err)
			}
		case <-ctx.Done():
			t.Fatal("owner wait never completed")
		}
		var after model.AddressBookCollection
		if err := f.Reopen(t).First(&after, row.ID).Error; err != nil || after.Name != "keep" {
			t.Fatal("wait-time revocation retained rename", err)
		}
	})
}

// First(&user, uintID) encodes the primary-key condition as IN/Eq before SQL
// execution. Inspect that condition instead of treating an actor row as owner.
func realCollectionsAttemptsOwner(tx *gorm.DB, owner uint) bool {
	if tx.Statement.Table != "users" {
		return false
	}
	if _, ok := tx.Statement.Clauses["FOR"]; !ok {
		return false
	}
	where, ok := tx.Statement.Clauses["WHERE"].Expression.(clause.Where)
	if !ok {
		return false
	}
	for _, expression := range where.Exprs {
		switch condition := expression.(type) {
		case clause.IN:
			if column, ok := condition.Column.(clause.Column); ok && (column.Name == clause.PrimaryKey || column.Name == "id") && len(condition.Values) == 1 && condition.Values[0] == owner {
				return true
			}
		case clause.Eq:
			if column, ok := condition.Column.(clause.Column); ok && (column.Name == clause.PrimaryKey || column.Name == "id") && condition.Value == owner {
				return true
			}
		}
	}
	return false
}

func TestCollectionsRealCLIAndDeleteSerializeInBothOrders(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		admin, owner, _ := realCollectionsSetup(t, f)
		for _, writerFirst := range []bool{true, false} {
			t.Run(map[bool]string{true: "writer_first", false: "delete_first"}[writerFirst], func(t *testing.T) {
				name := "delete-first"
				if writerFirst {
					name = "writer-first"
				}
				book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, name)
				peer := model.Peer{Id: name, Uuid: name, UserId: owner.User.ID, Note: "before"}
				transactionCreate(t, f.DB, &peer)
				writerDB, deleteDB := f.Reopen(t), f.Reopen(t)
				leading, following := writerDB, deleteDB
				if !writerFirst {
					leading, following = deleteDB, writerDB
				}
				locked, release, attempted, acquired := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
				var lockedOnce, attemptOnce, acquiredOnce sync.Once
				if err := leading.Callback().Query().After("gorm:query").Register("collections:hold-parent", func(tx *gorm.DB) {
					if _, ok := tx.Statement.Clauses["FOR"]; ok && tx.Statement.Table == "address_book_collections" && tx.Error == nil {
						lockedOnce.Do(func() {
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
				if err := following.Callback().Query().Before("gorm:query").Register("collections:attempt-user", func(tx *gorm.DB) {
					if realCollectionsAttemptsOwner(tx, owner.User.ID) {
						attemptOnce.Do(func() { close(attempted) })
					}
				}); err != nil {
					t.Fatal(err)
				}
				if err := following.Callback().Query().After("gorm:query").Register("collections:acquire-user", func(tx *gorm.DB) {
					user, ok := tx.Statement.Dest.(*model.User)
					if _, locking := tx.Statement.Clauses["FOR"]; locking && ok && user.Id == owner.User.ID && tx.Statement.Table == "users" && tx.Error == nil {
						acquiredOnce.Do(func() { close(acquired) })
					}
				}); err != nil {
					t.Fatal(err)
				}
				writer, err := clientassign.New(writerDB, clientassign.Options{})
				if err != nil {
					t.Fatal(err)
				}
				deleting := realCollectionsStore(t, deleteDB)
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				writerDone, deleteDone := make(chan error, 1), make(chan error, 1)
				write := func() {
					writerDone <- writer.Assign(ctx, admin, clientassign.Input{ID: peer.Id, UUID: peer.Uuid, AddressBookName: assignmentValue(book.Name), AddressBookTag: assignmentValue("new-tag"), Note: assignmentValue("writer-committed")})
				}
				del := func() {
					deleteDone <- deleting.Delete(ctx, owner, collectionadmin.My, collectionadmin.Input{ID: book.ID})
				}
				if writerFirst {
					go write()
				} else {
					go del()
				}
				select {
				case <-locked:
				case <-ctx.Done():
					close(release)
					t.Fatal("leader did not hold collection lock")
				}
				if writerFirst {
					go del()
				} else {
					go write()
				}
				select {
				case <-attempted:
				case <-ctx.Done():
					close(release)
					t.Fatal("follower did not attempt shared owner lock")
				}
				select {
				case <-acquired:
					close(release)
					t.Fatal("follower acquired user while leader held same owner")
				case <-time.After(100 * time.Millisecond):
				}
				close(release)
				var writeErr, deleteErr error
				select {
				case writeErr = <-writerDone:
				case <-ctx.Done():
					t.Fatal("writer deadlocked")
				}
				select {
				case deleteErr = <-deleteDone:
				case <-ctx.Done():
					t.Fatal("delete deadlocked")
				}
				if deleteErr != nil {
					t.Fatal("serialized delete failed", deleteErr)
				}
				if writerFirst && writeErr != nil || !writerFirst && !errors.Is(writeErr, clientassign.ErrInput) {
					t.Fatal("writer did not follow serial ordering", writeErr)
				}
				fresh := f.Reopen(t)
				var remaining int64
				for _, entity := range []any{&model.AddressBook{}, &model.Tag{}, &model.AddressBookCollectionRule{}} {
					if err := fresh.Model(entity).Where("collection_id=?", book.ID).Count(&remaining).Error; err != nil || remaining != 0 {
						t.Fatal("serialized writes left child orphan", err)
					}
				}
				if err := fresh.Model(&model.AddressBookCollection{}).Where("id=?", book.ID).Count(&remaining).Error; err != nil || remaining != 0 {
					t.Fatal("serialized delete left collection", err)
				}
				after := assignmentRead(t, fresh, peer.RowId)
				want := "before"
				if writerFirst {
					want = "writer-committed"
				}
				if after.Note != want || after.AbId != 0 {
					t.Fatal("failed writer partially changed peer or invented primary reference")
				}
			})
		}
	})
}
