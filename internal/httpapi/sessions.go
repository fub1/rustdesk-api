// Package httpapi adapts instance-owned authentication to the two existing
// RustDesk HTTP contracts. It does not read application/service globals.
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	apiResponse "github.com/lejianwen/rustdesk-api/v2/http/response/api"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientassign"
	"github.com/lejianwen/rustdesk-api/v2/internal/oidclogin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
)

const principalKey = "rustdesk.auth.principal"

type Translate func(language, message string) string

// FailureEvent deliberately carries no credential, request data or cause text.
type FailureEvent struct{ Operation, Code, Route string }
type Options struct {
	Translate Translate
	Timeout   time.Duration
	Observe   func(FailureEvent)
	OIDC      *oidclogin.Service
	Assign    *clientassign.Store
}

const DefaultTimeout = 5 * time.Second

type Handler struct {
	sessions                *auth.Service
	oidc                    *oidclogin.Service
	assign                  *clientassign.Store
	translate               Translate
	userRoutes, adminRoutes []string
	timeout                 time.Duration
	observe                 func(FailureEvent)
}

func New(sessions *auth.Service, options Options) (*Handler, error) {
	if sessions == nil || options.Translate == nil {
		return nil, errors.New("session service and translator are required")
	}
	if options.Timeout < 0 {
		return nil, errors.New("authentication timeout must not be negative")
	}
	if options.Timeout == 0 {
		options.Timeout = DefaultTimeout
	}
	return &Handler{sessions: sessions, oidc: options.OIDC, assign: options.Assign, translate: options.Translate, timeout: options.Timeout, observe: options.Observe, userRoutes: append([]string(nil), model.UserRouteNames...), adminRoutes: append([]string(nil), model.AdminRouteNames...)}, nil
}

func Current(c *gin.Context) (auth.Principal, bool) {
	p, ok := c.Get(principalKey)
	if !ok {
		return auth.Principal{}, false
	}
	principal, ok := p.(auth.Principal)
	return principal, ok
}

func (h *Handler) ClientAuth() gin.HandlerFunc        { return h.authenticate(false, false) }
func (h *Handler) AdminAuth() gin.HandlerFunc         { return h.authenticate(true, false) }
func (h *Handler) OptionalAdminAuth() gin.HandlerFunc { return h.authenticate(true, true) }

func requestToken(c *gin.Context, admin bool) string {
	name := "Authorization"
	if admin {
		name = "api-token"
	}
	values := c.Request.Header.Values(name)
	if len(values) != 1 {
		return ""
	}
	value := values[0]
	if len(value) > 8192 {
		return ""
	}
	if admin {
		if value == "" || strings.ContainsAny(value, " \t\r\n,") {
			return ""
		}
		return value
	}
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.Contains(parts[1], ",") {
		return ""
	}
	return parts[1]
}

func (h *Handler) authenticate(admin, optional bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := requestToken(c, admin)
		ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
		p, err := h.sessions.Authenticate(ctx, token)
		cancel()
		if err != nil {
			if optional && (errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrDisabled)) {
				c.Next()
				return
			}
			h.fail(c, admin, err)
			return
		}
		c.Set(principalKey, p)
		// This is the explicit compatibility boundary for legacy controllers.
		c.Set("curUser", legacyUser(p.User))
		c.Set("token", token)
		c.Next()
	}
}

func (h *Handler) RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := Current(c)
		if !ok {
			h.fail(c, true, auth.ErrUnauthenticated)
			return
		}
		if err := auth.RequireAdministrator(p); err != nil {
			h.failOperation(c, true, err, "authorize")
			return
		}
		c.Next()
	}
}

func (h *Handler) fail(c *gin.Context, admin bool, err error) {
	h.failOperation(c, admin, err, "authenticate")
}

