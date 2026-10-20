package collectionguard_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/collectionguard"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func fixture(t *testing.T) (*gorm.DB, uint) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "guard.db")+"?_txlock=immediate&_busy_timeout=5000"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&model.Peer{}, &model.AddressBookCollection{}, &model.AddressBook{}, &model.Tag{}, &model.AddressBookCollectionRule{}); err != nil {
		t.Fatal(err)
	}
	book := model.AddressBookCollection{UserId: 7, Name: "target"}
	if err := db.Create(&book).Error; err != nil {
		t.Fatal(err)
	}
	for _, cid := range []uint{0, book.Id, book.Id + 99} {
		for _, row := range []any{&model.Tag{UserId: 7, CollectionId: cid, Name: "tag"}, &model.AddressBook{Id: "device", UserId: 7, CollectionId: cid, Tags: custom_types.AutoJson(`[]`)}, &model.AddressBookCollectionRule{UserId: 7, CollectionId: cid, Rule: 3, Type: 1, ToId: 9}} {
			if err := db.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Exec("INSERT INTO peers (id,uuid,user_id,alias,note) VALUES ('device','uuid',9,'preserve-alias','preserve-note')").Error; err != nil {
		t.Fatal(err)
	}
	return db, book.Id
}

func snapshot(t *testing.T, db *gorm.DB) []any {
	t.Helper()
	var books []model.AddressBookCollection
	var tags []model.Tag
	var entries []model.AddressBook
	var rules []model.AddressBookCollectionRule
	var peers []model.Peer
	for _, rows := range []any{&books, &tags, &entries, &rules, &peers} {
		if err := db.Find(rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	return []any{books, tags, entries, rules, peers}
}

func TestCollectionGuardPrimaryReferenceIsGlobalAndCachesDoNotBlock(t *testing.T) {
	db, cid := fixture(t)
	if err := db.Exec("UPDATE peers SET ab_id=?,preset_ab_id=?", cid, cid).Error; err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, db)
	if err := db.Transaction(func(tx *gorm.DB) error { return collectionguard.EnsureUnreferenced(tx, []uint{cid, cid}) }); !errors.Is(err, collectionguard.ErrReferenced) {
		t.Fatal("another owner's primary reference ignored", err)
	}
	if !reflect.DeepEqual(before, snapshot(t, db)) {
		t.Fatal("reference check changed data")
	}
	if err := db.Exec("UPDATE peers SET ab_id=0").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := collectionguard.EnsureUnreferenced(tx, []uint{cid}); err != nil {
			return err
		}
		return collectionguard.DeleteContents(tx, cid)
	}); err != nil {
		t.Fatal("inactive preset cache blocked primary lifecycle", err)
	}
	for _, row := range []any{&model.AddressBook{}, &model.Tag{}, &model.AddressBookCollectionRule{}} {
		var count int64
		if err := db.Model(row).Where("collection_id=?", cid).Count(&count).Error; err != nil || count != 0 {
			t.Fatal("target child remains", err)
		}
		if err := db.Model(row).Count(&count).Error; err != nil || count != 2 {
			t.Fatal("delete changed personal/another collection scope", err)
		}
	}
	var peer model.Peer
	if err := db.First(&peer).Error; err != nil || peer.Alias != "preserve-alias" || peer.PresetAbId != cid || peer.AbId != 0 {
		t.Fatal("delete rewrote peer/alias/cache", err)
	}
}

