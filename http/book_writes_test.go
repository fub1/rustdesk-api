package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/bookadmin"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionadmin"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	"github.com/lejianwen/rustdesk-api/v2/model"
)

func bookManagementFixture(t *testing.T) (definitionFixture, model.AddressBookCollection) {
	t.Helper()
	f, book := collectionFixture(t)
	// These are valid controls; NULL/malformed historical tag storage has
	// separate fail-closed tests and must not stand in for an empty array.
	if err := f.db.Model(&model.AddressBook{}).Where("collection_id=?", book.Id).UpdateColumn("tags", "[]").Error; err != nil {
		t.Fatal(err)
	}
	sessions, err := app.NewSessions(f.db, time.Hour, global.Jwt.Key)
	if err != nil {
		t.Fatal(err)
	}
	books, err := bookadmin.New(f.db, bookadmin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	collections, err := collectionadmin.New(f.db, collectionadmin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h, err := httpapi.New(sessions, httpapi.Options{Translate: LegacyTranslator(), Books: books, Collections: collections})
	if err != nil {
		t.Fatal(err)
	}
	f.handler, err = NewHandler(h)
	if err != nil {
		t.Fatal(err)
	}
	return f, book
}

func TestBookManagementHTTPPostAuthenticationRevocation(t *testing.T) {
	for _, scope := range []string{"admin", "my"} {
		for _, resource := range []string{"tag", "address_book_collection_rule"} {
			for _, action := range []string{"create", "update", "delete"} {
				t.Run(scope+"/"+resource+"/"+action, func(t *testing.T) {
					f, book := bookManagementFixture(t)
					actor, prefix := f.admin, "/api/admin/"
					if scope == "my" {
						actor, prefix = f.owner, "/api/admin/my/"
					}
					id := uint(0)
					if action != "create" {
						if resource == "tag" {
							var row model.Tag
							if err := f.db.Where("collection_id=?", book.Id).First(&row).Error; err != nil {
								t.Fatal(err)
							}
							id = row.Id
						} else {
							var row model.AddressBookCollectionRule
							if err := f.db.Where("collection_id=?", book.Id).First(&row).Error; err != nil {
								t.Fatal(err)
							}
							id = row.Id
						}
					}
					body := fmt.Sprintf(`{"id":%d,"user_id":%d,"collection_id":%d,"name":"changed","color":7,"type":1,"to_id":%d,"rule":2}`, id, book.UserId, book.Id, f.hidden.Id)
					before := collectionSnapshot(t, f)
					fired := false
					reader := &collectionChangingReader{Reader: strings.NewReader(body), change: func() error {
						fired = true
						return f.db.Exec("DELETE FROM user_tokens WHERE token=?", f.tokens[actor.Id]).Error
					}}
					r := httptest.NewRequest("POST", prefix+resource+"/"+action, reader)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("api-token", f.tokens[actor.Id])
					w := httptest.NewRecorder()
					f.handler.ServeHTTP(w, r)
					definitionResult(t, w, 200, 403)
					if !fired || !reflect.DeepEqual(before, collectionSnapshot(t, f)) {
						t.Fatal("post-auth revoked management write retained data")
					}
					// Re-admit this user with a real stored session and prove that the
					// exact route/body reaches its intended resource successfully.
					if err := f.db.Create(&model.UserToken{UserId: actor.Id, Token: f.tokens[actor.Id], ExpiredAt: time.Now().Add(time.Hour).Unix()}).Error; err != nil {
						t.Fatal(err)
					}
					definitionResult(t, f.request(context.Background(), "POST", prefix+resource+"/"+action, body, f.tokens[actor.Id]), 200, 0)
					var count int64
					if resource == "tag" {
						query := f.db.Model(&model.Tag{}).Where("user_id=? AND collection_id=?", book.UserId, book.Id)
						if action == "delete" {
							query = query.Where("id=?", id)
						} else {
							query = query.Where("name=? AND color=?", "changed", 7)
						}
						if err := query.Count(&count).Error; err != nil {
							t.Fatal(err)
						}
					} else {
						query := f.db.Model(&model.AddressBookCollectionRule{}).Where("user_id=? AND collection_id=?", book.UserId, book.Id)
						if action == "delete" {
							query = query.Where("id=?", id)
						} else {
							query = query.Where("to_id=? AND type=? AND rule=?", f.hidden.Id, 1, 2)
						}
						if err := query.Count(&count).Error; err != nil {
							t.Fatal(err)
						}
					}
					want := int64(1)
					if action == "delete" {
						want = 0
					}
					if count != want {
						t.Fatal("valid control did not change the intended resource", resource, action, count)
					}
				})
			}
		}
	}
}

func TestBookManagementHTTPMyOwnerOnlyAndImmutableScope(t *testing.T) {
	f, book := bookManagementFixture(t)
	var tag model.Tag
	var rule model.AddressBookCollectionRule
	if err := f.db.Where("collection_id=?", book.Id).First(&tag).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Where("collection_id=?", book.Id).First(&rule).Error; err != nil {
		t.Fatal(err)
	}
	before := collectionSnapshot(t, f)
	// Actor is also fixture owner1 and a full-control grantee. Neither confers
	// management rights over another owner's book.
	for _, resource := range []string{"tag", "address_book_collection_rule"} {
		for _, action := range []string{"create", "update", "delete"} {
			id := uint(0)
			if action != "create" {
				id = tag.Id
				if resource != "tag" {
					id = rule.Id
				}
			}
			body := fmt.Sprintf(`{"id":%d,"user_id":%d,"collection_id":%d,"name":"forbidden","color":1,"type":1,"to_id":%d,"rule":2}`, id, book.UserId, book.Id, f.hidden.Id)
			w := f.request(context.Background(), "POST", "/api/admin/my/"+resource+"/"+action, body, f.tokens[f.actor.Id])
			var envelope struct{ Code int }
			if json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.Code == 0 || w.Code != 200 {
				t.Fatal("foreign my write was not rejected", w.Body.String())
			}
		}
	}
	for _, resource := range []string{"tag", "address_book_collection_rule"} {
		id := tag.Id
		if resource != "tag" {
			id = rule.Id
		}
		body := fmt.Sprintf(`{"id":%d,"user_id":%d,"collection_id":%d,"name":"transfer","color":3,"type":1,"to_id":%d,"rule":2}`, id, f.hidden.Id, book.Id, f.actor.Id)
		definitionResult(t, f.call("POST", "/api/admin/"+resource+"/update", body), 200, 101)
	}
	if !reflect.DeepEqual(before, collectionSnapshot(t, f)) {
		t.Fatal("rejected scope change changed records")
	}
	definitionResult(t, f.request(context.Background(), "POST", "/api/admin/my/tag/create", `{"name":"personal","color":0}`, f.tokens[f.actor.Id]), 200, 0)
	definitionResult(t, f.call("POST", "/api/admin/tag/create", fmt.Sprintf(`{"collection_id":%d,"name":"derived-owner","color":0}`, book.Id)), 200, 0)
}

func TestBookManagementHTTPTagReferencesAndSafeRollback(t *testing.T) {
	f, book := bookManagementFixture(t)
	var tag model.Tag
	if err := f.db.Where("collection_id=?", book.Id).First(&tag).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&model.AddressBook{}).Where("collection_id=?", book.Id).Updates(map[string]any{"tags": `["original-tag","keep"]`, "note": "note-preserved"}).Error; err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"id":%d,"name":"renamed","color":0,"created_at":"ignored"}`, tag.Id)
	definitionResult(t, f.call("POST", "/api/admin/tag/update", body), 200, 0)
	var entry model.AddressBook
	if err := f.db.Where("collection_id=?", book.Id).First(&entry).Error; err != nil || entry.Note != "note-preserved" || string(entry.Tags) != `["renamed","keep"]` {
		t.Fatal("tag rename did not atomically update exact book references", err, string(entry.Tags))
	}
	before := collectionSnapshot(t, f)
	if err := f.db.Exec("CREATE TRIGGER reject_legacy_tag_delete BEFORE DELETE ON tags BEGIN SELECT RAISE(ABORT,'private tag SQL'); END").Error; err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/api/admin/tag/delete", fmt.Sprintf(`{"id":%d}`, tag.Id))
	definitionResult(t, w, 503, 503)
	if strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "SQL") || !reflect.DeepEqual(before, collectionSnapshot(t, f)) {
		t.Fatal("tag SQL error leaked or retained reference rewrite", w.Body.String())
	}
	if err := f.db.Exec("DROP TRIGGER reject_legacy_tag_delete").Error; err != nil {
		t.Fatal(err)
	}
	definitionResult(t, f.call("POST", "/api/admin/tag/delete", fmt.Sprintf(`{"id":%d}`, tag.Id)), 200, 0)
	if err := f.db.Where("collection_id=?", book.Id).First(&entry).Error; err != nil || string(entry.Tags) != `["keep"]` || entry.Note != "note-preserved" {
		t.Fatal("tag removal changed unrelated fields or left references", err)
	}
}
