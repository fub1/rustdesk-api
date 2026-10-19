package middleware

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

type credentialPanic struct{}

func (credentialPanic) Error() string { panic("panic values must never be formatted") }

func TestRecoveryDoesNotDumpRequestOrPanicCredentials(t *testing.T) {
	var output bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&output)
	logger.SetLevel(logrus.DebugLevel)
	logger.SetFormatter(&logrus.JSONFormatter{})
	for _, value := range []interface{}{"private-panic-value", credentialPanic{}} {
		g := gin.New()
		g.Use(requestLogger(logger), Recovery(logger))
		g.POST("/callback/:provider", func(c *gin.Context) { panic(value) })
		r := httptest.NewRequest("POST", "/callback/private-provider?code=private-code&state=private-state", strings.NewReader(`{"password":"private-password"}`))
		r.Header.Set("Authorization", "Bearer private-token")
		r.Header.Set("api-token", "private-admin-token")
		r.Header.Set("Cookie", "private-cookie")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != 500 || strings.Contains(output.String()+w.Body.String(), "private-") {
			t.Fatalf("unsafe recovery result: status=%d logs=%s body=%s", w.Code, output.String(), w.Body.String())
		}
	}
	if !strings.Contains(output.String(), "HTTP handler panic") || !strings.Contains(output.String(), "/callback/:provider") {
		t.Fatalf("safe panic metadata missing: %s", output.String())
	}
}
