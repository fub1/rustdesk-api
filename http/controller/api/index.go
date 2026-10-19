package api

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	requstform "github.com/lejianwen/rustdesk-api/v2/http/request/api"
	"github.com/lejianwen/rustdesk-api/v2/http/response"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientstrategy"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"net/http"
	"time"
)

type Index struct {
}

// Index 首页
// @Tags 首页
// @Summary 首页
// @Description 首页
// @Accept  json
// @Produce  json
// @Success 200 {object} response.Response
// @Failure 500 {object} response.Response
// @Router / [get]
func (i *Index) Index(c *gin.Context) {
	response.Success(
		c,
		"Hello Gwen",
	)
}

// Heartbeat 心跳
// @Tags 首页
// @Summary 心跳
// @Description 心跳
// @Accept  json
// @Produce  json
// @Success 200 {object} nil
// @Failure 500 {object} response.Response
// @Router /heartbeat [post]
func (i *Index) Heartbeat(c *gin.Context) {
	info := &requstform.PeerInfoInHeartbeat{}
	err := c.ShouldBindJSON(info)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{})
		return
	}
	if info.Uuid == "" {
		c.JSON(http.StatusOK, gin.H{})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	strategy, err := service.AllService.PeerStrategyService.HeartbeatStrategy(ctx, info.Id, info.Uuid, info.ModifiedAt)
	if errors.Is(err, clientstrategy.ErrNotFound) {
		c.JSON(http.StatusOK, gin.H{})
		return
	}
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		} else if errors.Is(err, context.Canceled) {
			status = http.StatusRequestTimeout
		}
		c.JSON(status, gin.H{"error": "Strategy unavailable"})
		return
	}
	peer := strategy.Peer
	//如果在20s以内则不更新
	if time.Now().Unix()-peer.LastOnlineTime >= 20 {
		if err := service.AllService.PeerService.BufferHeartbeat(peer.RowId, time.Now().Unix(), c.ClientIP()); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Heartbeat unavailable"})
			return
		}
	}
	// 构建心跳响应，检查是否有策略配置需要下发给被控端
	// 对应 RustDesk 客户端 sync.rs 中心跳响应的 strategy 字段
	resp := gin.H{}
	if strategy.Changed {
		resp["strategy"] = gin.H{
			"config_options": strategy.ConfigOptions,
		}
		resp["modified_at"] = strategy.Revision
	}
	c.JSON(http.StatusOK, resp)
}

// Version 版本
// @Tags 首页
// @Summary 版本
// @Description 版本
// @Accept  json
// @Produce  json
// @Success 200 {object} response.Response
// @Failure 500 {object} response.Response
// @Router /version [get]
func (i *Index) Version(c *gin.Context) {
	//读取resources/version文件
	v := service.AllService.AppService.GetAppVersion()
	response.Success(
		c,
		v,
	)
}
