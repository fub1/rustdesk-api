package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/clientassign"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"gorm.io/gorm"
)

func assignmentBooks(t *testing.T, db *gorm.DB) []model.AddressBook {
	t.Helper()
	var rows []model.AddressBook
	if err := db.Order("row_id ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

// Inject after the last of two existing duplicate AB rows has been updated.
// No SQL remains before COMMIT; a peer UPDATE occurs earlier in this use case.
func failAssignmentBookCommit(t *testing.T, f *fixture) func() {
	t.Helper()
	if f.Engine == "postgres" {
		transactionExec(t, f.DB, "CREATE TABLE book_commit_parent (id bigint PRIMARY KEY)")
		transactionExec(t, f.DB, "CREATE TABLE book_commit_guard (parent_id bigint REFERENCES book_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)")
		transactionExec(t, f.DB, "CREATE FUNCTION book_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO book_commit_guard(parent_id) VALUES (999999); RETURN NEW; END $$")
		transactionExec(t, f.DB, "CREATE TRIGGER book_fail_commit AFTER UPDATE ON address_books FOR EACH ROW EXECUTE FUNCTION book_fail_commit()")
		return func() {
			transactionExec(t, f.DB, "DROP TRIGGER book_fail_commit ON address_books")
			transactionExec(t, f.DB, "DROP FUNCTION book_fail_commit()")
			transactionExec(t, f.DB, "DROP TABLE book_commit_guard")
			transactionExec(t, f.DB, "DROP TABLE book_commit_parent")
		}
	}
	control := f.Reopen(t)
	var updates atomic.Int32
	var killed atomic.Bool
	if err := f.DB.Callback().Update().After("gorm:update").Register("assignment:kill-after-final-book-update", func(tx *gorm.DB) {
		if tx.Statement.Table != "address_books" || tx.Error != nil || updates.Add(1) != 2 {
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
		if updates.Load() != 2 || !killed.Load() {
			t.Error("connection termination after final book UPDATE was not reached")
		}
		if err := f.DB.Callback().Update().Remove("assignment:kill-after-final-book-update"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClientAssignmentAddressBookExactScopeAndRollback(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		actor, owner, peer := assignmentSetup(t, f)
		collection := model.AddressBookCollection{UserId: owner.Id, Name: "ExactBook"}
		transactionCreate(t, f.DB, &collection)
		rows := []model.AddressBook{
			{Id: peer.Id, UserId: owner.Id, CollectionId: collection.Id, Alias: "first", Password: "first-password", Hash: "first-hash", Tags: custom_types.AutoJson(`["old"]`)},
			{Id: peer.Id, UserId: owner.Id, CollectionId: collection.Id, Alias: "second", Password: "second-password", Hash: "second-hash", Tags: custom_types.AutoJson(`["other"]`)},
			{Id: peer.Id, UserId: owner.Id, CollectionId: 0, Alias: "personal", Hash: "private-hash", Tags: custom_types.AutoJson(`[]`)},
			{Id: peer.Id, UserId: actor.User.ID, CollectionId: collection.Id, Alias: "wrong-owner", Tags: custom_types.AutoJson(`[]`)},
		}
		for i := range rows {
			transactionCreate(t, f.DB, &rows[i])
		}
		store, err := clientassign.New(f.DB, clientassign.Options{})
		if err != nil {
			t.Fatal(err)
		}
		beforeRows, beforePeer := assignmentBooks(t, f.DB), assignmentRead(t, f.DB, peer.RowId)
		input := clientassign.Input{ID: peer.Id, UUID: peer.Uuid, UserName: assignmentValue(owner.Username), AddressBookName: assignmentValue(collection.Name), AddressBookNote: assignmentValue("book-note"), AddressBookTag: assignmentValue("New, whole tag"), DeviceUsername: assignmentValue("final-user")}
		// The injected failure is reached after the final successful book UPDATE;
		// the tag and all duplicate AB changes must also roll back at real COMMIT.
		remove := failAssignmentBookCommit(t, f)
		err = store.Assign(context.Background(), actor, input)
		remove()
		if !errors.Is(err, clientassign.ErrUnavailable) {
			t.Fatalf("real COMMIT failure acknowledged: %v", err)
		}
		fresh := f.Reopen(t)
		if !reflect.DeepEqual(beforeRows, assignmentBooks(t, fresh)) || !reflect.DeepEqual(beforePeer, assignmentRead(t, fresh, peer.RowId)) {
			t.Fatal("book/peer changes escaped failed COMMIT")
		}
		var tags int64
		if err := fresh.Model(&model.Tag{}).Where("collection_id = ?", collection.Id).Count(&tags).Error; err != nil || tags != 0 {
			t.Fatal("new tag escaped failed COMMIT", err)
		}
		store, err = clientassign.New(fresh, clientassign.Options{})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := store.Assign(context.Background(), actor, input); err != nil {
				t.Fatal("retry", err)
			}
		}
		after := assignmentBooks(t, fresh)
		if len(after) != len(beforeRows) {
			t.Fatal("duplicate rows were removed or created")
		}
		for i := 0; i < 2; i++ {
			if after[i].RowId != beforeRows[i].RowId || after[i].Note != "book-note" || after[i].Alias != beforeRows[i].Alias || after[i].Password != beforeRows[i].Password || after[i].Hash != beforeRows[i].Hash || after[i].Hostname != beforeRows[i].Hostname {
				t.Fatal("explicit narrow update replaced omitted fields")
			}
			var got []string
			if json.Unmarshal([]byte(after[i].Tags), &got) != nil || len(got) != 2 || got[1] != "New, whole tag" {
				t.Fatal("tag was split, replaced or duplicated")
			}
		}
		if !reflect.DeepEqual(after[2:], beforeRows[2:]) {
			t.Fatal("join changed another scope")
		}
		if err := fresh.Model(&model.Tag{}).Where("collection_id = ?", collection.Id).Count(&tags).Error; err != nil || tags != 1 {
			t.Fatal("tag retry is not idempotent", err)
		}
		// MySQL's database uses a case/accent insensitive, PAD SPACE collation.
		// Every near-match still fails as an exact byte name, with no peer write.
		for _, name := range []string{"exactbook", "ExactBook ", "ÉxactBook"} {
			bad := input
			bad.AddressBookName = assignmentValue(name)
			bad.Note = assignmentValue("must-not-write")
			before := assignmentRead(t, fresh, peer.RowId)
			if err := store.Assign(context.Background(), actor, bad); err == nil {
				t.Fatalf("near match accepted: %q", name)
			}
			if !reflect.DeepEqual(before, assignmentRead(t, fresh, peer.RowId)) || !reflect.DeepEqual(after, assignmentBooks(t, fresh)) {
				t.Fatal("near-match failure had partial writes")
			}
		}
		transactionCreate(t, fresh, &model.AddressBookCollection{UserId: owner.Id, Name: collection.Name})
		if err := store.Assign(context.Background(), actor, input); !errors.Is(err, clientassign.ErrInput) {
			t.Fatalf("ambiguous collection accepted: %v", err)
		}
	})
}

func TestClientAssignmentAddressBookConcurrentClientTagWrites(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		actor, owner, peer := assignmentSetup(t, f)
		if err := f.DB.Model(&model.Peer{}).Where("row_id = ?", peer.RowId).Update("user_id", owner.Id).Error; err != nil {
			t.Fatal(err)
		}
		book := model.AddressBookCollection{UserId: owner.Id, Name: "Concurrent"}
		transactionCreate(t, f.DB, &book)
		transactionCreate(t, f.DB, &model.Tag{UserId: owner.Id, CollectionId: book.Id, Name: "old"})
		transactionCreate(t, f.DB, &model.AddressBook{Id: peer.Id, UserId: owner.Id, CollectionId: book.Id, Tags: custom_types.AutoJson(`["old"]`)})
		a, _ := clientassign.New(f.Reopen(t), clientassign.Options{})
		b, _ := clientassign.New(f.Reopen(t), clientassign.Options{})
		ordinary, _ := clientcompat.New(f.Reopen(t))
		start, results := make(chan struct{}), make(chan error, 3)
		var wg sync.WaitGroup
		for _, action := range []func(context.Context) error{
			func(ctx context.Context) error {
				return a.Assign(ctx, actor, clientassign.Input{ID: peer.Id, UUID: peer.Uuid, AddressBookName: assignmentValue(book.Name), AddressBookTag: assignmentValue("joined-a")})
			},
			func(ctx context.Context) error {
				return b.Assign(ctx, actor, clientassign.Input{ID: peer.Id, UUID: peer.Uuid, AddressBookName: assignmentValue(book.Name), AddressBookTag: assignmentValue("joined-b")})
			},
			func(ctx context.Context) error {
				return ordinary.RenameTag(ctx, clientcompat.Actor{ID: owner.Id}, fmt.Sprintf("%d-%d-%d", owner.GroupId, owner.Id, book.Id), "old", "renamed")
			},
		} {
			wg.Add(1)
			go func(run func(context.Context) error) {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				results <- run(ctx)
			}(action)
		}
		close(start)
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal("independent pool writes", err)
			}
		}
		rows := assignmentBooks(t, f.Reopen(t))
		if len(rows) != 1 {
			t.Fatal("concurrent join created duplicates")
		}
		var got []string
		if json.Unmarshal([]byte(rows[0].Tags), &got) != nil || len(got) != 3 {
			t.Fatal("concurrent tag updates lost values")
		}
		set := map[string]bool{}
		for _, name := range got {
			set[name] = true
		}
		if !set["renamed"] || !set["joined-a"] || !set["joined-b"] || set["old"] {
			t.Fatal("tag rename and joins were not serializable")
		}
	})
}

