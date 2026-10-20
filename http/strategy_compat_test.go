package http

import (
	"context"
	"encoding/json"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/service"
)

type strategyFixture struct {
	handler stdhttp.Handler
	peer    model.Peer
	token   string
}

func newStrategyFixture(t *testing.T) *strategyFixture {
	t.Helper()
	sessions := installLegacyFixture(t)
	global.DB.Config.DisableForeignKeyConstraintWhenMigrating = true
	if err := global.DB.AutoMigrate(&model.User{}, &model.UserToken{}, &model.Peer{}, &model.PeerStrategy{}, &model.PeerStrategyState{}, &model.AddressBookCollection{}, &model.OperationLog{}); err != nil {
		t.Fatal(err)
	}
	// Use the runtime schema for current-model peer writes.
	if err := database.Update(global.DB, 281, nil); err != nil {
		t.Fatal(err)
	}
	pool, err := global.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	if err := global.DB.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
		t.Fatal(err)
	}
	if err := global.DB.Migrator().CreateConstraint(&model.PeerStrategyState{}, "Peer"); err != nil {
		t.Fatal(err)
	}
	yes := true
	u := model.User{Username: "strategy-admin", IsAdmin: &yes, Status: model.COMMON_STATUS_ENABLE}
	if err := global.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	global.Jwt.TokenExpireDuration = time.Hour
	token := global.Jwt.GenerateToken(u.Id)
	if err := global.DB.Create(&model.UserToken{UserId: u.Id, Token: token, ExpiredAt: time.Now().Add(time.Hour).Unix()}).Error; err != nil {
		t.Fatal(err)
	}
	peer := model.Peer{Id: "test-strategy-peer", Uuid: "strategy-device-uuid", Alias: "bound", LastOnlineTime: time.Now().Unix()}
	if err := global.DB.Create(&peer).Error; err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(sessions)
	if err != nil {
		t.Fatal(err)
	}
	return &strategyFixture{handler, peer, token}
}

func (f *strategyFixture) call(ctx context.Context, path, body string, admin bool) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, "POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if admin {
		r.Header.Set("api-token", f.token)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func (f *strategyFixture) heartbeat(known int64) *httptest.ResponseRecorder {
	return f.call(context.Background(), "/api/heartbeat", fmt.Sprintf(`{"id":%q,"uuid":%q,"ver":1005001,"modified_at":%d}`, f.peer.Id, f.peer.Uuid, known), false)
}

