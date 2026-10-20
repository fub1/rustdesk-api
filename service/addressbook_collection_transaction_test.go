package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/collectionguard"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type collectionFixture struct {
	db           *gorm.DB
	owner, other model.User
	book         model.AddressBookCollection
}

func collectionFixtureDB(t *testing.T) collectionFixture {
	t.Helper()
	db := issuanceDB(t)
	previous := DB
	DB = db
	t.Cleanup(func() { DB = previous })
	owner := model.User{Username: "collection-owner", Status: model.COMMON_STATUS_ENABLE}
	other := model.User{Username: "collection-other", Status: model.COMMON_STATUS_ENABLE}
	issuanceCreate(t, db, &owner)
	issuanceCreate(t, db, &other)
	book := model.AddressBookCollection{UserId: owner.Id, Name: "original"}
	issuanceCreate(t, db, &book)
	issuanceCreate(t, db, &model.AddressBookCollectionRule{UserId: owner.Id, CollectionId: book.Id, ToId: other.Id, Type: 1, Rule: 3})
	issuanceCreate(t, db, &model.AddressBook{Id: "device", UserId: owner.Id, CollectionId: book.Id, Note: "keep note", Tags: custom_types.AutoJson(`["keep"]`)})
	issuanceCreate(t, db, &model.Tag{UserId: owner.Id, CollectionId: book.Id, Name: "keep"})
	return collectionFixture{db: db, owner: owner, other: other, book: book}
}

