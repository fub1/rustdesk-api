package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/bookadmin"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientassign"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionadmin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func realBookStore(t *testing.T, db *gorm.DB) *bookadmin.Store {
	t.Helper()
	s, err := bookadmin.New(db, bookadmin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func realBookTag(t *testing.T, db *gorm.DB, uid, cid uint, name string, color uint) model.Tag {
	t.Helper()
	row := model.Tag{UserId: uid, CollectionId: cid, Name: name, Color: color}
	transactionCreate(t, db, &row)
	return row
}
func realBookEntry(t *testing.T, db *gorm.DB, uid, cid uint, id, tags string) model.AddressBook {
	t.Helper()
	row := model.AddressBook{UserId: uid, CollectionId: cid, Id: id, Tags: custom_types.AutoJson(tags), Note: "independent note", Alias: "independent alias", Password: "private fixture value"}
	transactionCreate(t, db, &row)
	return row
}

// Raw SQL values are authoritative for NULL/corrupt JSON. AutoJson.Scan alone
// would normalize some invalid stored shapes and weaken rollback assertions.
type realBookState struct {
	Collections realCollectionState
	RawTags     []struct {
		RowID uint
		Tags  sql.NullString
	}
}

func realBookSnapshot(t *testing.T, db *gorm.DB) realBookState {
	t.Helper()
	s := realBookState{Collections: realCollectionSnapshot(t, db)}
	if err := db.Table("address_books").Select("row_id,tags").Order("row_id").Scan(&s.RawTags).Error; err != nil {
		t.Fatal(err)
	}
	return s
}
func realBookNoChange(t *testing.T, db *gorm.DB, before realBookState) {
	t.Helper()
	if !reflect.DeepEqual(before, realBookSnapshot(t, db)) {
		t.Fatal("rejected transaction retained business changes")
	}
}
func realBookRawEntry(t *testing.T, db *gorm.DB, id uint) map[string]any {
	t.Helper()
	var row map[string]any
	if err := db.Table("address_books").Where("row_id=?", id).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}
func realBookMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRealBookManagementPermissionsAndCurrentSession(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		admin, owner, other := realCollectionsSetup(t, f)
		ctx := context.Background()
		s := realBookStore(t, f.DB)
		book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "owned")
		foreign := realCollectionCreate(t, f.DB, other, collectionadmin.My, 0, "foreign")
		tag := realBookTag(t, f.DB, owner.User.ID, book.ID, "old", 1)
		rule := model.AddressBookCollectionRule{UserId: owner.User.ID, CollectionId: book.ID, Type: 1, Rule: 3, ToId: other.User.ID}
		transactionCreate(t, f.DB, &rule)
		// Explicit full control remains client-content permission, not metadata authority.
		before := realBookSnapshot(t, f.DB)
		for _, op := range []func() error{
			func() error {
				return s.CreateTag(ctx, other, bookadmin.My, bookadmin.TagInput{CollectionID: book.ID, Name: "forbidden"})
			},
			func() error {
				return s.UpdateTag(ctx, other, bookadmin.My, bookadmin.TagInput{ID: tag.Id, Name: "forbidden"})
			},
			func() error { return s.DeleteTag(ctx, other, bookadmin.My, bookadmin.TagInput{ID: tag.Id}) },
			func() error {
				return s.CreateRule(ctx, other, bookadmin.My, bookadmin.RuleInput{CollectionID: book.ID, Type: 1, Rule: 1, ToID: admin.User.ID})
			},
			func() error {
				return s.UpdateRule(ctx, other, bookadmin.My, bookadmin.RuleInput{ID: rule.Id, Type: 1, Rule: 1, ToID: admin.User.ID})
			},
			func() error { return s.DeleteRule(ctx, other, bookadmin.My, bookadmin.RuleInput{ID: rule.Id}) },
		} {
			err := op()
			if !errors.Is(err, auth.ErrForbidden) && !errors.Is(err, bookadmin.ErrInput) {
				t.Fatal("fullcontrol acquired management permission", err)
			}
			realBookNoChange(t, f.Reopen(t), before)
		}
		for _, input := range []bookadmin.TagInput{{ID: tag.Id, UserID: other.User.ID, Name: "move"}, {ID: tag.Id, CollectionID: foreign.ID, Name: "move"}} {
			if err := s.UpdateTag(ctx, admin, bookadmin.Admin, input); !errors.Is(err, bookadmin.ErrInput) {
				t.Fatal("tag moved scope", err)
			}
			realBookNoChange(t, f.DB, before)
		}
		for _, input := range []bookadmin.RuleInput{{ID: rule.Id, UserID: other.User.ID, Type: 1, Rule: 1, ToID: admin.User.ID}, {ID: rule.Id, CollectionID: foreign.ID, Type: 1, Rule: 1, ToID: admin.User.ID}} {
			if err := s.UpdateRule(ctx, admin, bookadmin.Admin, input); !errors.Is(err, bookadmin.ErrInput) {
				t.Fatal("rule moved scope", err)
			}
			realBookNoChange(t, f.DB, before)
		}
		// The seed user really has primary key 1; demote that stored row and
		// issue a fixture session so the legacy owner=1 exception is exercised.
		var one model.User
		realBookMust(t, f.DB.First(&one, 1).Error)
		realBookMust(t, f.DB.Model(&one).UpdateColumn("is_admin", false).Error)
		end := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
		token := model.UserToken{UserId: 1, Token: "book-owner-one-session", ExpiredAt: end.Unix()}
		transactionCreate(t, f.DB, &token)
		ordinary := auth.Principal{User: auth.User{ID: 1, Enabled: true}, Session: auth.Session{ID: token.Id, UserID: 1, Token: token.Token, ExpiresAt: end}}
		if err := s.UpdateTag(ctx, ordinary, bookadmin.My, bookadmin.TagInput{ID: tag.Id, Name: "owner-one-exception"}); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("owner=1 nonowner received implicit exception", err)
		}
		realBookNoChange(t, f.DB, before)
		realBookMust(t, s.CreateTag(ctx, admin, bookadmin.Admin, bookadmin.TagInput{CollectionID: book.ID, Name: "derived", Color: 8}))
		var derived model.Tag
		realBookMust(t, f.DB.Where("name=?", "derived").First(&derived).Error)
		if derived.UserId != owner.User.ID || derived.CollectionId != book.ID {
			t.Fatal("admin create did not derive real parent owner")
		}
		realBookMust(t, s.CreateRule(ctx, admin, bookadmin.Admin, bookadmin.RuleInput{CollectionID: book.ID, Type: 1, Rule: 2, ToID: admin.User.ID}))
		var made model.AddressBookCollectionRule
		realBookMust(t, f.DB.Where("collection_id=? AND to_id=?", book.ID, admin.User.ID).First(&made).Error)
		if made.UserId != owner.User.ID {
			t.Fatal("admin rule create guessed owner")
		}
		// Mutate only current storage, keeping the old cached principal unchanged.
		before = realBookSnapshot(t, f.DB)
		realBookMust(t, f.DB.Delete(&model.UserToken{}, owner.Session.ID).Error)
		for _, op := range []func() error{
			func() error {
				return s.CreateTag(ctx, owner, bookadmin.My, bookadmin.TagInput{CollectionID: book.ID, Name: "stale"})
			},
			func() error {
				return s.UpdateTag(ctx, owner, bookadmin.My, bookadmin.TagInput{ID: tag.Id, Name: "stale"})
			},
			func() error { return s.DeleteTag(ctx, owner, bookadmin.My, bookadmin.TagInput{ID: tag.Id}) },
			func() error {
				return s.CreateRule(ctx, owner, bookadmin.My, bookadmin.RuleInput{CollectionID: book.ID, Type: 1, Rule: 2, ToID: admin.User.ID})
			},
			func() error {
				return s.UpdateRule(ctx, owner, bookadmin.My, bookadmin.RuleInput{ID: rule.Id, Type: 1, Rule: 2, ToID: admin.User.ID})
			},
			func() error { return s.DeleteRule(ctx, owner, bookadmin.My, bookadmin.RuleInput{ID: rule.Id}) },
		} {
			if err := op(); !errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatal("revoked PK session accepted", err)
			}
			realBookNoChange(t, f.Reopen(t), before)
		}
	})
}

