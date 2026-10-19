package integration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

func TestClientTagConcurrentIndependentPools(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-target=%t", conflict), func(t *testing.T) {
			forEngines(t, func(t *testing.T, f *fixture) {
				owner := model.User{Username: "tag-owner", GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
				transactionCreate(t, f.DB, &owner)
				book := model.AddressBookCollection{UserId: owner.Id, Name: "concurrent"}
				transactionCreate(t, f.DB, &book)
				guid := fmt.Sprintf("%d-%d-%d", owner.GroupId, owner.Id, book.Id)
				labels := []model.Tag{{UserId: owner.Id, CollectionId: book.Id, Name: "old-a", Color: 7}, {UserId: owner.Id, CollectionId: book.Id, Name: "old-b", Color: 8}}
				transactionCreate(t, f.DB, &labels)
				row := model.AddressBook{Id: "both-tags", UserId: owner.Id, CollectionId: book.Id, Tags: custom_types.AutoJson(`["old-a","old-b","keep"]`), Note: "preserved", Alias: "alias", RdpPort: "3389", ForceAlwaysRelay: true}
				transactionCreate(t, f.DB, &row)
				start := make(chan struct{})
				type outcome struct {
					index int
					err   error
				}
				results := make(chan outcome, 2)
				for i := range labels {
					actor := model.User{Username: fmt.Sprintf("writer-%d", i), GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
					transactionCreate(t, f.DB, &actor)
					transactionCreate(t, f.DB, &model.AddressBookCollectionRule{UserId: owner.Id, CollectionId: book.Id, Type: 1, ToId: actor.Id, Rule: 2})
					store, err := clientcompat.New(f.Reopen(t))
					if err != nil {
						t.Fatal(err)
					}
					go func(i int, actor model.User, store *clientcompat.Store) {
						<-start
						ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
						defer cancel()
						name := fmt.Sprintf("new-%c", 'a'+i)
						if conflict {
							name = "same-new"
						}
						results <- outcome{i, store.RenameTag(ctx, clientcompat.Actor{ID: actor.Id}, guid, labels[i].Name, name)}
					}(i, actor, store)
				}
				close(start)
				winner, failures := -1, 0
				for i := 0; i < 2; i++ {
					result := <-results
					if result.err == nil {
						winner = result.index
					} else {
						failures++
						if !conflict || !errors.Is(result.err, clientcompat.ErrInput) {
							t.Fatalf("unexpected competing write failure: %v", result.err)
						}
					}
				}
				want := `["new-a","new-b","keep"]`
				if conflict {
					if winner < 0 || failures != 1 {
						t.Fatalf("same target: winner=%d failures=%d", winner, failures)
					}
					if winner == 0 {
						want = `["same-new","old-b","keep"]`
					} else {
						want = `["old-a","same-new","keep"]`
					}
				}
				var got model.AddressBook
				if err := f.Reopen(t).First(&got, row.RowId).Error; err != nil || string(got.Tags) != want || got.Note != row.Note || got.Alias != row.Alias || got.RdpPort != row.RdpPort || got.ForceAlwaysRelay != row.ForceAlwaysRelay {
					t.Fatalf("competition lost a reference/other field: %+v %v", got, err)
				}
				var stored []model.Tag
				if err := f.DB.Where("user_id = ? AND collection_id = ?", owner.Id, book.Id).Order("id").Find(&stored).Error; err != nil {
					t.Fatal(err)
				}
				for i, tag := range stored {
					name := fmt.Sprintf("new-%c", 'a'+i)
					if conflict {
						name = labels[i].Name
						if i == winner {
							name = "same-new"
						}
					}
					if tag.Name != name || tag.Color != labels[i].Color {
						t.Fatalf("tag/color and references disagree: %+v", stored)
					}
				}
			})
		})
	}
}

func TestClientTagExactNamesAndReferences(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		owner := model.User{Username: "exact-tag-owner", GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
		transactionCreate(t, f.DB, &owner)
		store, err := clientcompat.New(f.DB)
		if err != nil {
			t.Fatal(err)
		}
		ctx, actor, guid := context.Background(), clientcompat.Actor{ID: owner.Id}, fmt.Sprintf("1-%d-0", owner.Id)
		names := []string{"foo", "FOO", "fóo", "foo "}
		for i, name := range names {
			if err := store.AddTag(ctx, actor, guid, clientcompat.Tag{Name: name, Color: uint(i + 1)}); err != nil {
				t.Fatalf("distinct exact name %q rejected: %v", name, err)
			}
		}
		row := model.AddressBook{Id: "exact-tags", UserId: owner.Id, Tags: custom_types.AutoJson(`["foo","FOO","fóo","foo "]`), Note: "kept"}
		transactionCreate(t, f.DB, &row)
		if err := store.RenameTag(ctx, actor, guid, "foo", "bar"); err != nil {
			t.Fatal(err)
		}
		if err := store.RenameTag(ctx, actor, guid, "FOO", "fóo"); !errors.Is(err, clientcompat.ErrInput) {
			t.Fatal("existing exact destination accepted", err)
		}
		if err := store.ColorTag(ctx, actor, guid, clientcompat.Tag{Name: "FOO", Color: 500}); err != nil {
			t.Fatal(err)
		}
		labels, err := store.Tags(ctx, actor, guid)
		want := []clientcompat.Tag{{Name: "bar", Color: 1}, {Name: "FOO", Color: 500}, {Name: "fóo", Color: 3}, {Name: "foo ", Color: 4}}
		if err != nil || !reflect.DeepEqual(labels, want) {
			t.Fatalf("exact rename/color touched another name: %+v %v", labels, err)
		}
		if err := store.DeleteTags(ctx, actor, guid, []string{"FOO"}); err != nil {
			t.Fatal(err)
		}
		if err := f.DB.First(&row, row.RowId).Error; err != nil || string(row.Tags) != `["bar","fóo","foo "]` || row.Note != "kept" {
			t.Fatalf("exact deletion references: %s %v", row.Tags, err)
		}
		labels, err = store.Tags(ctx, actor, guid)
		want = []clientcompat.Tag{want[0], want[2], want[3]}
		if err != nil || !reflect.DeepEqual(labels, want) {
			t.Fatalf("exact deletion removed another name: %+v %v", labels, err)
		}
	})
}

