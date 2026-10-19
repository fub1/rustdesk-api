package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

func TestSciterAddressBookGetReadsOnlyAuthenticatedPersonalBook(t *testing.T) {
	f := newCompatFixture(t)
	collection := model.AddressBookCollection{UserId: f.actor.Id, Name: "other-book"}
	compatCreate(t, &collection)
	personal := compatRow("personal", f.actor, 0)
	personal.Note = "备注"
	personal.Tags = custom_types.AutoJson(`["local"]`)
	compatCreate(t, &[]model.AddressBook{personal, compatRow("collection-peer", f.actor, collection.Id), compatRow("foreign-peer", f.hidden, 0)})
	compatCreate(t, &[]model.Tag{{UserId: f.actor.Id, Name: "local", Color: 4294967295}, {UserId: f.actor.Id, CollectionId: collection.Id, Name: "other-tag"}, {UserId: f.hidden.Id, Name: "foreign-tag"}})
	// A caller-supplied scope cannot replace the authenticated personal scope.
	path := "/api/ab/get?ab=" + compatGUID(f.hidden, 0)
	result := f.call("POST", path, `{}`, f.actor)
	if result.Code != 200 {
		t.Fatalf("Sciter POST read: %d %s", result.Code, result.Body.String())
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	var encoded string
	if err := json.Unmarshal(envelope.Data, &encoded); err != nil {
		t.Fatalf("data must be a JSON string: %s", result.Body.String())
	}
	var document struct {
		Peers []struct {
			ID   string   `json:"id"`
			Tags []string `json:"tags"`
			Note string   `json:"note"`
		} `json:"peers"`
		Tags      []string `json:"tags"`
		TagColors string   `json:"tag_colors"`
	}
	if err := json.Unmarshal([]byte(encoded), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Peers) != 1 || document.Peers[0].ID != "personal" || document.Peers[0].Note != "备注" || len(document.Peers[0].Tags) != 1 || document.Peers[0].Tags[0] != "local" || len(document.Tags) != 1 || document.Tags[0] != "local" {
		t.Fatalf("Sciter read escaped personal scope or lost fields: %s", encoded)
	}
	var colors map[string]uint
	if err := json.Unmarshal([]byte(document.TagColors), &colors); err != nil || len(colors) != 1 || colors["local"] != 4294967295 {
		t.Fatalf("color metadata must be a JSON object string: %s", document.TagColors)
	}
	legacy := f.call("GET", "/api/ab", "", f.actor)
	if legacy.Code != 200 || legacy.Body.String() != result.Body.String() {
		t.Fatalf("POST and GET reads disagree: POST %s GET %s", result.Body.String(), legacy.Body.String())
	}

	request := httptest.NewRequest("POST", "/api/ab/get", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	anonymous := httptest.NewRecorder()
	f.handler.ServeHTTP(anonymous, request)
	if anonymous.Code != 401 || strings.Contains(anonymous.Body.String(), `"data"`) {
		t.Fatalf("anonymous Sciter read: %d %s", anonymous.Code, anonymous.Body.String())
	}
}

func TestSciterAddressBookGetEmptyDocumentWireTypes(t *testing.T) {
	f := newCompatFixture(t)
	result := f.call("POST", "/api/ab/get", `{}`, f.actor)
	var envelope struct {
		Data string `json:"data"`
	}
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &envelope) != nil {
		t.Fatalf("empty Sciter read: %d %s", result.Code, result.Body.String())
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(envelope.Data), &document); err != nil {
		t.Fatal(err)
	}
	if string(document["peers"]) != "[]" || string(document["tags"]) != "[]" || string(document["tag_colors"]) != `"{}"` {
		t.Fatalf("empty wire document types changed: %s", envelope.Data)
	}
}

func TestSciterAddressBookGetStorageFailureAndCancellationFailClosed(t *testing.T) {
	for _, table := range []string{"address_books", "tags"} {
		t.Run(table, func(t *testing.T) {
			f := newCompatFixture(t)
			if err := global.DB.Callback().Query().Before("gorm:query").Register("sciter-get-query-failure", func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(errors.New("private Sciter read failure"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			result := f.call("POST", "/api/ab/get", `{}`, f.actor)
			if result.Code != 503 || strings.Contains(result.Body.String(), `"data"`) || strings.Contains(result.Body.String(), "private Sciter") {
				t.Fatalf("Sciter storage failure acknowledged or leaked: %d %s", result.Code, result.Body.String())
			}
		})
	}
	f := newCompatFixture(t)
	request := httptest.NewRequest("POST", "/api/ab/get", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+f.tokens[f.actor.Id])
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := httptest.NewRecorder()
	f.handler.ServeHTTP(result, request.WithContext(ctx))
	if result.Code != 408 || strings.Contains(result.Body.String(), `"data"`) {
		t.Fatalf("canceled Sciter read continued: %d %s", result.Code, result.Body.String())
	}
}
