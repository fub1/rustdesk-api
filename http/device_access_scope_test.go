package http

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/model"
)

// Exercise the registered HTTP routes and authorize before counting/paging.
func deviceScopePages(t *testing.T, f *compatFixture, path, field, query string, actor model.User, want []string) {
	t.Helper()
	got := []string{}
	for page := 1; page <= len(want)+1; page++ {
		w := f.call("GET", fmt.Sprintf("%s?current=%d&pageSize=1&%s", path, page, query), "", actor)
		var result struct {
			Total int
			Data  []map[string]json.RawMessage
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Total != len(want) || result.Data == nil {
			t.Fatalf("%s page%d: code=%d body=%s", path, page, w.Code, w.Body.String())
		}
		if page == len(want)+1 {
			if len(result.Data) != 0 {
				t.Fatal("page beyond authorized total returned rows")
			}
			continue
		}
		if len(result.Data) != 1 {
			t.Fatal("one-row authorized page was not complete")
		}
		var value string
		if err := json.Unmarshal(result.Data[0][field], &value); err != nil {
			t.Fatal(err)
		}
		got = append(got, value)
		for _, forbidden := range []string{"password", "hash", "token", "uuid", "ip", "remark", "strategy", "strategy_definition_id"} {
			if _, exists := result.Data[0][forbidden]; exists {
				t.Fatalf("directory exposed an internal field: %s", forbidden)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s values=%v want=%v", path, got, want)
	}
}

func TestDeviceAccessHTTPOrdinaryAndAdministratorScopes(t *testing.T) {
	f := newCompatFixture(t)
	no := false
	disabled := model.User{Username: "scope-disabled", GroupId: 1, Status: model.COMMON_STATUS_DISABLED, IsAdmin: &no}
	compatCreate(t, &disabled)
	groups := []model.DeviceGroup{{Name: "actor-dg"}, {Name: "owner-dg"}, {Name: "foreign-dg"}, {Name: "disabled-dg"}, {Name: "unused-dg"}}
	compatCreate(t, &groups)
	peers := []model.Peer{
		{Id: "scope-self", UserId: f.actor.Id, GroupId: groups[0].Id, LastOnlineTime: time.Now().UTC().Unix()},
		{Id: "scope-owner-online", UserId: f.owner.Id, GroupId: groups[1].Id, LastOnlineTime: time.Now().UTC().Unix()},
		{Id: "scope-owner-offline", UserId: f.owner.Id, GroupId: groups[1].Id},
		{Id: "scope-self-no-group", UserId: f.actor.Id, GroupId: 999999},
		{Id: "scope-foreign", UserId: f.hidden.Id, GroupId: groups[2].Id},
		{Id: "scope-disabled", UserId: disabled.Id, GroupId: groups[3].Id},
		{Id: "scope-owner-zero", UserId: 0, GroupId: groups[4].Id},
		{Id: "scope-owner-missing", UserId: 999999, GroupId: groups[4].Id},
	}
	compatCreate(t, &peers)
	book := model.AddressBookCollection{UserId: f.hidden.Id, Name: "full-control-foreign-book"}
	compatCreate(t, &book)
	rule := model.AddressBookCollectionRule{UserId: f.hidden.Id, CollectionId: book.Id, ToId: f.actor.Id, Type: model.ShareAddressBookRuleTypePersonal, Rule: 3}
	compatCreate(t, &rule)
	entry := compatRow("scope-foreign", f.hidden, book.Id)
	compatCreate(t, &entry)

	for _, query := range []string{"", "status=0", "status=1&accessible="} {
		deviceScopePages(t, f, "/api/users", "name", query, f.actor, []string{"actor", "owner", "administrator"})
		deviceScopePages(t, f, "/api/peers", "id", query, f.actor, []string{"scope-self", "scope-owner-online", "scope-owner-offline", "scope-self-no-group"})
	}
	deviceScopePages(t, f, "/api/device-group/accessible", "name", "", f.actor, []string{"actor-dg", "owner-dg"})
	for _, query := range []string{"", "status=0"} {
		deviceScopePages(t, f, "/api/users", "name", query, f.admin, []string{"actor", "owner", "hidden", "administrator", "scope-disabled"})
		deviceScopePages(t, f, "/api/peers", "id", query, f.admin, []string{"scope-self", "scope-owner-online", "scope-owner-offline", "scope-self-no-group", "scope-foreign", "scope-disabled"})
	}
	deviceScopePages(t, f, "/api/users", "name", "status=1", f.admin, []string{"actor", "owner", "hidden", "administrator"})
	deviceScopePages(t, f, "/api/peers", "id", "status=1", f.admin, []string{"scope-self", "scope-owner-online", "scope-owner-offline", "scope-self-no-group", "scope-foreign"})
	deviceScopePages(t, f, "/api/device-group/accessible", "name", "", f.admin, []string{"actor-dg", "owner-dg", "foreign-dg", "disabled-dg", "unused-dg"})

	// The role is read from the current database, not trusted from the old token.
	if err := global.DB.Model(&model.User{}).Where("id=?", f.admin.Id).UpdateColumn("is_admin", false).Error; err != nil {
		t.Fatal(err)
	}
	deviceScopePages(t, f, "/api/users", "name", "", f.admin, []string{"actor", "owner", "administrator"})
	deviceScopePages(t, f, "/api/device-group/accessible", "name", "", f.admin, []string{"actor-dg", "owner-dg"})
	if err := global.DB.Model(&model.User{}).Where("id=?", f.owner.Id).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
		t.Fatal(err)
	}
	deviceScopePages(t, f, "/api/peers", "id", "status=0", f.actor, []string{"scope-self", "scope-self-no-group"})
	deviceScopePages(t, f, "/api/device-group/accessible", "name", "", f.actor, []string{"actor-dg"})
	for _, groupID := range []uint{2, 0, 999999} {
		if err := global.DB.Model(&model.User{}).Where("id=?", f.actor.Id).UpdateColumn("group_id", groupID).Error; err != nil {
			t.Fatal(err)
		}
		deviceScopePages(t, f, "/api/users", "name", "", f.actor, []string{"actor"})
		deviceScopePages(t, f, "/api/peers", "id", "", f.actor, []string{"scope-self", "scope-self-no-group"})
		deviceScopePages(t, f, "/api/device-group/accessible", "name", "", f.actor, []string{"actor-dg"})
	}
}

func TestDeviceAccessHTTPAuthenticationAndQueryFailure(t *testing.T) {
	f := newCompatFixture(t)
	paths := []string{"/api/users", "/api/peers", "/api/device-group/accessible"}
	for _, path := range paths {
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatalf("anonymous %s returned %d", path, w.Code)
		}
	}
	if err := global.DB.Exec("DROP TABLE groups").Error; err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		w := f.call("GET", path, "", f.actor)
		if w.Code != 503 || strings.Contains(w.Body.String(), `"data"`) || strings.Contains(w.Body.String(), "no such table") {
			t.Fatalf("directory dependency failure: %s %d %s", path, w.Code, w.Body.String())
		}
	}
}