func TestCollectionGuardEveryDeleteFailureRollsBackAndRetry(t *testing.T) {
	for _, table := range []string{"address_book_collection_rules", "address_books", "tags", "address_book_collections"} {
		t.Run(table, func(t *testing.T) {
			db, cid := fixture(t)
			before := snapshot(t, db)
			if err := db.Exec(fmt.Sprintf("CREATE TRIGGER fail_guard BEFORE DELETE ON %s BEGIN SELECT RAISE(ABORT,'private child SQL fault'); END", table)).Error; err != nil {
				t.Fatal(err)
			}
			err := db.Transaction(func(tx *gorm.DB) error { return collectionguard.DeleteContents(tx, cid) })
			if !errors.Is(err, collectionguard.ErrUnavailable) || strings.Contains(err.Error(), "private") {
				t.Fatal("delete failure not safely classified", err)
			}
			if !reflect.DeepEqual(before, snapshot(t, db)) {
				t.Fatal("failed child/parent statement retained earlier deletes")
			}
			if err := db.Exec("DROP TRIGGER fail_guard").Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Transaction(func(tx *gorm.DB) error { return collectionguard.DeleteContents(tx, cid) }); err != nil {
				t.Fatal("retry", err)
			}
		})
	}
}

func TestCollectionGuardInputQueryAndContextFailClosed(t *testing.T) {
	db, cid := fixture(t)
	if err := collectionguard.EnsureUnreferenced(nil, nil); !errors.Is(err, collectionguard.ErrUnavailable) {
		t.Fatal(err)
	}
	if err := collectionguard.DeleteContents(nil, cid); !errors.Is(err, collectionguard.ErrUnavailable) {
		t.Fatal(err)
	}
	if err := collectionguard.EnsureUnreferenced(db, []uint{0}); !errors.Is(err, collectionguard.ErrInput) {
		t.Fatal(err)
	}
	if err := collectionguard.DeleteContents(db, 0); !errors.Is(err, collectionguard.ErrInput) {
		t.Fatal(err)
	}
	if err := collectionguard.EnsureUnreferenced(db, nil); err != nil {
		t.Fatal("empty collection set must be no-op", err)
	}
	reached := false
	if err := db.Callback().Query().Before("gorm:query").Register("guard:query-fault", func(tx *gorm.DB) {
		if tx.Statement.Table == "peers" {
			reached = true
			tx.AddError(errors.New("private query failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := collectionguard.EnsureUnreferenced(db, []uint{cid}); !errors.Is(err, collectionguard.ErrUnavailable) || !reached {
		t.Fatal("reference query failure acknowledged", err)
	}
	if err := db.Callback().Query().Remove("guard:query-fault"); err != nil {
		t.Fatal(err)
	}
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		want := context.Canceled
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			want = context.DeadlineExceeded
		} else {
			cancel()
		}
		if err := collectionguard.EnsureUnreferenced(db.WithContext(ctx), []uint{cid}); !errors.Is(err, want) {
			t.Fatal("context ignored", err)
		}
		if err := collectionguard.DeleteContents(db.WithContext(ctx), cid); !errors.Is(err, want) {
			t.Fatal("delete context ignored", err)
		}
		cancel()
	}
	before := snapshot(t, db)
	if err := db.Transaction(func(tx *gorm.DB) error { return collectionguard.DeleteContents(tx, cid+99) }); !errors.Is(err, collectionguard.ErrUnavailable) {
		t.Fatal("missing parent acknowledged", err)
	}
	if !reflect.DeepEqual(before, snapshot(t, db)) {
		t.Fatal("missing parent retained child deletion")
	}
}

func TestCollectionGuardCancellationAfterChildDeleteRollsBack(t *testing.T) {
	db, cid := fixture(t)
	before := snapshot(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reached := false
	if err := db.Callback().Delete().After("gorm:delete").Register("guard:cancel-after-child", func(tx *gorm.DB) {
		if tx.Statement.Table == "tags" && tx.Error == nil {
			reached = true
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return collectionguard.DeleteContents(tx, cid) })
	if !errors.Is(err, context.Canceled) || !reached {
		t.Fatal("mid-transaction cancellation acknowledged", err)
	}
	if err := db.Callback().Delete().Remove("guard:cancel-after-child"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, snapshot(t, db)) {
		t.Fatal("cancellation retained partial cleanup")
	}
}