func TestRealBookManagementExactTagsAndNarrowWrites(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		admin, owner, other := realCollectionsSetup(t, f)
		ctx := context.Background()
		s := realBookStore(t, f.DB)
		book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "tags")
		a := realBookTag(t, f.DB, owner.User.ID, book.ID, "old", 1)
		b := realBookTag(t, f.DB, owner.User.ID, book.ID, "old", 2)
		exact := []string{"Old", "old ", "öld"}
		for _, name := range exact {
			realBookTag(t, f.DB, owner.User.ID, book.ID, name, 3)
		}
		personal := realBookTag(t, f.DB, owner.User.ID, 0, "old", 4)
		foreign := realBookTag(t, f.DB, other.User.ID, book.ID, "old", 5)
		changed := realBookEntry(t, f.DB, owner.User.ID, book.ID, "changed", `["new","old","keep","old","new"]`)
		untouched := realBookEntry(t, f.DB, owner.User.ID, book.ID, "untouched", `["new","new","keep"]`)
		outside := realBookEntry(t, f.DB, other.User.ID, book.ID, "outside", `["old"]`)
		before := realBookRawEntry(t, f.DB, changed.RowId)
		sentinel := realBookRawEntry(t, f.DB, untouched.RowId)
		foreignEntry := realBookRawEntry(t, f.DB, outside.RowId)
		realBookMust(t, s.UpdateTag(ctx, owner, bookadmin.My, bookadmin.TagInput{ID: a.Id, Name: "new", Color: 9}))
		after := realBookRawEntry(t, f.DB, changed.RowId)
		var raw string
		realBookMust(t, f.DB.Table("address_books").Select("tags").Where("row_id=?", changed.RowId).Scan(&raw).Error)
		if raw != `["new","keep"]` {
			t.Fatal("duplicate tag references were not rewritten exactly")
		}
		before["tags"] = after["tags"]
		if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(sentinel, realBookRawEntry(t, f.DB, untouched.RowId)) || !reflect.DeepEqual(foreignEntry, realBookRawEntry(t, f.DB, outside.RowId)) {
			t.Fatal("tag rewrite changed note/time/other metadata or crossed scope")
		}
		var tags []model.Tag
		realBookMust(t, f.DB.Order("id").Find(&tags).Error)
		for _, tag := range tags {
			switch tag.Id {
			case a.Id:
				if tag.Name != "new" || tag.Color != 9 {
					t.Fatal("chosen duplicate mismatch")
				}
			case b.Id:
				if tag.Name != "new" || tag.Color != 2 {
					t.Fatal("other duplicate color changed")
				}
			case personal.Id, foreign.Id:
				if tag.Name != "old" {
					t.Fatal("scope escaped")
				}
			}
		}
		beforeColor := realBookRawEntry(t, f.DB, changed.RowId)
		realBookMust(t, s.UpdateTag(ctx, admin, bookadmin.Admin, bookadmin.TagInput{ID: a.Id, Name: "new", Color: 0}))
		var colored, duplicate model.Tag
		realBookMust(t, f.DB.First(&colored, a.Id).Error)
		realBookMust(t, f.DB.First(&duplicate, b.Id).Error)
		if colored.Color != 0 || duplicate.Color != 2 || !reflect.DeepEqual(beforeColor, realBookRawEntry(t, f.DB, changed.RowId)) {
			t.Fatal("color-only write touched duplicate definition or address-book metadata")
		}

		realBookMust(t, s.DeleteTag(ctx, owner, bookadmin.My, bookadmin.TagInput{ID: b.Id}))
		var count int64
		realBookMust(t, f.DB.Model(&model.Tag{}).Where("id IN ?", []uint{a.Id, b.Id}).Count(&count).Error)
		if count != 0 {
			t.Fatal("historical duplicate definitions survived delete")
		}
		realBookMust(t, f.DB.Table("address_books").Select("tags").Where("row_id=?", changed.RowId).Scan(&raw).Error)
		if raw != `["keep"]` {
			t.Fatal("duplicate definition delete left references")
		}
		var retained []model.Tag
		realBookMust(t, f.DB.Where("user_id=? AND collection_id=?", owner.User.ID, book.ID).Find(&retained).Error)
		names := map[string]bool{}
		for _, row := range retained {
			names[row.Name] = true
		}
		if len(retained) != len(exact) {
			t.Fatal("tag delete removed byte-distinct definitions or retained old duplicates")
		}
		for _, name := range exact {
			if !names[name] {
				t.Fatal("byte-distinct tag was removed")
			}
		}

	})
}

