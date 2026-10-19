package clientaudit_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	apiController "github.com/lejianwen/rustdesk-api/v2/http/controller/api"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"gorm.io/gorm"
)

func auditHTTP(t *testing.T) (*gorm.DB, http.Handler) {
	t.Helper()
	db := auditDB(t)
	previous := service.AllService
	service.AllService = &service.Service{AuditService: service.NewAuditService(db)}
	t.Cleanup(func() { service.AllService = previous })
	g := gin.New()
	controller := &apiController.Audit{}
	g.POST("/api/audit/conn", controller.AuditConn)
	g.POST("/api/audit/file", controller.AuditFile)
	return db, g
}

func postAudit(handler http.Handler, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestAuditHTTPPreservesFullUint64AndEmptyACK(t *testing.T) {
	db, handler := auditHTTP(t)
	for i, session := range []string{"0", "9007199254740993", "9223372036854775808", "18446744073709551615"} {
		body := fmt.Sprintf(`{"action":"new","id":"device","uuid":"uuid","conn_id":%d,"session_id":%s,"nonce":"open-%d"}`, i+1, session, i)
		for retry := 0; retry < 2; retry++ {
			response := postAudit(handler, "/api/audit/conn", body)
			if response.Code != 200 || response.Body.Len() != 0 {
				t.Fatalf("audit ACK status=%d body=%q", response.Code, response.Body.String())
			}
		}
		var row model.AuditConn
		if err := db.Where("conn_id = ?", i+1).First(&row).Error; err != nil {
			t.Fatal(err)
		}
		if row.SessionId != session {
			t.Fatalf("uint64 lost precision: got %s want %s", row.SessionId, session)
		}
	}
	if count(t, db, &model.AuditConn{}) != 4 {
		t.Fatal("retry duplicated audit records")
	}
}

func TestAuditHTTPInvalidInputsNeverReceiveSuccessfulACK(t *testing.T) {
	db, handler := auditHTTP(t)
	for _, session := range []string{"-1", "1.5", "18446744073709551616", `"18446744073709551615"`} {
		body := `{"action":"new","id":"device","uuid":"uuid","conn_id":1,"session_id":` + session + `}`
		response := postAudit(handler, "/api/audit/conn", body)
		if response.Code != 400 || response.Body.Len() == 0 {
			t.Fatalf("invalid session accepted: %d %q", response.Code, response.Body.String())
		}
	}
	for _, body := range []string{
		`{"id":"device","session_id":9,"note":"legacy private note"}`,
		`{"action":"unknown","id":"device","uuid":"uuid","conn_id":1,"session_id":0}`,
		`{"action":"new","id":"device","uuid":"uuid","conn_id":1,"session_id":0} {}`,
	} {
		response := postAudit(handler, "/api/audit/conn", body)
		if response.Code != 400 || response.Body.Len() == 0 || strings.Contains(response.Body.String(), "private") {
			t.Fatalf("invalid shape accepted or reflected: %d %q", response.Code, response.Body.String())
		}
	}
	if count(t, db, &model.AuditConn{}) != 0 {
		t.Fatal("invalid request wrote audit")
	}
}

func TestAuditHTTPMissingConnectionAndDatabaseFailureStayRetryable(t *testing.T) {
	db, handler := auditHTTP(t)
	patch := `{"id":"device","uuid":"uuid","conn_id":1,"session_id":18446744073709551615,"type":0,"nonce":"auth"}`
	response := postAudit(handler, "/api/audit/conn", patch)
	if response.Code != 503 || response.Body.Len() == 0 {
		t.Fatal("missing open falsely acknowledged")
	}
	open := `{"action":"new","id":"device","uuid":"uuid","conn_id":1,"session_id":0,"nonce":"open"}`
	exec(t, db, "CREATE TRIGGER audit_fault BEFORE INSERT ON audit_conns BEGIN SELECT RAISE(ABORT, 'private-sql-body-secret'); END")
	response = postAudit(handler, "/api/audit/conn", open)
	if response.Code != 503 || strings.Contains(response.Body.String(), "private") {
		t.Fatalf("database failure leaked or ACKed: %d %s", response.Code, response.Body.String())
	}
	exec(t, db, "DROP TRIGGER audit_fault")
	response = postAudit(handler, "/api/audit/conn", open)
	if response.Code != 200 || response.Body.Len() != 0 {
		t.Fatal("failed write burned nonce")
	}
	response = postAudit(handler, "/api/audit/conn", patch)
	if response.Code != 200 || response.Body.Len() != 0 {
		t.Fatal("patch retry not stored")
	}
	exec(t, db, "DROP TABLE audit_conns")
	patch = strings.Replace(patch, `"nonce":"auth"`, `"nonce":"auth-after-drop"`, 1)
	response = postAudit(handler, "/api/audit/conn", patch)
	if response.Code != 503 || strings.Contains(response.Body.String(), "SELECT") || strings.Contains(response.Body.String(), "audit_conns") {
		t.Fatal("query failure acknowledged or leaked SQL")
	}
}

func TestAuditHTTPFileRetryAndConflict(t *testing.T) {
	db, handler := auditHTTP(t)
	body := `{"id":"device","uuid":"uuid","peer_id":"controller","conn_id":1,"type":0,"path":"C:\\file.txt","is_file":true,"info":"{\"ip\":\"192.0.2.1\",\"name\":\"Controller\",\"num\":1,\"files\":[[\"\",12]]}","nonce":"file"}`
	for retry := 0; retry < 2; retry++ {
		response := postAudit(handler, "/api/audit/file", body)
		if response.Code != 200 || response.Body.Len() != 0 {
			t.Fatalf("file ACK: %d %s", response.Code, response.Body.String())
		}
	}
	if count(t, db, &model.AuditFile{}) != 1 {
		t.Fatal("file retry duplicated")
	}
	changed := strings.Replace(body, "Controller", "Other", 1)
	response := postAudit(handler, "/api/audit/file", changed)
	if response.Code != 409 || response.Body.Len() == 0 {
		t.Fatalf("same nonce different file event: %d %s", response.Code, response.Body.String())
	}
	response = postAudit(handler, "/api/audit/file", `{"id":"device","uuid":"uuid","conn_id":1,"info":"private-invalid-json"}`)
	if response.Code != 400 || strings.Contains(response.Body.String(), "private") {
		t.Fatal("malformed info accepted or reflected")
	}
}

func TestAuditHTTPCommitFailureDoesNotACKOrBurnNonce(t *testing.T) {
	db, handler := auditHTTP(t)
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	exec(t, db, "PRAGMA foreign_keys = ON")
	exec(t, db, "CREATE TABLE commit_parent (id INTEGER PRIMARY KEY)")
	exec(t, db, "CREATE TABLE commit_guard (parent_id INTEGER REFERENCES commit_parent(id) DEFERRABLE INITIALLY DEFERRED)")
	exec(t, db, "CREATE TRIGGER fail_commit AFTER INSERT ON audit_conns BEGIN INSERT INTO commit_guard(parent_id) VALUES(99999); END")
	body := `{"action":"new","id":"device","uuid":"uuid","conn_id":1,"session_id":0,"nonce":"retry-after-commit-failure"}`
	response := postAudit(handler, "/api/audit/conn", body)
	if response.Code != 503 || response.Body.Len() == 0 || count(t, db, &model.AuditConn{}) != 0 {
		t.Fatal("commit failure acknowledged or persisted")
	}
	if count(t, db, &model.AuditDeliveryReceipt{}) != 0 {
		t.Fatal("failed COMMIT burned delivery receipt")
	}
	exec(t, db, "DROP TRIGGER fail_commit")
	response = postAudit(handler, "/api/audit/conn", body)
	if response.Code != 200 || response.Body.Len() != 0 || count(t, db, &model.AuditConn{}) != 1 {
		t.Fatal("commit failure burned retry nonce")
	}
	if count(t, db, &model.AuditDeliveryReceipt{}) != 1 {
		t.Fatal("successful retry has no committed receipt")
	}
}

func TestAuditHTTPReceiptPreservesOriginalFileSemantics(t *testing.T) {
	db, handler := auditHTTP(t)
	body := `{"id":"device","uuid":"uuid","peer_id":"controller","conn_id":1,"type":0,"path":"file.txt","is_file":true,"info":"{\"num\":1,\"files\":[[\"\",9007199254740993]]}","nonce":"file-original"}`
	response := postAudit(handler, "/api/audit/file", body)
	if response.Code != 200 || response.Body.Len() != 0 {
		t.Fatalf("original file: %d %s", response.Code, response.Body.String())
	}
	var file model.AuditFile
	if err := db.First(&file).Error; err != nil {
		t.Fatal(err)
	}
	if file.IsFile || !strings.Contains(file.Info, "9007199254740993") || !strings.Contains(file.Info, "file.txt") {
		t.Fatal("filename adapter changed a large integer or was skipped")
	}
	// The adapter makes both of these storage flags false. Their wire values
	// still differ and must not be accepted as the same original request.
	response = postAudit(handler, "/api/audit/file", strings.Replace(body, `"is_file":true`, `"is_file":false`, 1))
	if response.Code != 409 {
		t.Fatalf("original is_file was not fingerprinted: %d %s", response.Code, response.Body.String())
	}
	response = postAudit(handler, "/api/audit/file", strings.Replace(body, "9007199254740993", "9007199254740992", 1))
	if response.Code != 409 {
		t.Fatal("large integers collapsed in fingerprint")
	}
	// JSON whitespace and object-key order do not change request semantics.
	reordered := strings.Replace(body, `{\"num\":1,\"files\":[[\"\",9007199254740993]]}`, `{ \"files\" : [[\"\",9007199254740993]], \"num\" : 1 }`, 1)
	response = postAudit(handler, "/api/audit/file", reordered)
	if response.Code != 200 || response.Body.Len() != 0 {
		t.Fatalf("normalized original Info differed: %d %s", response.Code, response.Body.String())
	}
	exec(t, db, "DELETE FROM audit_files")
	response = postAudit(handler, "/api/audit/file", body)
	if response.Code != 200 || response.Body.Len() != 0 || count(t, db, &model.AuditFile{}) != 0 {
		t.Fatal("receipt replay recreated deleted file audit")
	}
}

func TestAuditHTTPCrossActionNonceIsConflictAndFailureDoesNotBurnReceipt(t *testing.T) {
	db, handler := auditHTTP(t)
	open := `{"action":"new","id":"device","uuid":"uuid","conn_id":1,"session_id":0,"nonce":"shared"}`
	if response := postAudit(handler, "/api/audit/conn", open); response.Code != 200 || response.Body.Len() != 0 {
		t.Fatal(response.Body.String())
	}
	close := strings.Replace(open, `"action":"new"`, `"action":"close"`, 1)
	response := postAudit(handler, "/api/audit/conn", close)
	if response.Code != 409 || response.Body.Len() == 0 {
		t.Fatal("nonce reused across new/close was silently acknowledged")
	}
	auth := `{"id":"device","uuid":"uuid","conn_id":1,"session_id":18446744073709551615,"nonce":"auth"}`
	exec(t, db, "CREATE TRIGGER update_fault BEFORE UPDATE ON audit_conns BEGIN SELECT RAISE(ABORT, 'private-update-fault'); END")
	response = postAudit(handler, "/api/audit/conn", auth)
	if response.Code != 503 || strings.Contains(response.Body.String(), "private") || count(t, db, &model.AuditDeliveryReceipt{}) != 1 {
		t.Fatal("failed auth update acknowledged or burned its nonce")
	}
	exec(t, db, "DROP TRIGGER update_fault")
	response = postAudit(handler, "/api/audit/conn", auth)
	if response.Code != 200 || response.Body.Len() != 0 || count(t, db, &model.AuditDeliveryReceipt{}) != 2 {
		t.Fatal("auth retry not committed atomically")
	}
}

func TestAuditHTTPPropagatesInflightDeadline(t *testing.T) {
	db, handler := auditHTTP(t)
	if err := db.Callback().Create().Before("gorm:create").Register("audit-deadline", func(tx *gorm.DB) { <-tx.Statement.Context.Done() }); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Create().Remove("audit-deadline")
	request := httptest.NewRequest(http.MethodPost, "/api/audit/conn", strings.NewReader(`{"action":"new","id":"device","uuid":"uuid","conn_id":1,"session_id":0}`))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request.WithContext(ctx))
	if response.Code != 504 || response.Body.Len() == 0 || count(t, db, &model.AuditConn{}) != 0 {
		t.Fatalf("deadline ACK or write: %d %s", response.Code, response.Body.String())
	}
}
