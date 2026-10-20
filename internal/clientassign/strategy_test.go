package clientassign_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientassign"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func namedAssignmentFixture(t *testing.T) (*fixture, model.AddressBookCollection, model.StrategyDefinition) {
	t.Helper()
	f, book := bookFixture(t)
	if err := database.Update(f.db, 281, nil); err != nil {
		t.Fatal(err)
	}
	definition := model.StrategyDefinition{Name: model.StrategyName("Exact Strategy"), ConfigOptions: `{"key":"named"}`}
	if err := f.db.Create(&definition).Error; err != nil {
		t.Fatal(err)
	}
	return f, book, definition
}

func TestNamedAssignmentPresenceAndByteExactSelection(t *testing.T) {
	f, _, definition := namedAssignmentFixture(t)
	if err := f.store.Assign(context.Background(), f.actor, clientassign.Input{ID: f.peer.Id, UUID: f.peer.Uuid, StrategyName: field(string(definition.Name))}); err != nil {
		t.Fatal(err)
	}
	if peer := readPeer(t, f); peer.StrategyDefinitionID == nil || *peer.StrategyDefinitionID != definition.Id || peer.UserId != 0 {
		t.Fatal("global named assignment changed owner or did not link")
	}
	if err := f.store.Assign(context.Background(), f.actor, input()); err != nil {
		t.Fatal(err)
	}
	if peer := readPeer(t, f); peer.StrategyDefinitionID == nil || *peer.StrategyDefinitionID != definition.Id {
		t.Fatal("omitted strategy detached")
	}
	for _, name := range []string{"exact strategy", "Exact Strategy ", "é", "e\u0301", "a\x00b"} {
		row := model.StrategyDefinition{Name: model.StrategyName(name), ConfigOptions: `{}`}
		if err := f.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		for retry := 0; retry < 2; retry++ {
			if err := f.store.Assign(context.Background(), f.actor, clientassign.Input{ID: f.peer.Id, UUID: f.peer.Uuid, StrategyName: field(name)}); err != nil {
				t.Fatal("byte-exact named assignment", err)
			}
			if peer := readPeer(t, f); peer.StrategyDefinitionID == nil || *peer.StrategyDefinitionID != row.Id {
				t.Fatal("wrong exact name selected")
			}
		}
	}
	beforeDetach := readPeer(t, f)
	if err := f.store.Assign(context.Background(), f.actor, clientassign.Input{ID: f.peer.Id, UUID: f.peer.Uuid, UserName: field(f.owner.Username), AddressBookName: field("missing-book"), StrategyName: field("")}); !errors.Is(err, clientassign.ErrInput) {
		t.Fatal("invalid mixed detach/book assignment acknowledged", err)
	}
	if !reflect.DeepEqual(beforeDetach, readPeer(t, f)) {
		t.Fatal("failed book assignment retained SQL NULL detach/owner change")
	}
	if err := f.store.Assign(context.Background(), f.actor, clientassign.Input{ID: f.peer.Id, UUID: f.peer.Uuid, StrategyName: field("")}); err != nil {
		t.Fatal(err)
	}
	if readPeer(t, f).StrategyDefinitionID != nil {
		t.Fatal("empty name did not write SQL NULL")
	}
	var policies int64
	if err := f.db.Model(&model.PeerStrategy{}).Count(&policies).Error; err != nil || policies != 0 {
		t.Fatal("named assignment copied into direct device policy", err)
	}
}

func TestNamedAssignmentRejectsUnknownAndCorruptDefinitionAtomically(t *testing.T) {
	for _, raw := range []string{"missing", `null`, `[]`, `{"":"bad"}`, `{"key":null}`, `{"key":3}`, `{"key":"first","key":"second"}`, `broken`} {
		t.Run(raw, func(t *testing.T) {
			f, book, definition := namedAssignmentFixture(t)
			row := model.AddressBook{Id: f.peer.Id, UserId: f.owner.Id, CollectionId: book.Id, Alias: "before-book", Tags: custom_types.AutoJson(`[]`)}
			if err := f.db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.db.First(&row, row.RowId).Error; err != nil {
				t.Fatal(err)
			}
			name, want := string(definition.Name), clientassign.ErrUnavailable
			if raw == "missing" {
				name, want = "not-found", clientassign.ErrInput
			} else if err := f.db.Model(&definition).UpdateColumn("config_options", raw).Error; err != nil {
				t.Fatal(err)
			}
			before := readPeer(t, f)
			request := clientassign.Input{ID: f.peer.Id, UUID: f.peer.Uuid, UserName: field(f.owner.Username), Note: field("must-rollback"), AddressBookName: field(book.Name), AddressBookAlias: field("must-rollback"), AddressBookTag: field("must-rollback"), StrategyName: field(name)}
			if err := f.store.Assign(context.Background(), f.actor, request); !errors.Is(err, want) {
				t.Fatalf("bad named definition acknowledged: %v", err)
			}
			if !reflect.DeepEqual(before, readPeer(t, f)) {
				t.Fatal("invalid strategy retained owner/device writes")
			}
			var current model.AddressBook
			if err := f.db.First(&current, row.RowId).Error; err != nil || !reflect.DeepEqual(current, row) {
				t.Fatal("invalid strategy retained book writes", err)
			}
			var tags int64
			if err := f.db.Model(&model.Tag{}).Count(&tags).Error; err != nil || tags != 0 {
				t.Fatal("invalid strategy retained tag", err)
			}
		})
	}
	for _, tail := range []string{`"strategy_name":null`, `"strategy_name":true`, `"strategy_name":1`, `"strategy_name":[]`, `"strategy_name":"one","strategy_name":"two"`} {
		var input clientassign.Input
		if err := json.Unmarshal([]byte(`{"id":"device","uuid":"uuid",`+tail+`}`), &input); !errors.Is(err, clientassign.ErrInput) {
			t.Fatal("bad strategy input accepted", tail, err)
		}
	}
}

