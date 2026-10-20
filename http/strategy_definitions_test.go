package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	"github.com/lejianwen/rustdesk-api/v2/internal/strategydefinitions"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

const definitionPath = "/api/admin/strategy-definitions"

type definitionFixture struct {
	*compatFixture
	db *gorm.DB
}

func newDefinitionFixture(t *testing.T) definitionFixture {
	t.Helper()
	f := newCompatFixture(t)
	if err := database.Update(global.DB, 281, nil); err != nil {
		t.Fatal(err)
	}
	result := definitionFixture{f, global.DB}
	result.install(t, true, 0)
	return result
}

func (f definitionFixture) install(t *testing.T, configured bool, timeout time.Duration) {
	t.Helper()
	sessions, err := app.NewSessions(f.db, time.Hour, global.Jwt.Key)
	if err != nil {
		t.Fatal(err)
	}
	var store *strategydefinitions.Store
	if configured {
		store, err = strategydefinitions.New(f.db, strategydefinitions.Options{})
		if err != nil {
			t.Fatal(err)
		}
	}
	handler, err := httpapi.New(sessions, httpapi.Options{Translate: LegacyTranslator(), StrategyDefinitions: store, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	f.handler, err = NewHandler(handler)
	if err != nil {
		t.Fatal(err)
	}
}

func (f definitionFixture) request(ctx context.Context, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("api-token", token)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func (f definitionFixture) call(method, path, body string) *httptest.ResponseRecorder {
	return f.request(context.Background(), method, path, body, f.tokens[f.admin.Id])
}

func definitionResult(t *testing.T, w *httptest.ResponseRecorder, status, code int) json.RawMessage {
	t.Helper()
	var result map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != status || len(result) != 3 {
		t.Fatalf("definition response: HTTP%d %s err=%v", w.Code, w.Body.String(), err)
	}
	var actualCode int
	if json.Unmarshal(result["code"], &actualCode) != nil || actualCode != code || result["message"] == nil || result["data"] == nil {
		t.Fatalf("definition envelope: %s", w.Body.String())
	}
	if code != 0 && string(result["data"]) != "null" {
		t.Fatal("failed definition response exposed data", w.Body.String())
	}
	return result["data"]
}

func definitionID(t *testing.T, raw json.RawMessage) uint {
	t.Helper()
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 3 || fields["name"] == nil || fields["config_options"] == nil {
		t.Fatalf("unexpected definition projection: %s", raw)
	}
	var id uint
	if json.Unmarshal(fields["id"], &id) != nil || id == 0 {
		t.Fatal("missing definition id", string(raw))
	}
	return id
}

func definitionSnapshot(t *testing.T, f definitionFixture) []model.StrategyDefinition {
	t.Helper()
	var rows []model.StrategyDefinition
	if err := f.db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestStrategyDefinitionsHTTPCRUDAndBoundedPagination(t *testing.T) {
	f := newDefinitionFixture(t)
	var ids []uint
	for _, name := range []string{"Name", "name", "name ", "a\x00b", "界"} {
		body, _ := json.Marshal(map[string]any{"name": name, "config_options": map[string]string{"secret-value": "private-original", "empty": ""}})
		raw := definitionResult(t, f.call("POST", definitionPath, string(body)), 200, 0)
		ids = append(ids, definitionID(t, raw))
		var row strategydefinitions.Definition
		if json.Unmarshal(raw, &row) != nil || row.Name != name || row.ConfigOptions["secret-value"] != "private-original" {
			t.Fatal("definition values changed", string(raw))
		}
	}
	path := fmt.Sprintf("%s/%d", definitionPath, ids[0])
	for _, body := range []string{`{"name":"replacement","config_options":{}}`, fmt.Sprintf(`{"id":%d,"name":"replacement","config_options":{}}`, ids[0])} {
		definitionResult(t, f.call("PUT", path, body), 200, 0)
	}
	var updated strategydefinitions.Definition
	if json.Unmarshal(definitionResult(t, f.call("GET", path, ""), 200, 0), &updated) != nil || updated.Name != "replacement" || updated.ConfigOptions == nil || len(updated.ConfigOptions) != 0 {
		t.Fatal("full replacement/empty config did not persist")
	}
	all := []uint{}
	for current := 1; current <= 4; current++ {
		var page struct {
			List  []strategydefinitions.Definition `json:"list"`
			Total int64                            `json:"total"`
		}
		raw := definitionResult(t, f.call("GET", fmt.Sprintf("%s?current=%d&pageSize=2", definitionPath, current), ""), 200, 0)
		if json.Unmarshal(raw, &page) != nil || page.Total != 5 || page.List == nil {
			t.Fatal("invalid page projection", string(raw))
		}
		for _, row := range page.List {
			all = append(all, row.ID)
		}
	}
	if !reflect.DeepEqual(ids, all) {
		t.Fatal("pages repeated/skipped definitions", all)
	}
	var defaultPage struct {
		List  []strategydefinitions.Definition
		Total int64
	}
	if json.Unmarshal(definitionResult(t, f.call("GET", definitionPath, ""), 200, 0), &defaultPage) != nil || len(defaultPage.List) != 5 || defaultPage.Total != 5 {
		t.Fatal("default bounded page failed")
	}
	definitionResult(t, f.call("POST", definitionPath, `{"name":"replacement","config_options":{}}`), 409, 409)
	definitionResult(t, f.call("DELETE", path, ""), 200, 0)
	definitionResult(t, f.call("GET", path, ""), 404, 404)
	definitionResult(t, f.call("PUT", path, `{"name":"missing","config_options":{}}`), 404, 404)
	definitionResult(t, f.call("DELETE", path, ""), 404, 404)
}

func TestStrategyDefinitionsHTTPRejectsStrictInputWithoutWrites(t *testing.T) {
	f := newDefinitionFixture(t)
	id := definitionID(t, definitionResult(t, f.call("POST", definitionPath, `{"name":"stable","config_options":{"keep":"value"}}`), 200, 0))
	path := fmt.Sprintf("%s/%d", definitionPath, id)
	before := definitionSnapshot(t, f)
	cases := []struct{ method, path, body string }{}
	for _, body := range []string{`null`, `[]`, `{}`, `{"name":"x"}`, `{"config_options":{}}`, `{"name":"","config_options":{}}`, `{"name":null,"config_options":{}}`, `{"name":"x","config_options":null}`, `{"name":"x","config_options":[]}`, `{"name":"x","config_options":{"":"bad"}}`, `{"name":"x","config_options":{"x":null}}`, `{"name":"x","config_options":{"x":1}}`, `{"name":"x","config_options":{"x":true}}`, `{"name":"x","config_options":{"x":"a","x":"b"}}`, `{"name":"x","name":"y","config_options":{}}`, `{"name":"x","config_options":{},"unknown":1}`, `{"name":"x","config_options":{}} {}`, `{"name":"x","config_options":{}}` + strings.Repeat(" ", 1<<20)} {
		cases = append(cases, struct{ method, path, body string }{"POST", definitionPath, body}, struct{ method, path, body string }{"PUT", path, body})
	}
	for _, body := range []string{`{"id":0,"name":"x","config_options":{}}`, `{"id":1,"name":"x","config_options":{}}`, `{"id":null,"name":"x","config_options":{}}`} {
		cases = append(cases, struct{ method, path, body string }{"POST", definitionPath, body})
	}
	for _, badID := range []string{"0", "-1", "+1", "x", "1.5", "184467440737095516160"} {
		for _, method := range []string{"GET", "PUT", "DELETE"} {
			cases = append(cases, struct{ method, path, body string }{method, definitionPath + "/" + badID, `{"name":"x","config_options":{}}`})
		}
	}
	for _, badID := range []uint{0, id + 100} {
		cases = append(cases, struct{ method, path, body string }{"PUT", path, fmt.Sprintf(`{"id":%d,"name":"x","config_options":{}}`, badID)})
	}
	for index, tc := range cases {
		w := f.call(tc.method, tc.path, tc.body)
		definitionResult(t, w, 400, 400)
		if !reflect.DeepEqual(before, definitionSnapshot(t, f)) {
			t.Fatalf("invalid request %d retained writes", index)
		}
	}
	for _, query := range []string{"current=0", "current=-1", "current=", "current=1.5", "current=184467440737095516160", "current=1&current=2", "pageSize=0", "pageSize=-1", "pageSize=", "pageSize=1001", "pageSize=1&pageSize=2", "page=1", "unknown=1", "current=1;pageSize=2", "current=%zz", "current=9223372036854775807&pageSize=1000"} {
		definitionResult(t, f.call("GET", definitionPath+"?"+query, ""), 400, 400)
	}
}

func TestStrategyDefinitionsHTTPAdminSessionBoundary(t *testing.T) {
	for _, state := range []string{"anonymous", "bad-token", "revoked", "expired", "disabled", "ordinary"} {
		t.Run(state, func(t *testing.T) {
			f := newDefinitionFixture(t)
			id := definitionID(t, definitionResult(t, f.call("POST", definitionPath, `{"name":"keep","config_options":{}}`), 200, 0))
			token, status, code := f.tokens[f.admin.Id], 200, 403
			switch state {
			case "anonymous":
				token = ""
			case "bad-token":
				token = "invalid-session"
			case "revoked":
				definitionResult(t, f.call("POST", "/api/admin/logout", ""), 200, 0)
			case "expired":
				if err := f.db.Model(&model.UserToken{}).Where("token = ?", token).UpdateColumn("expired_at", time.Now().Add(-time.Hour).Unix()).Error; err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := f.db.Model(&model.User{}).Where("id = ?", f.admin.Id).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
					t.Fatal(err)
				}
				status, code = 401, 401
			case "ordinary":
				token = f.tokens[f.actor.Id]
			}
			before := definitionSnapshot(t, f)
			for _, route := range []struct{ method, path, body string }{{"GET", definitionPath, ""}, {"POST", definitionPath, `{"name":"new","config_options":{}}`}, {"GET", fmt.Sprintf("%s/%d", definitionPath, id), ""}, {"PUT", fmt.Sprintf("%s/%d", definitionPath, id), `{"name":"changed","config_options":{}}`}, {"DELETE", fmt.Sprintf("%s/%d", definitionPath, id), ""}} {
				definitionResult(t, f.request(context.Background(), route.method, route.path, route.body, token), status, code)
				if !reflect.DeepEqual(before, definitionSnapshot(t, f)) {
					t.Fatal("invalid administrator session changed definitions")
				}
			}
		})
	}
}

func TestStrategyDefinitionsHTTPStoreRechecksMiddlewarePrincipal(t *testing.T) {
	for _, change := range []string{"demote", "disable", "revoke"} {
		t.Run(change, func(t *testing.T) {
			f := newDefinitionFixture(t)
			fired := false
			if err := f.db.Callback().Query().After("gorm:query").Register("definition:stale-principal", func(tx *gorm.DB) {
				if fired || tx.Statement.Table != "users" {
					return
				}
				fired = true
				var result *gorm.DB
				write := tx.Session(&gorm.Session{NewDB: true})
				switch change {
				case "demote":
					result = write.Exec("UPDATE users SET is_admin = ? WHERE id = ?", false, f.admin.Id)
				case "disable":
					result = write.Exec("UPDATE users SET status = ? WHERE id = ?", model.COMMON_STATUS_DISABLED, f.admin.Id)
				case "revoke":
					result = write.Exec("DELETE FROM user_tokens WHERE token = ?", f.tokens[f.admin.Id])
				}
				if result.Error != nil || result.RowsAffected != 1 {
					tx.AddError(fmt.Errorf("principal injection failed: %v", result.Error))
				}
			}); err != nil {
				t.Fatal(err)
			}
			w := f.call("POST", definitionPath, `{"name":"must-not-create","config_options":{"private":"keep out of error"}}`)
			definitionResult(t, w, 200, 403)
			if !fired || len(definitionSnapshot(t, f)) != 0 {
				t.Fatal("Store trusted stale middleware principal")
			}
		})
	}
}

func TestStrategyDefinitionsHTTPSQLFailuresAndInstanceIsolation(t *testing.T) {
	f := newDefinitionFixture(t)
	id := definitionID(t, definitionResult(t, f.call("POST", definitionPath, `{"name":"keep","config_options":{"secret":"original"}}`), 200, 0))
	path := fmt.Sprintf("%s/%d", definitionPath, id)
	for _, tc := range []struct{ sql, method, path, body string }{
		{"INSERT", "POST", definitionPath, `{"name":"new","config_options":{"private":"must-not-leak"}}`},
		{"UPDATE", "PUT", path, `{"name":"changed","config_options":{"private":"must-not-leak"}}`},
		{"DELETE", "DELETE", path, ""},
	} {
		before := definitionSnapshot(t, f)
		if err := f.db.Exec("CREATE TRIGGER reject_definition_write BEFORE " + tc.sql + " ON strategy_definitions BEGIN SELECT RAISE(ABORT, 'private sql must-not-leak'); END").Error; err != nil {
			t.Fatal(err)
		}
		w := f.call(tc.method, tc.path, tc.body)
		definitionResult(t, w, 503, 503)
		if !strings.Contains(w.Body.String(), `"message":"Service unavailable"`) || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "must-not-leak") || !reflect.DeepEqual(before, definitionSnapshot(t, f)) {
			t.Fatal("SQL failure leaked data or retained writes", w.Body.String())
		}
		if err := f.db.Exec("DROP TRIGGER reject_definition_write").Error; err != nil {
			t.Fatal(err)
		}
	}
	f.install(t, false, 0)
	definitionResult(t, f.call("GET", definitionPath, ""), 503, 503)
	f.install(t, true, 0)
	other := newDefinitionFixture(t)
	definitionResult(t, other.call("POST", definitionPath, `{"name":"other-instance","config_options":{}}`), 200, 0)
	definitionResult(t, f.call("PUT", path, `{"name":"original-instance-only","config_options":{}}`), 200, 0)
	first, second := definitionSnapshot(t, f), definitionSnapshot(t, other)
	if len(first) != 1 || string(first[0].Name) != "original-instance-only" || len(second) != 1 || string(second[0].Name) != "other-instance" {
		t.Fatal("definition handler consulted latest global database")
	}
}

func TestStrategyDefinitionsHTTPContextAndNoOperationBodyLog(t *testing.T) {
	f := newDefinitionFixture(t)
	// Prove the legacy writer is active, rather than asserting zero on a fixture
	// in which operation logging cannot work at all.
	definitionResult(t, f.call("POST", "/api/admin/device_group/create", `{"name":"control-group"}`), 200, 0)
	var baseline int64
	if err := f.db.Model(&model.OperationLog{}).Count(&baseline).Error; err != nil || baseline != 1 {
		t.Fatal("legacy operation log control did not write", err, baseline)
	}
	definitionResult(t, f.call("POST", definitionPath, `{"name":"sensitive","config_options":{"plain-field":"secret-operation-value"}}`), 200, 0)
	definitionResult(t, f.call("POST", definitionPath, `{"name":"sensitive","config_options":{"plain-field":"secret-operation-value"}}`), 409, 409)
	var records []model.OperationLog
	if err := f.db.Find(&records).Error; err != nil || int64(len(records)) != baseline {
		t.Fatal("definition request reached generic operation log", err)
	}
	encoded, _ := json.Marshal(records)
	if bytes.Contains(encoded, []byte("secret-operation-value")) {
		t.Fatal("definition config leaked to operation log")
	}
	for _, cancelKind := range []string{"canceled", "deadline"} {
		t.Run(cancelKind, func(t *testing.T) {
			f.install(t, true, 100*time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fired := false
			if err := f.db.Callback().Query().Before("gorm:query").Register("definition:context", func(tx *gorm.DB) {
				if tx.Statement.Table != "strategy_definitions" {
					return
				}
				fired = true
				if cancelKind == "canceled" {
					cancel()
				} else {
					time.Sleep(200 * time.Millisecond)
				}
			}); err != nil {
				t.Fatal(err)
			}
			w := f.request(ctx, "GET", definitionPath, "", f.tokens[f.admin.Id])
			if err := f.db.Callback().Query().Remove("definition:context"); err != nil {
				t.Fatal(err)
			}
			status := 408
			if cancelKind == "deadline" {
				status = 504
			}
			definitionResult(t, w, status, status)
			if !fired {
				t.Fatal("request never reached Store context boundary")
			}
		})
	}
}
