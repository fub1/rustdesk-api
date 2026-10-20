package clientassign_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientassign"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func bookFixture(t *testing.T) (*fixture, model.AddressBookCollection) {
	t.Helper()
	f := newFixture(t)
	if err := f.db.AutoMigrate(&model.AddressBookCollection{}, &model.AddressBook{}, &model.Tag{}); err != nil {
		t.Fatal(err)
	}
	book := model.AddressBookCollection{UserId: f.owner.Id, Name: "Exact Book"}
	if err := f.db.Create(&book).Error; err != nil {
		t.Fatal(err)
	}
	return f, book
}

func TestAddressBookAssignmentChineseTagAndDamagedMembershipRollback(t *testing.T) {
	for _, raw := range []string{`["保留"]`, `{"tag":"bad"}`, `null`, `["keep",null]`, `[1]`, `["unfinished"`, "[\"\xff\"]"} {
		t.Run(fmt.Sprintf("%v", raw), func(t *testing.T) {
			f, book := bookFixture(t)
			rows := []model.AddressBook{{Id: f.peer.Id, UserId: f.owner.Id, CollectionId: book.Id, Alias: "first", Tags: custom_types.AutoJson(`[]`)}, {Id: f.peer.Id, UserId: f.owner.Id, CollectionId: book.Id, Alias: "second", Tags: custom_types.AutoJson(`[]`)}}
			if err := f.db.Create(&rows).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.db.Model(&model.AddressBook{}).Where("row_id = ?", rows[1].RowId).UpdateColumn("tags", raw).Error; err != nil {
				t.Fatal(err)
			}
			request := bookInput(t, `{"id":"device","uuid":"uuid","user_name":"Exact Owner","note":"changed","address_book_name":"Exact Book","address_book_alias":"changed","address_book_tag":"标签,一"}`)
			err := f.store.Assign(context.Background(), f.actor, request)
			valid := raw == `["保留"]`
			if valid && err != nil {
				t.Fatal(err)
			}
			if !valid && !errors.Is(err, clientassign.ErrUnavailable) {
				t.Fatalf("damaged tags acknowledged: %v", err)
			}
			var got []model.AddressBook
			if err := f.db.Where("collection_id = ?", book.Id).Order("row_id").Find(&got).Error; err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := f.db.Model(&model.Tag{}).Where("user_id = ? AND collection_id = ? AND name = ?", f.owner.Id, book.Id, "标签,一").Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if valid {
				if count != 1 || string(got[0].Tags) != `["标签,一"]` || string(got[1].Tags) != `["保留","标签,一"]` {
					t.Fatalf("Chinese tag changed or split: %+v", got)
				}
			} else {
				if count != 0 || got[0].Alias != "first" || string(got[0].Tags) != `[]` || got[1].Alias != "second" || readPeer(t, f).UserId != 0 || readPeer(t, f).Note != "before-note" {
					t.Fatal("damaged second row retained partial writes")
				}
				// An unrelated narrow edit must not normalize damaged membership.
				request.AddressBookTag = clientassign.OptionalString{}
				if err := f.store.Assign(context.Background(), f.actor, request); err != nil {
					t.Fatal("unrelated edit rejected damaged untouched tags", err)
				}
				var stored any
				if err := f.db.Raw("SELECT tags FROM address_books WHERE row_id = ?", rows[1].RowId).Row().Scan(&stored); err != nil {
					t.Fatal(err)
				}
				if fmt.Sprint(stored) != raw {
					t.Fatalf("raw tags normalized: %v", stored)
				}
			}
		})
	}
}