func (h *Handler) failOperation(c *gin.Context, admin bool, err error, operation string) {
	status, code, message := http.StatusServiceUnavailable, 503, "Service unavailable"
	result := "unavailable"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		result = "deadline"
		status, code, message = 504, 504, "Authentication timed out"
	case errors.Is(err, context.Canceled):
		result = "canceled"
		status, code, message = 408, 408, "Request canceled"
	case errors.Is(err, auth.ErrForbidden):
		result = "forbidden"
		status, code, message = 200, 403, h.translate(c.GetHeader("Accept-Language"), "NoAccess")
	case errors.Is(err, auth.ErrInvalidInput):
		result = "invalid_input"
		status, code, message = 200, 101, h.translate(c.GetHeader("Accept-Language"), "ParamsError")
	case errors.Is(err, auth.ErrDisabled):
		result = "disabled"
		status, code, message = 401, 401, "Unauthorized"
	case errors.Is(err, auth.ErrUnauthenticated):
		result = "unauthenticated"
		status, code, message = 401, 403, "Unauthorized"
		if admin {
			status = 200
			message = h.translate(c.GetHeader("Accept-Language"), "NeedLogin")
		}
	}
	if h.observe != nil {
		route := c.FullPath()
		if route == "" {
			route = "<unmatched>"
		}
		h.observe(FailureEvent{Operation: operation, Code: result, Route: route})
	}
	if admin {
		c.AbortWithStatusJSON(status, gin.H{"code": code, "message": message, "data": nil})
	} else {
		c.AbortWithStatusJSON(status, gin.H{"error": message})
	}
}

func legacyUser(u auth.User) *model.User {
	admin := u.IsAdmin
	status := model.COMMON_STATUS_DISABLED
	if u.Enabled {
		status = model.COMMON_STATUS_ENABLE
	}
	return &model.User{
		IdModel: model.IdModel{Id: u.ID}, Username: u.Username, Email: u.Email, Nickname: u.Nickname, Avatar: u.Avatar,
		GroupId: u.GroupID, IsAdmin: &admin, Status: status, Password: u.PasswordHash, Remark: u.Remark,
		TimeModel: model.TimeModel{CreatedAt: custom_types.AutoTime(u.CreatedAt), UpdatedAt: custom_types.AutoTime(u.UpdatedAt)},
	}
}

func (h *Handler) ClientCurrent(c *gin.Context) {
	p, ok := Current(c)
	if !ok {
		h.fail(c, false, auth.ErrUnauthenticated)
		return
	}
	c.JSON(http.StatusOK, (&apiResponse.UserPayload{}).FromUser(legacyUser(p.User)))
}

func (h *Handler) AdminCurrent(c *gin.Context) {
	p, ok := Current(c)
	if !ok {
		h.fail(c, true, auth.ErrUnauthenticated)
		return
	}
	routes := append([]string(nil), h.userRoutes...)
	if p.User.IsAdmin {
		routes = append([]string(nil), h.adminRoutes...)
	}
	c.JSON(200, gin.H{"code": 0, "message": "success", "data": gin.H{"username": p.User.Username, "email": p.User.Email, "avatar": p.User.Avatar, "nickname": p.User.Nickname, "token": p.Session.Token, "route_names": routes}})
}

func (h *Handler) ClientLogout(c *gin.Context) { h.logout(c, false) }
func (h *Handler) AdminLogout(c *gin.Context)  { h.logout(c, true) }
func (h *Handler) logout(c *gin.Context, admin bool) {
	p, ok := Current(c)
	if !ok {
		h.fail(c, admin, auth.ErrUnauthenticated)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	if err := h.sessions.Revoke(ctx, p, p.Session.Token); err != nil {
		h.failOperation(c, admin, err, "revoke")
		return
	}
	if admin {
		c.JSON(200, gin.H{"code": 0, "message": "success", "data": nil})
	} else {
		c.JSON(200, nil)
	}
}

func (h *Handler) DeleteSession(c *gin.Context) {
	var f struct {
		ID uint `json:"id"`
	}
	if err := c.ShouldBindJSON(&f); err != nil || f.ID == 0 {
		h.fail(c, true, auth.ErrInvalidInput)
		return
	}
	h.revokeIDs(c, []uint{f.ID})
}
func (h *Handler) DeleteSessions(c *gin.Context) {
	var f struct {
		IDs []uint `json:"ids"`
	}
	if err := c.ShouldBindJSON(&f); err != nil || len(f.IDs) == 0 {
		h.fail(c, true, auth.ErrInvalidInput)
		return
	}
	h.revokeIDs(c, f.IDs)
}
func (h *Handler) revokeIDs(c *gin.Context, ids []uint) {
	p, ok := Current(c)
	if !ok {
		h.fail(c, true, auth.ErrUnauthenticated)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	if err := h.sessions.RevokeSessions(ctx, p, ids); err != nil {
		h.failOperation(c, true, err, "revoke_sessions")
		return
	}
	c.JSON(200, gin.H{"code": 0, "message": "success", "data": nil})
}
