package http

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/model"
)

func TestClientAddressBookSettingsAcceptsEmptyBodyAndReturnsUnlimitedInteger(t *testing.T) {
	f := newCompatFixture(t)
	for _, personal := range []int{-1, 0, 1, 2, 99} {
		global.Config.Rustdesk.Personal = personal
		for _, body := range []string{"", `{}`} {
			result := f.call("POST", "/api/ab/settings", body, f.actor)
			var response map[string]json.RawMessage
			if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &response) != nil || len(response) != 1 {
				t.Fatalf("settings personal=%d body=%q: %d %s", personal, body, result.Code, result.Body.String())
			}
			var limit int
			if value, present := response["max_peer_one_ab"]; !present || string(value) == "null" || json.Unmarshal(value, &limit) != nil || limit != 0 {
				t.Fatalf("client expects max_peer_one_ab integer zero: %s", result.Body.String())
			}
		}
	}
}

func TestClientPersonalProfileEnabledUsesAuthenticatedIdentity(t *testing.T) {
	f := newCompatFixture(t)
	global.Config.Rustdesk.Personal = 1
	for _, actor := range []model.User{f.actor, f.hidden} {
		for _, body := range []string{"", `{}`, fmt.Sprintf(`{"owner":%d,"user_id":%d,"group_id":%d,"guid":%q,"rule":0}`, f.owner.Id, f.owner.Id, f.owner.GroupId, compatGUID(f.owner, 999))} {
			path := fmt.Sprintf("/api/ab/personal?owner=%d&guid=%s&rule=0", f.owner.Id, compatGUID(f.owner, 999))
			result := f.call("POST", path, body, actor)
			var response map[string]json.RawMessage
			if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &response) != nil || len(response) != 3 {
				t.Fatalf("personal actor=%d body=%q: %d %s", actor.Id, body, result.Code, result.Body.String())
			}
			var guid, name string
			var rule int
			if json.Unmarshal(response["guid"], &guid) != nil || guid != compatGUID(actor, 0) || json.Unmarshal(response["name"], &name) != nil || name != actor.Username || json.Unmarshal(response["rule"], &rule) != nil || rule != 3 {
				t.Fatalf("personal profile did not use authenticated identity and full control: %s", result.Body.String())
			}
		}
	}
}

func TestClientPersonalProfileDisabledUsesClientLegacyFallback(t *testing.T) {
	f := newCompatFixture(t)
	// Locked RustDesk 1.5.0 ab_model.dart:270-294 returns true immediately on
	// HTTP404 and retains the legacy GUID state. HTTP200 null instead enters
	// _jsonDecodeRespMap:1976-1986, fails its non-null Map assignment and stops
	// the pull. This asserts that explicit fallback contract, not a Dart run.
	for _, personal := range []int{-1, 0, 2, 99} {
		global.Config.Rustdesk.Personal = personal
		for _, body := range []string{"", `{}`} {
			result := f.call("POST", "/api/ab/personal", body, f.actor)
			if result.Code != 404 {
				t.Errorf("personal=%d body=%q: client requires 404 to continue legacy initialization; got %d %s", personal, body, result.Code, result.Body.String())
			}
		}
	}
	// Authentication must run before the configured legacy fallback, so an
	// anonymous request cannot receive the authenticated 404 downgrade signal.
	global.Config.Rustdesk.Personal = 0
	request := httptest.NewRequest("POST", "/api/ab/personal", nil)
	result := httptest.NewRecorder()
	f.handler.ServeHTTP(result, request)
	if result.Code != 401 {
		t.Fatalf("disabled personal profile bypassed authentication: %d %s", result.Code, result.Body.String())
	}
}
