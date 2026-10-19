package integration

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
)

func sciterDocument(t *testing.T, raw string) clientcompat.Document {
	t.Helper()
	var doc clientcompat.Document
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestClientSciterOmittedColorsRealEngines(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		owner := model.User{Username: "sciter-owner", GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
		foreign := model.User{Username: "sciter-foreign", GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
		transactionCreate(t, f.DB, &owner)
		transactionCreate(t, f.DB, &foreign)
		book := model.AddressBookCollection{UserId: owner.Id, Name: "separate"}
		transactionCreate(t, f.DB, &book)
		for _, tag := range []model.Tag{
			{UserId: owner.Id, Name: "foo", Color: 7},
			{UserId: owner.Id, Name: "FOO", Color: 8},
			{UserId: owner.Id, Name: "drop", Color: 9},
			{UserId: owner.Id, CollectionId: book.Id, Name: "shared", Color: 10},
			{UserId: foreign.Id, Name: "foreign", Color: 11},
		} {
			transactionCreate(t, f.DB, &tag)
		}
		peer := model.AddressBook{Id: "kept", UserId: owner.Id, Alias: "before", Note: "keep note", RdpPort: "3389", Tags: custom_types.AutoJson(`["foo","FOO"]`)}
		transactionCreate(t, f.DB, &peer)
		for _, row := range []model.AddressBook{
			{Id: "shared-peer", UserId: owner.Id, CollectionId: book.Id, Tags: custom_types.AutoJson(`[]`)},
			{Id: "foreign-peer", UserId: foreign.Id, Tags: custom_types.AutoJson(`[]`)},
		} {
			transactionCreate(t, f.DB, &row)
		}
		store, err := clientcompat.New(f.Reopen(t))
		if err != nil {
			t.Fatal(err)
		}
		actor, ctx := clientcompat.Actor{ID: owner.Id}, context.Background()
		doc := sciterDocument(t, `{"tags":["foo","FOO","new","foo"],"peers":[{"id":"kept","alias":"after"}]}`)
		if err := store.ReplacePersonal(ctx, actor, doc); err != nil {
			t.Fatal("Sciter omitted colors rejected", err)
		}
		peers, tags, err := store.Personal(ctx, actor)
		want := []clientcompat.Tag{{Name: "foo", Color: 7}, {Name: "FOO", Color: 8}, {Name: "new", Color: 0}}
		if err != nil || !reflect.DeepEqual(tags, want) || len(peers) != 1 || peers[0].Alias != "after" || peers[0].Note != peer.Note || peers[0].RdpPort != peer.RdpPort || !reflect.DeepEqual(peers[0].Tags, []string{"foo", "FOO"}) {
			t.Fatalf("omission lost colors or extension fields: %+v %+v %v", tags, peers, err)
		}
		// An explicit zero still overrides, missing keys retain current metadata,
		// and a stale color key does not recreate a tag absent from the fact set.
		doc = sciterDocument(t, `{"tags":["foo","FOO","new"],"peers":[{"id":"kept"}],"tag_colors":"{\"foo\":0,\"drop\":42}"}`)
		if err := store.ReplacePersonal(ctx, actor, doc); err != nil {
			t.Fatal(err)
		}
		_, tags, err = store.Personal(ctx, actor)
		want[0].Color = 0
		if err != nil || !reflect.DeepEqual(tags, want) {
			t.Fatalf("explicit colors differ: %+v %v", tags, err)
		}
		if err := store.ReplacePersonal(ctx, actor, sciterDocument(t, `{"tags":[],"peers":[]}`)); err != nil {
			t.Fatal("Sciter reset rejected", err)
		}
		peers, tags, err = store.Personal(ctx, actor)
		if err != nil || len(peers) != 0 || len(tags) != 0 {
			t.Fatalf("reset did not clear personal book: %+v %+v %v", peers, tags, err)
		}
		if transactionCount(t, f.DB, &model.AddressBook{}) != 2 || transactionCount(t, f.DB, &model.Tag{}) != 2 {
			t.Fatal("personal replacement changed another owner/collection")
		}
	})
}

func TestClientSciterReplacementRealCommitRollback(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "omitted-colors"
		if explicit {
			name = "explicit-colors"
		}
		t.Run(name, func(t *testing.T) {
			forEngines(t, func(t *testing.T, f *fixture) {
				owner := model.User{Username: "sciter-commit-owner", GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
				transactionCreate(t, f.DB, &owner)
				tag := model.Tag{UserId: owner.Id, Name: "old", Color: 7}
				peer := model.AddressBook{Id: "kept", UserId: owner.Id, Alias: "before", Note: "note", Tags: custom_types.AutoJson(`["old"]`)}
				transactionCreate(t, f.DB, &tag)
				transactionCreate(t, f.DB, &peer)
				store, err := clientcompat.New(f.DB)
				if err != nil {
					t.Fatal(err)
				}
				raw := `{"tags":["new"],"peers":[{"id":"kept","alias":"after","tags":["new"]}]}`
				if explicit {
					raw = `{"tags":["new"],"peers":[{"id":"kept","alias":"after","tags":["new"]}],"tag_colors":"{\"new\":42}"}`
				}
				doc := sciterDocument(t, raw)
				// Exactly one new tag makes this successful INSERT the final SQL
				// statement: the fault is at real COMMIT, not midway through a loop.
				remove := failTransactionCommit(t, f, "tags")
				actor, ctx := clientcompat.Actor{ID: owner.Id}, context.Background()
				if err := store.ReplacePersonal(ctx, actor, doc); !errors.Is(err, clientcompat.ErrUnavailable) {
					t.Fatalf("real COMMIT failure acknowledged: %v", err)
				}
				remove()
				fresh, err := clientcompat.New(f.Reopen(t))
				if err != nil {
					t.Fatal(err)
				}
				peers, tags, err := fresh.Personal(ctx, actor)
				if err != nil || len(peers) != 1 || peers[0].Alias != "before" || peers[0].Note != "note" || !reflect.DeepEqual(peers[0].Tags, []string{"old"}) || !reflect.DeepEqual(tags, []clientcompat.Tag{{Name: "old", Color: 7}}) {
					t.Fatalf("failed COMMIT persisted partial book: %+v %+v %v", peers, tags, err)
				}
				if err := fresh.ReplacePersonal(ctx, actor, doc); err != nil {
					t.Fatal("failed COMMIT poisoned retry", err)
				}
				peers, tags, err = fresh.Personal(ctx, actor)
				color := uint(0)
				if explicit {
					color = 42
				}
				if err != nil || len(peers) != 1 || peers[0].Alias != "after" || !reflect.DeepEqual(peers[0].Tags, []string{"new"}) || !reflect.DeepEqual(tags, []clientcompat.Tag{{Name: "new", Color: color}}) {
					t.Fatalf("retry did not persist one complete book: %+v %+v %v", peers, tags, err)
				}
			})
		})
	}
}
