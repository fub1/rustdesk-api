package api

import (
	"context"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientsysinfo"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"net/http"
	"time"
)

type Peer struct {
}

// SysInfo
// @Tags System
// @Summary 提交系统信息
// @Description 提交系统信息
// @Accept  json
// @Produce  json
// @Param body body clientsysinfo.Input true "系统信息表单"
// @Success 200 {string} string "SYSINFO_UPDATED,ID_NOT_FOUND"
// @Failure 500 {object} response.ErrorResponse
// @Router /sysinfo [post]
func (p *Peer) SysInfo(c *gin.Context) {
	var input clientsysinfo.Input
	if !clientBody(c, &input, false) {
		clientFailure(c, clientcompat.ErrInput)
		return
	}
	store, err := clientsysinfo.New(service.DB)
	if err != nil {
		clientFailure(c, err)
		return
	}
	budget := global.Config.App.AuthTimeout
	if budget <= 0 {
		budget = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), budget)
	defer cancel()
	if err := store.Report(ctx, input); err != nil {
		if errors.Is(err, clientsysinfo.ErrManagementPreset) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Management presets require authenticated administrator assignment"})
			return
		}
		if errors.Is(err, clientsysinfo.ErrInput) {
			err = clientcompat.ErrInput
		}
		clientFailure(c, err)
		return
	}
	c.String(http.StatusOK, "SYSINFO_UPDATED")
}

// SysInfoVer
// @Tags System
// @Summary 获取系统版本信息
// @Description 获取系统版本信息
// @Accept  json
// @Produce  json
// @Success 200 {string} string ""
// @Failure 500 {object} response.ErrorResponse
// @Router /sysinfo_ver [post]
func (p *Peer) SysInfoVer(c *gin.Context) {
	//读取resources/version文件
	v := service.AllService.AppService.GetAppVersion()
	// 加上启动时间，方便client上传信息
	v = fmt.Sprintf("%s\n%s", v, service.AllService.AppService.GetStartTime())
	c.String(http.StatusOK, v)
}
