package middleware

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// Recovery reports only static metadata. A panic's value, request headers,
// query, body and cookies never go through a formatter or request dump.
func Recovery(logger *logrus.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if value := recover(); value != nil {
				route := c.FullPath()
				if route == "" {
					route = "<unmatched>"
				}
				logger.WithFields(logrus.Fields{"route": route, "method": c.Request.Method, "panic_type": fmt.Sprintf("%T", value)}).Error("HTTP handler panic")
				c.AbortWithStatus(500)
			}
		}()
		c.Next()
	}
}
