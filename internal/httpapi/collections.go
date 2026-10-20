package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionadmin"
)

// Keep the existing admin envelopes and routes. Bound ORM metadata is ignored;
// only these explicit business fields can reach the instance store.
func collectionInput(c *gin.Context) (collectionadmin.Input, error) {
	var input collectionadmin.Input
	if c.Request.Body == nil {
		return input, collectionadmin.ErrInput
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	d := json.NewDecoder(c.Request.Body)
	var raw map[string]json.RawMessage
	var trailing any
	if d.Decode(&raw) != nil || raw == nil || d.Decode(&trailing) != io.EOF {
		return input, collectionadmin.ErrInput
	}
	for name, target := range map[string]any{"id": &input.ID, "user_id": &input.UserID, "name": &input.Name} {
		if value, ok := raw[name]; ok && json.Unmarshal(value, target) != nil {
			return input, collectionadmin.ErrInput
		}
	}
	return input, nil
}

func (h *Handler) collectionActor(c *gin.Context, admin bool) (auth.Principal, bool) {
	actor, ok := Current(c)
	if !ok {
		h.fail(c, true, auth.ErrUnauthenticated)
		return auth.Principal{}, false
	}
	if admin {
		if err := auth.RequireAdministrator(actor); err != nil {
			h.failOperation(c, true, err, "collections")
			return auth.Principal{}, false
		}
	}
	if h.collections == nil {
		h.collectionFailure(c, collectionadmin.ErrUnavailable)
		return auth.Principal{}, false
	}
	return actor, true
}

func collectionMode(admin bool) collectionadmin.Mode {
	if admin {
		return collectionadmin.Admin
	}
	return collectionadmin.My
}

func (h *Handler) CollectionCreate(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) { h.collectionWrite(c, admin, "create") }
}
func (h *Handler) CollectionUpdate(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) { h.collectionWrite(c, admin, "update") }
}
func (h *Handler) CollectionDelete(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) { h.collectionWrite(c, admin, "delete") }
}

func (h *Handler) collectionWrite(c *gin.Context, admin bool, action string) {
	actor, ok := h.collectionActor(c, admin)
	if !ok {
		return
	}
	input, err := collectionInput(c)
	if err != nil {
		h.collectionFailure(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	switch action {
	case "create":
		_, err = h.collections.Create(ctx, actor, collectionMode(admin), input)
	case "update":
		_, err = h.collections.Update(ctx, actor, collectionMode(admin), input)
	case "delete":
		err = h.collections.Delete(ctx, actor, collectionMode(admin), input)
	}
	if err != nil {
		h.collectionFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": nil})
}

func (h *Handler) CollectionDetail(c *gin.Context) {
	actor, ok := h.collectionActor(c, true)
	if !ok {
		return
	}
	id, err := positiveDecimal(c.Param("id"))
	if err != nil {
		h.collectionFailure(c, collectionadmin.ErrInput)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	row, err := h.collections.Get(ctx, actor, collectionadmin.Admin, uint(id))
	if err != nil {
		h.collectionFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": row})
}

func legacyCollectionPage(raw string) (collectionadmin.Page, uint, error) {
	p := collectionadmin.Page{Current: 1, Size: 10}
	var owner uint
	q, err := url.ParseQuery(raw)
	if err != nil {
		return p, 0, collectionadmin.ErrInput
	}
	for _, name := range []string{"page", "page_size", "user_id"} {
		values, ok := q[name]
		if !ok {
			continue
		}
		if len(values) != 1 {
			return p, 0, collectionadmin.ErrInput
		}
		if values[0] == "0" {
			continue
		}
		v, err := positiveDecimal(values[0])
		if err != nil || v > uint64(int(^uint(0)>>1)) {
			return p, 0, collectionadmin.ErrInput
		}
		switch name {
		case "page":
			p.Current = int(v)
		case "page_size":
			p.Size = int(v)
		case "user_id":
			owner = uint(v)
		}
	}
	if p.Size > 1000 || p.Current-1 > int(^uint(0)>>1)/p.Size {
		return p, 0, collectionadmin.ErrInput
	}
	return p, owner, nil
}

func (h *Handler) CollectionList(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, ok := h.collectionActor(c, admin)
		if !ok {
			return
		}
		page, owner, err := legacyCollectionPage(c.Request.URL.RawQuery)
		if err != nil {
			h.collectionFailure(c, err)
			return
		}
		if !admin {
			owner = 0
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
		defer cancel()
		total, rows, err := h.collections.List(ctx, actor, collectionMode(admin), page, owner)
		if err != nil {
			h.collectionFailure(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": gin.H{"list": rows, "total": total, "page": page.Current, "page_size": page.Size}})
	}
}

func (h *Handler) collectionFailure(c *gin.Context, err error) {
	if errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrDisabled) || errors.Is(err, auth.ErrForbidden) {
		h.failOperation(c, true, err, "collections")
		return
	}
	status, code, message := http.StatusServiceUnavailable, 503, "Service unavailable"
	switch {
	case errors.Is(err, collectionadmin.ErrInput):
		status, code, message = 200, 101, h.translate(c.GetHeader("Accept-Language"), "ParamsError")
	case errors.Is(err, collectionadmin.ErrNotFound):
		status, code, message = 200, 101, h.translate(c.GetHeader("Accept-Language"), "ItemNotFound")
	case errors.Is(err, collectionadmin.ErrReferenced):
		status, code, message = 200, 101, "Collection is referenced by a device"
	case errors.Is(err, context.Canceled):
		status, code, message = 408, 408, "Request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = 504, 504, "Request timed out"
	}
	c.AbortWithStatusJSON(status, gin.H{"code": code, "message": message, "data": nil})
}
