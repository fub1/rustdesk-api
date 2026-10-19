package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
)

// BackendUserAuth delegates to the instance-owned authentication adapter.
func BackendUserAuth(sessions *httpapi.Handler) gin.HandlerFunc { return sessions.AdminAuth() }
