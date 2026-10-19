package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
)

// JwtAuth delegates to the instance-owned authentication adapter.
func JwtAuth(sessions *httpapi.Handler) gin.HandlerFunc { return sessions.AdminAuth() }
