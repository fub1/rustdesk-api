package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
)

func TestClientSharedWriteRulesAndBatchRollback(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		owner := model.User{Username: "book-owner", GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
		actor := model.User{Username: "book-writer", GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
		other := model.User{Username: "other-owner", GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
		for _, user := range []*model.User{&owner, &actor, &other} {
			transactionCreate(t, f.DB, user)
		}
		book := model.AddressBookCollection{UserId: owner.Id, Name: "shared-book"}
		wrong := model.AddressBookCollection{UserId: owner.Id, Name: "wrong-rule-book"}
		transactionCreate(t, f.DB, &book)
		transactionCreate(t, f.DB, &wrong)
		for _, level := range []int{1, 3} {
			transactionCreate(t, f.DB, &model.AddressBookCollectionRule{UserId: owner.Id, CollectionId: book.Id, Type: 1, ToId: actor.Id, Rule: level})
		}
		transactionCreate(t, f.DB, &model.AddressBookCollectionRule{UserId: owner.Id, CollectionId: book.Id, Type: 2, ToId: actor.GroupId, Rule: 2})
		// The collection ID and recipient match, but the rule's owner is false.
		transactionCreate(t, f.DB, &model.AddressBookCollectionRule{UserId: other.Id, CollectionId: wrong.Id, Type: 1, ToId: actor.Id, Rule: 3})
		store, err := clientcompat.New(f.DB)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		who := clientcompat.Actor{ID: actor.Id}
		guid := fmt.Sprintf("%d-%d-%d", owner.GroupId, owner.Id, book.Id)
		wrongGuid := fmt.Sprintf("%d-%d-%d", owner.GroupId, owner.Id, wrong.Id)
		if err := store.AddTag(ctx, who, guid, clientcompat.Tag{Name: "max-rule", Color: 7}); err != nil {
			t.Fatalf("MAX rule denied: %v", err)
		}
		if err := store.AddTag(ctx, who, wrongGuid, clientcompat.Tag{Name: "forbidden"}); !errors.Is(err, clientcompat.ErrForbidden) {
			t.Fatalf("wrong owner's rule was accepted: %v", err)
		}
		if err := store.ColorTag(ctx, who, guid, clientcompat.Tag{Name: "max-rule", Color: 0}); err != nil {
			t.Fatal(err)
		}
		var tag model.Tag
		if err := f.DB.Where("user_id = ? AND collection_id = ? AND name = ?", owner.Id, book.Id, "max-rule").First(&tag).Error; err != nil || tag.Color != 0 {
			t.Fatalf("explicit zero color not stored: %v", err)
		}
		for _, id := range []string{"batch-a", "batch-b"} {
			transactionCreate(t, f.DB, &model.Tag{UserId: owner.Id, CollectionId: book.Id, Name: id})
			transactionCreate(t, f.DB, &model.AddressBook{Id: id, UserId: owner.Id, CollectionId: book.Id, Tags: custom_types.AutoJson(`[]`), Note: "preserve"})
		}
		if err := store.DeleteTags(ctx, who, guid, []string{"batch-a", "missing"}); !errors.Is(err, clientcompat.ErrNotFound) {
			t.Fatalf("missing tag failure: %v", err)
		}
		if err := store.DeletePeers(ctx, who, guid, []string{"batch-a", "missing"}); !errors.Is(err, clientcompat.ErrNotFound) {
			t.Fatalf("missing peer failure: %v", err)
		}
		var tags, peers int64
		if err := f.DB.Model(&model.Tag{}).Where("user_id = ? AND collection_id = ? AND name IN ?", owner.Id, book.Id, []string{"batch-a", "batch-b"}).Count(&tags).Error; err != nil {
			t.Fatal(err)
		}
		if err := f.DB.Model(&model.AddressBook{}).Where("user_id = ? AND collection_id = ?", owner.Id, book.Id).Count(&peers).Error; err != nil {
			t.Fatal(err)
		}
		if tags != 2 || peers != 2 {
			t.Fatalf("partial batch commit: tags=%d peers=%d", tags, peers)
		}
		// Lower all personal grants: group rule 2 still permits writes, not deletes.
		if err := f.DB.Model(&model.AddressBookCollectionRule{}).Where("user_id = ? AND collection_id = ? AND type = 1", owner.Id, book.Id).Update("rule", 1).Error; err != nil {
			t.Fatal(err)
		}
		if err := store.AddTag(ctx, who, guid, clientcompat.Tag{Name: "read-write"}); err != nil {
			t.Fatal(err)
		}
		for _, err := range []error{store.DeleteTags(ctx, who, guid, []string{"batch-a"}), store.DeletePeers(ctx, who, guid, []string{"batch-a"})} {
			if !errors.Is(err, clientcompat.ErrForbidden) {
				t.Fatalf("read-write rule authorized deletion: %v", err)
			}
		}
	})
}

func TestClientSharedWriteRealCommitFailure(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		store, err := clientcompat.New(f.DB)
		if err != nil {
			t.Fatal(err)
		}
		var owner model.User
		if err := f.DB.First(&owner, 1).Error; err != nil {
			t.Fatal(err)
		}
		guid := fmt.Sprintf("%d-%d-0", owner.GroupId, owner.Id)
		remove := failTransactionCommit(t, f, "tags")
		err = store.AddTag(context.Background(), clientcompat.Actor{ID: owner.Id}, guid, clientcompat.Tag{Name: "commit-failed"})
		if !errors.Is(err, clientcompat.ErrUnavailable) {
			t.Fatalf("failed COMMIT returned success: %v", err)
		}
		remove()
		fresh := f.Reopen(t)
		var count int64
		if err := fresh.Model(&model.Tag{}).Where("user_id = ? AND name = ?", owner.Id, "commit-failed").Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("failed COMMIT retained tag: count=%d err=%v", count, err)
		}
		if err := store.AddTag(context.Background(), clientcompat.Actor{ID: owner.Id}, guid, clientcompat.Tag{Name: "commit-failed"}); err != nil {
			t.Fatalf("failed COMMIT poisoned retry: %v", err)
		}
	})
}
