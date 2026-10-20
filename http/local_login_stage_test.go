package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/utils"
)

// Valid primary credentials must not turn an unsupported challenge request into
// an ordinary password login. Local MFA is not enabled by this regression.
func TestClientLocalLoginRejectsUnsupportedStages(t *testing.T) {
	f := newCompatFixture(t)
	if err := global.DB.AutoMigrate(&model.LoginLog{}); err != nil {
		t.Fatal(err)
	}
	password := "stage-test-password"
	hash, err := utils.EncryptPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if err := global.DB.Model(&model.User{}).Where("id = ?", f.actor.Id).Update("password", hash).Error; err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string][]byte {
		result := map[string][]byte{}
		for _, table := range []string{"users", "user_tokens", "login_logs", "peers"} {
			order := "id"
			if table == "peers" {
				order = "row_id"
			}
			result[table] = addressBookSessionRows(t, table, order, "")
		}
		return result
	}

	cases := map[string]map[string]any{}
	for _, stage := range []string{"email_code", "tfa_code", "mobile", "sms_code", "unknown", "ACCOUNT"} {
		cases[stage] = map[string]any{"type": stage}
	}
	for _, stage := range []string{"account", ""} {
		for _, field := range []string{"verificationCode", "tfaCode", "secret"} {
			for _, value := range []any{"123456", "", nil} {
				cases[fmt.Sprintf("%s/%s/%v", stage, field, value)] = map[string]any{"type": stage, field: value}
			}
		}
	}
	// These password-less shapes are what the fixed Flutter and Sciter clients
	// actually send after email_check. The capability remains unsupported here.
	cases["native-email"] = map[string]any{"type": "email_code", "password": nil, "verificationCode": "123456", "secret": "opaque-challenge"}
	cases["native-totp"] = map[string]any{"type": "email_code", "password": nil, "verificationCode": "123456", "tfaCode": "123456", "secret": "opaque-challenge"}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			body := map[string]any{"username": f.actor.Username, "password": password, "id": "stage-device", "uuid": "stage-uuid"}
			for key, value := range extra {
				if key == "password" && value == nil {
					delete(body, key)
					continue
				}
				body[key] = value
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			before := snapshot()
			w := addressBookSessionRequest(f, "POST", "/api/login", string(encoded), "")
			var envelope map[string]json.RawMessage
			if json.Unmarshal(w.Body.Bytes(), &envelope) != nil || w.Code != 400 || len(envelope) != 1 || len(envelope["error"]) == 0 {
				t.Errorf("unsupported stage did not return a client error (HTTP %d)", w.Code)
			}
			for table, after := range snapshot() {
				if !bytes.Equal(before[table], after) {
					t.Errorf("unsupported stage changed %s", table)
				}
			}
		})
	}

	// Both the fixed account request and the fork's historical omitted type are
	// still real password logins; the issued token must authenticate afterwards.
	for _, stage := range []string{"account", ""} {
		t.Run("positive/"+stage, func(t *testing.T) {
			body := map[string]any{"username": f.actor.Username, "password": password}
			if stage != "" {
				body["type"] = stage
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			w := addressBookSessionRequest(f, "POST", "/api/login", string(encoded), "")
			var login struct {
				AccessToken string `json:"access_token"`
				Type        string `json:"type"`
			}
			if json.Unmarshal(w.Body.Bytes(), &login) != nil || w.Code != 200 || login.Type != "access_token" || login.AccessToken == "" {
				t.Fatalf("ordinary password login failed (HTTP %d)", w.Code)
			}
			current := addressBookSessionRequest(f, "POST", "/api/currentUser", "{}", login.AccessToken)
			if current.Code != 200 {
				t.Fatalf("issued ordinary session rejected (HTTP %d)", current.Code)
			}
		})
	}
}
