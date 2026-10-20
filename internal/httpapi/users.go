package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionguard"
	"github.com/lejianwen/rustdesk-api/v2/internal/useradmin"
	"github.com/lejianwen/rustdesk-api/v2/model"
)

// Explicit writable fields preserve the existing form; ORM metadata and
// password inputs on account metadata routes never reach persistence.
type managementUserForm struct {
	ID       uint             `json:"id"`
	Username string           `json:"username"`
	Email    string           `json:"email"`
	Nickname string           `json:"nickname"`
	Avatar   string           `json:"avatar"`
	GroupID  uint             `json:"group_id"`
	IsAdmin  *bool            `json:"is_admin"`
	Status   model.StatusCode `json:"status"`
	Remark   string           `json:"remark"`
}

func (f managementUserForm) input() useradmin.UserInput {
	return useradmin.UserInput{ID: f.ID, Username: f.Username, Email: f.Email, Nickname: f.Nickname, Avatar: f.Avatar, GroupID: f.GroupID, IsAdmin: f.IsAdmin, Status: f.Status, Remark: f.Remark}
}
func (f managementUserForm) valid() bool {
	n := utf8.RuneCountInString(f.Username)
	return n >= 2 && n <= 32 && f.GroupID != 0 && f.Status > 0
}
func validManagementPassword(value string) bool {
	n := utf8.RuneCountInString(value)
	return n >= 4 && n <= 32
}
func (h *Handler) userActor(c *gin.Context, admin bool) (auth.Principal, bool) {
	actor, ok := Current(c)
	if !ok {
		h.fail(c, true, auth.ErrUnauthenticated)
		return auth.Principal{}, false
	}
	if admin {
		if err := auth.RequireAdministrator(actor); err != nil {
			h.failOperation(c, true, err, "user_management")
			return auth.Principal{}, false
		}
	}
	if h.users == nil {
		h.userFailure(c, useradmin.ErrUnavailable)
		return auth.Principal{}, false
	}
	return actor, true
}

var errManagementUsernameExists = errors.New("username exists")

func (h *Handler) UserCreate(c *gin.Context) { h.userMetadata(c, true) }
func (h *Handler) UserUpdate(c *gin.Context) { h.userMetadata(c, false) }
func (h *Handler) userMetadata(c *gin.Context, create bool) {
	actor, ok := h.userActor(c, true)
	if !ok {
		return
	}
	var form managementUserForm
	if managementJSON(c, &form) != nil || !form.valid() || (create && form.ID != 0) || (!create && form.ID == 0) {
		h.userFailure(c, useradmin.ErrInput)
		return
	}
	normalized := strings.ToLower(strings.ReplaceAll(form.Username, " ", ""))
	if create && normalized == "" {
		h.userFailure(c, useradmin.ErrInput)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	var err error
	if create {
		if h.checkUsernameInLDAP != nil {
			names := []string{form.Username}
			if normalized != form.Username {
				names = append(names, normalized)
			}
			for _, name := range names {
				exists, lookupErr := h.checkUsernameInLDAP(ctx, name)
				if lookupErr != nil {
					if ctx.Err() != nil {
						lookupErr = ctx.Err()
					}
					h.userFailure(c, lookupErr)
					return
				}
				if exists {
					h.userFailure(c, errManagementUsernameExists)
					return
				}
			}
		}
		err = h.users.Create(ctx, actor, form.input())
	} else {
		err = h.users.Update(ctx, actor, form.input())
	}
	if err != nil {
		h.userFailure(c, err)
		return
	}
	userSuccess(c)
}
func (h *Handler) UserDelete(c *gin.Context) {
	actor, ok := h.userActor(c, true)
	if !ok {
		return
	}
	var form struct {
		ID uint `json:"id"`
	}
	if managementJSON(c, &form) != nil || form.ID == 0 {
		h.userFailure(c, useradmin.ErrInput)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	if err := h.users.Delete(ctx, actor, form.ID); err != nil {
		h.userFailure(c, err)
		return
	}
	userSuccess(c)
}
func (h *Handler) UserResetPassword(c *gin.Context) {
	actor, ok := h.userActor(c, true)
	if !ok {
		return
	}
	var form struct {
		ID       uint   `json:"id"`
		Password string `json:"password"`
	}
	if managementJSON(c, &form) != nil || form.ID == 0 || !validManagementPassword(form.Password) {
		h.userFailure(c, useradmin.ErrInput)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	if err := h.users.ResetPassword(ctx, actor, useradmin.PasswordInput{ID: form.ID, Password: form.Password}); err != nil {
		h.userFailure(c, err)
		return
	}
	userSuccess(c)
}
func (h *Handler) UserChangePassword(c *gin.Context) {
	actor, ok := h.userActor(c, false)
	if !ok {
		return
	}
	var form struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if managementJSON(c, &form) != nil || !validManagementPassword(form.Old) || !validManagementPassword(form.New) {
		h.userFailure(c, useradmin.ErrInput)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	if err := h.users.ChangePassword(ctx, actor, form.Old, form.New); err != nil {
		h.userFailure(c, err)
		return
	}
	userSuccess(c)
}
func userSuccess(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": nil})
}
func (h *Handler) userFailure(c *gin.Context, err error) {
	if errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrDisabled) || errors.Is(err, auth.ErrForbidden) {
		h.failOperation(c, true, err, "user_management")
		return
	}
	status, code, message := 503, 503, "Service unavailable"
	switch {
	case errors.Is(err, useradmin.ErrInput):
		status, code, message = 200, 101, h.translate(c.GetHeader("Accept-Language"), "ParamsError")
	case errors.Is(err, errManagementUsernameExists):
		status, code, message = 200, 101, h.translate(c.GetHeader("Accept-Language"), "OperationFailed")
	case errors.Is(err, useradmin.ErrNotFound):
		status, code, message = 200, 101, h.translate(c.GetHeader("Accept-Language"), "ItemNotFound")
	case errors.Is(err, useradmin.ErrOldPassword):
		status, code, message = 200, 101, h.translate(c.GetHeader("Accept-Language"), "OldPasswordError")
	case errors.Is(err, useradmin.ErrLastEnabledAdmin):
		status, code, message = 200, 101, "The last enabled administrator cannot be removed"
	case errors.Is(err, collectionguard.ErrReferenced):
		status, code, message = 200, 101, "Collection is referenced by a device"
	case errors.Is(err, context.Canceled):
		status, code, message = 408, 408, "Request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = 504, 504, "Request timed out"
	}
	c.AbortWithStatusJSON(status, gin.H{"code": code, "message": message, "data": nil})
}
