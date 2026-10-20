package http

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientassign"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	"github.com/lejianwen/rustdesk-api/v2/internal/strategydefinitions"
	"github.com/lejianwen/rustdesk-api/v2/model"
)

func TestNamedStrategyProductionCLIHeartbeatAndDefinitionLifecycle(t *testing.T) {
	f := newCLIFixture(t)
	assign, err := clientassign.New(global.DB, clientassign.Options{})
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := strategydefinitions.New(global.DB, strategydefinitions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := app.NewSessions(global.DB, time.Hour, global.Jwt.Key)
	if err != nil {
		t.Fatal(err)
	}
	h, err := httpapi.New(sessions, httpapi.Options{Translate: LegacyTranslator(), Assign: assign, StrategyDefinitions: definitions})
	if err != nil {
		t.Fatal(err)
	}
	f.handler, err = NewHandler(h)
	if err != nil {
		t.Fatal(err)
	}
	df := definitionFixture{f, global.DB}
	// Use existing unbound defaults, without a real primary book reference.
	if err := global.DB.Model(&model.Peer{}).Where("id = ?", "cli-device").Updates(map[string]any{"alias": "", "ab_id": 0}).Error; err != nil {
		t.Fatal(err)
	}
	id := definitionID(t, definitionResult(t, df.call("POST", definitionPath, `{"name":"Policy\u0000Exact","config_options":{"named":"one","removed":"value"}}`), 200, 0))
	cli := func(body string, want int) {
		t.Helper()
		w := f.call("POST", "/api/devices/cli", body, f.admin)
		if w.Code != want || (want == 200 && w.Body.Len() != 0) || (want != 200 && w.Body.Len() == 0) {
			t.Fatalf("CLI response %d %s", w.Code, w.Body.String())
		}
	}
	cli(`{"id":"cli-device","uuid":"cli-uuid","strategy_name":"Policy\u0000Exact"}`, 200)
	if p := cliPeer(t); p.StrategyDefinitionID == nil || *p.StrategyDefinitionID != id {
		t.Fatal("CLI did not persist definition relation")
	}
	heartbeat := func(known int64) (int64, map[string]string) {
		t.Helper()
		w := f.call("POST", "/api/heartbeat", fmt.Sprintf(`{"id":"cli-device","uuid":"cli-uuid","modified_at":%d}`, known), f.admin)
		var got struct {
			ModifiedAt int64 `json:"modified_at"`
			Strategy   struct {
				ConfigOptions map[string]string `json:"config_options"`
			} `json:"strategy"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.ModifiedAt == 0 {
			t.Fatalf("heartbeat %d %s", w.Code, w.Body.String())
		}
		return got.ModifiedAt, got.Strategy.ConfigOptions
	}
	v, options := heartbeat(0)
	if options["named"] != "one" || options["removed"] != "value" {
		t.Fatal("named definition did not reach production heartbeat")
	}
	definitionResult(t, df.call("DELETE", fmt.Sprintf("%s/%d", definitionPath, id), ""), 409, 409)
	definitionResult(t, df.call("PUT", fmt.Sprintf("%s/%d", definitionPath, id), `{"name":"Renamed","config_options":{"named":"two"}}`), 200, 0)
	newV, options := heartbeat(v)
	if newV == v || options["named"] != "two" {
		t.Fatal("live definition update did not change payload")
	}
	if value, ok := options["removed"]; !ok || value != "" {
		t.Fatal("removed named key not explicitly cleared")
	}
	cli(`{"id":"cli-device","uuid":"cli-uuid","note":"preserve-link"}`, 200)
	if p := cliPeer(t); p.StrategyDefinitionID == nil || *p.StrategyDefinitionID != id {
		t.Fatal("omitted name detached association")
	}
	cli(`{"id":"cli-device","uuid":"cli-uuid","strategy_name":"Renamed","address_book_name":"missing","note":"must-rollback"}`, 400)
	if p := cliPeer(t); p.Note != "preserve-link" {
		t.Fatal("mixed failure changed peer")
	}
	cli(`{"id":"cli-device","uuid":"cli-uuid","strategy_name":""}`, 200)
	if p := cliPeer(t); p.StrategyDefinitionID != nil {
		t.Fatal("empty name was not SQL NULL detach")
	}
	_, options = heartbeat(0)
	for _, key := range []string{"named", "removed"} {
		if value, ok := options[key]; !ok || value != "" {
			t.Fatal("detach did not preserve managed-key clearing")
		}
	}
	definitionResult(t, df.call("DELETE", fmt.Sprintf("%s/%d", definitionPath, id), ""), 200, 0)
	for _, raw := range []string{`null`, `1`, `true`, `[]`} {
		cli(`{"id":"cli-device","uuid":"cli-uuid","strategy_name":`+raw+`}`, 400)
	}
	cli(`{"id":"cli-device","uuid":"cli-uuid","strategy_name":"`+strings.Repeat("x", 256)+`"}`, 400)
}
