package http

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/utils"
)

func profileObject(t *testing.T, data []byte, keys ...string) map[string]json.RawMessage {
	t.Helper()
	var got map[string]json.RawMessage
	if json.Unmarshal(data, &got) != nil || len(got) != len(keys) {
		t.Fatalf("unexpected profile object: %s", data)
	}
	for _, key := range keys {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing %s in %s", key, data)
		}
	}
	return got
}

func assertNativeProfile(t *testing.T, data []byte, user model.User) {
	t.Helper()
	got := profileObject(t, data, "name", "display_name", "avatar", "email", "note", "is_admin", "status", "info")
	for key, want := range map[string]string{"name": user.Username, "display_name": user.Nickname, "avatar": user.Avatar, "email": user.Email, "note": ""} {
		var value string
		if json.Unmarshal(got[key], &value) != nil || string(got[key]) == "null" || value != want {
			t.Fatalf("%s=%s want string %q", key, got[key], want)
		}
	}
	var admin bool
	if json.Unmarshal(got["is_admin"], &admin) != nil || admin != (user.IsAdmin != nil && *user.IsAdmin) {
		t.Fatalf("administrator projection: %s", data)
	}
	status := "0"
	if user.Status == model.COMMON_STATUS_ENABLE {
		status = "1"
	}
	if string(got["status"]) != status || string(got["info"]) != "{}" {
		t.Fatalf("status/info projection: %s", data)
	}
}