func TestRealBookManagementCorruptRawTagsAndRules(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		admin, owner, other := realCollectionsSetup(t, f)
		ctx := context.Background()
		s := realBookStore(t, f.DB)
		book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "rules")
		tag := realBookTag(t, f.DB, owner.User.ID, book.ID, "old", 1)
		realBookEntry(t, f.DB, owner.User.ID, book.ID, "good", `["old"]`)
		bad := realBookEntry(t, f.DB, owner.User.ID, book.ID, "bad", `[]`)
		if f.Engine == "mysql" {
			transactionExec(t, f.DB, "ALTER TABLE address_books MODIFY COLUMN tags LONGTEXT NULL")
		} else {
			transactionExec(t, f.DB, "ALTER TABLE address_books ALTER COLUMN tags DROP NOT NULL")
		}
		for _, raw := range []any{nil, "null", `{"old":true}`, `["old",1]`, `[`} {
			realBookMust(t, f.DB.Model(&model.AddressBook{}).Where("row_id=?", bad.RowId).UpdateColumn("tags", raw).Error)
			before := realBookSnapshot(t, f.DB)
			for _, op := range []func() error{func() error {
				return s.UpdateTag(ctx, owner, bookadmin.My, bookadmin.TagInput{ID: tag.Id, Name: "new"})
			}, func() error { return s.DeleteTag(ctx, owner, bookadmin.My, bookadmin.TagInput{ID: tag.Id}) }} {
				if err := op(); !errors.Is(err, bookadmin.ErrUnavailable) {
					t.Fatal("corrupt stored tags accepted", err)
				}
				realBookNoChange(t, f.Reopen(t), before)
			}
		}
		realBookMust(t, f.DB.Model(&model.AddressBook{}).Where("row_id=?", bad.RowId).UpdateColumn("tags", "[]").Error)
		for _, input := range []bookadmin.RuleInput{
			{CollectionID: book.ID, Type: 0, Rule: 1, ToID: other.User.ID}, {CollectionID: book.ID, Type: 3, Rule: 1, ToID: other.User.ID},
			{CollectionID: book.ID, Type: 1, Rule: 0, ToID: other.User.ID}, {CollectionID: book.ID, Type: 1, Rule: 4, ToID: other.User.ID},
			{CollectionID: book.ID, Type: 1, Rule: 1, ToID: 0}, {CollectionID: book.ID, Type: 1, Rule: 1, ToID: owner.User.ID},
			{CollectionID: book.ID, Type: 1, Rule: 1, ToID: 999999}, {CollectionID: book.ID, Type: 2, Rule: 1, ToID: 999999},
		} {
			before := realBookSnapshot(t, f.DB)
			if err := s.CreateRule(ctx, owner, bookadmin.My, input); !errors.Is(err, bookadmin.ErrInput) {
				t.Fatal("invalid rule accepted", err)
			}
			realBookNoChange(t, f.DB, before)
		}
		realBookMust(t, s.CreateRule(ctx, owner, bookadmin.My, bookadmin.RuleInput{CollectionID: book.ID, Type: 1, Rule: 1, ToID: other.User.ID}))
		var rule model.AddressBookCollectionRule
		realBookMust(t, f.DB.Where("collection_id=? AND type=1", book.ID).First(&rule).Error)
		before := realBookSnapshot(t, f.DB)
		if err := s.CreateRule(ctx, admin, bookadmin.Admin, bookadmin.RuleInput{CollectionID: book.ID, Type: 1, Rule: 3, ToID: other.User.ID}); !errors.Is(err, bookadmin.ErrInput) {
			t.Fatal("same-scope duplicate rule accepted", err)
		}
		realBookNoChange(t, f.DB, before)
		realBookMust(t, s.UpdateRule(ctx, owner, bookadmin.My, bookadmin.RuleInput{ID: rule.Id, Type: 1, Rule: 2, ToID: other.User.ID}))
		realBookMust(t, s.UpdateRule(ctx, owner, bookadmin.My, bookadmin.RuleInput{ID: rule.Id, Type: 2, Rule: 3, ToID: 1}))
		realBookMust(t, s.DeleteRule(ctx, owner, bookadmin.My, bookadmin.RuleInput{ID: rule.Id}))
	})
}