func TestAddressBookAssignmentFailuresRollbackAndRetry(t *testing.T) {
	for _, stage := range []string{"collection-read", "tag-read", "tag-create", "entry-read", "entry-create", "second-entry-update", "expiry-after-write", "cancel-after-write", "commit"} {
		t.Run(stage, func(t *testing.T) {
			f, book := bookFixture(t)
			rows := []model.AddressBook{}
			if stage != "entry-create" {
				rows = []model.AddressBook{{Id: f.peer.Id, UserId: f.owner.Id, CollectionId: book.Id, Alias: "first", Tags: custom_types.AutoJson(`[]`)}, {Id: f.peer.Id, UserId: f.owner.Id, CollectionId: book.Id, Alias: "second", Tags: custom_types.AutoJson(`[]`)}}
				if err := f.db.Create(&rows).Error; err != nil {
					t.Fatal(err)
				}
			}
			clock := time.Now().UTC()
			store, err := clientassign.New(f.db, clientassign.Options{Now: func() time.Time { return clock }})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reached, updates := false, 0
			query := func(tx *gorm.DB) {
				if stage == "collection-read" && tx.Statement.Table == "address_book_collections" || stage == "tag-read" && tx.Statement.Table == "tags" || stage == "entry-read" && tx.Statement.Table == "address_books" {
					reached = true
					tx.AddError(errors.New("private query failure"))
				}
			}
			create := func(tx *gorm.DB) {
				if stage == "tag-create" && tx.Statement.Table == "tags" || stage == "entry-create" && tx.Statement.Table == "address_books" {
					reached = true
					tx.AddError(errors.New("private create failure"))
				}
			}
			update := func(tx *gorm.DB) {
				if tx.Statement.Table != "address_books" {
					return
				}
				updates++
				if stage == "second-entry-update" && updates == 2 {
					reached = true
					tx.AddError(errors.New("private second update failure"))
				}
				if stage == "expiry-after-write" {
					reached = true
					clock = f.actor.Session.ExpiresAt.Add(time.Second)
				}
				if stage == "cancel-after-write" {
					reached = true
					cancel()
				}
			}
			if err := f.db.Callback().Query().Before("gorm:query").Register("book-fault-query", query); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Callback().Create().Before("gorm:create").Register("book-fault-create", create); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Callback().Update().After("gorm:update").Register("book-fault-update", update); err != nil {
				t.Fatal(err)
			}
			if stage == "commit" {
				pool, err := f.db.DB()
				if err != nil {
					t.Fatal(err)
				}
				faultDB, err := gorm.Open(sqlite.Dialector{Conn: commitPool{ConnPool: pool, db: pool, reached: &reached}}, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
				if err != nil {
					t.Fatal(err)
				}
				store, err = clientassign.New(faultDB, clientassign.Options{})
				if err != nil {
					t.Fatal(err)
				}
			}
			request := bookInput(t, `{"id":"device","uuid":"uuid","user_name":"Exact Owner","note":"changed","address_book_name":"Exact Book","address_book_alias":"changed","address_book_tag":"new"}`)
			want := clientassign.ErrUnavailable
			if stage == "expiry-after-write" {
				want = auth.ErrUnauthenticated
			}
			if stage == "cancel-after-write" {
				want = context.Canceled
			}
			if err := store.Assign(ctx, f.actor, request); !errors.Is(err, want) || !reached {
				t.Fatalf("failure boundary not reached/rejected: err=%v reached=%t", err, reached)
			}
			if err := f.db.Callback().Query().Remove("book-fault-query"); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Callback().Create().Remove("book-fault-create"); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Callback().Update().Remove("book-fault-update"); err != nil {
				t.Fatal(err)
			}
			if peer := readPeer(t, f); peer.Note != "before-note" || peer.UserId != 0 {
				t.Fatal("failed book transaction retained peer changes")
			}
			var got []model.AddressBook
			if err := f.db.Where("collection_id = ?", book.Id).Order("row_id").Find(&got).Error; err != nil {
				t.Fatal(err)
			}
			if len(got) != len(rows) {
				t.Fatal("failed transaction retained new entry")
			}
			for i := range got {
				if got[i].Alias != rows[i].Alias || string(got[i].Tags) != `[]` {
					t.Fatal("failed transaction retained duplicate entry changes")
				}
			}
			var tags int64
			if err := f.db.Model(&model.Tag{}).Count(&tags).Error; err != nil {
				t.Fatal(err)
			}
			if tags != 0 {
				t.Fatal("failed transaction retained new tag")
			}
			if err := f.store.Assign(context.Background(), f.actor, request); err != nil {
				t.Fatal("retry failed", err)
			}
		})
	}
}

func TestAddressBookAssignmentInputRequiresExplicitNonemptyBook(t *testing.T) {
	for _, tail := range []string{`"address_book_alias":"x"`, `"address_book_tag":"x"`, `"address_book_password":"x"`, `"address_book_note":"x"`, `"address_book_name":""`, `"address_book_name":null`, `"address_book_name":"Exact Book","address_book_name":"Exact Book"`, `"address_book_name":"Exact Book","address_book_tag":null`, `"strategy_name":"unapproved"`} {
		var request clientassign.Input
		if err := json.Unmarshal([]byte(`{"id":"device","uuid":"uuid",`+tail+`}`), &request); !errors.Is(err, clientassign.ErrInput) {
			t.Fatalf("invalid input accepted: %s err=%v", tail, err)
		}
	}
	var request clientassign.Input
	if err := json.Unmarshal([]byte("{\"id\":\"device\",\"uuid\":\"uuid\",\"address_book_name\":\"\xff\"}"), &request); !errors.Is(err, clientassign.ErrInput) {
		t.Fatalf("invalid UTF8 was silently replaced: %v", err)
	}
}

func bookInput(t *testing.T, body string) clientassign.Input {
	t.Helper()
	var request clientassign.Input
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("stock address-book input was rejected: %v", err)
	}
	return request
}

