package middleware

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCommittedActionLogIsBoundedTrackedAndSurvivesClientCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called, failed := false, false
	router := gin.New()
	router.Use(operationLogContext(func(ctx context.Context, record *model.OperationLog) error {
		called = true
		if ctx.Err() != nil {
			t.Error("client cancellation prevented committed action log")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("log has no bounded deadline")
		}
		return errors.New("private SQL cause")
	}, func() { failed = true }))
	router.POST("/api/admin/group/create", func(c *gin.Context) {
		c.Set("curUser", &model.User{IdModel: model.IdModel{Id: 1}})
		cancel()
		c.Status(204)
	})
	req := httptest.NewRequest("POST", "/api/admin/group/create", strings.NewReader(`{"name":"sample"}`)).WithContext(ctx)
	router.ServeHTTP(httptest.NewRecorder(), req)
	if !called || !failed {
		t.Fatal("handler returned before synchronous log/failure observation")
	}
}