// The only participating AB row is updated last in the tag rewrite. PostgreSQL
// defers a real FK failure until COMMIT; MySQL kills this exact transaction's
// connection only after its final UPDATE succeeded, without injecting AddError.
func realBookFailCommit(t *testing.T, f *fixture) func() {
	t.Helper()
	if f.Engine == "postgres" {
		transactionExec(t, f.DB, "CREATE TABLE book_commit_parent(id bigint PRIMARY KEY)")
		transactionExec(t, f.DB, "CREATE TABLE book_commit_guard(parent_id bigint REFERENCES book_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)")
		transactionExec(t, f.DB, "CREATE FUNCTION book_commit_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO book_commit_guard(parent_id) VALUES(999999); RETURN NEW; END $$")
		transactionExec(t, f.DB, "CREATE TRIGGER book_commit_fault AFTER UPDATE OF tags ON address_books FOR EACH ROW EXECUTE FUNCTION book_commit_fault()")
		var reached atomic.Bool
		realBookMust(t, f.DB.Callback().Update().After("gorm:update").Register("book:deferred-update-reached", func(tx *gorm.DB) {
			if tx.Statement.Table == "address_books" && tx.Error == nil && tx.RowsAffected == 1 {
				reached.Store(true)
			}
		}))
		return func() {
			if !reached.Load() {
				t.Error("final AB UPDATE did not succeed before native COMMIT failure")
			}
			realBookMust(t, f.DB.Callback().Update().Remove("book:deferred-update-reached"))
			transactionExec(t, f.DB, "DROP TRIGGER book_commit_fault ON address_books")
			transactionExec(t, f.DB, "DROP FUNCTION book_commit_fault()")
			transactionExec(t, f.DB, "DROP TABLE book_commit_guard")
			transactionExec(t, f.DB, "DROP TABLE book_commit_parent")
		}
	}
	control := f.Reopen(t)
	var fired, killed atomic.Bool
	realBookMust(t, f.DB.Callback().Update().After("gorm:update").Register("book:kill-last-update", func(tx *gorm.DB) {
		if tx.Statement.Table != "address_books" || tx.Error != nil || tx.RowsAffected != 1 || !fired.CompareAndSwap(false, true) {
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
	}))
	return func() {
		if !fired.Load() || !killed.Load() {
			t.Error("final AB UPDATE was not reached before connection kill")
		}
		realBookMust(t, f.DB.Callback().Update().Remove("book:kill-last-update"))
	}
}
func TestRealBookManagementNativeSQLAndCommitRollbackRetry(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, owner, _ := realCollectionsSetup(t, f)
		ctx := context.Background()
		s := realBookStore(t, f.DB)
		book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "fault")
		tag := realBookTag(t, f.DB, owner.User.ID, book.ID, "old", 1)
		realBookTag(t, f.DB, owner.User.ID, book.ID, "old", 2)
		row := realBookEntry(t, f.DB, owner.User.ID, book.ID, "only-participant", `["old","keep"]`)
		before := realBookSnapshot(t, f.DB)
		if f.Engine == "mysql" {
			transactionExec(t, f.DB, "CREATE TRIGGER book_reject_update BEFORE UPDATE ON address_books FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='private fixture cause'")
		} else {
			transactionExec(t, f.DB, "CREATE FUNCTION book_reject_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'private fixture cause'; END $$")
			transactionExec(t, f.DB, "CREATE TRIGGER book_reject_update BEFORE UPDATE ON address_books FOR EACH ROW EXECUTE FUNCTION book_reject_update()")
		}
		change := func() error {
			return s.UpdateTag(ctx, owner, bookadmin.My, bookadmin.TagInput{ID: tag.Id, Name: "new", Color: 9})
		}
		if err := change(); !errors.Is(err, bookadmin.ErrUnavailable) {
			t.Fatal("native SQL failure was acknowledged", err)
		}
		realBookNoChange(t, f.Reopen(t), before)
		if f.Engine == "mysql" {
			transactionExec(t, f.DB, "DROP TRIGGER book_reject_update")
		} else {
			transactionExec(t, f.DB, "DROP TRIGGER book_reject_update ON address_books")
			transactionExec(t, f.DB, "DROP FUNCTION book_reject_update()")
		}
		remove := realBookFailCommit(t, f)
		err := change()
		remove()
		if !errors.Is(err, bookadmin.ErrUnavailable) {
			t.Fatal("actual COMMIT failure was acknowledged", err)
		}
		realBookNoChange(t, f.Reopen(t), before)
		realBookMust(t, change())
		after := realBookRawEntry(t, f.DB, row.RowId)
		var raw string
		realBookMust(t, f.DB.Table("address_books").Select("tags").Where("row_id=?", row.RowId).Scan(&raw).Error)
		if raw != `["new","keep"]` || after["note"] != "independent note" {
			t.Fatal("retry did not apply only intended tag rewrite")
		}
	})
}

