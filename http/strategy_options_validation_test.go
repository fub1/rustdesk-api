package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/model"
)

func strategyWriteFixture(t *testing.T, target string) (*strategyFixture, string, string, map[string]any) {
	t.Helper()
	f := newStrategyFixture(t)
	path, peerID := target, f.peer.Id
	body := map[string]any{}
	switch target {
	case "create":
		strategyAdmin(t, f, "default/update", `{"config_options":{"keep":"before"}}`)
		body["peer_id"] = peerID
	case "update":
		strategyAdmin(t, f, "create", fmt.Sprintf(`{"peer_id":%q,"config_options":{"keep":"before"}}`, peerID))
		var current model.PeerStrategy
		if err := global.DB.Where("peer_id = ?", peerID).First(&current).Error; err != nil {
			t.Fatal(err)
		}
		body["id"], body["peer_id"] = current.Id, peerID
	case "default":
		peerID, path = "", "default/update"
		strategyAdmin(t, f, path, `{"config_options":{"keep":"before"}}`)
	case "default_unbound":
		peerID, path = model.UnboundDefaultPeerId, "default_unbound/update"
		if err := global.DB.Model(&f.peer).Update("alias", "").Error; err != nil {
			t.Fatal(err)
		}
		strategyAdmin(t, f, path, `{"config_options":{"keep":"before"}}`)
	}
	_, options := strategyResponse(t, f.heartbeat(0))
	if options["keep"] != "before" {
		t.Fatal("valid baseline policy was not delivered")
	}
	return f, path, peerID, body
}

func strategyWriteSnapshot(t *testing.T) map[string][]byte {
	t.Helper()
	return map[string][]byte{
		"policies": addressBookSessionRows(t, "peer_strategies", "id", ""),
		"history":  addressBookSessionRows(t, "peer_strategy_states", "peer_row_id", ""),
		"devices":  addressBookSessionRows(t, "peers", "row_id", ""),
	}
}

func TestProductionStrategyWritesRejectEmptyOptionKey(t *testing.T) {
	for _, target := range []string{"create", "update", "default", "default_unbound"} {
		t.Run(target, func(t *testing.T) {
			f, path, peerID, body := strategyWriteFixture(t, target)
			body["config_options"] = map[string]string{"": "invalid-empty-key", "keep": "must-not-apply"}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			before := strategyWriteSnapshot(t)
			w := f.call(context.Background(), "/api/admin/peer_strategy/"+path, string(encoded), true)
			var response struct {
				Code    int
				Message string
				Data    any
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Code != 101 || response.Message == "" || response.Data != nil {
				t.Errorf("empty option key was accepted: HTTP%d code=%d", w.Code, response.Code)
			}
			for kind, after := range strategyWriteSnapshot(t) {
				if !bytes.Equal(before[kind], after) {
					t.Errorf("invalid strategy changed %s", kind)
				}
			}
			// On the unfixed server this shows successful persistence followed by
			// a failed production heartbeat, rather than merely a rejected fixture.
			heartbeat := f.heartbeat(0)
			if heartbeat.Code != 200 {
				var raw string
				if err := global.DB.Model(&model.PeerStrategy{}).Select("config_options").Where("peer_id = ?", peerID).Scan(&raw).Error; err != nil {
					t.Fatal(err)
				}
				var persisted map[string]string
				_ = json.Unmarshal([]byte(raw), &persisted)
				_, hasEmpty := persisted[""]
				t.Errorf("writer/reader conflict: stored_empty_key=%t heartbeat=HTTP%d", hasEmpty, heartbeat.Code)
				return
			}
			_, options := strategyResponse(t, heartbeat)
			if options["keep"] != "before" {
				t.Fatal("rejected document changed delivered policy")
			}
		})
	}
}

func TestProductionStrategyWritesPreserveEmptyReplacementAndEmptyValues(t *testing.T) {
	for _, target := range []string{"create", "update", "default", "default_unbound"} {
		for _, shape := range []string{"omitted", "null", "object", "empty-value", "whitespace-key"} {
			t.Run(target+"/"+shape, func(t *testing.T) {
				f, path, peerID, body := strategyWriteFixture(t, target)
				want := map[string]string{}
				switch shape {
				case "null":
					body["config_options"] = nil
				case "object":
					body["config_options"] = map[string]string{}
				case "empty-value":
					want = map[string]string{"custom-client-option": ""}
					body["config_options"] = want
				case "whitespace-key":
					want = map[string]string{" ": "allowed-nonempty-key"}
					body["config_options"] = want
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				strategyAdmin(t, f, path, string(encoded))
				var raw string
				if err := global.DB.Model(&model.PeerStrategy{}).Select("config_options").Where("peer_id = ?", peerID).Scan(&raw).Error; err != nil {
					t.Fatal(err)
				}
				canonical, _ := json.Marshal(want)
				if raw != string(canonical) {
					t.Fatal("valid replacement was changed by validation")
				}
				_, options := strategyResponse(t, f.heartbeat(0))
				for key, value := range want {
					if got, ok := options[key]; !ok || got != value {
						t.Fatal("valid nonempty key was not delivered")
					}
				}
			})
		}
	}
}
