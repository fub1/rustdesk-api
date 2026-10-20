package http

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/model"
)

func TestClientDeviceCLIAddressBookReadbackAndPermissions(t *testing.T) {
	f := newCLIFixture(t)
	collection := model.AddressBookCollection{UserId: f.owner.Id, Name: "Operations"}
	compatCreate(t, &collection)
	compatCreate(t, &model.AddressBookCollectionRule{UserId: f.owner.Id, CollectionId: collection.Id, Type: model.ShareAddressBookRuleTypePersonal, ToId: f.actor.Id, Rule: model.ShareAddressBookRuleRuleRead})
	beforePeer := cliPeer(t)
	body := fmt.Sprintf(`{"id":"cli-device","uuid":"cli-uuid","user_name":%q,"device_name":"final-host","address_book_name":"Operations","address_book_alias":"Display","address_book_note":"book-note","address_book_password":"raw,+/密码","address_book_tag":"One, whole tag"}`, f.owner.Username)
	for n := 0; n < 2; n++ {
		w := f.call("POST", "/api/devices/cli", body, f.admin)
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatalf("join/retry: %d %s", w.Code, w.Body.String())
		}
	}
	peer := cliPeer(t)
	if peer.Alias != beforePeer.Alias || peer.AbId != beforePeer.AbId || peer.Note != beforePeer.Note {
		t.Fatal("book assignment changed peer preset metadata or device note")
	}
	for _, user := range []model.User{f.owner, f.actor} {
		w := f.call("POST", "/api/ab/peers?ab="+compatGUID(f.owner, collection.Id), "", user)
		var response struct {
			Total int
			Data  []struct {
				ID, Alias, Note, Password, Hash, Hostname string
				Tags                                      []string
			}
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Total != 1 || len(response.Data) != 1 {
			t.Fatalf("authorized readback: %d %s", w.Code, w.Body.String())
		}
		row := response.Data[0]
		if row.ID != "cli-device" || row.Alias != "Display" || row.Note != "book-note" || row.Password != "raw,+/密码" || row.Hash != "" || row.Hostname != "final-host" || !reflect.DeepEqual(row.Tags, []string{"One, whole tag"}) {
			t.Fatal("incorrect shared readback")
		}
	}
	w := f.call("POST", "/api/ab/peers?ab="+compatGUID(f.owner, collection.Id), "", f.hidden)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "NoAccess") || strings.Contains(w.Body.String(), "raw,+") {
		t.Fatalf("password escaped book permissions: %d %s", w.Code, w.Body.String())
	}
	// Existing owner can be used without resending user_name. Empty tag preserves;
	// explicit empty text clears only those three book columns.
	w = f.call("POST", "/api/devices/cli", `{"id":"cli-device","uuid":"cli-uuid","address_book_name":"Operations","address_book_alias":"","address_book_note":"","address_book_password":"","address_book_tag":""}`, f.admin)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	var rows []model.AddressBook
	if err := global.DB.Where("user_id = ? AND collection_id = ?", f.owner.Id, collection.Id).Find(&rows).Error; err != nil || len(rows) != 1 {
		t.Fatal("join was not idempotent", err)
	}
	if rows[0].Alias != "" || rows[0].Note != "" || rows[0].Password != "" || string(rows[0].Tags) != `["One, whole tag"]` {
		t.Fatal("clear enlarged scope")
	}
	var personal model.AddressBook
	if err := global.DB.Where("user_id = ? AND collection_id = 0", f.owner.Id).First(&personal).Error; err != nil || personal.Note != "address-book-only" {
		t.Fatal("join changed personal entry", err)
	}
}

func TestClientDeviceCLIAddressBookRejectsShapeAndUnauthorizedWrites(t *testing.T) {
	f := newCLIFixture(t)
	for _, field := range []string{"address_book_name", "address_book_alias", "address_book_note", "address_book_password", "address_book_tag"} {
		for _, value := range []any{nil, 1, true, []string{"x"}} {
			request, _ := json.Marshal(map[string]any{"id": "cli-device", "uuid": "cli-uuid", "device_name": "must-not-change", "address_book_name": "book", field: value})
			before := cliSnapshot(t)
			w := f.call("POST", "/api/devices/cli", string(request), f.admin)
			if w.Code != 400 || w.Body.Len() == 0 {
				t.Fatalf("invalid book shape: %d %s", w.Code, w.Body.String())
			}
			assertCLIUnchanged(t, before)
		}
	}
	for _, body := range []string{
		`{"id":"cli-device","uuid":"cli-uuid","address_book_name":""}`,
		`{"id":"cli-device","uuid":"cli-uuid","address_book_alias":"x"}`,
		`{"id":"cli-device","uuid":"cli-uuid","address_book_tag":""}`,
		`{"id":"cli-device","uuid":"cli-uuid","address_book_name":"x","address_book_name":"y"}`,
	} {
		before := cliSnapshot(t)
		w := f.call("POST", "/api/devices/cli", body, f.admin)
		if w.Code != 400 || w.Body.Len() == 0 {
			t.Fatalf("invalid book request: %d %s", w.Code, w.Body.String())
		}
		assertCLIUnchanged(t, before)
	}
	collection := model.AddressBookCollection{UserId: f.owner.Id, Name: "book"}
	compatCreate(t, &collection)
	body := fmt.Sprintf(`{"id":"cli-device","uuid":"cli-uuid","user_name":%q,"address_book_name":"book","address_book_password":"secret","address_book_tag":"new"}`, f.owner.Username)
	before := cliSnapshot(t)
	for _, tc := range []struct {
		token  string
		status int
	}{{"", 401}, {f.tokens[f.actor.Id], 403}} {
		w := addressBookSessionRequest(f, "POST", "/api/devices/cli", body, tc.token)
		if w.Code != tc.status || w.Body.Len() == 0 {
			t.Fatalf("book admission: %d %s", w.Code, w.Body.String())
		}
		assertCLIUnchanged(t, before)
	}
}