func realBookWait(t *testing.T, ctx context.Context, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(message)
	}
}
func realBookResult(t *testing.T, ctx context.Context, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		t.Fatal("concurrent operation did not finish")
		return ctx.Err()
	}
}
func realBookNativeWait(t *testing.T, f *fixture, ctx context.Context, table string) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		var n int64
		var err error
		if f.Engine == "mysql" {
			err = f.DB.WithContext(ctx).Raw("SELECT count(*) FROM performance_schema.data_locks WHERE OBJECT_SCHEMA=DATABASE() AND OBJECT_NAME=? AND LOCK_STATUS='WAITING'", table).Scan(&n).Error
		} else {
			err = f.DB.WithContext(ctx).Raw("SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE ?", "%"+table+"%").Scan(&n).Error
		}
		if err != nil {
			t.Fatal("native lock observer unavailable (details omitted)")
		}
		if n > 0 {
			t.Log("confirmed native lock wait on", table)
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("follower never reached native lock wait on", table)
}
func realBookIsLockedTable(tx *gorm.DB, table string) bool {
	_, ok := tx.Statement.Clauses["FOR"]
	return ok && tx.Statement.Table == table
}

func TestRealBookManagementHistoricalZeroOwnerRenameSerializes(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		admin, _, _ := realCollectionsSetup(t, f)
		second := definitionAdmin(t, f.DB, "second-zero-admin")
		a := realBookTag(t, f.DB, 0, 0, "old-a", 1)
		b := realBookTag(t, f.DB, 0, 0, "old-b", 2)
		entryA := realBookEntry(t, f.DB, 0, 0, "zero-a", `["old-a"]`)
		entryB := realBookEntry(t, f.DB, 0, 0, "zero-b", `["old-b"]`)
		beforeA, beforeB := realBookRawEntry(t, f.DB, entryA.RowId), realBookRawEntry(t, f.DB, entryB.RowId)
		leading, following := f.Reopen(t), f.Reopen(t)
		locked, release, attempted := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once, attempt sync.Once
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		var releaseOnce sync.Once
		releaseLeader := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseLeader()
		realBookMust(t, leading.Callback().Query().After("gorm:query").Register("book:hold-whole-zero-scope", func(tx *gorm.DB) {
			rows, ok := tx.Statement.Dest.(*[]model.Tag)
			if realBookIsLockedTable(tx, "tags") && ok && len(*rows) == 2 && tx.Error == nil {
				once.Do(func() {
					close(locked)
					select {
					case <-release:
					case <-tx.Statement.Context.Done():
						tx.AddError(tx.Statement.Context.Err())
					}
				})
			}
		}))
		realBookMust(t, following.Callback().Query().Before("gorm:query").Register("book:wait-whole-zero-scope", func(tx *gorm.DB) {
			if realBookIsLockedTable(tx, "tags") {
				attempt.Do(func() { close(attempted) })
			}
		}))
		first, secondDone := make(chan error, 1), make(chan error, 1)
		s1, s2 := realBookStore(t, leading), realBookStore(t, following)
		go func() {
			first <- s1.UpdateTag(ctx, admin, bookadmin.Admin, bookadmin.TagInput{ID: a.Id, Name: "target", Color: 9})
		}()
		realBookWait(t, ctx, locked, "leader did not hold all existing zero-owner definitions")
		go func() {
			secondDone <- s2.UpdateTag(ctx, second, bookadmin.Admin, bookadmin.TagInput{ID: b.Id, Name: "target", Color: 8})
		}()
		realBookWait(t, ctx, attempted, "second rename did not attempt definitions")
		realBookNativeWait(t, f, ctx, "tags")
		releaseLeader()
		realBookMust(t, realBookResult(t, ctx, first))
		if err := realBookResult(t, ctx, secondDone); !errors.Is(err, bookadmin.ErrInput) {
			t.Fatal("zero-owner duplicate target race not rejected", err)
		}
		var rows []model.Tag
		realBookMust(t, f.DB.Where("user_id=0 AND collection_id=0").Order("id").Find(&rows).Error)
		if len(rows) != 2 || rows[0].Name != "target" || rows[1].Name != "old-b" {
			t.Fatal("zero-owner rename did not linearize")
		}
		afterA, afterB := realBookRawEntry(t, f.DB, entryA.RowId), realBookRawEntry(t, f.DB, entryB.RowId)
		var rewritten string
		realBookMust(t, f.DB.Table("address_books").Select("tags").Where("row_id=?", entryA.RowId).Scan(&rewritten).Error)
		if rewritten != `["target"]` {
			t.Fatal("winning zero-owner rename did not rewrite its references")
		}
		beforeA["tags"] = afterA["tags"]
		if !reflect.DeepEqual(beforeA, afterA) || !reflect.DeepEqual(beforeB, afterB) {
			t.Fatal("losing zero-owner rename retained writes or metadata changed")
		}
		before := realBookSnapshot(t, f.DB)
		if err := s1.CreateTag(ctx, admin, bookadmin.Admin, bookadmin.TagInput{Name: "new-zero"}); !errors.Is(err, bookadmin.ErrInput) {
			t.Fatal("new owner-zero child created", err)
		}
		realBookNoChange(t, f.DB, before)
	})
}

