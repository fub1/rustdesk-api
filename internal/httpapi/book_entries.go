package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/bookadmin"
)

// This is the existing management form's writable set, not a persistence model.
// Fields absent from the old form (including the per-book note) stay protected.
type managementEntryForm struct {
	RowID            uint     `json:"row_id"`
	ID               string   `json:"id"`
	UserID           uint     `json:"user_id"`
	CollectionID     uint     `json:"collection_id"`
	Username         string   `json:"username"`
	Password         string   `json:"password"`
	Hostname         string   `json:"hostname"`
	Alias            string   `json:"alias"`
	Platform         string   `json:"platform"`
	Tags             []string `json:"tags"`
	Hash             string   `json:"hash"`
	ForceAlwaysRelay bool     `json:"forceAlwaysRelay"`
	RDPPort          string   `json:"rdpPort"`
	RDPUsername      string   `json:"rdpUsername"`
	Online           bool     `json:"online"`
	LoginName        string   `json:"loginName"`
	SameServer       bool     `json:"sameServer"`
}

func (f managementEntryForm) input() bookadmin.EntryInput {
	return bookadmin.EntryInput{
		RowID: f.RowID, ID: f.ID, UserID: f.UserID, CollectionID: f.CollectionID,
		Username: f.Username, Password: f.Password, Hostname: f.Hostname, Alias: f.Alias, Platform: f.Platform, Tags: f.Tags, Hash: f.Hash,
		ForceAlwaysRelay: f.ForceAlwaysRelay, RDPPort: f.RDPPort, RDPUsername: f.RDPUsername, Online: f.Online, LoginName: f.LoginName, SameServer: f.SameServer,
	}
}

func (h *Handler) EntryCreate(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) { h.entryWrite(c, admin, "create") }
}
func (h *Handler) EntryUpdate(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) { h.entryWrite(c, admin, "update") }
}
func (h *Handler) EntryDelete(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) { h.entryWrite(c, admin, "delete") }
}
func (h *Handler) entryWrite(c *gin.Context, admin bool, action string) {
	actor, ok := h.bookActor(c, admin)
	if !ok {
		return
	}
	var form managementEntryForm
	if err := managementJSON(c, &form); err != nil {
		h.bookFailure(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	var err error
	switch action {
	case "create":
		err = h.books.CreateEntry(ctx, actor, bookMode(admin), form.input())
	case "update":
		err = h.books.UpdateEntry(ctx, actor, bookMode(admin), form.input())
	case "delete":
		err = h.books.DeleteEntry(ctx, actor, bookMode(admin), form.input())
	}
	if err != nil {
		h.bookFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": nil})
}