func profileResponse(t *testing.T, w *httptest.ResponseRecorder) []byte {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("profile HTTP %d: %s", w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

func assertAdminProfile(t *testing.T, data []byte, user model.User) {
	t.Helper()
	envelope := profileObject(t, data, "code", "message", "data")
	if string(envelope["code"]) != "0" {
		t.Fatalf("admin profile envelope: %s", data)
	}
	got := profileObject(t, envelope["data"], "username", "email", "nickname", "avatar", "token", "route_names")
	for key, want := range map[string]string{"username": user.Username, "email": user.Email, "nickname": user.Nickname, "avatar": user.Avatar} {
		var value string
		if json.Unmarshal(got[key], &value) != nil || string(got[key]) == "null" || value != want {
			t.Fatalf("admin %s=%s want %q", key, got[key], want)
		}
	}
	var token string
	var routes []string
	if json.Unmarshal(got["token"], &token) != nil || token == "" || json.Unmarshal(got["route_names"], &routes) != nil || len(routes) == 0 {
		t.Fatalf("admin session projection: %s", data)
	}
}

func updateProfile(t *testing.T, user *model.User, displayName, avatar string) {
	t.Helper()
	user.Nickname, user.Avatar, user.Email, user.Remark = displayName, avatar, "profile@example.test", "private-internal-remark"
	if err := global.DB.Model(&model.User{}).Where("id = ?", user.Id).Updates(map[string]any{
		"nickname": user.Nickname, "avatar": user.Avatar, "email": user.Email, "remark": user.Remark,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestClientProfileCurrentAndLocalLogin(t *testing.T) {
	for _, profile := range []struct{ name, display, avatar string }{
		{"populated", "Local Display 名称", "/upload/profile.png"}, {"empty", "", ""},
	} {
		t.Run(profile.name, func(t *testing.T) {
			f := newCompatFixture(t)
			if err := global.DB.AutoMigrate(&model.LoginLog{}); err != nil {
				t.Fatal(err)
			}
			updateProfile(t, &f.actor, profile.display, profile.avatar)
			hash, err := utils.EncryptPassword("test-profile-password")
			if err != nil {
				t.Fatal(err)
			}
			if err := global.DB.Model(&model.User{}).Where("id = ?", f.actor.Id).Update("password", hash).Error; err != nil {
				t.Fatal(err)
			}
			for _, endpoint := range []struct{ method, path string }{{"POST", "/api/currentUser"}, {"GET", "/api/user/info"}} {
				assertNativeProfile(t, profileResponse(t, f.call(endpoint.method, endpoint.path, "{}", f.actor)), f.actor)
			}
			login := profileObject(t, profileResponse(t, f.call("POST", "/api/login", `{"username":"actor","password":"test-profile-password","type":"account"}`, f.actor)), "access_token", "type", "user")
			assertNativeProfile(t, login["user"], f.actor)
			var token string
			if json.Unmarshal(login["access_token"], &token) != nil || token == "" || string(login["type"]) != `"access_token"` {
				t.Fatal("local login lost token envelope")
			}
			r := httptest.NewRequest("GET", "/api/admin/user/current", nil)
			r.Header.Set("api-token", token)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			assertAdminProfile(t, profileResponse(t, w), f.actor)
		})
	}
}

func TestClientProfileUsersPreservesVisibilityAndPagination(t *testing.T) {
	f := newCompatFixture(t)
	updateProfile(t, &f.actor, "Visible Display", "https://example.test/avatar.png")
	updateProfile(t, &f.owner, "", "")
	updateProfile(t, &f.hidden, "Never Visible", "https://example.test/hidden.png")
	no := false
	disabled := model.User{Username: "disabled-profile", GroupId: 1, IsAdmin: &no, Status: model.COMMON_STATUS_DISABLED, Nickname: "Disabled Display", Avatar: "/disabled.png", Remark: "private-disabled-remark"}
	compatCreate(t, &disabled)
	want := []model.User{f.actor, f.owner, f.admin}
	for page, user := range want {
		w := f.call("GET", fmt.Sprintf("/api/users?accessible=&status=1&current=%d&pageSize=1", page+1), "", f.actor)
		envelope := profileObject(t, profileResponse(t, w), "total", "data")
		var rows []json.RawMessage
		if string(envelope["total"]) != "3" || json.Unmarshal(envelope["data"], &rows) != nil || len(rows) != 1 {
			t.Fatalf("authorized profile page: %s", w.Body.String())
		}
		assertNativeProfile(t, rows[0], user)
	}
	w := f.call("GET", "/api/users?current=1&pageSize=100", "", f.actor)
	envelope := profileObject(t, profileResponse(t, w), "total", "data")
	var rows []json.RawMessage
	if string(envelope["total"]) != "3" || json.Unmarshal(envelope["data"], &rows) != nil || len(rows) != 3 {
		t.Fatalf("ordinary profile list exposed a disabled member: %s", w.Body.String())
	}
	w = f.call("GET", "/api/users?current=1&pageSize=100", "", f.admin)
	envelope = profileObject(t, profileResponse(t, w), "total", "data")
	if string(envelope["total"]) != "5" || json.Unmarshal(envelope["data"], &rows) != nil || len(rows) != 5 {
		t.Fatalf("administrator full-site profile list: %s", w.Body.String())
	}
	assertNativeProfile(t, rows[4], disabled)
	if err := global.DB.Model(&model.User{}).Where("id = ?", f.actor.Id).Update("group_id", 2).Error; err != nil {
		t.Fatal(err)
	}
	w = f.call("GET", "/api/users?accessible=&status=1&current=1&pageSize=100", "", f.actor)
	envelope = profileObject(t, profileResponse(t, w), "total", "data")
	if string(envelope["total"]) != "1" || json.Unmarshal(envelope["data"], &rows) != nil || len(rows) != 1 {
		t.Fatalf("private profile list expanded scope: %s", w.Body.String())
	}
	assertNativeProfile(t, rows[0], f.actor)
}

func TestClientProfileOIDCPollUsesLocalProjection(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin=%t", admin), func(t *testing.T) {
			f := newRouteOIDCFixture(t)
			a := f.begin(t, admin, "ours")
			state := f.idp.register(t, a.URL, "profile-code")
			profileResponse(t, f.call("GET", "/api/oidc/callback?"+url.Values{"state": {state}, "code": {"profile-code"}}.Encode(), "", "", false))
			// Admission occurs when polling, after protocol verification completes.
			profileResponse(t, f.query(a.Code, admin))
			var identity model.OIDCIdentity
			if err := global.DB.First(&identity).Error; err != nil {
				t.Fatal(err)
			}
			var user model.User
			if err := global.DB.First(&user, identity.UserID).Error; err != nil {
				t.Fatal(err)
			}
			// Response attributes come from the local account, even when the IdP
			// claims use a different name or request administrator privileges.
			updateProfile(t, &user, "Locally Managed Display", "/local-avatar.png")
			poll := profileResponse(t, f.query(a.Code, admin))
			var token string
			if admin {
				assertAdminProfile(t, poll, user)
				var envelope struct{ Data struct{ Token string } }
				if json.Unmarshal(poll, &envelope) != nil {
					t.Fatal("invalid admin envelope")
				}
				token = envelope.Data.Token
			} else {
				login := profileObject(t, poll, "access_token", "type", "user")
				assertNativeProfile(t, login["user"], user)
				if json.Unmarshal(login["access_token"], &token) != nil || token == "" {
					t.Fatal("OIDC poll lost local token")
				}
			}
			for _, endpoint := range []struct{ method, path string }{{"POST", "/api/currentUser"}, {"GET", "/api/user/info"}} {
				assertNativeProfile(t, profileResponse(t, f.call(endpoint.method, endpoint.path, "{}", token, false)), user)
			}
			// Explicit empty strings must clear the client's cached profile.
			updateProfile(t, &user, "", "")
			assertNativeProfile(t, profileResponse(t, f.call("POST", "/api/currentUser", "{}", token, false)), user)
			if user.IsAdmin == nil || *user.IsAdmin {
				t.Fatal("IdP role claim elevated local account")
			}
		})
	}
}
