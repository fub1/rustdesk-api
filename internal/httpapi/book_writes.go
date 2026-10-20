package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/bookadmin"
)

// Decode a bounded legacy form, ignoring old read-only ORM metadata. Only the
// explicit fields on the adapter input can reach the instance-owned store.
func managementJSON(c *gin.Context, target any) error {
	if c.Request.Body == nil {
		return bookadmin.ErrInput
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	decoder := json.NewDecoder(c.Request.Body)
	if err := decoder.Decode(target); err != nil {
		return bookadmin.ErrInput
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return bookadmin.ErrInput
	}
	return nil
}

func (h *Handler) bookActor(c *gin.Context, admin bool) (auth.Principal, bool) {
	actor, ok := Current(c)
	if !ok {
		h.fail(c, true, auth.ErrUnauthenticated)
		return auth.Principal{}, false
	}
	if admin {
		if err := auth.RequireAdministrator(actor); err != nil {
			h.failOperation(c, true, err, "book_management")
			return auth.Principal{}, false
		}
	}
	if h.books == nil {
		h.bookFailure(c, bookadmin.ErrUnavailable)
		return auth.Principal{}, false
	}
	return actor, true
}
func bookMode(admin bool) bookadmin.Mode {
	if admin {
		return bookadmin.Admin
	}
	return bookadmin.My
}

func (h *Handler) TagCreate(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) { h.tagWrite(c, admin, "create") }
}
func (h *Handler) TagUpdate(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) { h.tagWrite(c, admin, "update") }
}
func (h *Handler) TagDelete(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) { h.tagWrite(c, admin, "delete") }
}
func (h *Handler) tagWrite(c *gin.Context, admin bool, action string) {
	actor, ok := h.bookActor(c, admin)
	if !ok {
		return
	}
	var raw struct {
		ID           uint   `json:"id"`
		UserID       uint   `json:"user_id"`
		CollectionID uint   `json:"collection_id"`
		Name         string `json:"name"`
		Color        uint   `json:"color"`
	}
	if err := managementJSON(c, &raw); err != nil {
		h.bookFailure(c, err)
		return
	}
	input := bookadmin.TagInput{ID: raw.ID, UserID: raw.UserID, CollectionID: raw.CollectionID, Name: raw.Name, Color: raw.Color}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	var err error
	switch action {
	case "create":
		err = h.books.CreateTag(ctx, actor, bookMode(admin), input)
	case "update":
		err = h.books.UpdateTag(ctx, actor, bookMode(admin), input)
	case "delete":
		err = h.books.DeleteTag(ctx, actor, bookMode(admin), input)
	}
	if err != nil {
		h.bookFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": nil})
}

func (h *Handler) RuleCreate(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) { h.ruleWrite(c, admin, "create") }
}
func (h *Handler) RuleUpdate(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) { h.ruleWrite(c, admin, "update") }
}
func (h *Handler) RuleDelete(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) { h.ruleWrite(c, admin, "delete") }
}
func (h *Handler) ruleWrite(c *gin.Context, admin bool, action string) {
	actor, ok := h.bookActor(c, admin)
	if !ok {
		return
	}
	var raw struct {
		ID           uint `json:"id"`
		UserID       uint `json:"user_id"`
		CollectionID uint `json:"collection_id"`
		ToID         uint `json:"to_id"`
		Type         int  `json:"type"`
		Rule         int  `json:"rule"`
	}
	if err := managementJSON(c, &raw); err != nil {
		h.bookFailure(c, err)
		return
	}
	input := bookadmin.RuleInput{ID: raw.ID, UserID: raw.UserID, CollectionID: raw.CollectionID, ToID: raw.ToID, Type: raw.Type, Rule: raw.Rule}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	var err error
	switch action {
	case "create":
		err = h.books.CreateRule(ctx, actor, bookMode(admin), input)
	case "update":
		err = h.books.UpdateRule(ctx, actor, bookMode(admin), input)
	case "delete":
		err = h.books.DeleteRule(ctx, actor, bookMode(admin), input)
	}
	if err != nil {
		h.bookFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": nil})
}

func (h *Handler) bookFailure(c *gin.Context, err error) {
	if errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrDisabled) || errors.Is(err, auth.ErrForbidden) {
		h.failOperation(c, true, err, "book_management")
		return
	}
	status, code, message := 503, 503, "Service unavailable"
	switch {
	case errors.Is(err, bookadmin.ErrInput):
		status, code, message = 200, 101, h.translate(c.GetHeader("Accept-Language"), "ParamsError")
	case errors.Is(err, bookadmin.ErrNotFound):
		status, code, message = 200, 101, h.translate(c.GetHeader("Accept-Language"), "ItemNotFound")
	case errors.Is(err, context.Canceled):
		status, code, message = 408, 408, "Request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = 504, 504, "Request timed out"
	}
	c.AbortWithStatusJSON(status, gin.H{"code": code, "message": message, "data": nil})
}
