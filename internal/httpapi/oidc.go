package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	adminResponse "github.com/lejianwen/rustdesk-api/v2/http/response/admin"
	apiResponse "github.com/lejianwen/rustdesk-api/v2/http/response/api"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/loginattempt"
	"github.com/lejianwen/rustdesk-api/v2/utils"
)

const maxOIDCBody = 64 << 10

type oidcBeginRequest struct {
	Op         string `json:"op"`
	ID         string `json:"id"`
	UUID       string `json:"uuid"`
	DeviceInfo struct {
		Name string `json:"name"`
		OS   string `json:"os"`
		Type string `json:"type"`
	} `json:"deviceInfo"`
}

func (h *Handler) ClientOIDCBegin(c *gin.Context) { h.oidcBegin(c, false, false) }
func (h *Handler) AdminOIDCBegin(c *gin.Context)  { h.oidcBegin(c, true, false) }
func (h *Handler) OIDCBind(c *gin.Context)        { h.oidcBegin(c, true, true) }

func (h *Handler) oidcBegin(c *gin.Context, admin, bind bool) {
	if !h.oidcReady(c, admin) {
		return
	}
	var actor *auth.Principal
	if bind {
		current, ok := Current(c)
		if !ok {
			h.failOperation(c, admin, auth.ErrUnauthenticated, "oidc_bind")
			return
		}
		actor = &current
	}
	var input oidcBeginRequest
	if !decodeOIDCBody(c, &input) || !oidcParameter(input.Op, 128, true) ||
		!oidcParameter(input.ID, 256, false) || !oidcParameter(input.UUID, 256, false) ||
		!oidcParameter(input.DeviceInfo.OS, 128, false) || !oidcParameter(input.DeviceInfo.Name, 256, false) ||
		!oidcParameter(input.DeviceInfo.Type, 64, false) {
		h.failOperation(c, admin, auth.ErrInvalidInput, "oidc_begin")
		return
	}
	client := "client"
	if admin {
		client = "admin"
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	started, err := h.oidc.Begin(ctx, input.Op, loginattempt.Device{
		ID: input.ID, UUID: input.UUID, Client: client, Platform: input.DeviceInfo.OS,
	}, actor)
	if err != nil {
		h.oidcFailure(c, admin, err, "oidc_begin")
		return
	}
	data := gin.H{"code": started.Code, "url": started.URL}
	if admin {
		oidcSuccess(c, data)
	} else {
		c.JSON(http.StatusOK, data)
	}
}

func (h *Handler) ClientOIDCPoll(c *gin.Context) { h.oidcPoll(c, false) }
func (h *Handler) AdminOIDCPoll(c *gin.Context)  { h.oidcPoll(c, true) }

func (h *Handler) oidcPoll(c *gin.Context, admin bool) {
	if !h.oidcReady(c, admin) {
		return
	}
	query, ok := oidcQuery(c, map[string]int{"code": 256, "id": 256, "uuid": 256})
	if !ok || query.Get("code") == "" {
		h.failOperation(c, admin, auth.ErrInvalidInput, "oidc_poll")
		return
	}
	client := "client"
	if admin {
		client = "admin"
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	principal, err := h.oidc.PollEither(ctx, query.Get("code"), loginattempt.Device{
		ID: query.Get("id"), UUID: query.Get("uuid"), Client: client,
	}, c.ClientIP())
	if err != nil {
		if errors.Is(err, loginattempt.ErrPending) {
			// Both client variants depend on this top-level error for polling.
			c.JSON(http.StatusOK, gin.H{"message": "Authorization in progress, please login and bind", "error": "No authed oidc is found"})
			return
		}
		h.oidcFailure(c, admin, err, "oidc_poll")
		return
	}
	user := legacyUser(principal.User)
	if admin {
		var payload adminResponse.LoginPayload
		payload.FromUser(user)
		payload.Token = principal.Session.Token
		payload.RouteNames = append([]string(nil), h.userRoutes...)
		if principal.User.IsAdmin {
			payload.RouteNames = append([]string(nil), h.adminRoutes...)
		}
		oidcSuccess(c, payload)
	} else {
		c.JSON(http.StatusOK, apiResponse.LoginRes{AccessToken: principal.Session.Token, Type: "access_token",
			User: *(&apiResponse.UserPayload{}).FromUser(user)})
	}
}

func (h *Handler) OIDCCallback(c *gin.Context) {
	if !h.oidcReady(c, false) {
		return
	}
	c.Header("Content-Security-Policy", "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	query, ok := oidcQuery(c, map[string]int{"state": 256, "code": 4096, "error": 256, "iss": 2048,
		"error_description": 2048, "error_uri": 2048})
	_, issuerSupplied := query["iss"]
	if !ok || query.Get("state") == "" || (query.Get("code") == "") == (query.Get("error") == "") ||
		(issuerSupplied && query.Get("iss") == "") {
		c.HTML(http.StatusOK, "oauth_fail.html", gin.H{"message": "OauthFailed"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	if err := h.oidc.Callback(ctx, query.Get("state"), query.Get("code"), query.Get("error"), query.Get("iss")); err != nil {
		if oidcInvalidAttempt(err) || errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrDisabled) ||
			errors.Is(err, auth.ErrForbidden) || errors.Is(err, auth.ErrInvalidInput) {
			c.HTML(http.StatusOK, "oauth_fail.html", gin.H{"message": "OauthFailed"})
		} else {
			h.oidcFailure(c, false, err, "oidc_callback")
		}
		return
	}
	c.HTML(http.StatusOK, "oauth_success.html", gin.H{"message": "OauthSuccess"})
}

func (h *Handler) OIDCInfo(c *gin.Context) {
	if !h.oidcReady(c, true) {
		return
	}
	actor, ok := Current(c)
	if !ok {
		h.failOperation(c, true, auth.ErrUnauthenticated, "oidc_info")
		return
	}
	query, valid := oidcQuery(c, map[string]int{"code": 256})
	if !valid || query.Get("code") == "" {
		h.failOperation(c, true, auth.ErrInvalidInput, "oidc_info")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	attempt, err := h.oidc.Inspect(ctx, query.Get("code"), actor)
	if err != nil {
		h.oidcFailure(c, true, err, "oidc_info")
		return
	}
	action := "login"
	if attempt.Purpose == loginattempt.PurposeBind {
		action = "bind"
	}
	oidcSuccess(c, gin.H{"id": attempt.Device.ID, "uuid": attempt.Device.UUID, "device_os": attempt.Device.Platform,
		"device_type": attempt.Device.Client, "action": action, "status": attempt.Status})
}

func (h *Handler) OIDCConfirm(c *gin.Context) {
	if !h.oidcReady(c, true) {
		return
	}
	actor, ok := Current(c)
	if !ok {
		h.failOperation(c, true, auth.ErrUnauthenticated, "oidc_approve")
		return
	}
	var input struct {
		Code string `json:"code"`
	}
	if !decodeOIDCBody(c, &input) || !oidcParameter(input.Code, 256, true) {
		h.failOperation(c, true, auth.ErrInvalidInput, "oidc_approve")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	current, err := h.sessions.Authenticate(ctx, actor.Session.Token)
	if err != nil {
		h.failOperation(c, true, err, "oidc_approve")
		return
	}
	if current.User.ID != actor.User.ID || !approvalPasswordAllowed(current.User.PasswordHash) {
		h.failOperation(c, true, auth.ErrForbidden, "oidc_approve")
		return
	}
	if err := h.oidc.ApproveWeb(ctx, input.Code, current); err != nil {
		h.oidcFailure(c, true, err, "oidc_approve")
		return
	}
	oidcSuccess(c, nil)
}

func (h *Handler) OIDCBindConfirm(c *gin.Context) {
	if !h.oidcReady(c, true) {
		return
	}
	// A cached code is no longer authority to attach an identity. Binding must
	// originate from the authenticated actor's own OIDC authorization attempt.
	h.failOperation(c, true, auth.ErrInvalidInput, "oidc_bind_confirm")
}

func (h *Handler) OIDCUnbind(c *gin.Context) {
	if !h.oidcReady(c, true) {
		return
	}
	actor, ok := Current(c)
	if !ok {
		h.failOperation(c, true, auth.ErrUnauthenticated, "oidc_unbind")
		return
	}
	var input struct {
		Op string `json:"op"`
	}
	if !decodeOIDCBody(c, &input) || !oidcParameter(input.Op, 128, true) {
		h.failOperation(c, true, auth.ErrInvalidInput, "oidc_unbind")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	if err := h.oidc.Unbind(ctx, input.Op, actor); err != nil {
		h.oidcFailure(c, true, err, "oidc_unbind")
		return
	}
	oidcSuccess(c, nil)
}

func (h *Handler) OIDCMyOauth(c *gin.Context) {
	if !h.oidcReady(c, true) {
		return
	}
	actor, ok := Current(c)
	if !ok {
		h.failOperation(c, true, auth.ErrUnauthenticated, "oidc_links")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	links, err := h.oidc.MyOauth(ctx, actor)
	if err != nil {
		h.oidcFailure(c, true, err, "oidc_links")
		return
	}
	items := make([]adminResponse.UserOauthItem, 0, len(links))
	for _, link := range links {
		status := 0
		if link.Linked {
			status = 1
		}
		items = append(items, adminResponse.UserOauthItem{Op: link.Op, Status: status})
	}
	oidcSuccess(c, items)
}

func (h *Handler) oidcReady(c *gin.Context, admin bool) bool {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
	if h.oidc == nil {
		h.failOperation(c, admin, auth.ErrUnavailable, "oidc")
		return false
	}
	return true
}

func (h *Handler) oidcFailure(c *gin.Context, admin bool, err error, operation string) {
	var failure *loginattempt.FailureError
	if errors.As(err, &failure) && failure.Cause != nil {
		err = failure.Cause
	}
	if oidcInvalidAttempt(err) {
		err = auth.ErrInvalidInput
	}
	h.failOperation(c, admin, err, operation)
}

func oidcInvalidAttempt(err error) bool {
	return errors.Is(err, loginattempt.ErrNotFound) || errors.Is(err, loginattempt.ErrExpired) ||
		errors.Is(err, loginattempt.ErrForbidden) || errors.Is(err, loginattempt.ErrClaimed) ||
		errors.Is(err, loginattempt.ErrInvalidState) || errors.Is(err, loginattempt.ErrInvalidInput) ||
		errors.Is(err, loginattempt.ErrFailed)
}

func oidcSuccess(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": data})
}

func oidcParameter(value string, max int, required bool) bool {
	return len(value) <= max && (!required || value != "") && !strings.ContainsAny(value, "\x00\r\n")
}

func decodeOIDCBody(c *gin.Context, target interface{}) bool {
	if c.Request.Body == nil {
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxOIDCBody)
	decoder := json.NewDecoder(c.Request.Body)
	if err := decoder.Decode(target); err != nil {
		return false
	}
	var trailing interface{}
	return decoder.Decode(&trailing) == io.EOF
}

func oidcQuery(c *gin.Context, limits map[string]int) (url.Values, bool) {
	if len(c.Request.URL.RawQuery) > 16<<10 {
		return nil, false
	}
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		return nil, false
	}
	for key, limit := range limits {
		values := query[key]
		if len(values) > 1 || (len(values) == 1 && !oidcParameter(values[0], limit, false)) {
			return nil, false
		}
	}
	return query, true
}

func approvalPasswordAllowed(hash string) bool {
	if hash == "" || strings.EqualFold(hash, utils.Md5("rustdesk-api")) || strings.EqualFold(hash, utils.Md5(utils.DefaultInitPassword+"rustdesk-api")) {
		return false
	}
	if strings.HasPrefix(hash, "$2") {
		for _, password := range []string{"", utils.DefaultInitPassword} {
			matches, _, err := utils.VerifyPassword(hash, password)
			if err != nil || matches {
				return false
			}
		}
		return true
	}
	decoded, err := hex.DecodeString(hash)
	return err == nil && len(decoded) == 16
}