func TestClientAssignmentAddressBookConcurrentCollectionDelete(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		actor, owner, peer := assignmentSetup(t, f)
		if err := f.DB.Model(&model.Peer{}).Where("row_id = ?", peer.RowId).Update("user_id", owner.Id).Error; err != nil {
			t.Fatal(err)
		}
		store, _ := clientassign.New(f.Reopen(t), clientassign.Options{})
		previous := service.DB
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		service.DB = f.Reopen(t).WithContext(ctx)
		t.Cleanup(func() { service.DB = previous })
		for i := 0; i < 3; i++ {
			book := model.AddressBookCollection{UserId: owner.Id, Name: fmt.Sprintf("delete-race-%d", i)}
			transactionCreate(t, f.DB, &book)
			start := make(chan struct{})
			assigned, deleted := make(chan error, 1), make(chan error, 1)
			go func() {
				<-start
				assigned <- store.Assign(ctx, actor, clientassign.Input{ID: peer.Id, UUID: peer.Uuid, AddressBookName: assignmentValue(book.Name), AddressBookNote: assignmentValue("race-note")})
			}()
			go func() { <-start; deleted <- (&service.AddressBookService{}).DeleteCollection(&book) }()
			close(start)
			if err := <-assigned; err != nil && !errors.Is(err, clientassign.ErrInput) {
				t.Fatal("assignment/delete lock competition", err)
			}
			if err := <-deleted; err != nil {
				t.Fatal("collection deletion failed", err)
			}
			var n int64
			if err := f.DB.Model(&model.AddressBook{}).Where("collection_id = ?", book.Id).Count(&n).Error; err != nil || n != 0 {
				t.Fatal("assignment escaped collection deletion", err)
			}
		}
	})
}

