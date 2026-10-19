package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/global"
	request "github.com/lejianwen/rustdesk-api/v2/http/request/api"
	"github.com/lejianwen/rustdesk-api/v2/http/response"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientaudit"
	"github.com/lejianwen/rustdesk-api/v2/service"
)

type Audit struct{}

// A successful audit post has no response body. Only a confirmed transaction
// or a known duplicate can acknowledge storage.
func auditStored(c *gin.Context) { c.Status(http.StatusOK) }

func auditFailed(c *gin.Context, err error) {
	status, message := http.StatusServiceUnavailable, "Audit storage unavailable"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		status, message = http.StatusGatewayTimeout, "Audit storage timed out"
	case errors.Is(err, context.Canceled):
		status, message = http.StatusRequestTimeout, "Audit request canceled"
	case errors.Is(err, clientaudit.ErrInvalidInput):
		status, message = http.StatusBadRequest, "Invalid audit request"
	case errors.Is(err, clientaudit.ErrConflict):
		status, message = http.StatusConflict, "Audit nonce conflict"
	}
	c.JSON(status, response.ErrorResponse{Error: message})
}

func auditContext(c *gin.Context) (context.Context, context.CancelFunc) {
	budget := global.Config.App.AuthTimeout
	if budget <= 0 {
		budget = 5 * time.Second
	}
	return context.WithTimeout(c.Request.Context(), budget)
}

func decodeAudit(c *gin.Context, value interface{}) bool {
	if c.Request.Body == nil {
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	decoder := json.NewDecoder(c.Request.Body)
	if decoder.Decode(value) != nil {
		return false
	}
	var trailing interface{}
	return decoder.Decode(&trailing) == io.EOF
}

// AuditConn stores open/auth/close events; the old note-only payload is not an
// audit event and cannot receive a successful storage acknowledgement here.
func (a *Audit) AuditConn(c *gin.Context) {
	form := &request.AuditConnForm{}
	if !decodeAudit(c, form) || form.Note != nil || len(form.Peer) > 2 {
		auditFailed(c, clientaudit.ErrInvalidInput)
		return
	}
	if service.AllService == nil {
		auditFailed(c, clientaudit.ErrUnavailable)
		return
	}
	ctx, cancel := auditContext(c)
	defer cancel()
	if err := service.AllService.AuditService.ReceiveConn(ctx, form.ToAuditConn()); err != nil {
		auditFailed(c, err)
		return
	}
	auditStored(c)
}

func (a *Audit) AuditFile(c *gin.Context) {
	form := &request.AuditFileForm{}
	if !decodeAudit(c, form) {
		auditFailed(c, clientaudit.ErrInvalidInput)
		return
	}
	// Validate before the legacy filename adapter reads the nested info. It
	// must not log decoder errors or turn malformed JSON into a stored success.
	var info request.AuditFileInfo
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(form.Info), &object) != nil || object == nil || json.Unmarshal([]byte(form.Info), &info) != nil {
		auditFailed(c, clientaudit.ErrInvalidInput)
		return
	}
	if service.AllService == nil {
		auditFailed(c, clientaudit.ErrUnavailable)
		return
	}
	ctx, cancel := auditContext(c)
	defer cancel()
	if err := service.AllService.AuditService.ReceiveFile(ctx, form.ToAuditFile(), clientaudit.FileSource{Info: form.Info, IsFile: form.IsFile}); err != nil {
		auditFailed(c, err)
		return
	}
	auditStored(c)
}
