package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
)

// AdminPrivilege delegates to the instance-owned authentication adapter.
func AdminPrivilege(sessions *httpapi.Handler) gin.HandlerFunc { return sessions.RequireAdmin() }
