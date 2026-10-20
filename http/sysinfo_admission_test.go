package http

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/model"
)

func TestSysInfoRejectsManagementPresetsBeforeAnyMutation(t *testing.T) {
	f := newSysInfoFixture(t)
	peer := model.Peer{Id: "device", UserId: f.owner.Id, Username: "before", Hostname: "before-host", Alias: "managed", AbId: 7}
	compatCreate(t, &peer)
	book := model.AddressBookCollection{UserId: 1, Name: "target"}
	compatCreate(t, &book)
	entry := compatRow("device", f.actor, 0)
	compatCreate(t, &entry)
	tag := model.Tag{UserId: 1, Name: "sentinel"}
	compatCreate(t, &tag)
	// Snapshot stored defaults/timestamps through the same read projection used
	// below; in-memory create values can have different time locations.
	for _, row := range []any{&peer, &entry, &book, &tag} {
		if err := global.DB.First(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	keys := []string{"preset-address-book-name", "preset-address-book-tag", "preset-address-book-alias", "preset-address-book-password", "preset-address-book-note", "preset-user-name", "preset-strategy-name", "preset-device-group-name", "preset-note", "preset-device-username", "preset-device-name", "preset-future-option", "PRESET-FUTURE-OPTION"}
	for _, key := range keys {
		for _, id := range []string{"device", "unregistered"} {
			body, err := json.Marshal(map[string]any{"id": id, "username": "after", "hostname": "after-host", key: "private-value"})
			if err != nil {
				t.Fatal(err)
			}
			result := sysInfoCall(f, string(body), nil)
			if result.Code != 403 || strings.Contains(result.Body.String(), "SYSINFO_UPDATED") || strings.Contains(result.Body.String(), "private-value") || !strings.Contains(result.Body.String(), "administrator") {
				t.Errorf("management preset %s falsely acknowledged or leaked: %d %s", key, result.Code, result.Body.String())
			}
		}
	}
	result := sysInfoCall(f, `{"id":"device","username":"after","preset-address-book-name":"target","preset-address-book-alias":"private-value"}`, nil)
	if result.Code != 403 {
		t.Fatalf("matching legacy owner1 target was accepted: %d %s", result.Code, result.Body.String())
	}
	result = f.call("POST", "/api/sysinfo", `{"id":"device","preset-address-book-alias":"private-value"}`, f.admin)
	if result.Code != 403 {
		t.Fatalf("Bearer header incorrectly upgraded basic-only sysinfo into assignment: %d %s", result.Code, result.Body.String())
	}
	var peers []model.Peer
	var entries []model.AddressBook
	var books []model.AddressBookCollection
	var tags []model.Tag
	for _, dest := range []any{&peers, &entries, &books, &tags} {
		if err := global.DB.Find(dest).Error; err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(peers, []model.Peer{peer}) || !reflect.DeepEqual(entries, []model.AddressBook{entry}) || !reflect.DeepEqual(books, []model.AddressBookCollection{book}) || !reflect.DeepEqual(tags, []model.Tag{tag}) {
		t.Fatal("rejected management request changed peer/entry/book/tag sentinels")
	}
}

func TestSysInfoBasicReportsDoNotDependOnLoginOrManagementTables(t *testing.T) {
	f := newSysInfoFixture(t)
	for _, table := range []any{&model.LoginLog{}, &model.AddressBook{}, &model.AddressBookCollection{}, &model.Tag{}} {
		if err := global.DB.Migrator().DropTable(table); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{`{"id":"device","uuid":"uuid","username":"user"}`, `{"id":"device","hostname":"host"}`} {
		result := sysInfoCall(f, body, nil)
		if result.Code != 200 || result.Body.String() != "SYSINFO_UPDATED" {
			t.Fatalf("basic report still depended on management tables: %d %s", result.Code, result.Body.String())
		}
	}
}

func TestSysInfoPresetNamespaceTypesEmptyValuesAndDuplicateKeys(t *testing.T) {
	f := newSysInfoFixture(t)
	for _, key := range []string{"preset-address-book-name", "preset-user-name", "preset-future-option"} {
		for _, value := range []string{"null", "1", "true", "[]", `{}`} {
			result := sysInfoCall(f, `{"id":"device","`+key+`":`+value+`}`, nil)
			if result.Code != 400 {
				t.Errorf("invalid preset namespace type accepted: %s=%s => %d %s", key, value, result.Code, result.Body.String())
			}
		}
	}
	for _, body := range []string{`{"id":"device","preset-future-option":"private-value","preset-future-option":""}`, `{"id":"device","preset-address-book-alias":"private-value","preset-address-book-alias":""}`} {
		result := sysInfoCall(f, body, nil)
		if result.Code != 403 || strings.Contains(result.Body.String(), "private-value") {
			t.Errorf("duplicate key hid management intent: %d %s", result.Code, result.Body.String())
		}
	}
	result := sysInfoCall(f, `{"id":"device","hostname":"host","username":"user","preset-user-name":"","preset-address-book-name":"","preset-future-option":""}`, nil)
	if result.Code != 200 || result.Body.String() != "SYSINFO_UPDATED" {
		t.Fatalf("empty preset no-op rejected basic report: %d %s", result.Code, result.Body.String())
	}
	var peer model.Peer
	if err := global.DB.Where("id = ?", "device").First(&peer).Error; err != nil {
		t.Fatal(err)
	}
	if peer.UserId != 0 || peer.Username != "user" || peer.Hostname != "host" {
		t.Fatalf("empty preset no-op changed basic semantics: %+v", peer)
	}
}

func TestSysInfoNewAnonymousPeerNeverInfersOwnerFromLoginHistory(t *testing.T) {
	f := newSysInfoFixture(t)
	compatCreate(t, &model.LoginLog{UserId: f.owner.Id, DeviceId: "device", Uuid: "uuid"})
	result := sysInfoCall(f, `{"id":"device","uuid":"uuid","user_id":2,"hostname":"host"}`, nil)
	if result.Code != 200 || result.Body.String() != "SYSINFO_UPDATED" {
		t.Fatal(result.Body.String())
	}
	var peer model.Peer
	if err := global.DB.Where("id = ?", "device").First(&peer).Error; err != nil {
		t.Fatal(err)
	}
	if peer.UserId != 0 {
		t.Fatalf("anonymous id/uuid inherited login-log ownership: %d", peer.UserId)
	}
}
