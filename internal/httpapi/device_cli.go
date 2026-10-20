package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientassign"
)

func (h *Handler) ClientDeviceCLI(c *gin.Context) {
	actor, ok := Current(c)
	if !ok {
		writeCLIFailure(c, auth.ErrUnauthenticated)
		return
	}
	if err := auth.RequireAdministrator(actor); err != nil {
		writeCLIFailure(c, err)
		return
	}
	if h.assign == nil {
		writeCLIFailure(c, clientassign.ErrUnavailable)
		return
	}
	if c.Request.Body == nil {
		writeCLIFailure(c, clientassign.ErrInput)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	decoder := json.NewDecoder(c.Request.Body)
	var input clientassign.Input
	var trailing any
	if decoder.Decode(&input) != nil || decoder.Decode(&trailing) != io.EOF {
		writeCLIFailure(c, clientassign.ErrInput)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	if err := h.assign.Assign(ctx, actor, input); err != nil {
		writeCLIFailure(c, err)
		return
	}
	// The official CLI prints Done! only for an empty response body.
	c.String(http.StatusOK, "")
}

func writeCLIFailure(c *gin.Context, err error) {
	status, message := http.StatusServiceUnavailable, "Service unavailable"
	switch {
	case errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, auth.ErrDisabled):
		status, message = http.StatusUnauthorized, "Unauthorized"
	case errors.Is(err, auth.ErrForbidden):
		status, message = http.StatusForbidden, "NoAccess"
	case errors.Is(err, clientassign.ErrInput):
		status, message = http.StatusBadRequest, "ParamsError"
	case errors.Is(err, clientassign.ErrNotFound):
		status, message = http.StatusBadRequest, "ItemNotFound"
	case errors.Is(err, context.Canceled):
		status, message = 408, "Request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		status, message = 504, "Request timed out"
	}
	c.JSON(status, gin.H{"error": message})
}
