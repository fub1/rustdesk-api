package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/sirupsen/logrus"
)

func TestRequestLoggerUsesRouteTemplate(t *testing.T) {
	var output bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&output)
	logger.SetLevel(logrus.DebugLevel)
	logger.SetFormatter(&logrus.JSONFormatter{})
	g := gin.New()
	g.Use(requestLogger(logger))
	g.GET("/callback/:provider", func(c *gin.Context) { c.Status(204) })
	for _, path := range []string{"/callback/secret-provider?code=secret-code&state=secret-state", "/secret-unmatched?token=secret-token"} {
		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	if strings.Contains(output.String(), "secret-") {
		t.Fatalf("request credentials leaked: %s", output.String())
	}
	for i, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var fields map[string]interface{}
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatal(err)
		}
		want := "/callback/:provider"
		if i == 1 {
			want = "<unmatched>"
		}
		if fields["route"] != want || fields["method"] != "GET" {
			t.Fatalf("request metadata: %v", fields)
		}
	}
}

func captureOperation(t *testing.T, path, body string, extra gin.H) *model.OperationLog {
	t.Helper()
	records := make(chan *model.OperationLog, 1)
	g := gin.New()
	g.Use(operationLog(func(record *model.OperationLog) error { records <- record; return nil }))
	g.POST(path, func(c *gin.Context) {
		actual, err := io.ReadAll(c.Request.Body)
		if err != nil || string(actual) != body {
			t.Errorf("audit changed request body: %d bytes, %v", len(actual), err)
		}
		c.Set("curUser", &model.User{IdModel: model.IdModel{Id: 7}, Username: "operator"})
		c.Set("op_log_extra", extra)
		c.Status(204)
	})
	g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", path, strings.NewReader(body)))
	select {
	case record := <-records:
		if record.UserId != 7 || record.Username != "operator" || record.Path != path || record.Op == "" || record.Resource == "" {
			t.Fatalf("audit metadata lost: %#v", record)
		}
		return record
	case <-time.After(time.Second):
		t.Fatal("operation audit not written")
		return nil
	}
}

func TestOperationLogOmitsSensitiveDetailsAndKeepsTarget(t *testing.T) {
	for _, path := range []string{
		"/api/admin/user/changePwd", "/api/admin/user/changeCurPwd", "/api/admin/oauth/create",
		"/api/admin/oauth/bindConfirm", "/api/admin/user_token/delete", "/api/admin/peer_strategy/update",
		"/api/admin/config/update",
	} {
		t.Run(path, func(t *testing.T) {
			body := `{"id":42,"password":"secret-password","code":"secret-code","unknown":"secret-unknown","config_options":{"preset-connect-password":"secret-preset"}}`
			record := captureOperation(t, path, body, gin.H{"username": "target-user", "client_secret": "secret-client", "unknown": "secret-extra"})
			if strings.Contains(record.Detail, "secret-") || !strings.Contains(record.Detail, `"id":42`) || !strings.Contains(record.Detail, "target-user") {
				t.Fatalf("unsafe or incomplete sensitive audit: %s", record.Detail)
			}
		})
	}
}

func TestOperationLogRedactsNestedAndTypedExtraValues(t *testing.T) {
	record := captureOperation(t, "/api/admin/peer/update",
		`{"id":42,"alias":"visible","nested":[{"old_password":"secret-old","code_verifier":"secret-verifier"}],"token":"secret-token"}`,
		gin.H{"extra": map[string]string{"client-secret": "secret-client", "api_key": "secret-key", "label": "keep-label"}})
	if strings.Contains(record.Detail, "secret-") || !strings.Contains(record.Detail, "visible") || !strings.Contains(record.Detail, "keep-label") {
		t.Fatalf("unsafe or incomplete audit: %s", record.Detail)
	}
	var detail map[string]interface{}
	if err := json.Unmarshal([]byte(record.Detail), &detail); err != nil {
		t.Fatal(err)
	}
}

func TestAddressBookCredentialsAreNotOperationDetails(t *testing.T) {
	for _, path := range []string{"/api/admin/address_book/create", "/api/admin/address_book/update", "/api/admin/my/address_book/create", "/api/admin/my/address_book/update"} {
		record := captureOperation(t, path, `{"id":42,"hash":"private-remote-password-hash","nested":{"verifier":"private-verifier"},"alias":"device-name"}`, nil)
		if strings.Contains(record.Detail, "private-") || !strings.Contains(record.Detail, "device-name") {
			t.Fatalf("address-book credential leaked or useful metadata lost: %s", record.Detail)
		}
	}
}

func TestOperationLogDoesNotFallBackToUnsafeRawBody(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		extra      gin.H
	}{
		{"invalid", `{"password":"secret-invalid"`, nil},
		{"oversized", `{"password":"secret-large","padding":"` + strings.Repeat("x", maxOperationBodyBytes) + `"}`, nil},
		{"unserializable extra", `{"password":"secret-extra"}`, gin.H{"unsupported": make(chan int)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := captureOperation(t, "/api/admin/peer/update", tc.body, tc.extra)
			if strings.Contains(record.Detail, "secret-") || !strings.Contains(record.Detail, "OMITTED") {
				t.Fatalf("unsafe audit fallback: %s", record.Detail)
			}
		})
	}
	record := captureOperation(t, "/api/admin/peer/update", `{"password":"secret-truncated","note":"`+strings.Repeat("界", 1000)+`"}`, nil)
	if strings.Contains(record.Detail, "secret-") || !utf8.ValidString(record.Detail) || !strings.HasSuffix(record.Detail, "...[truncated]") {
		t.Fatalf("unsafe truncation: %s", record.Detail)
	}
}
