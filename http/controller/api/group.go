package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/global"
	apiReq "github.com/lejianwen/rustdesk-api/v2/http/request/api"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/service"
)

type Group struct{}

// clientOperation is the explicit legacy dependency boundary. Selected use
// cases receive their own DB, actor value and bounded request context.
func clientOperation(c *gin.Context) (*clientcompat.Store, clientcompat.Actor, context.Context, context.CancelFunc, bool) {
	u := service.AllService.UserService.CurUser(c)
	if u == nil || u.Id == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return nil, clientcompat.Actor{}, nil, nil, false
	}
	store, err := clientcompat.New(service.DB)
	if err != nil {
		clientFailure(c, err)
		return nil, clientcompat.Actor{}, nil, nil, false
	}
	budget := global.Config.App.AuthTimeout
	if budget <= 0 {
		budget = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), budget)
	return store, clientcompat.Actor{ID: u.Id}, ctx, cancel, true
}
func clientFailure(c *gin.Context, err error) {
	status, message := http.StatusServiceUnavailable, "Service unavailable"
	switch {
	case errors.Is(err, context.Canceled):
		status, message = 408, "Request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		status, message = 504, "Request timed out"
	case errors.Is(err, clientcompat.ErrInput):
		status, message = 400, "ParamsError"
	case errors.Is(err, clientcompat.ErrNotFound):
		status, message = 400, "ItemNotFound"
	case errors.Is(err, clientcompat.ErrForbidden):
		status, message = 400, "NoAccess"
	}
	c.JSON(status, gin.H{"error": message})
}
func clientBody(c *gin.Context, value any, strict bool) bool {
	if c.Request.Body == nil {
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<20)
	decoder := json.NewDecoder(c.Request.Body)
	if strict {
		decoder.DisallowUnknownFields()
	}
	if decoder.Decode(value) != nil {
		return false
	}
	var extra any
	return decoder.Decode(&extra) == io.EOF
}
func groupPage(c *gin.Context) (clientcompat.Page, bool, bool) {
	query := c.Request.URL.Query()
	page, err := apiReq.ClientPage(query)
	if err != nil || len(query["status"]) > 1 || len(query["accessible"]) > 1 {
		clientFailure(c, clientcompat.ErrInput)
		return page, false, false
	}
	status := query.Get("status")
	if status != "" && status != "0" && status != "1" {
		clientFailure(c, clientcompat.ErrInput)
		return page, false, false
	}
	return page, status == "1", true
}
func (g *Group) Users(c *gin.Context) {
	page, enabled, ok := groupPage(c)
	if !ok {
		return
	}
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	total, rows, err := store.Users(ctx, actor, page, enabled)
	if err != nil {
		clientFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "data": rows})
}
func (g *Group) Peers(c *gin.Context) {
	page, enabled, ok := groupPage(c)
	if !ok {
		return
	}
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	total, rows, err := store.GroupPeers(ctx, actor, page, enabled)
	if err != nil {
		clientFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "data": rows})
}
func (g *Group) Device(c *gin.Context) {
	page, err := apiReq.ClientPage(c.Request.URL.Query())
	if err != nil {
		clientFailure(c, err)
		return
	}
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	total, rows, err := store.DeviceGroups(ctx, actor, page)
	if err != nil {
		clientFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "data": rows})
}
