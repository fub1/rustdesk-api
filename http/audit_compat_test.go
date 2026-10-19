package http

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/model"
)

func TestProductionAuditRoutesUseAssembledTransactionalWriter(t *testing.T) {
	sessions := installLegacyFixture(t)
	if err := global.DB.AutoMigrate(&model.AuditConn{}, &model.AuditFile{}, &model.AuditDeliveryReceipt{}); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(sessions)
	if err != nil {
		t.Fatal(err)
	}
	post := func(path, body string, want int) {
		t.Helper()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != want || (want == 200 && response.Body.Len() != 0) {
			t.Fatalf("%s: %d %q", path, response.Code, response.Body.String())
		}
	}
	conn := `{"action":"new","id":"device","uuid":"uuid","conn_id":1,"session_id":18446744073709551615,"nonce":"open"}`
	file := `{"id":"device","uuid":"uuid","conn_id":1,"nonce":"file","info":"{}","type":0}`
	for retry := 0; retry < 2; retry++ {
		post("/api/audit/conn", conn, 200)
		post("/api/audit/file", file, 200)
	}
	post("/api/audit/conn", strings.Replace(conn, `"conn_id":1`, `"conn_id":2`, 1), 409)
	var row model.AuditConn
	if err := global.DB.First(&row).Error; err != nil || row.SessionId != "18446744073709551615" {
		t.Fatalf("production uint64 persistence: %+v / %v", row, err)
	}
	for _, tc := range []struct {
		model any
		want  int64
	}{{&model.AuditConn{}, 1}, {&model.AuditFile{}, 1}, {&model.AuditDeliveryReceipt{}, 2}} {
		var got int64
		if err := global.DB.Model(tc.model).Count(&got).Error; err != nil || got != tc.want {
			t.Fatalf("production rows %T = %d, want %d: %v", tc.model, got, tc.want, err)
		}
	}
	if err := global.DB.Migrator().DropTable(&model.AuditDeliveryReceipt{}); err != nil {
		t.Fatal(err)
	}
	post("/api/audit/conn", strings.Replace(conn, `"open"`, `"new"`, 1), 503)
}