func TestLegacyCollectionRealStatementRollbackAndRetry(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, owner, peer := assignmentSetup(t, f)
		previous := service.DB
		service.DB = f.DB
		t.Cleanup(func() { service.DB = previous })
		for _, table := range []string{"address_book_collection_rules", "address_books", "address_book_collections"} {
			book := model.AddressBookCollection{UserId: owner.Id, Name: table}
			transactionCreate(t, f.DB, &book)
			entry := model.AddressBook{Id: peer.Id, UserId: owner.Id, CollectionId: book.Id, Tags: custom_types.AutoJson(`[]`)}
			transactionCreate(t, f.DB, &entry)
			rule := model.AddressBookCollectionRule{UserId: owner.Id, CollectionId: book.Id, Type: 1, ToId: owner.Id, Rule: 3}
			transactionCreate(t, f.DB, &rule)
			if f.Engine == "mysql" {
				transactionExec(t, f.DB, "CREATE TRIGGER reject_book_delete BEFORE DELETE ON "+table+" FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'book delete fault'")
			} else {
				transactionExec(t, f.DB, "CREATE FUNCTION reject_book_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'book delete fault'; END $$")
				transactionExec(t, f.DB, "CREATE TRIGGER reject_book_delete BEFORE DELETE ON "+table+" FOR EACH ROW EXECUTE FUNCTION reject_book_delete()")
			}
			if err := (&service.AddressBookService{}).DeleteCollection(&book); err == nil {
				t.Fatal("failed statement acknowledged")
			}
			fresh := f.Reopen(t)
			for _, row := range []struct {
				model  any
				column string
				id     uint
			}{{&model.AddressBookCollection{}, "id", book.Id}, {&model.AddressBook{}, "row_id", entry.RowId}, {&model.AddressBookCollectionRule{}, "id", rule.Id}} {
				var n int64
				if err := fresh.Model(row.model).Where(row.column+" = ?", row.id).Count(&n).Error; err != nil || n != 1 {
					t.Fatal("delete committed partial cleanup", err)
				}
			}
			if f.Engine == "mysql" {
				transactionExec(t, f.DB, "DROP TRIGGER reject_book_delete")
			} else {
				transactionExec(t, f.DB, "DROP TRIGGER reject_book_delete ON "+table)
				transactionExec(t, f.DB, "DROP FUNCTION reject_book_delete()")
			}
			if err := (&service.AddressBookService{}).DeleteCollection(&book); err != nil {
				t.Fatal("delete retry", err)
			}
		}
	})
}
