package bookadmin

import (
	"context"
	"errors"
	"strings"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

type BatchEntryInput struct {
	Entry   EntryInput
	UserIDs []uint
}
type FromPeersInput struct {
	PeerIDs              []uint
	UserID, CollectionID uint
	Tags                 []string
}
type EntryTagsInput struct {
	RowIDs []uint
	Tags   []string
}

func checkedBatchIDs(ids []uint, rejectDuplicates bool) ([]uint, error) {
	if len(ids) == 0 {
		return nil, ErrInput
	}
	seen := map[uint]bool{}
	for _, id := range ids {
		if id == 0 || (rejectDuplicates && seen[id]) {
			return nil, ErrInput
		}
		seen[id] = true
	}
	return uniqueIDs(ids), nil
}

func firstBatchEntry(tx *gorm.DB, scope bookScope, deviceID string) (model.AddressBook, error) {
	var row model.AddressBook
	err := exactEntryDevice(rowLock(tx), deviceID).Where("user_id = ? AND collection_id = ?", scope.OwnerID, scope.CollectionID).Order("row_id ASC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.AddressBook{}, nil
	}
	return row, err
}

func insertBatchEntry(tx *gorm.DB, scope bookScope, input EntryInput) error {
	fields, err := entryFields(input)
	if err != nil {
		return err
	}
	now := tx.NowFunc()
	fields["id"], fields["user_id"], fields["collection_id"], fields["note"] = input.ID, scope.OwnerID, scope.CollectionID, ""
	fields["created_at"], fields["updated_at"] = now, now
	return tx.Model(&model.AddressBook{}).Create(fields).Error
}

// Existing batch entries retain the legacy struct Updates nonzero patch:
// empty strings/false do not clear, while tags are always an explicit JSON list.
func batchPatch(input EntryInput) (map[string]any, error) {
	all, err := entryFields(input)
	if err != nil {
		return nil, err
	}
	patch := map[string]any{"tags": all["tags"]}
	for key, value := range all {
		switch v := value.(type) {
		case string:
			if v != "" {
				patch[key] = v
			}
		case bool:
			if v {
				patch[key] = v
			}
		}
	}
	return patch, nil
}

// BatchEntries retains the stock raw multiple-target switch before canonical
// de-duplication. UserIDs alone determine owners; Entry.UserID is ignored.
func (s *Store) BatchEntries(ctx context.Context, actor auth.Principal, input BatchEntryInput) error {
	ids, err := checkedBatchIDs(input.UserIDs, false)
	if err != nil || input.Entry.RowID != 0 || input.Entry.ID == "" {
		return ErrInput
	}
	entry := input.Entry
	if len(input.UserIDs) > 1 {
		entry.CollectionID, entry.Tags = 0, []string{}
	}
	plan := writePlan{}
	for _, owner := range ids {
		scope, err := s.createScope(ctx, actor, Admin, owner, entry.CollectionID)
		if err != nil {
			return err
		}
		plan.Scopes = append(plan.Scopes, scope)
	}
	return s.run(ctx, actor, Admin, plan, func(tx *gorm.DB, _ lockedWrite) error {
		patch, err := batchPatch(entry)
		if err != nil {
			return err
		}
		for _, scope := range plan.Scopes {
			row, err := firstBatchEntry(tx, scope, entry.ID)
			if err != nil {
				return err
			}
			if row.RowId == 0 {
				if err := insertBatchEntry(tx, scope, entry); err != nil {
					return err
				}
			} else if err := tx.Model(&model.AddressBook{}).Where("row_id = ?", row.RowId).Updates(patch).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) batchPeerCandidates(ctx context.Context, actor auth.Principal, ids []uint) ([]model.Peer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !proof(actor) || !actor.Session.ExpiresAt.After(s.now()) {
		return nil, auth.ErrUnauthenticated
	}
	work, cancel := context.WithDeadline(ctx, actor.Session.ExpiresAt)
	defer cancel()
	rows := make([]model.Peer, 0, len(ids))
	for _, id := range ids {
		var peer model.Peer
		err := s.db.WithContext(work).Select("row_id,id,uuid,user_id").First(&peer, id).Error
		if err != nil && work.Err() != nil {
			return nil, work.Err()
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// run authenticates again before reporting a missing requested row.
			continue
		}
		if err != nil {
			return nil, classify(err)
		}
		rows = append(rows, peer)
	}
	return rows, nil
}

// Pure compatibility mapping from service.PlatformFromOs, without a service or
// global DB dependency. Do not extend its cases or infer aliases/notes here.
func batchPlatform(os string) string {
	switch {
	case strings.Contains(os, "Android") || strings.Contains(os, "android"):
		return "Android"
	case strings.Contains(os, "Windows") || strings.Contains(os, "windows"):
		return "Windows"
	case strings.Contains(os, "Linux") || strings.Contains(os, "linux"):
		return "Linux"
	case strings.Contains(os, "mac") || strings.Contains(os, "Mac"):
		return "Mac OS"
	default:
		return ""
	}
}

func (s *Store) EntriesFromPeers(ctx context.Context, actor auth.Principal, mode Mode, input FromPeersInput) error {
	ids, err := checkedBatchIDs(input.PeerIDs, true)
	if err != nil {
		return err
	}
	scope, err := s.createScope(ctx, actor, mode, input.UserID, input.CollectionID)
	if err != nil {
		return err
	}
	candidates, err := s.batchPeerCandidates(ctx, actor, ids)
	if err != nil {
		return err
	}
	plan := entryPlan(scope, candidates)
	plan.PeerIDs = ids // Missing candidates must also be checked under fresh auth.
	return s.run(ctx, actor, mode, plan, func(tx *gorm.DB, locked lockedWrite) error {
		if scope.OwnerID == 0 || len(candidates) != len(ids) {
			return ErrInput
		}
		// Validate the complete source set before writing any destination row.
		for _, candidate := range candidates {
			peer, ok := locked.Peers[candidate.RowId]
			if !ok || peer.RowId != candidate.RowId || peer.Id != candidate.Id || peer.Uuid != candidate.Uuid || peer.UserId != candidate.UserId || peer.Id == "" {
				return ErrInput
			}
			if mode == My && peer.UserId != actor.User.ID {
				return auth.ErrForbidden
			}
		}
		for _, candidate := range candidates {
			peer := locked.Peers[candidate.RowId]
			existing, err := firstBatchEntry(tx, scope, peer.Id)
			if err != nil {
				return err
			}
			if existing.RowId != 0 {
				continue
			}
			entry := EntryInput{ID: peer.Id, Username: peer.Username, Hostname: peer.Hostname, Platform: batchPlatform(peer.Os), Tags: input.Tags}
			if err := insertBatchEntry(tx, scope, entry); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReplaceEntryTags is the existing My operation: every requested row must still
// belong to the actor and its planned parent before any tags are replaced.
func (s *Store) ReplaceEntryTags(ctx context.Context, actor auth.Principal, input EntryTagsInput) error {
	ids, err := checkedBatchIDs(input.RowIDs, false)
	if err != nil {
		return err
	}
	candidates := make([]model.AddressBook, 0, len(ids))
	plan := writePlan{}
	for _, id := range ids {
		row, err := s.entryCandidate(ctx, actor, id)
		if err != nil {
			return err
		}
		if row.RowId == 0 {
			return s.missing(ctx, actor, My)
		}
		candidates = append(candidates, row)
		plan.Scopes = append(plan.Scopes, entryScope(row))
	}
	return s.run(ctx, actor, My, plan, func(tx *gorm.DB, _ lockedWrite) error {
		for _, candidate := range candidates {
			if _, err := lockedEntry(tx, candidate); err != nil {
				return err
			}
		}
		fields, err := entryFields(EntryInput{Tags: input.Tags})
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := tx.Model(&model.AddressBook{}).Where("row_id = ?", id).Update("tags", fields["tags"]).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