// Operations sharing a nonzero owner must wait on that exact row, rather than
// merely observing an unrelated lower-ID administrator lock.
func realBookOwnerBarrier(t *testing.T, leading, following *gorm.DB, owner uint) (<-chan struct{}, <-chan struct{}, func()) {
	t.Helper()
	locked, attempted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once, attempt, released sync.Once
	realBookMust(t, leading.Callback().Query().After("gorm:query").Register("book:hold-common-owner", func(tx *gorm.DB) {
		if realCollectionsAttemptsOwner(tx, owner) && tx.Error == nil {
			once.Do(func() {
				close(locked)
				select {
				case <-release:
				case <-tx.Statement.Context.Done():
					tx.AddError(tx.Statement.Context.Err())
				}
			})
		}
	}))
	realBookMust(t, following.Callback().Query().Before("gorm:query").Register("book:attempt-common-owner", func(tx *gorm.DB) {
		if realCollectionsAttemptsOwner(tx, owner) {
			attempt.Do(func() { close(attempted) })
		}
	}))
	return locked, attempted, func() { released.Do(func() { close(release) }) }
}

func TestRealBookManagementRenameAndClientWritersSerialize(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		admin, owner, _ := realCollectionsSetup(t, f)
		realBookMust(t, f.DB.Model(&model.User{}).Where("id=?", owner.User.ID).UpdateColumn("group_id", 1).Error)
		for _, kind := range []string{"native_rename", "cli_append"} {
			t.Run(kind, func(t *testing.T) {
				book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, kind)
				tag := realBookTag(t, f.DB, owner.User.ID, book.ID, "old", 1)
				entry := realBookEntry(t, f.DB, owner.User.ID, book.ID, kind, `["old"]`)
				peer := model.Peer{Id: kind, Uuid: kind, UserId: owner.User.ID}
				transactionCreate(t, f.DB, &peer)
				leading, following := f.Reopen(t), f.Reopen(t)
				locked, attempted, release := realBookOwnerBarrier(t, leading, following, owner.User.ID)
				defer release()
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				first, second := make(chan error, 1), make(chan error, 1)
				management := realBookStore(t, leading)
				go func() {
					first <- management.UpdateTag(ctx, owner, bookadmin.My, bookadmin.TagInput{ID: tag.Id, Name: "new", Color: 7})
				}()
				realBookWait(t, ctx, locked, "management did not hold owner")
				if kind == "native_rename" {
					native, err := clientcompat.New(following)
					realBookMust(t, err)
					go func() {
						second <- native.RenameTag(ctx, clientcompat.Actor{ID: owner.User.ID}, fmt.Sprintf("1-%d-%d", owner.User.ID, book.ID), "new", "final")
					}()
				} else {
					cli, err := clientassign.New(following, clientassign.Options{})
					realBookMust(t, err)
					go func() {
						second <- cli.Assign(ctx, admin, clientassign.Input{ID: peer.Id, UUID: peer.Uuid, AddressBookName: assignmentValue(book.Name), AddressBookTag: assignmentValue("new")})
					}()
				}
				realBookWait(t, ctx, attempted, "client writer did not attempt same owner")
				realBookNativeWait(t, f, ctx, "users")
				release()
				realBookMust(t, realBookResult(t, ctx, first))
				realBookMust(t, realBookResult(t, ctx, second))
				var got model.Tag
				realBookMust(t, f.DB.First(&got, tag.Id).Error)
				want := "new"
				if kind == "native_rename" {
					want = "final"
				}
				if got.Name != want {
					t.Fatal("client writer ignored committed management rename")
				}
				var raw string
				realBookMust(t, f.DB.Table("address_books").Select("tags").Where("row_id=?", entry.RowId).Scan(&raw).Error)
				if raw != fmt.Sprintf("[\"%s\"]", want) {
					t.Fatal("scope references did not follow serial rename")
				}
			})
		}
	})
}