func strategyResponse(t *testing.T, w *httptest.ResponseRecorder) (int64, map[string]string) {
	t.Helper()
	var got struct {
		ModifiedAt int64 `json:"modified_at"`
		Strategy   struct {
			ConfigOptions map[string]string `json:"config_options"`
		} `json:"strategy"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.ModifiedAt <= 0 || got.Strategy.ConfigOptions == nil {
		t.Fatalf("heartbeat contract: %d %s", w.Code, w.Body.String())
	}
	return got.ModifiedAt, got.Strategy.ConfigOptions
}

func strategyAdmin(t *testing.T, f *strategyFixture, path, body string) {
	t.Helper()
	w := f.call(context.Background(), "/api/admin/peer_strategy/"+path, body, true)
	var got struct {
		Code  int    `json:"code"`
		Error string `json:"error"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Code != 0 || got.Error != "" {
		t.Fatalf("admin policy contract: %d %s", w.Code, w.Body.String())
	}
}

func TestProductionHeartbeatPolicyDeletionAndClientClearSemantics(t *testing.T) {
	f := newStrategyFixture(t)
	strategyAdmin(t, f, "default/update", `{"config_options":{"enable-keyboard":"N","default-only":"default"}}`)
	strategyAdmin(t, f, "create", fmt.Sprintf(`{"peer_id":%q,"config_options":{"enable-keyboard":"Y","device-only":"sensitive-value"}}`, f.peer.Id))
	revision, options := strategyResponse(t, f.heartbeat(0))
	if options["enable-keyboard"] != "Y" || options["default-only"] != "default" {
		t.Fatal(options)
	}
	if w := f.heartbeat(revision); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatal(w.Code, w.Body.String())
	}
	var strategy model.PeerStrategy
	if err := global.DB.Where("peer_id = ?", f.peer.Id).First(&strategy).Error; err != nil {
		t.Fatal(err)
	}
	// Real administrator deletion must send the removed key explicitly and
	// inherit the existing default on the next production heartbeat route.
	strategyAdmin(t, f, "delete", fmt.Sprintf(`{"id":%d}`, strategy.Id))
	next, fallback := strategyResponse(t, f.heartbeat(revision))
	if next == revision || fallback["enable-keyboard"] != "N" || fallback["device-only"] != "" {
		t.Fatal(fallback)
	}
	strategyAdmin(t, f, "default/update", `{"config_options":{}}`)
	finalRevision, cleared := strategyResponse(t, f.heartbeat(next))
	for _, key := range []string{"enable-keyboard", "device-only", "default-only"} {
		if value, exists := cleared[key]; !exists || value != "" {
			t.Fatalf("deleted key %q missing clear", key)
		}
	}
	if _, exists := cleared["personal-only"]; exists {
		t.Fatal("unmanaged local setting included")
	}
	var state model.PeerStrategyState
	if err := global.DB.First(&state, "peer_row_id = ?", f.peer.RowId).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(state.ManagedKeys, "sensitive-value") || strings.Contains(state.ManagedKeys, "default\"") {
		t.Fatal("history persisted values")
	}
	// Equivalent algorithm sample from locked RustDesk sync.rs:292-308 and
	// hbb_common 5d52530 config.rs:1148-1177,2584-2596. This does not execute Rust.
	local := map[string]string{"personal-only": "keep", "device-only": "sensitive-value", "enable-keyboard": "Y"}
	user, effective := applyLockedClientPolicy(local, nil, nil, cleared)
	if user["personal-only"] != "keep" || effective["personal-only"] != "keep" {
		t.Fatal("unmanaged personal key changed")
	}
	if _, exists := user["device-only"]; exists {
		t.Fatal("empty default did not remove user override")
	}
	user, effective = applyLockedClientPolicy(local, map[string]string{"device-only": "custom-default"}, nil, cleared)
	if value, exists := user["device-only"]; !exists || value != "" || effective["device-only"] != "" {
		t.Fatal("accepted explicit empty did not suppress custom default")
	}
	_, effective = applyLockedClientPolicy(local, nil, map[string]string{"device-only": "fixed-overwrite"}, cleared)
	if effective["device-only"] != "fixed-overwrite" {
		t.Fatal("policy unexpectedly overrode fixed custom setting")
	}
	// History is already committed; a dropped response can be fetched again.
	// A fresh store over this DB is also covered in the core's restart sample.
	if w := f.heartbeat(0); w.Code != 200 {
		t.Fatal(w.Code)
	}
	got, _ := strategyResponse(t, f.heartbeat(0))
	if got != finalRevision {
		t.Fatal("stable final payload revision changed")
	}
}