func collectionRows(t *testing.T, db *gorm.DB) []any {
	t.Helper()
	var books []model.AddressBookCollection
	var rules []model.AddressBookCollectionRule
	var entries []model.AddressBook
	var tags []model.Tag
	for _, rows := range []any{&books, &rules, &entries, &tags} {
		if err := db.Find(rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	return []any{books, rules, entries, tags}
}

func TestLegacyCollectionDeleteRollsBackEveryStatementFailure(t *testing.T) {
	for _, table := range []string{"address_book_collection_rules", "address_books", "tags", "address_book_collections"} {
		t.Run(table, func(t *testing.T) {
			f := collectionFixtureDB(t)
			before := collectionRows(t, f.db)
			issuanceSQL(t, f.db, fmt.Sprintf("CREATE TRIGGER fail_collection_delete BEFORE DELETE ON %s BEGIN SELECT RAISE(ABORT, 'collection statement fault'); END", table))
			if err := (&AddressBookService{}).DeleteCollection(&f.book); err == nil {
				t.Error("a failed delete statement was acknowledged")
			}
			if !reflect.DeepEqual(before, collectionRows(t, f.db)) {
				t.Fatal("failed statement committed partial collection cleanup")
			}
			issuanceSQL(t, f.db, "DROP TRIGGER fail_collection_delete")
			if err := (&AddressBookService{}).DeleteCollection(&f.book); err != nil {
				t.Fatal("retry failed", err)
			}
			for _, row := range []any{&model.AddressBookCollection{}, &model.AddressBookCollectionRule{}, &model.AddressBook{}, &model.Tag{}} {
				if issuanceCount(t, f.db, row) != 0 {
					t.Fatal("successful collection delete left its related rows")
				}
			}
		})
	}
}

type collectionCommitPool struct {
	gorm.ConnPool
	db      *sql.DB
	reached *bool
}

func (p collectionCommitPool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.db.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &collectionCommitTx{Tx: tx, reached: p.reached}, nil
}

type collectionCommitTx struct {
	*sql.Tx
	reached *bool
}

func (tx collectionCommitTx) Commit() error {
	*tx.reached = true
	_ = tx.Tx.Rollback()
	return errors.New("collection commit fault")
}

func TestLegacyCollectionWritesRejectCommitFailure(t *testing.T) {
	for _, operation := range []string{"create", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			f := collectionFixtureDB(t)
			before := collectionRows(t, f.db)
			pool, err := f.db.DB()
			if err != nil {
				t.Fatal(err)
			}
			reached := false
			DB, err = gorm.Open(sqlite.Dialector{Conn: collectionCommitPool{ConnPool: pool, db: pool, reached: &reached}}, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			if operation == "create" {
				err = (&AddressBookService{}).CreateCollection(&model.AddressBookCollection{UserId: f.owner.Id, Name: "created"})
			} else if operation == "update" {
				err = (&AddressBookService{}).UpdateCollection(&model.AddressBookCollection{IdModel: f.book.IdModel, UserId: f.owner.Id, Name: "changed"})
			} else {
				err = (&AddressBookService{}).DeleteCollection(&f.book)
			}
			if err == nil || !reached {
				t.Fatalf("failed commit was not reached/rejected: reached=%t err=%v", reached, err)
			}
			if !reflect.DeepEqual(before, collectionRows(t, f.db)) {
				t.Fatal("failed commit retained collection changes")
			}
		})
	}
}

func TestLegacyCollectionWritesRejectMissingRowsAndQueryErrors(t *testing.T) {
	for _, operation := range []string{"update", "delete"} {
		for _, failure := range []string{"nil", "zero-id", "missing-collection", "candidate-query", "owner-query", "locked-collection-query"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				f := collectionFixtureDB(t)
				before := collectionRows(t, f.db)
				input := f.book
				input.Name = "must-not-apply"
				argument := &input
				switch failure {
				case "nil":
					argument = nil
				case "zero-id":
					input.Id = 0
				case "missing-collection":
					input.Id = f.book.Id + 100
				}
				injected := errors.New("collection query fault")
				triggered, collectionQueries := false, 0
				callback := "collection-query-fault"
				if err := f.db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "address_book_collections" {
						collectionQueries++
					}
					if (failure == "candidate-query" && tx.Statement.Table == "address_book_collections" && collectionQueries == 1) ||
						(failure == "owner-query" && tx.Statement.Table == "users") ||
						(failure == "locked-collection-query" && tx.Statement.Table == "address_book_collections" && collectionQueries == 2) {
						triggered = true
						tx.AddError(injected)
					}
				}); err != nil {
					t.Fatal(err)
				}
				var err error
				if operation == "update" {
					err = (&AddressBookService{}).UpdateCollection(argument)
				} else {
					err = (&AddressBookService{}).DeleteCollection(argument)
				}
				if removeErr := f.db.Callback().Query().Remove(callback); removeErr != nil {
					t.Fatal(removeErr)
				}
				if failure == "nil" || failure == "zero-id" || failure == "missing-collection" {
					if !errors.Is(err, gorm.ErrRecordNotFound) {
						t.Fatalf("missing collection did not return notfound: %v", err)
					}
				} else if !triggered || !errors.Is(err, injected) {
					t.Fatalf("query fault was not reached/returned: triggered=%t err=%v", triggered, err)
				}
				if !reflect.DeepEqual(before, collectionRows(t, f.db)) {
					t.Fatal("rejected collection write changed persisted data")
				}
			})
		}
	}
}

func TestLegacyCollectionUpdateRollsBackStatementFailure(t *testing.T) {
	f := collectionFixtureDB(t)
	before := collectionRows(t, f.db)
	issuanceSQL(t, f.db, "CREATE TRIGGER fail_collection_update BEFORE UPDATE ON address_book_collections BEGIN SELECT RAISE(ABORT, 'collection update fault'); END")
	if err := (&AddressBookService{}).UpdateCollection(&model.AddressBookCollection{IdModel: f.book.IdModel, UserId: f.owner.Id, Name: "must-not-apply"}); err == nil {
		t.Fatal("a failed update statement was acknowledged")
	}
	if !reflect.DeepEqual(before, collectionRows(t, f.db)) {
		t.Fatal("failed update retained collection changes")
	}
}

