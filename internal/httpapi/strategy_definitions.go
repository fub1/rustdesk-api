package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/strategydefinitions"
)

// All routes also carry AdminAuth and RequireAdmin. Keep the direct checks so
// reuse of a handler never makes an unwired route an authentication bypass.
func (h *Handler) strategyDefinitionActor(c *gin.Context) (auth.Principal, bool) {
	actor, ok := Current(c)
	if !ok {
		h.fail(c, true, auth.ErrUnauthenticated)
		return auth.Principal{}, false
	}
	if err := auth.RequireAdministrator(actor); err != nil {
		h.failOperation(c, true, err, "authorize")
		return auth.Principal{}, false
	}
	if h.strategyDefinitions == nil {
		h.strategyDefinitionFailure(c, strategydefinitions.ErrUnavailable)
		return auth.Principal{}, false
	}
	return actor, true
}

func strategyDefinitionInput(c *gin.Context) (strategydefinitions.Input, error) {
	if c.Request.Body == nil {
		return strategydefinitions.Input{}, strategydefinitions.ErrInput
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	d := json.NewDecoder(c.Request.Body)
	var input strategydefinitions.Input
	var trailing any
	if d.Decode(&input) != nil || d.Decode(&trailing) != io.EOF {
		return strategydefinitions.Input{}, strategydefinitions.ErrInput
	}
	return input, nil
}

func positiveDecimal(value string) (uint64, error) {
	if value == "" {
		return 0, strategydefinitions.ErrInput
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, strategydefinitions.ErrInput
		}
	}
	n, err := strconv.ParseUint(value, 10, strconv.IntSize)
	if err != nil || n == 0 {
		return 0, strategydefinitions.ErrInput
	}
	return n, nil
}

func strategyDefinitionPage(raw string) (strategydefinitions.Page, error) {
	page := strategydefinitions.Page{Current: 1, Size: 20}
	query, err := url.ParseQuery(raw)
	if err != nil {
		return page, strategydefinitions.ErrInput
	}
	for name, values := range query {
		if len(values) != 1 || (name != "current" && name != "pageSize") {
			return page, strategydefinitions.ErrInput
		}
		value, err := positiveDecimal(values[0])
		if err != nil || value > uint64(int(^uint(0)>>1)) {
			return page, strategydefinitions.ErrInput
		}
		if name == "current" {
			page.Current = int(value)
		} else {
			page.Size = int(value)
		}
	}
	if page.Size > 1000 || page.Current-1 > int(^uint(0)>>1)/page.Size {
		return page, strategydefinitions.ErrInput
	}
	return page, nil
}

func (h *Handler) StrategyDefinitionsCreate(c *gin.Context) {
	actor, ok := h.strategyDefinitionActor(c)
	if !ok {
		return
	}
	input, err := strategyDefinitionInput(c)
	if err != nil || input.IDPresent {
		h.strategyDefinitionFailure(c, strategydefinitions.ErrInput)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	result, err := h.strategyDefinitions.Create(ctx, actor, input)
	if err != nil {
		h.strategyDefinitionFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": result})
}

func (h *Handler) StrategyDefinitionsUpdate(c *gin.Context) {
	actor, ok := h.strategyDefinitionActor(c)
	if !ok {
		return
	}
	id, err := positiveDecimal(c.Param("id"))
	if err != nil {
		h.strategyDefinitionFailure(c, err)
		return
	}
	input, err := strategyDefinitionInput(c)
	if err != nil || (input.IDPresent && input.ID != uint(id)) {
		h.strategyDefinitionFailure(c, strategydefinitions.ErrInput)
		return
	}
	input.ID = uint(id)
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	result, err := h.strategyDefinitions.Update(ctx, actor, input)
	if err != nil {
		h.strategyDefinitionFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": result})
}

func (h *Handler) StrategyDefinitionsGet(c *gin.Context) {
	actor, ok := h.strategyDefinitionActor(c)
	if !ok {
		return
	}
	id, err := positiveDecimal(c.Param("id"))
	if err != nil {
		h.strategyDefinitionFailure(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	result, err := h.strategyDefinitions.Get(ctx, actor, uint(id))
	if err != nil {
		h.strategyDefinitionFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": result})
}

func (h *Handler) StrategyDefinitionsList(c *gin.Context) {
	actor, ok := h.strategyDefinitionActor(c)
	if !ok {
		return
	}
	page, err := strategyDefinitionPage(c.Request.URL.RawQuery)
	if err != nil {
		h.strategyDefinitionFailure(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	total, rows, err := h.strategyDefinitions.List(ctx, actor, page)
	if err != nil {
		h.strategyDefinitionFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": gin.H{"list": rows, "total": total}})
}

func (h *Handler) StrategyDefinitionsDelete(c *gin.Context) {
	actor, ok := h.strategyDefinitionActor(c)
	if !ok {
		return
	}
	id, err := positiveDecimal(c.Param("id"))
	if err != nil {
		h.strategyDefinitionFailure(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	if err := h.strategyDefinitions.Delete(ctx, actor, uint(id)); err != nil {
		h.strategyDefinitionFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": nil})
}

func (h *Handler) strategyDefinitionFailure(c *gin.Context, err error) {
	if errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrDisabled) || errors.Is(err, auth.ErrForbidden) {
		h.failOperation(c, true, err, "strategy_definitions")
		return
	}
	status, message := http.StatusServiceUnavailable, "Service unavailable"
	switch {
	case errors.Is(err, strategydefinitions.ErrInput):
		status, message = http.StatusBadRequest, "ParamsError"
	case errors.Is(err, strategydefinitions.ErrNotFound):
		status, message = http.StatusNotFound, "ItemNotFound"
	case errors.Is(err, strategydefinitions.ErrConflict), errors.Is(err, strategydefinitions.ErrReferenced):
		status, message = http.StatusConflict, "Conflict"
	case errors.Is(err, context.Canceled):
		status, message = http.StatusRequestTimeout, "Request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		status, message = http.StatusGatewayTimeout, "Request timed out"
	}
	c.AbortWithStatusJSON(status, gin.H{"code": status, "message": message, "data": nil})
}
