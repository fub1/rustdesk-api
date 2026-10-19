package http

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

func TestClientCompatSharedWritesUseMaximumScopedRule(t *testing.T) {
	for _, endpoint := range []string{"add", "rename", "update", "tag-delete", "peer-delete"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newCompatFixture(t)
			book := model.AddressBookCollection{UserId: f.owner.Id, Name: "shared"}
			compatCreate(t, &book)
			// First personal and group rows are read-only. Later rows jointly grant
			// full control; writes must agree with the profile's MAX projection.
			grants := []model.AddressBookCollectionRule{}
			for _, typ := range []int{1, 2} {
				to := f.actor.Id
				if typ == 2 {
					to = f.actor.GroupId
				}
				for _, rule := range []int{1, 2, 3} {
					grants = append(grants, model.AddressBookCollectionRule{UserId: f.owner.Id, CollectionId: book.Id, Type: typ, ToId: to, Rule: rule})
				}
			}
			compatCreate(t, &grants)
			compatCreate(t, &model.Tag{UserId: f.owner.Id, CollectionId: book.Id, Name: "old"})
			peer := compatRow("peer", f.owner, book.Id)
			compatCreate(t, &peer)
			method, path, body := sharedWriteRequest(endpoint, compatGUID(f.owner, book.Id))
			result := f.call(method, path, body, f.actor)
			if result.Code != 200 || result.Body.Len() != 0 {
				t.Fatalf("MAX rule write denied: %d %s", result.Code, result.Body.String())
			}
		})
	}
}

func sharedWriteRequest(endpoint, guid string) (string, string, string) {
	switch endpoint {
	case "add":
		return "POST", "/api/ab/tag/add/" + guid, `{"name":"new","color":7}`
	case "rename":
		return "PUT", "/api/ab/tag/rename/" + guid, `{"old":"old","new":"new"}`
	case "update":
		return "PUT", "/api/ab/tag/update/" + guid, `{"name":"old","color":7}`
	case "tag-delete":
		return "DELETE", "/api/ab/tag/" + guid, `["old"]`
	default:
		return "DELETE", "/api/ab/peer/" + guid, `["peer"]`
	}
}

func TestClientCompatSharedWritesRejectWrongOwnerRules(t *testing.T) {
	f := newCompatFixture(t)
	book := model.AddressBookCollection{UserId: f.owner.Id, Name: "shared"}
	compatCreate(t, &book)
	compatCreate(t, &model.AddressBookCollectionRule{UserId: f.hidden.Id, CollectionId: book.Id, Type: 1, ToId: f.actor.Id, Rule: 3})
	compatCreate(t, &model.Tag{UserId: f.owner.Id, CollectionId: book.Id, Name: "old"})
	peer := compatRow("peer", f.owner, book.Id)
	compatCreate(t, &peer)
	for _, endpoint := range []string{"add", "rename", "update", "tag-delete", "peer-delete"} {
		method, path, body := sharedWriteRequest(endpoint, compatGUID(f.owner, book.Id))
		result := f.call(method, path, body, f.actor)
		if result.Code != 400 || !strings.Contains(result.Body.String(), "NoAccess") {
			t.Errorf("wrong owner's rule authorized %s: %d %s", endpoint, result.Code, result.Body.String())
		}
	}
}

func TestClientCompatSharedAuthorizationStorageFailureIsUnavailable(t *testing.T) {
	f := newCompatFixture(t)
	book := model.AddressBookCollection{UserId: f.owner.Id, Name: "book"}
	compatCreate(t, &book)
	if err := global.DB.Migrator().DropTable(&model.AddressBookCollectionRule{}); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"add", "rename", "update", "tag-delete", "peer-delete"} {
		method, path, body := sharedWriteRequest(endpoint, compatGUID(f.owner, book.Id))
		result := f.call(method, path, body, f.actor)
		if result.Code != 503 || strings.Contains(result.Body.String(), "no such table") {
			t.Fatalf("grant storage failure mishandled %s: %d %s", endpoint, result.Code, result.Body.String())
		}
	}
}

