package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/internal/collectionguard"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

func TestLegacyCollectionGuardRejectsReferencedBook(t *testing.T) {
	f := collectionFixtureDB(t)
	issuanceCreate(t, f.db, &model.Tag{UserId: f.owner.Id, CollectionId: f.book.Id, Name: "keep"})
	peer := model.Peer{Id: "primary-reference", Uuid: "primary-uuid", UserId: f.other.Id, AbId: f.book.Id, Alias: "keep-alias"}
	issuanceCreate(t, f.db, &peer)
	if err := f.db.First(&peer, peer.RowId).Error; err != nil {
		t.Fatal(err)
	}
	before := collectionRows(t, f.db)
	if err := (&AddressBookService{}).DeleteCollection(&f.book); !errors.Is(err, collectionguard.ErrReferenced) {
		t.Fatal("referenced primary book was not rejected", err)
	}
	if !reflect.DeepEqual(before, collectionRows(t, f.db)) {
		t.Fatal("referenced delete changed children")
	}
	var after model.Peer
	if err := f.db.First(&after, peer.RowId).Error; err != nil || !reflect.DeepEqual(peer, after) {
		t.Fatal("referenced delete changed peer/alias", err)
	}
}

func TestLegacyCollectionGuardChecksBothCountsAndFailsClosed(t *testing.T) {
	for _, mode := range []string{"first-count-error", "second-count-error", "new-reference-before-parent-lock"} {
		t.Run(mode, func(t *testing.T) {
			f := collectionFixtureDB(t)
			before := collectionRows(t, f.db)
			counts, reached := 0, false
			if err := f.db.Callback().Query().After("gorm:query").Register("collection:reference-transition", func(tx *gorm.DB) {
				if tx.Statement.Table != "peers" {
					return
				}
				counts++
				if mode == "first-count-error" && counts == 1 || mode == "second-count-error" && counts == 2 {
					reached = true
					tx.AddError(errors.New("private reference query failure"))
				}
				if mode == "new-reference-before-parent-lock" && counts == 1 {
					reached = true
					if err := tx.Session(&gorm.Session{NewDB: true}).Exec("INSERT INTO peers(id,uuid,user_id,ab_id,note) VALUES ('late-reference','late-uuid',?,?, '')", f.other.Id, f.book.Id).Error; err != nil {
						tx.AddError(err)
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			want := collectionguard.ErrUnavailable
			if mode == "new-reference-before-parent-lock" {
				want = collectionguard.ErrReferenced
			}
			if err := (&AddressBookService{}).DeleteCollection(&f.book); !errors.Is(err, want) || !reached {
				t.Fatalf("reference boundary not reached/rejected counts=%d err=%v", counts, err)
			}
			if err := f.db.Callback().Query().Remove("collection:reference-transition"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, collectionRows(t, f.db)) {
				t.Fatal("reference rejection changed collection children")
			}
			var refs int64
			if err := f.db.Model(&model.Peer{}).Where("ab_id=?", f.book.Id).Count(&refs).Error; err != nil || refs != 0 {
				t.Fatal("injected transaction reference did not roll back", err)
			}
		})
	}
}

func TestLegacyCollectionGuardContextAndCreateResult(t *testing.T) {
	f := collectionFixtureDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	DB = f.db.WithContext(ctx)
	for _, run := range []func() error{
		func() error {
			return (&AddressBookService{}).CreateCollection(&model.AddressBookCollection{UserId: f.owner.Id, Name: "cancelled"})
		},
		func() error {
			return (&AddressBookService{}).UpdateCollection(&model.AddressBookCollection{IdModel: f.book.IdModel, Name: "cancelled"})
		},
		func() error { return (&AddressBookService{}).DeleteCollection(&f.book) },
	} {
		if err := run(); !errors.Is(err, context.Canceled) {
			t.Fatal("cancelled legacy operation acknowledged", err)
		}
	}
	DB = f.db
	created := model.AddressBookCollection{UserId: f.owner.Id, Name: "valid"}
	if err := (&AddressBookService{}).CreateCollection(&created); err != nil || created.Id == 0 || created.UserId != f.owner.Id || created.Name != "valid" {
		t.Fatal("committed create result missing", err)
	}
}

func TestLegacyCollectionGuardCleansTagsAndPreservesOtherScopes(t *testing.T) {
	f := collectionFixtureDB(t)
	issuanceCreate(t, f.db, &model.Tag{UserId: f.owner.Id, CollectionId: f.book.Id, Name: "delete"})
	personal := model.Tag{UserId: f.owner.Id, CollectionId: 0, Name: "keep-personal"}
	issuanceCreate(t, f.db, &personal)
	other := model.AddressBookCollection{UserId: f.other.Id, Name: "keep-book"}
	issuanceCreate(t, f.db, &other)
	issuanceCreate(t, f.db, &model.Tag{UserId: f.other.Id, CollectionId: other.Id, Name: "keep-other"})
	if err := (&AddressBookService{}).DeleteCollection(&f.book); err != nil {
		t.Fatal(err)
	}
	var deleted, retained int64
	if err := f.db.Model(&model.Tag{}).Where("collection_id = ?", f.book.Id).Count(&deleted).Error; err != nil || deleted != 0 {
		t.Fatal("collection delete left orphan tags", err)
	}
	if err := f.db.Model(&model.Tag{}).Count(&retained).Error; err != nil || retained != 2 {
		t.Fatal("collection delete crossed another scope", err)
	}
}

func TestLegacyCollectionGuardRejectsTransferAndInvalidCreateOwner(t *testing.T) {
	f := collectionFixtureDB(t)
	before := collectionRows(t, f.db)
	if err := (&AddressBookService{}).UpdateCollection(&model.AddressBookCollection{IdModel: f.book.IdModel, UserId: f.other.Id, Name: "must-not-transfer"}); err == nil {
		t.Fatal("ordinary collection edit transferred owner")
	}
	if !reflect.DeepEqual(before, collectionRows(t, f.db)) {
		t.Fatal("rejected transfer changed child or parent")
	}
	for _, owner := range []uint{0, 999999} {
		if err := (&AddressBookService{}).CreateCollection(&model.AddressBookCollection{UserId: owner, Name: "invalid"}); err == nil {
			t.Fatalf("invalid owner %d accepted", owner)
		}
	}
	if err := f.db.Model(&f.other).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&AddressBookService{}).CreateCollection(&model.AddressBookCollection{UserId: f.other.Id, Name: "disabled"}); err == nil {
		t.Fatal("disabled owner accepted")
	}
}
