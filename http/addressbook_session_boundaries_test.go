package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
)

// Read raw columns rather than model serializers, which can normalize stored
// tags or omit credentials. Row IDs and timestamps participate in comparison.
func addressBookSessionRows(t *testing.T, table, order, where string, args ...any) []byte {
	t.Helper()
	pool, err := global.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	query := "SELECT * FROM " + table
	if where != "" {
		query += " WHERE " + where
	}
	rows, err := pool.Query(query+" ORDER BY "+order, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	values := [][]any{}
	for rows.Next() {
		row := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range row {
			pointers[i] = &row[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		for i, value := range row {
			if raw, ok := value.([]byte); ok {
				row[i] = append([]byte(nil), raw...)
			}
		}
		values = append(values, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(struct {
		Columns []string
		Rows    [][]any
	}{columns, values})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func addressBookSessionSnapshot(t *testing.T) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	for _, table := range []string{"users", "user_tokens", "tags", "address_book_collections", "address_book_collection_rules", "peers", "address_books"} {
		order := "id"
		if table == "peers" || table == "address_books" {
			order = "row_id"
		}
		result[table] = addressBookSessionRows(t, table, order, "")
	}
	return result
}

func addressBookSessionRequest(f *compatFixture, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func TestClientAddressBookSessionBoundaries(t *testing.T) {
	endpoints := []string{"settings", "personal", "add", "rename", "update", "tag-delete", "peer-delete"}
	states := []string{"anonymous", "bad-token", "logged-out", "disabled-user", "revoked-session", "expired-session"}
	for _, endpoint := range endpoints {
		for _, state := range states {
			t.Run(endpoint+"/"+state, func(t *testing.T) {
				f := newCompatFixture(t)
				global.Config.Rustdesk.Personal = 1
				book := model.AddressBookCollection{UserId: f.owner.Id, Name: "session-boundary-target"}
				otherBook := model.AddressBookCollection{UserId: f.owner.Id, Name: "session-boundary-sentinel"}
				compatCreate(t, &book)
				compatCreate(t, &otherBook)
				compatCreate(t, &model.AddressBookCollectionRule{UserId: f.owner.Id, CollectionId: book.Id, Type: model.ShareAddressBookRuleTypePersonal, ToId: f.actor.Id, Rule: 3})
				// The same names exist outside the target owner and collection.
				for _, scope := range []struct {
					owner model.User
					cid   uint
				}{{f.owner, book.Id}, {f.owner, otherBook.Id}, {f.actor, 0}, {f.hidden, 0}} {
					compatCreate(t, &model.Tag{UserId: scope.owner.Id, CollectionId: scope.cid, Name: "old", Color: 2})
					peer := compatRow("peer", scope.owner, scope.cid)
					peer.Tags = custom_types.AutoJson(`["old"]`)
					peer.Alias, peer.Note, peer.Hash = "preserved-alias", "preserved-note", "preserved-hash"
					compatCreate(t, &peer)
				}
				method, path, body := sharedWriteRequest(endpoint, compatGUID(f.owner, book.Id))
				if endpoint == "settings" || endpoint == "personal" {
					method, path, body = "POST", "/api/ab/"+endpoint, "{}"
				}
				token := f.tokens[f.actor.Id]
				switch state {
				case "anonymous":
					token = ""
				case "bad-token":
					token = "not-a-session-token"
				case "logged-out":
					logout := addressBookSessionRequest(f, "POST", "/api/logout", "{}", token)
					if logout.Code != 200 || logout.Body.String() != "null" {
						t.Fatalf("local logout preparation failed: %d %s", logout.Code, logout.Body.String())
					}
				case "disabled-user":
					if err := global.DB.Model(&model.User{}).Where("id = ?", f.actor.Id).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
						t.Fatal(err)
					}
				case "revoked-session":
					removed := global.DB.Where("user_id = ? AND token = ?", f.actor.Id, token).Delete(&model.UserToken{})
					if removed.Error != nil || removed.RowsAffected != 1 {
						t.Fatal("session revocation preparation failed", removed.Error)
					}
				case "expired-session":
					updated := global.DB.Model(&model.UserToken{}).Where("user_id = ? AND token = ?", f.actor.Id, token).Update("expired_at", time.Now().UTC().Add(-time.Minute).Unix())
					if updated.Error != nil || updated.RowsAffected != 1 {
						t.Fatal("session expiration preparation failed", updated.Error)
					}
				}
				before := addressBookSessionSnapshot(t)
				denied := addressBookSessionRequest(f, method, path, body, token)
				if denied.Code != 401 || denied.Body.String() != `{"error":"Unauthorized"}` {
					t.Fatalf("invalid session crossed production route: %d %s", denied.Code, denied.Body.String())
				}
				for table, after := range addressBookSessionSnapshot(t) {
					if !bytes.Equal(before[table], after) {
						t.Fatalf("invalid session changed table %s", table)
					}
				}
				outside := map[string][]byte{}
				for _, table := range []string{"tags", "address_books"} {
					order := "id"
					if table == "address_books" {
						order = "row_id"
					}
					outside[table] = addressBookSessionRows(t, table, order, "NOT (user_id = ? AND collection_id = ?)", f.owner.Id, book.Id)
				}
				// This valid owner token uses the exact same body and named scope.
				// It remains valid even when the shared actor was disabled/revoked.
				accepted := addressBookSessionRequest(f, method, path, body, f.tokens[f.owner.Id])
				if accepted.Code != 200 {
					t.Fatalf("valid scoped control rejected: %d %s", accepted.Code, accepted.Body.String())
				}
				switch endpoint {
				case "settings":
					if accepted.Body.String() != `{"max_peer_one_ab":0}` {
						t.Fatal("settings control did not reach expected response")
					}
				case "personal":
					var got struct {
						GUID string `json:"guid"`
						Name string `json:"name"`
						Rule int    `json:"rule"`
					}
					if json.Unmarshal(accepted.Body.Bytes(), &got) != nil || got.GUID != compatGUID(f.owner, 0) || got.Name != f.owner.Username || got.Rule != 3 {
						t.Fatalf("personal control scope: %s", accepted.Body.String())
					}
				default:
					if accepted.Body.Len() != 0 {
						t.Fatalf("write control ACK: %s", accepted.Body.String())
					}
				}
				assertSessionControlEffect(t, endpoint, f.owner.Id, book.Id)
				for table, before := range outside {
					order := "id"
					if table == "address_books" {
						order = "row_id"
					}
					if !bytes.Equal(before, addressBookSessionRows(t, table, order, "NOT (user_id = ? AND collection_id = ?)", f.owner.Id, book.Id)) {
						t.Fatalf("valid control changed another scope in %s", table)
					}
				}
			})
		}
	}
}

func assertSessionControlEffect(t *testing.T, endpoint string, owner, cid uint) {
	t.Helper()
	var tags []model.Tag
	if err := global.DB.Where("user_id = ? AND collection_id = ?", owner, cid).Order("id").Find(&tags).Error; err != nil {
		t.Fatal(err)
	}
	actual := map[string]uint{}
	for _, tag := range tags {
		actual[tag.Name] = tag.Color
	}
	want := map[string]uint{"old": 2}
	switch endpoint {
	case "add":
		want["new"] = 7
	case "rename":
		want = map[string]uint{"new": 2}
	case "update":
		want["old"] = 7
	case "tag-delete":
		want = map[string]uint{}
	}
	if len(tags) != len(want) || !reflect.DeepEqual(actual, want) {
		t.Fatalf("valid %s control did not produce scoped tag effect", endpoint)
	}
	var peers []model.AddressBook
	if err := global.DB.Where("user_id = ? AND collection_id = ?", owner, cid).Find(&peers).Error; err != nil {
		t.Fatal(err)
	}
	if endpoint == "peer-delete" {
		if len(peers) != 0 {
			t.Fatal("valid peer deletion had no effect")
		}
		return
	}
	wantTags := `["old"]`
	if endpoint == "rename" {
		wantTags = `["new"]`
	} else if endpoint == "tag-delete" {
		wantTags = `[]`
	}
	if len(peers) != 1 || peers[0].Id != "peer" || string(peers[0].Tags) != wantTags || peers[0].Alias != "preserved-alias" || peers[0].Note != "preserved-note" || peers[0].Hash != "preserved-hash" {
		t.Fatal(fmt.Sprintf("valid %s control changed unexpected peer data", endpoint))
	}
}