func TestNamedAssignmentMixedFailureRollbackAndRetry(t *testing.T) {
	for _, fault := range []string{"definition-query", "peer-update", "book-update", "commit"} {
		t.Run(fault, func(t *testing.T) {
			f, book, definition := namedAssignmentFixture(t)
			entry := model.AddressBook{Id: f.peer.Id, UserId: f.owner.Id, CollectionId: book.Id, Alias: "original", Tags: custom_types.AutoJson(`[]`)}
			if err := f.db.Create(&entry).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.db.First(&entry, entry.RowId).Error; err != nil {
				t.Fatal(err)
			}
			before := readPeer(t, f)
			reached := false
			if err := f.db.Callback().Query().Before("gorm:query").Register("named:query-fault", func(tx *gorm.DB) {
				if fault == "definition-query" && tx.Statement.Table == "strategy_definitions" {
					reached = true
					tx.AddError(errors.New("private named query fault"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Callback().Update().After("gorm:update").Register("named:update-fault", func(tx *gorm.DB) {
				if (fault == "peer-update" && tx.Statement.Table == "peers") || (fault == "book-update" && tx.Statement.Table == "address_books") {
					reached = true
					tx.AddError(errors.New("private named update fault"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			store := f.store
			if fault == "commit" {
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
			request := clientassign.Input{ID: f.peer.Id, UUID: f.peer.Uuid, UserName: field(f.owner.Username), Note: field("after"), AddressBookName: field(book.Name), AddressBookAlias: field("after"), AddressBookTag: field("after"), StrategyName: field(string(definition.Name))}
			if err := store.Assign(context.Background(), f.actor, request); !errors.Is(err, clientassign.ErrUnavailable) || !reached {
				t.Fatalf("fault not reached/rejected: %v reached=%t", err, reached)
			}
			if err := f.db.Callback().Query().Remove("named:query-fault"); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Callback().Update().Remove("named:update-fault"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, readPeer(t, f)) {
				t.Fatal("mixed failure retained peer association/owner/text")
			}
			var current model.AddressBook
			if err := f.db.First(&current, entry.RowId).Error; err != nil || !reflect.DeepEqual(current, entry) {
				t.Fatal("mixed failure retained book write", err)
			}
			var tags int64
			if err := f.db.Model(&model.Tag{}).Count(&tags).Error; err != nil || tags != 0 {
				t.Fatal("mixed failure retained tag", err)
			}
			if err := f.store.Assign(context.Background(), f.actor, request); err != nil {
				t.Fatal("retry", err)
			}
			peer := readPeer(t, f)
			if peer.StrategyDefinitionID == nil || *peer.StrategyDefinitionID != definition.Id || peer.UserId != f.owner.Id || peer.Note != "after" {
				t.Fatal("retry lost mixed assignment")
			}
			if err := f.db.First(&current, entry.RowId).Error; err != nil || current.Alias != "after" || string(current.Tags) != `["after"]` {
				t.Fatal("retry lost book assignment", err)
			}
		})
	}
}

func TestNamedAssignmentRechecksExpiryAndCancellationAfterDefinition(t *testing.T) {
	for _, mode := range []string{"expiry", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, book, definition := namedAssignmentFixture(t)
			clock := time.Now().UTC()
			store, err := clientassign.New(f.db, clientassign.Options{Now: func() time.Time { return clock }})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reached := false
			if err := f.db.Callback().Query().After("gorm:query").Register("named:expiry-after-definition", func(tx *gorm.DB) {
				if tx.Statement.Table != "strategy_definitions" {
					return
				}
				reached = true
				if mode == "expiry" {
					clock = f.actor.Session.ExpiresAt.Add(time.Second)
				} else {
					cancel()
				}
			}); err != nil {
				t.Fatal(err)
			}
			before := readPeer(t, f)
			request := clientassign.Input{ID: f.peer.Id, UUID: f.peer.Uuid, UserName: field(f.owner.Username), Note: field("must-rollback"), AddressBookName: field(book.Name), AddressBookTag: field("must-rollback"), StrategyName: field(string(definition.Name))}
			want := auth.ErrUnauthenticated
			if mode == "cancel" {
				want = context.Canceled
			}
			if err := store.Assign(ctx, f.actor, request); !errors.Is(err, want) || !reached {
				t.Fatalf("lock-time lifetime change not reached/rejected: %v reached=%t", err, reached)
			}
			if !reflect.DeepEqual(before, readPeer(t, f)) {
				t.Fatal("expired/canceled assignment retained owner/strategy/device writes")
			}
			for _, row := range []any{&model.AddressBook{}, &model.Tag{}} {
				var count int64
				if err := f.db.Model(row).Count(&count).Error; err != nil || count != 0 {
					t.Fatal("expired/canceled assignment retained book/tag writes", err)
				}
			}
		})
	}
}