func TestRealBookManagementCreateAndCollectionDeleteBothOrders(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, owner, other := realCollectionsSetup(t, f)
		for _, kind := range []string{"tag", "rule"} {
			for _, createFirst := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/create_first_%v", kind, createFirst), func(t *testing.T) {
					book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, fmt.Sprintf("%s-%v", kind, createFirst))
					creatorDB, deleteDB := f.Reopen(t), f.Reopen(t)
					leading, following := creatorDB, deleteDB
					if !createFirst {
						leading, following = deleteDB, creatorDB
					}
					locked, attempted, release := realBookOwnerBarrier(t, leading, following, owner.User.ID)
					defer release()
					ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
					defer cancel()
					creator := realBookStore(t, creatorDB)
					deleter := realCollectionsStore(t, deleteDB)
					created, deleted := make(chan error, 1), make(chan error, 1)
					create := func() {
						if kind == "tag" {
							created <- creator.CreateTag(ctx, owner, bookadmin.My, bookadmin.TagInput{CollectionID: book.ID, Name: "new"})
						} else {
							created <- creator.CreateRule(ctx, owner, bookadmin.My, bookadmin.RuleInput{CollectionID: book.ID, Type: 1, Rule: 3, ToID: other.User.ID})
						}
					}
					del := func() { deleted <- deleter.Delete(ctx, owner, collectionadmin.My, collectionadmin.Input{ID: book.ID}) }
					if createFirst {
						go create()
					} else {
						go del()
					}
					realBookWait(t, ctx, locked, "leader did not hold owner")
					if createFirst {
						go del()
					} else {
						go create()
					}
					realBookWait(t, ctx, attempted, "follower did not attempt same owner")
					realBookNativeWait(t, f, ctx, "users")
					release()
					createErr, deleteErr := realBookResult(t, ctx, created), realBookResult(t, ctx, deleted)
					realBookMust(t, deleteErr)
					if createFirst {
						realBookMust(t, createErr)
					} else if !errors.Is(createErr, bookadmin.ErrNotFound) {
						t.Fatal("create after collection removal was not rejected", createErr)
					}
					var n int64
					for _, entity := range []any{&model.Tag{}, &model.AddressBookCollectionRule{}, &model.AddressBook{}} {
						realBookMust(t, f.DB.Model(entity).Where("collection_id=?", book.ID).Count(&n).Error)
						if n != 0 {
							t.Fatal("serialized create/delete left orphan")
						}
					}
					realBookMust(t, f.DB.Model(&model.AddressBookCollection{}).Where("id=?", book.ID).Count(&n).Error)
					if n != 0 {
						t.Fatal("parent survived delete")
					}
				})
			}
		}
	})
}

