package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientassign"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	"github.com/lejianwen/rustdesk-api/v2/model"
)

func newCLIFixture(t *testing.T) *compatFixture {
	t.Helper()
	f := newCompatFixture(t)
	store, err := clientassign.New(global.DB, clientassign.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := app.NewSessions(global.DB, time.Hour, global.Jwt.Key)
	if err != nil {
		t.Fatal(err)
	}
	h, err := httpapi.New(sessions, httpapi.Options{Translate: LegacyTranslator(), Assign: store})
	if err != nil {
		t.Fatal(err)
	}
	f.handler, err = NewHandler(h)
	if err != nil {
		t.Fatal(err)
	}
	compatCreate(t, &model.Peer{Id: "cli-device", Uuid: "cli-uuid", Username: "old-user", Hostname: "old-host", Note: "device-only", Alias: "alias", AbId: 99})
	compatCreate(t, &model.AddressBook{Id: "cli-device", UserId: f.owner.Id, Note: "address-book-only"})
	return f
}

func cliSnapshot(t *testing.T) map[string][]byte {
	t.Helper()
	result := addressBookSessionSnapshot(t)
	result["device_groups"] = addressBookSessionRows(t, "device_groups", "id", "")
	return result
}

func assertCLIUnchanged(t *testing.T, before map[string][]byte) {
	t.Helper()
	for table, after := range cliSnapshot(t) {
		if !bytes.Equal(before[table], after) {
			t.Errorf("rejected CLI request changed %s", table)
		}
	}
}

func cliPeer(t *testing.T) model.Peer {
	t.Helper()
	var peer model.Peer
	if err := global.DB.Where("id = ?", "cli-device").First(&peer).Error; err != nil {
		t.Fatal(err)
	}
	return peer
}

func TestClientDeviceCLIInitialRoute(t *testing.T) {
	f := newCLIFixture(t)
	w := f.call("POST", "/api/devices/cli", `{"id":"cli-device","uuid":"cli-uuid","device_name":"assigned-host"}`, f.admin)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("CLI assignment failed: %d %s", w.Code, w.Body.String())
	}
	var peer model.Peer
	if err := global.DB.Where("id = ?", "cli-device").First(&peer).Error; err != nil || peer.Hostname != "assigned-host" {
		t.Fatal("CLI assignment did not update registered peer", err)
	}
}

func TestClientDeviceCLISupportedFieldsAndClearing(t *testing.T) {
	f := newCLIFixture(t)
	group := model.DeviceGroup{Name: "Exact Group"}
	compatCreate(t, &group)
	body := fmt.Sprintf(`{"id":"cli-device","uuid":"cli-uuid","user_name":%q,"device_group_name":"Exact Group","device_name":"new-host","device_username":"new-user","note":"设备备注"}`, f.owner.Username)
	for i := 0; i < 2; i++ {
		w := f.call("POST", "/api/devices/cli", body, f.admin)
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatalf("assignment/retry: %d %s", w.Code, w.Body.String())
		}
	}
	peer := cliPeer(t)
	if peer.UserId != f.owner.Id || peer.GroupId != group.Id || peer.Hostname != "new-host" || peer.Username != "new-user" || peer.Note != "设备备注" || peer.Alias != "alias" || peer.AbId != 99 {
		t.Fatal("incorrect assignment projection")
	}
	w := f.call("POST", "/api/devices/cli", `{"id":"cli-device","uuid":"cli-uuid","device_name":"","device_username":"","note":""}`, f.admin)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("explicit clear rejected")
	}
	peer = cliPeer(t)
	if peer.UserId != f.owner.Id || peer.GroupId != group.Id || peer.Hostname != "" || peer.Username != "" || peer.Note != "" {
		t.Fatal("clear changed ownership/group or preserved stale device values")
	}
	var book model.AddressBook
	if err := global.DB.Where("id = ?", "cli-device").First(&book).Error; err != nil || book.Note != "address-book-only" {
		t.Fatal("device note changed address book note", err)
	}
}