func TestProductionHeartbeatFailuresCannotBecomeEmptySuccess(t *testing.T) {
	for _, fault := range []string{"read", "write", "history", "policy", "stopped_worker"} {
		t.Run(fault, func(t *testing.T) {
			f := newStrategyFixture(t)
			switch fault {
			case "read":
				if err := global.DB.Migrator().DropTable(&model.PeerStrategy{}); err != nil {
					t.Fatal(err)
				}
			case "write":
				if err := global.DB.Exec("CREATE TRIGGER deny_history BEFORE INSERT ON peer_strategy_states BEGIN SELECT RAISE(FAIL, 'do-not-expose-sql-secret'); END").Error; err != nil {
					t.Fatal(err)
				}
			case "history":
				if err := global.DB.Create(&model.PeerStrategyState{PeerRowID: f.peer.RowId, ManagedKeys: `corrupt-history-secret`}).Error; err != nil {
					t.Fatal(err)
				}
			case "policy":
				if err := global.DB.Exec("INSERT INTO peer_strategies(peer_id,config_options,modified_at) VALUES(?,?,?)", f.peer.Id, `corrupt-policy-secret`, 1).Error; err != nil {
					t.Fatal(err)
				}
			case "stopped_worker":
				if err := service.AllService.PeerService.Stop(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := global.DB.Model(&f.peer).Update("last_online_time", 0).Error; err != nil {
					t.Fatal(err)
				}
			}
			w := f.heartbeat(0)
			if w.Code != 503 || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "strategy\"") {
				t.Fatalf("failure response: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestProductionHeartbeatDeviceAndContextBoundaries(t *testing.T) {
	f := newStrategyFixture(t)
	for _, body := range []string{`{}`, `{"id":"unknown","uuid":"uuid"}`, fmt.Sprintf(`{"id":%q,"uuid":"wrong-uuid"}`, f.peer.Id)} {
		w := f.call(context.Background(), "/api/heartbeat", body, false)
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{}" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	var count int64
	global.DB.Model(&model.PeerStrategyState{}).Count(&count)
	if count != 0 {
		t.Fatal("unknown device obtained managed state")
	}
	body := fmt.Sprintf(`{"id":%q,"uuid":%q}`, f.peer.Id, f.peer.Uuid)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if w := f.call(ctx, "/api/heartbeat", body, false); w.Code != 408 {
		t.Fatal(w.Code, w.Body.String())
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if w := f.call(ctx, "/api/heartbeat", body, false); w.Code != 504 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestProductionHeartbeatNilStrategyOptionsClearAsJSONObject(t *testing.T) {
	for _, target := range []string{"default", "default_unbound", "device"} {
		for _, options := range []string{"omitted", "null", "object"} {
			t.Run(target+"/"+options, func(t *testing.T) {
				f := newStrategyFixture(t)
				peerID, path := "", target+"/update"
				if target == "default_unbound" {
					peerID = model.UnboundDefaultPeerId
					if err := global.DB.Model(&f.peer).Update("alias", "").Error; err != nil {
						t.Fatal(err)
					}
				} else if target == "device" {
					peerID, path = f.peer.Id, "update"
				}
				if target == "device" {
					strategyAdmin(t, f, "create", fmt.Sprintf(`{"peer_id":%q,"config_options":{"previously-managed":"value"}}`, peerID))
				} else {
					strategyAdmin(t, f, path, `{"config_options":{"previously-managed":"value"}}`)
				}
				before, current := strategyResponse(t, f.heartbeat(0))
				if current["previously-managed"] != "value" {
					t.Fatal("initial policy not delivered")
				}
				body := map[string]interface{}{}
				if target == "device" {
					var row model.PeerStrategy
					if err := global.DB.Where("peer_id = ?", peerID).First(&row).Error; err != nil {
						t.Fatal(err)
					}
					body["id"], body["peer_id"] = row.Id, peerID
				}
				if options == "null" {
					body["config_options"] = nil
				} else if options == "object" {
					body["config_options"] = map[string]string{}
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				strategyAdmin(t, f, path, string(encoded))
				// Read the raw column instead of AutoJson.Scan, which normalizes
				// malformed input and could hide an accidentally persisted null.
				var stored string
				if err := global.DB.Model(&model.PeerStrategy{}).Select("config_options").Where("peer_id = ?", peerID).Scan(&stored).Error; err != nil {
					t.Fatal(err)
				}
				if stored != "{}" {
					t.Fatalf("empty full policy stored %q, want {}", stored)
				}
				after, cleared := strategyResponse(t, f.heartbeat(before))
				if value, exists := cleared["previously-managed"]; after == before || !exists || value != "" {
					t.Fatal("empty replacement did not explicitly clear historical managed key", cleared)
				}
				if _, exists := cleared["unmanaged-personal"]; exists {
					t.Fatal("empty replacement claimed unrelated local key")
				}
			})
		}
	}
}

func applyLockedClientPolicy(user, defaults, overwrite, received map[string]string) (map[string]string, map[string]string) {
	copyMap := func(source map[string]string) map[string]string {
		result := map[string]string{}
		for k, v := range source {
			result[k] = v
		}
		return result
	}
	options := copyMap(defaults)
	for k, v := range user {
		options[k] = v
	}
	for k, v := range overwrite {
		options[k] = v
	}
	for k, v := range received {
		if v == "" && defaults[k] == "" {
			delete(options, k)
		} else {
			options[k] = v
		}
	}
	for k, v := range options {
		_, fixed := overwrite[k]
		defaultValue, hasDefault := defaults[k]
		if fixed || hasDefault && defaultValue == v {
			delete(options, k)
		}
	}
	effective := copyMap(defaults)
	for k, v := range options {
		effective[k] = v
	}
	for k, v := range overwrite {
		effective[k] = v
	}
	return options, effective
}
