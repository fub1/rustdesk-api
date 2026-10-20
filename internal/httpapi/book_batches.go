package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/bookadmin"
)

// EntryBatchCreate preserves the existing flattened AddressBookForm JSON.
func (h *Handler) EntryBatchCreate() gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, ok := h.bookActor(c, true)
		if !ok {
			return
		}
		var form struct {
			managementEntryForm
			UserIDs []uint `json:"user_ids"`
		}
		if err := managementJSON(c, &form); err != nil {
			h.bookFailure(c, err)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
		defer cancel()
		if err := h.books.BatchEntries(ctx, actor, bookadmin.BatchEntryInput{Entry: form.managementEntryForm.input(), UserIDs: form.UserIDs}); err != nil {
			h.bookFailure(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": nil})
	}
}

func (h *Handler) EntriesFromPeers(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, ok := h.bookActor(c, admin)
		if !ok {
			return
		}
		var form struct {
			PeerIDs      []uint   `json:"peer_ids"`
			UserID       uint     `json:"user_id"`
			CollectionID uint     `json:"collection_id"`
			Tags         []string `json:"tags"`
		}
		if err := managementJSON(c, &form); err != nil {
			h.bookFailure(c, err)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
		defer cancel()
		if err := h.books.EntriesFromPeers(ctx, actor, bookMode(admin), bookadmin.FromPeersInput{PeerIDs: form.PeerIDs, UserID: form.UserID, CollectionID: form.CollectionID, Tags: form.Tags}); err != nil {
			h.bookFailure(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": nil})
	}
}

func (h *Handler) ReplaceEntryTags() gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, ok := h.bookActor(c, false)
		if !ok {
			return
		}
		var form struct {
			RowIDs []uint   `json:"row_ids"`
			Tags   []string `json:"tags"`
		}
		if err := managementJSON(c, &form); err != nil {
			h.bookFailure(c, err)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
		defer cancel()
		if err := h.books.ReplaceEntryTags(ctx, actor, bookadmin.EntryTagsInput{RowIDs: form.RowIDs, Tags: form.Tags}); err != nil {
			h.bookFailure(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": nil})
	}
}
