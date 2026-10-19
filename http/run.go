package http

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/lifecycle"
)

func Run(g *gin.Engine, addr string) error {
	return RunContext(context.Background(), g, addr, nil, lifecycle.Options{})
}

func RunContext(ctx context.Context, g *gin.Engine, addr string, workers []lifecycle.Worker, options lifecycle.Options) error {
	return lifecycle.Run(ctx, addr, g, workers, options)
}
