package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/sirupsen/logrus"
)

// Logger 日志中间件
func Logger() gin.HandlerFunc {
	return requestLogger(global.Logger)
}

func requestLogger(logger *logrus.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		if route == "" {
			route = "<unmatched>"
		}
		logger.WithFields(
			logrus.Fields{
				"route":  route,
				"ip":     c.ClientIP(),
				"method": c.Request.Method,
			}).Debug("Request")
		c.Next()
	}
}