func TestClientDeviceCLIRejectsWholeUnsupportedOrMalformedRequest(t *testing.T) {
	f := newCLIFixture(t)
	cases := []string{`null`, `[]`, `{}`, `{"id":"cli-device","uuid":"cli-uuid"}`, `{"id":null,"uuid":"cli-uuid","note":"x"}`, `{"id":"cli-device","uuid":null,"note":"x"}`, `{"id":"cli-device","uuid":"cli-uuid","note":null}`, `{"id":"cli-device","uuid":"cli-uuid","note":"a","note":"b"}`, `{"id":"cli-device","uuid":"cli-uuid","note":"x"}{}`, `{"id":"cli-device","uuid":"wrong","note":"x"}`, `{"id":"CLI-device","uuid":"cli-uuid","note":"x"}`, `{"id":"cli-device","uuid":"cli-uuid","user_name":""}`, `{"id":"cli-device","uuid":"cli-uuid","user_name":"OWNER"}`, `{"id":"cli-device","uuid":"cli-uuid","user_name":"owner "}`, `{"id":"cli-device","uuid":"cli-uuid","device_group_name":""}`}
	for _, key := range []string{"unsupported_assignment", "user_id", "unknown"} {
		for _, value := range []any{"", nil, "value"} {
			body, _ := json.Marshal(map[string]any{"id": "cli-device", "uuid": "cli-uuid", "device_name": "must-not-apply", key: value})
			cases = append(cases, string(body))
		}
	}
	cases = append(cases, `{"id":"cli-device","uuid":"cli-uuid","note":"`+strings.Repeat("x", 1<<20)+`"}`)
	for i, body := range cases {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			before := cliSnapshot(t)
			w := f.call("POST", "/api/devices/cli", body, f.admin)
			if w.Code != 400 || w.Body.Len() == 0 || !strings.Contains(w.Body.String(), `"error"`) {
				t.Errorf("invalid request: %d %s", w.Code, w.Body.String())
			}
			assertCLIUnchanged(t, before)
		})
	}
}

func TestClientDeviceCLISessionAndAdministratorBoundary(t *testing.T) {
	for _, state := range []string{"anonymous", "bad", "ordinary", "disabled", "revoked", "expired", "demoted"} {
		t.Run(state, func(t *testing.T) {
			f := newCLIFixture(t)
			token, want := f.tokens[f.admin.Id], 401
			switch state {
			case "anonymous":
				token = ""
			case "bad":
				token = "bad-token"
			case "ordinary":
				token, want = f.tokens[f.actor.Id], 403
			case "disabled":
				if err := global.DB.Model(&f.admin).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
					t.Fatal(err)
				}
			case "revoked":
				if err := global.DB.Where("token = ?", token).Delete(&model.UserToken{}).Error; err != nil {
					t.Fatal(err)
				}
			case "expired":
				if err := global.DB.Model(&model.UserToken{}).Where("token = ?", token).Update("expired_at", time.Now().Add(-time.Minute).Unix()).Error; err != nil {
					t.Fatal(err)
				}
			case "demoted":
				want = 403
				if err := global.DB.Model(&f.admin).Update("is_admin", false).Error; err != nil {
					t.Fatal(err)
				}
			}
			before := cliSnapshot(t)
			w := addressBookSessionRequest(f, "POST", "/api/devices/cli", `{"id":"cli-device","uuid":"cli-uuid","note":"rejected"}`, token)
			var envelope map[string]json.RawMessage
			if w.Code != want || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || len(envelope) != 1 || len(envelope["error"]) == 0 {
				t.Errorf("session/admin envelope: %d %s", w.Code, w.Body.String())
			}
			assertCLIUnchanged(t, before)
		})
	}
}

func TestClientDeviceCLINoteProjectionRespectsGroupScope(t *testing.T) {
	f := newCLIFixture(t)
	compatCreate(t, &model.Peer{Id: "hidden-device", Uuid: "hidden-uuid", UserId: f.hidden.Id, Note: "hidden-device-note"})
	compatCreate(t, &model.Peer{Id: "second-visible", Uuid: "second-uuid", UserId: f.owner.Id, Note: "second-visible-note"})
	body := fmt.Sprintf(`{"id":"cli-device","uuid":"cli-uuid","user_name":%q,"note":"visible-device-note"}`, f.owner.Username)
	if w := f.call("POST", "/api/devices/cli", body, f.admin); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("note assignment: %d %s", w.Code, w.Body.String())
	}
	read := func(user model.User, current int, wantTotal int, wantID, wantNote string) {
		t.Helper()
		w := f.call("GET", fmt.Sprintf("/api/peers?current=%d&pageSize=1&status=1", current), "", user)
		var got struct {
			Total int
			Data  []struct{ ID, Note string }
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Total != wantTotal || len(got.Data) != 1 || got.Data[0].ID != wantID || got.Data[0].Note != wantNote {
			t.Fatalf("scoped note projection: %d %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "hidden-device-note") {
			t.Fatal("out-of-group device note leaked")
		}
	}
	read(f.actor, 1, 2, "cli-device", "visible-device-note")
	read(f.actor, 2, 2, "second-visible", "second-visible-note")
	if w := f.call("POST", "/api/devices/cli", `{"id":"cli-device","uuid":"cli-uuid","note":""}`, f.admin); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("clear note failed")
	}
	read(f.actor, 1, 2, "cli-device", "")
	if err := global.DB.Model(&model.User{}).Where("id = ?", f.actor.Id).Update("group_id", 2).Error; err != nil {
		t.Fatal(err)
	}
	w := f.call("GET", "/api/peers?current=1&pageSize=100&status=1", "", f.actor)
	var private struct {
		Total int
		Data  []any
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &private) != nil || private.Total != 0 || len(private.Data) != 0 || strings.Contains(w.Body.String(), "visible-device-note") {
		t.Fatal("private group exposed another user's device note")
	}
}