func TestLegacyCollectionWritesRecheckOwnerAfterCandidateRead(t *testing.T) {
	for _, operation := range []string{"update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			f := collectionFixtureDB(t)
			triggered := false
			if err := f.db.Callback().Query().After("gorm:query").Register("collection-owner-transition", func(tx *gorm.DB) {
				if !triggered && tx.Statement.Table == "address_book_collections" {
					triggered = true
					result := tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE address_book_collections SET user_id = ? WHERE id = ?", f.other.Id, f.book.Id)
					if result.Error != nil || result.RowsAffected != 1 {
						tx.AddError(errors.New("owner transition injection failed"))
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			var err error
			if operation == "update" {
				err = (&AddressBookService{}).UpdateCollection(&model.AddressBookCollection{IdModel: f.book.IdModel, Name: "must-not-apply"})
			} else {
				err = (&AddressBookService{}).DeleteCollection(&f.book)
			}
			if err == nil || !triggered {
				t.Fatalf("owner transition was not detected: triggered=%t err=%v", triggered, err)
			}
			var current model.AddressBookCollection
			if err := f.db.First(&current, f.book.Id).Error; err != nil || current.UserId != f.other.Id || current.Name != f.book.Name {
				t.Fatal("owner transition caused an unintended update/delete", err)
			}
			if issuanceCount(t, f.db, &model.AddressBookCollectionRule{}) != 1 || issuanceCount(t, f.db, &model.AddressBook{}) != 1 {
				t.Fatal("rejected operation removed related rows")
			}
		})
	}
}

func TestLegacyCollectionUpdateRejectsTransferAndPreservesHistoricalZeroOwner(t *testing.T) {
	f := collectionFixtureDB(t)
	service := &AddressBookService{}
	if err := service.UpdateCollection(&model.AddressBookCollection{IdModel: f.book.IdModel, UserId: f.other.Id, Name: "moved"}); !errors.Is(err, collectionguard.ErrInput) {
		t.Fatal("ordinary editing must reject owner transfer", err)
	}
	if err := service.UpdateCollection(&model.AddressBookCollection{IdModel: f.book.IdModel, UserId: f.owner.Id, Name: "renamed"}); err != nil {
		t.Fatal("same-owner rename failed", err)
	}
	if err := service.UpdateCollection(&model.AddressBookCollection{IdModel: f.book.IdModel}); err != nil {
		t.Fatal(err)
	}
	var current model.AddressBookCollection
	if err := f.db.First(&current, f.book.Id).Error; err != nil || current.UserId != f.owner.Id || current.Name != "renamed" {
		t.Fatal("legacy omitted/zero fields changed update semantics", err)
	}
	zero := model.AddressBookCollection{Name: "unowned"}
	issuanceCreate(t, f.db, &zero)
	if err := service.UpdateCollection(&model.AddressBookCollection{IdModel: zero.IdModel, Name: "unowned-renamed"}); err != nil {
		t.Fatal("historical zero-owner update rejected", err)
	}
	if err := service.DeleteCollection(&zero); err != nil {
		t.Fatal("historical zero-owner cleanup rejected", err)
	}
	stale := f.book
	stale.UserId = f.other.Id
	if err := service.DeleteCollection(&stale); err == nil {
		t.Fatal("stale caller owner deleted a collection")
	}
}

func TestLegacyCollectionConcurrentOppositeTransfersAreRejected(t *testing.T) {
	f := collectionFixtureDB(t)
	second := model.AddressBookCollection{UserId: f.other.Id, Name: "second"}
	issuanceCreate(t, f.db, &second)
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, update := range []*model.AddressBookCollection{{IdModel: f.book.IdModel, UserId: f.other.Id, Name: "first-moved"}, {IdModel: second.IdModel, UserId: f.owner.Id, Name: "second-moved"}} {
		group.Add(1)
		go func(update *model.AddressBookCollection) {
			defer group.Done()
			<-start
			results <- (&AddressBookService{}).UpdateCollection(update)
		}(update)
	}
	close(start)
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("opposite transfers blocked")
	}
	for n := 0; n < 2; n++ {
		if err := <-results; !errors.Is(err, collectionguard.ErrInput) {
			t.Fatal("opposite transfer was not rejected", err)
		}
	}
	var books []model.AddressBookCollection
	if err := f.db.Order("id").Find(&books).Error; err != nil || len(books) != 2 || books[0].UserId != f.owner.Id || books[1].UserId != f.other.Id || books[0].Name != f.book.Name || books[1].Name != second.Name {
		t.Fatal("rejected opposite transfers changed ownership", err)
	}
}