// Fail at the actual COMMIT after a successful reference UPDATE, rather than
// simulating a SQL error before the cross-table transaction has done its work.
func failReferenceCommit(t *testing.T, f *fixture) func() {
	t.Helper()
	if f.Engine == "postgres" {
		transactionExec(t, f.DB, "CREATE TABLE integration_ref_parent (id bigint PRIMARY KEY)")
		transactionExec(t, f.DB, "CREATE TABLE integration_ref_guard (parent_id bigint REFERENCES integration_ref_parent(id) DEFERRABLE INITIALLY DEFERRED)")
		transactionExec(t, f.DB, "CREATE FUNCTION integration_ref_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO integration_ref_guard(parent_id) VALUES (999999); RETURN NEW; END $$")
		transactionExec(t, f.DB, "CREATE TRIGGER integration_ref_fail_commit AFTER UPDATE ON address_books FOR EACH ROW EXECUTE FUNCTION integration_ref_fail_commit()")
		return func() {
			var count int64
			if err := f.DB.Model(&model.AddressBook{}).Where("tags LIKE ?", "%old%").Count(&count).Error; err != nil || count != 1 {
				t.Fatal("failed PostgreSQL COMMIT did not preserve reference", err)
			}
			transactionExec(t, f.DB, "DROP TRIGGER integration_ref_fail_commit ON address_books")
			transactionExec(t, f.DB, "DROP FUNCTION integration_ref_fail_commit()")
			transactionExec(t, f.DB, "DROP TABLE integration_ref_guard")
			transactionExec(t, f.DB, "DROP TABLE integration_ref_parent")
		}
	}
	control := f.Reopen(t)
	var reached atomic.Bool
	var killed atomic.Bool
	if err := f.DB.Callback().Update().After("gorm:update").Register("tagrefs:kill-before-commit", func(tx *gorm.DB) {
		if tx.Statement.Table != "address_books" || tx.Error != nil || !reached.CompareAndSwap(false, true) {
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
		if !reached.Load() {
			t.Error("real reference UPDATE was never reached")
		}
		if !killed.Load() {
			t.Error("actual MySQL transaction connection was not terminated")
		}
		if err := f.DB.Callback().Update().Remove("tagrefs:kill-before-commit"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClientTagReferencesRealCommitRollback(t *testing.T) {
	for _, action := range []string{"rename", "delete"} {
		t.Run(action, func(t *testing.T) {
			forEngines(t, func(t *testing.T, f *fixture) {
				owner := model.User{Username: "tag-commit-owner", GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
				transactionCreate(t, f.DB, &owner)
				label := model.Tag{UserId: owner.Id, Name: "old", Color: 7}
				transactionCreate(t, f.DB, &label)
				row := model.AddressBook{Id: "commit-peer", UserId: owner.Id, Tags: custom_types.AutoJson(`["old","keep"]`), Note: "kept"}
				transactionCreate(t, f.DB, &row)
				store, err := clientcompat.New(f.DB)
				if err != nil {
					t.Fatal(err)
				}
				remove := failReferenceCommit(t, f)
				actor, guid := clientcompat.Actor{ID: owner.Id}, fmt.Sprintf("1-%d-0", owner.Id)
				if action == "rename" {
					err = store.RenameTag(context.Background(), actor, guid, "old", "new")
				} else {
					err = store.DeleteTags(context.Background(), actor, guid, []string{"old"})
				}
				if !errors.Is(err, clientcompat.ErrUnavailable) {
					t.Fatalf("real COMMIT failure acknowledged: %v", err)
				}
				remove()
				fresh := f.Reopen(t)
				var got model.Tag
				if err := fresh.First(&got, label.Id).Error; err != nil || got.Name != "old" || got.Color != 7 {
					t.Fatalf("tag/color escaped failed COMMIT: %+v %v", got, err)
				}
				if err := fresh.First(&row, row.RowId).Error; err != nil || string(row.Tags) != `["old","keep"]` || row.Note != "kept" {
					t.Fatalf("reference escaped failed COMMIT: %s %v", row.Tags, err)
				}
				if action == "rename" {
					err = store.RenameTag(context.Background(), actor, guid, "old", "new")
				} else {
					err = store.DeleteTags(context.Background(), actor, guid, []string{"old"})
				}
				if err != nil {
					t.Fatal("failed COMMIT poisoned retry", err)
				}
			})
		})
	}
}
