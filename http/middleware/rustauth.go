package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
)

// RustAuth delegates to the instance-owned authentication adapter.
func RustAuth(sessions *httpapi.Handler) gin.HandlerFunc { return sessions.ClientAuth() }