func TestAddressBookAssignmentCreatesFromFinalPeerAndPreservesOtherScopes(t *testing.T) {
	f, book := bookFixture(t)
	other := model.AddressBookCollection{UserId: f.owner.Id, Name: "other"}
	if err := f.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	sentinels := []model.AddressBook{{Id: f.peer.Id, UserId: f.owner.Id, CollectionId: other.Id, Alias: "other", Tags: custom_types.AutoJson(`["other"]`)}, {Id: f.peer.Id, UserId: f.owner.Id, Alias: "personal", Tags: custom_types.AutoJson(`[]`)}, {Id: f.peer.Id, UserId: f.actor.User.ID, Alias: "admin-personal", Tags: custom_types.AutoJson(`[]`)}}
	if err := f.db.Create(&sentinels).Error; err != nil {
		t.Fatal(err)
	}
	var before []model.AddressBook
	if err := f.db.Order("row_id").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"id":"device","uuid":"uuid","user_name":%q,"device_name":"final-host","device_username":"final-user","address_book_name":%q,"address_book_alias":"book-alias","address_book_note":"book-note","address_book_password":" raw 密码 ","address_book_tag":"A,B"}`, f.owner.Username, book.Name)
	request := bookInput(t, body)
	for retry := 0; retry < 2; retry++ {
		if err := f.store.Assign(context.Background(), f.actor, request); err != nil {
			t.Fatal(err)
		}
	}
	peer := readPeer(t, f)
	if peer.UserId != f.owner.Id || peer.Hostname != "final-host" || peer.Username != "final-user" || peer.Alias != f.peer.Alias || peer.AbId != f.peer.AbId {
		t.Fatalf("book action changed device assignment semantics: %+v", peer)
	}
	var rows []model.AddressBook
	if err := f.db.Where("user_id = ? AND collection_id = ?", f.owner.Id, book.Id).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Alias != "book-alias" || rows[0].Note != "book-note" || rows[0].Password != " raw 密码 " || rows[0].Hash != "" || rows[0].Hostname != "final-host" || rows[0].Username != "final-user" || string(rows[0].Tags) != `["A,B"]` {
		t.Fatalf("new entry lost raw values or split tag: %+v", rows)
	}
	var tags []model.Tag
	if err := f.db.Find(&tags).Error; err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Name != "A,B" || tags[0].Color != 0 || tags[0].UserId != f.owner.Id || tags[0].CollectionId != book.Id {
		t.Fatalf("tag definition mismatch: %+v", tags)
	}
	var after []model.AddressBook
	if err := f.db.Where("collection_id != ? OR user_id != ?", book.Id, f.owner.Id).Order("row_id").Find(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("CLI book assignment changed unrelated scopes")
	}
}

func TestAddressBookAssignmentNarrowUpdatesEveryHistoricalDuplicate(t *testing.T) {
	f, book := bookFixture(t)
	if err := f.db.Model(&f.peer).Update("user_id", f.owner.Id).Error; err != nil {
		t.Fatal(err)
	}
	rows := []model.AddressBook{}
	for i := 0; i < 2; i++ {
		rows = append(rows, model.AddressBook{Id: f.peer.Id, UserId: f.owner.Id, CollectionId: book.Id, Alias: fmt.Sprintf("alias%d", i), Note: fmt.Sprintf("note%d", i), Password: "password", Hash: "personal-hash-preserved", Hostname: "manual-host", Username: "manual-user", RdpPort: "3389", Tags: custom_types.AutoJson(`["keep","chosen","keep","chosen"]`)})
	}
	if err := f.db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&model.Tag{UserId: f.owner.Id, CollectionId: book.Id, Name: "chosen", Color: 77}).Error; err != nil {
		t.Fatal(err)
	}
	request := bookInput(t, `{"id":"device","uuid":"uuid","address_book_name":"Exact Book","address_book_tag":"chosen"}`)
	if err := f.store.Assign(context.Background(), f.actor, request); err != nil {
		t.Fatal(err)
	}
	var got []model.AddressBook
	if err := f.db.Where("collection_id = ?", book.Id).Order("row_id").Find(&got).Error; err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatal("duplicate physical entries were removed")
	}
	for i, row := range got {
		if row.Alias != rows[i].Alias || row.Note != rows[i].Note || row.Password != rows[i].Password || row.Hash != rows[i].Hash || row.Hostname != rows[i].Hostname || row.Username != rows[i].Username || row.RdpPort != rows[i].RdpPort || string(row.Tags) != `["keep","chosen","keep"]` {
			t.Fatalf("wide update or missed duplicate: %+v", row)
		}
	}
	request = bookInput(t, `{"id":"device","uuid":"uuid","address_book_name":"Exact Book","address_book_alias":"","address_book_note":"","address_book_password":"","address_book_tag":""}`)
	if err := f.store.Assign(context.Background(), f.actor, request); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Where("collection_id = ?", book.Id).Find(&got).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range got {
		if row.Alias != "" || row.Note != "" || row.Password != "" || row.Hash != "personal-hash-preserved" || string(row.Tags) != `["keep","chosen","keep"]` {
			t.Fatalf("empty field behavior: %+v", row)
		}
	}
	var tag model.Tag
	if err := f.db.First(&tag).Error; err != nil || tag.Color != 77 {
		t.Fatal("existing tag color changed", err)
	}
}

func TestAddressBookAssignmentRejectsInvalidOwnerAndAmbiguousExactNames(t *testing.T) {
	for _, mode := range []string{"unowned", "owner-disabled", "missing-book", "different-case", "duplicate", "wrong-owner"} {
		t.Run(mode, func(t *testing.T) {
			f, book := bookFixture(t)
			if mode != "unowned" {
				if err := f.db.Model(&f.peer).Update("user_id", f.owner.Id).Error; err != nil {
					t.Fatal(err)
				}
			}
			name := book.Name
			switch mode {
			case "owner-disabled":
				if err := f.db.Model(&f.owner).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
					t.Fatal(err)
				}
			case "missing-book":
				name = "missing"
			case "different-case":
				name = "exact book"
			case "duplicate":
				if err := f.db.Create(&model.AddressBookCollection{UserId: f.owner.Id, Name: name}).Error; err != nil {
					t.Fatal(err)
				}
			case "wrong-owner":
				if err := f.db.Model(&book).Update("user_id", f.actor.User.ID).Error; err != nil {
					t.Fatal(err)
				}
			}
			request := bookInput(t, fmt.Sprintf(`{"id":"device","uuid":"uuid","note":"must-rollback","address_book_name":%q}`, name))
			if err := f.store.Assign(context.Background(), f.actor, request); !errors.Is(err, clientassign.ErrInput) {
				t.Fatalf("invalid book target accepted: %v", err)
			}
			if readPeer(t, f).Note != f.peer.Note {
				t.Fatal("invalid book retained partial peer update")
			}
		})
	}
}

func TestAddressBookAssignmentOwnerCandidateRevalidatedAfterPeerLock(t *testing.T) {
	f, _ := bookFixture(t)
	if err := f.db.Model(&f.peer).Update("user_id", f.owner.Id).Error; err != nil {
		t.Fatal(err)
	}
	fired := false
	if err := f.db.Callback().Query().After("gorm:query").Register("book-owner-transition", func(tx *gorm.DB) {
		if tx.Statement.Table == "peers" && !fired {
			fired = true
			if err := tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE peers SET user_id = ? WHERE row_id = ?", f.actor.User.ID, f.peer.RowId).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	request := bookInput(t, `{"id":"device","uuid":"uuid","address_book_name":"Exact Book"}`)
	if err := f.store.Assign(context.Background(), f.actor, request); !errors.Is(err, clientassign.ErrInput) || !fired {
		t.Fatalf("owner changed after candidate was not rejected: %v fired=%v", err, fired)
	}
}
