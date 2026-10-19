//go:build !windows

package http

import (
	"github.com/fvbock/endless"
	"github.com/gin-gonic/gin"
)

func Run(g *gin.Engine, addr string) error {
	return endless.ListenAndServe(addr, g)
}