func TestRealBookManagementOwnerWaitRechecksSessionPK(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, owner, _ := realCollectionsSetup(t, f)
		book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "revoke-wait")
		tag := realBookTag(t, f.DB, owner.User.ID, book.ID, "keep", 1)
		hold := f.Reopen(t).Begin()
		if hold.Error != nil {
			t.Fatal(hold.Error)
		}
		defer hold.Rollback()
		var user model.User
		realBookMust(t, hold.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, owner.User.ID).Error)
		db := f.Reopen(t)
		attempted := make(chan struct{})
		var once sync.Once
		realBookMust(t, db.Callback().Query().Before("gorm:query").Register("book:revocation-attempt-owner", func(tx *gorm.DB) {
			if realCollectionsAttemptsOwner(tx, owner.User.ID) {
				once.Do(func() { close(attempted) })
			}
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		done := make(chan error, 1)
		s := realBookStore(t, db)
		before := realBookSnapshot(t, f.DB)
		go func() {
			done <- s.UpdateTag(ctx, owner, bookadmin.My, bookadmin.TagInput{ID: tag.Id, Name: "must-not-write"})
		}()
		realBookWait(t, ctx, attempted, "writer did not attempt owner")
		realBookNativeWait(t, f, ctx, "users")
		realBookMust(t, f.Reopen(t).Delete(&model.UserToken{}, owner.Session.ID).Error)
		realBookMust(t, hold.Commit().Error)
		if err := realBookResult(t, ctx, done); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatal("revocation during owner wait was ignored", err)
		}
		realBookNoChange(t, f.Reopen(t), before)
	})
}