func TestClientCompatBatchDeletesRollbackMissingAndWriteFailures(t *testing.T) {
	for _, kind := range []string{"tag", "peer"} {
		for _, fault := range []string{"missing", "write"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				f := newCompatFixture(t)
				table := "tags"
				if kind == "tag" {
					compatCreate(t, &[]model.Tag{{UserId: f.actor.Id, Name: "first"}, {UserId: f.actor.Id, Name: "second"}})
				} else {
					table = "address_books"
					compatCreate(t, &[]model.AddressBook{compatRow("first", f.actor, 0), compatRow("second", f.actor, 0)})
				}
				body := `["first","missing"]`
				want := 400
				if fault == "write" {
					body, want = `["first","second"]`, 503
					deletes := 0
					if err := global.DB.Callback().Delete().Before("gorm:delete").Register("shared-batch-failure", func(tx *gorm.DB) {
						if tx.Statement.Table == table {
							deletes++
							if deletes == 2 {
								tx.AddError(errors.New("private batch failure"))
							}
						}
					}); err != nil {
						t.Fatal(err)
					}
				}
				result := f.call("DELETE", "/api/ab/"+kind+"/"+compatGUID(f.actor, 0), body, f.actor)
				if result.Code != want || strings.Contains(result.Body.String(), "private batch") {
					t.Errorf("batch failure mapping: %d %s", result.Code, result.Body.String())
				}
				var count int64
				if err := global.DB.Table(table).Where("user_id = ?", f.actor.Id).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != 2 {
					t.Fatalf("batch partially committed: %s count %d", table, count)
				}
			})
		}
	}
}

func TestClientCompatTagLookupFailureDoesNotCreate(t *testing.T) {
	f := newCompatFixture(t)
	if err := global.DB.Callback().Query().Before("gorm:query").Register("shared-tag-query-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "tags" {
			tx.AddError(fmt.Errorf("private tag lookup failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	result := f.call("POST", "/api/ab/tag/add/"+compatGUID(f.actor, 0), `{"name":"new","color":7}`, f.actor)
	if result.Code != 503 || strings.Contains(result.Body.String(), "private tag") {
		t.Fatalf("tag lookup error swallowed: %d %s", result.Code, result.Body.String())
	}
	if err := global.DB.Callback().Query().Remove("shared-tag-query-failure"); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := global.DB.Model(&model.Tag{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed lookup still created tag")
	}
}

func TestClientCompatSharedWriteThresholdsAndScope(t *testing.T) {
	for _, rule := range []int{0, 1, 2, 3, 4} {
		for _, endpoint := range []string{"add", "rename", "update", "tag-delete", "peer-delete"} {
			t.Run(fmt.Sprintf("rule%d/%s", rule, endpoint), func(t *testing.T) {
				f := newCompatFixture(t)
				book := model.AddressBookCollection{UserId: f.owner.Id, Name: "book"}
				compatCreate(t, &book)
				compatCreate(t, &model.AddressBookCollectionRule{UserId: f.owner.Id, CollectionId: book.Id, Type: 1, ToId: f.actor.Id, Rule: rule})
				compatCreate(t, &model.Tag{UserId: f.owner.Id, CollectionId: book.Id, Name: "old", Color: 2})
				row := compatRow("peer", f.owner, book.Id)
				compatCreate(t, &row)
				method, path, body := sharedWriteRequest(endpoint, compatGUID(f.owner, book.Id))
				want := 400
				required := 2
				if strings.Contains(endpoint, "delete") {
					required = 3
				}
				if rule >= required && rule <= 3 {
					want = 200
				}
				result := f.call(method, path, body, f.actor)
				if result.Code != want {
					t.Fatalf("threshold: got %d want %d: %s", result.Code, want, result.Body.String())
				}
			})
		}
	}
	f := newCompatFixture(t)
	book := model.AddressBookCollection{UserId: f.owner.Id, Name: "book"}
	compatCreate(t, &book)
	compatCreate(t, &model.AddressBookCollectionRule{UserId: f.owner.Id, CollectionId: book.Id, Type: 2, ToId: f.actor.GroupId, Rule: 3})
	for _, raw := range []string{compatGUID(f.owner, 0), compatGUID(f.hidden, book.Id), fmt.Sprintf("%d-%d-%d", f.hidden.GroupId, f.owner.Id, book.Id), "1-2-0-extra", "-1-1-0"} {
		for _, endpoint := range []string{"add", "rename", "update", "tag-delete", "peer-delete"} {
			method, path, body := sharedWriteRequest(endpoint, raw)
			if result := f.call(method, path, body, f.actor); result.Code == 200 {
				t.Fatalf("forged/private scope accepted: %s %s", endpoint, raw)
			}
		}
	}
	// The owner needs no grant and may write both own personal and collection scopes.
	for _, cid := range []uint{0, book.Id} {
		if result := f.call("POST", "/api/ab/tag/add/"+compatGUID(f.owner, cid), `{"name":"mine","color":0}`, f.owner); result.Code != 200 {
			t.Fatal(result.Body.String())
		}
	}
}

func TestClientCompatSharedDeleteDuplicatesRetryAndIsolation(t *testing.T) {
	for _, kind := range []string{"tag", "peer"} {
		t.Run(kind, func(t *testing.T) {
			f := newCompatFixture(t)
			book := model.AddressBookCollection{UserId: f.actor.Id, Name: "protected"}
			compatCreate(t, &book)
			table := "tags"
			if kind == "tag" {
				compatCreate(t, &[]model.Tag{{UserId: f.actor.Id, Name: "same"}, {UserId: f.actor.Id, Name: "same"}, {UserId: f.actor.Id, CollectionId: book.Id, Name: "same"}, {UserId: f.hidden.Id, Name: "same"}})
			} else {
				table = "address_books"
				compatCreate(t, &[]model.AddressBook{compatRow("same", f.actor, 0), compatRow("same", f.actor, 0), compatRow("same", f.actor, book.Id), compatRow("same", f.hidden, 0)})
			}
			path := "/api/ab/" + kind + "/" + compatGUID(f.actor, 0)
			if result := f.call("DELETE", path, `["same","same"]`, f.actor); result.Code != 200 || result.Body.Len() != 0 {
				t.Fatalf("duplicate request failed: %d %s", result.Code, result.Body.String())
			}
			var count int64
			if err := global.DB.Table(table).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 2 {
				t.Fatalf("duplicate row deletion crossed scope or left rows: %d", count)
			}
			if result := f.call("DELETE", path, `["same"]`, f.actor); result.Code != 400 {
				t.Fatal("missing retry acknowledged", result.Body.String())
			}
			for _, body := range []string{"null", "[]", `[""]`, `["same",null]`, `[1]`, `["same"] {}`} {
				if result := f.call("DELETE", path, body, f.actor); result.Code != 400 {
					t.Fatalf("malformed batch accepted %s", body)
				}
			}
		})
	}
}

func TestClientCompatSharedWritesRejectInjectedFieldsAndCancellation(t *testing.T) {
	f := newCompatFixture(t)
	guid := compatGUID(f.actor, 0)
	for _, tc := range []struct{ method, action, body string }{
		{"POST", "add", `{"name":"new","color":7,"id":999}`},
		{"POST", "add", `{"name":"new","color":7,"user_id":3}`},
		{"POST", "add", `{"name":"new","color":7,"collection_id":3}`},
		{"POST", "add", `{"name":null,"color":7}`},
		{"PUT", "update", `{"name":"old","color":null}`},
		{"PUT", "rename", `{"old":"old","new":"new","rule":3}`},
	} {
		if result := f.call(tc.method, "/api/ab/tag/"+tc.action+"/"+guid, tc.body, f.actor); result.Code != 400 {
			t.Fatalf("injected/invalid body accepted %s: %d", tc.body, result.Code)
		}
	}
	for _, endpoint := range []string{"add", "rename", "update", "tag-delete", "peer-delete"} {
		method, path, body := sharedWriteRequest(endpoint, guid)
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+f.tokens[f.actor.Id])
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result := httptest.NewRecorder()
		f.handler.ServeHTTP(result, request.WithContext(ctx))
		if result.Code != 408 {
			t.Fatalf("canceled write continued %s: %d %s", endpoint, result.Code, result.Body.String())
		}
	}
}
