package bookadmin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

// EntryInput is the existing management form's writable field set. Note is
// deliberately absent: management entry edits do not replace that newer field.
type EntryInput struct {
	RowID, UserID, CollectionID                             uint
	ID, Username, Password, Hostname, Alias, Platform, Hash string
	RDPPort, RDPUsername, LoginName                         string
	Tags                                                    []string
	ForceAlwaysRelay, Online, SameServer                    bool
}

func entryScope(row model.AddressBook) bookScope { return bookScope{row.UserId, row.CollectionId} }

// AddressBook has a row_id PK and a string device id. The Tag/Rule candidate
// helper's numeric id projection must never be reused for this model.
func (s *Store) entryCandidate(ctx context.Context, actor auth.Principal, id uint) (model.AddressBook, error) {
	var row model.AddressBook
	if err := ctx.Err(); err != nil {
		return row, err
	}
	if !proof(actor) || !actor.Session.ExpiresAt.After(s.now()) {
		return row, auth.ErrUnauthenticated
	}
	work, cancel := context.WithDeadline(ctx, actor.Session.ExpiresAt)
	defer cancel()
	err := s.db.WithContext(work).Select("row_id,user_id,collection_id,id").First(&row, id).Error
	if err != nil && work.Err() != nil {
		return row, work.Err()
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.AddressBook{}, nil
	}
	return row, classify(err)
}

func exactEntryDevice(tx *gorm.DB, id string) *gorm.DB {
	switch tx.Dialector.Name() {
	case "mysql":
		return tx.Where("BINARY id = ?", id)
	case "sqlite":
		return tx.Where("id COLLATE BINARY = ?", id)
	default:
		return tx.Where("id = ?", id)
	}
}

// Peer discovery is an ordinary pre-read. run acquires its owner/user and PK
// locks before parents; the operation must then verify the complete candidate.
func (s *Store) entryPeerCandidates(ctx context.Context, actor auth.Principal, id string) ([]model.Peer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !proof(actor) || !actor.Session.ExpiresAt.After(s.now()) {
		return nil, auth.ErrUnauthenticated
	}
	work, cancel := context.WithDeadline(ctx, actor.Session.ExpiresAt)
	defer cancel()
	var peers []model.Peer
	err := exactEntryDevice(s.db.WithContext(work), id).Select("row_id,id,uuid,user_id").Order("row_id ASC").Limit(2).Find(&peers).Error
	if err != nil && work.Err() != nil {
		return nil, work.Err()
	}
	return peers, classify(err)
}

func entryPlan(scope bookScope, peers []model.Peer) writePlan {
	plan := writePlan{Scopes: []bookScope{scope}}
	for _, peer := range peers {
		plan.PeerIDs = append(plan.PeerIDs, peer.RowId)
		plan.UserIDs = append(plan.UserIDs, peer.UserId)
	}
	return plan
}

func lockedEntry(tx *gorm.DB, candidate model.AddressBook) (model.AddressBook, error) {
	var row model.AddressBook
	if err := rowLock(tx).First(&row, candidate.RowId).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return row, ErrNotFound
		}
		return row, err
	}
	if row.RowId != candidate.RowId || row.Id != candidate.Id || !sameScope(entryScope(row), entryScope(candidate)) {
		return row, ErrInput
	}
	return row, nil
}

func entryIdentity(input EntryInput, candidate model.AddressBook) error {
	if err := fixedScope(input.UserID, input.CollectionID, entryScope(candidate)); err != nil {
		return err
	}
	if input.ID != "" && input.ID != candidate.Id {
		return ErrInput
	}
	return nil
}

// No new user/peer lock may be taken here. Compare the prelocked identities and
// an ordinary exact-match set read; a changed/ambiguous target fails closed.
func checkedEntryPeer(tx *gorm.DB, locked lockedWrite, candidates []model.Peer, deviceID string) (*model.Peer, error) {
	if len(candidates) > 1 {
		return nil, ErrInput
	}
	var current []model.Peer
	if err := exactEntryDevice(tx, deviceID).Select("row_id,id,uuid,user_id").Order("row_id ASC").Limit(2).Find(&current).Error; err != nil {
		return nil, err
	}
	if len(current) != len(candidates) {
		return nil, ErrInput
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	candidate := candidates[0]
	peer, ok := locked.Peers[candidate.RowId]
	if !ok || peer.RowId != candidate.RowId || peer.Id != deviceID || peer.Id != candidate.Id || peer.Uuid != candidate.Uuid || peer.UserId != candidate.UserId {
		return nil, ErrInput
	}
	now := current[0]
	if now.RowId != candidate.RowId || now.Id != candidate.Id || now.Uuid != candidate.Uuid || now.UserId != candidate.UserId {
		return nil, ErrInput
	}
	return &peer, nil
}

func entryFields(input EntryInput) (map[string]any, error) {
	tags := input.Tags
	if tags == nil {
		tags = []string{}
	}
	raw, err := json.Marshal(tags)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"username": input.Username, "password": input.Password, "hostname": input.Hostname,
		"alias": input.Alias, "platform": input.Platform, "hash": input.Hash,
		"tags": custom_types.AutoJson(raw), "force_always_relay": input.ForceAlwaysRelay,
		"rdp_port": input.RDPPort, "rdp_username": input.RDPUsername, "online": input.Online,
		"login_name": input.LoginName, "same_server": input.SameServer,
	}, nil
}

func (s *Store) CreateEntry(ctx context.Context, actor auth.Principal, mode Mode, input EntryInput) error {
	if input.RowID != 0 || input.ID == "" {
		return ErrInput
	}
	scope, err := s.createScope(ctx, actor, mode, input.UserID, input.CollectionID)
	if err != nil {
		return err
	}
	return s.run(ctx, actor, mode, writePlan{Scopes: []bookScope{scope}}, func(tx *gorm.DB, _ lockedWrite) error {
		if scope.OwnerID == 0 {
			return ErrInput
		}
		var count int64
		if err := exactEntryDevice(tx.Model(&model.AddressBook{}), input.ID).Where("user_id = ? AND collection_id = ?", scope.OwnerID, scope.CollectionID).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrInput
		}
		fields, err := entryFields(input)
		if err != nil {
			return err
		}
		// Explicit map includes zero values and only the old DTO's columns.
		fields["id"], fields["user_id"], fields["collection_id"], fields["note"] = input.ID, scope.OwnerID, scope.CollectionID, ""
		// Map creates do not receive GORM's struct automatic timestamps.
		now := tx.NowFunc()
		fields["created_at"], fields["updated_at"] = now, now
		return tx.Model(&model.AddressBook{}).Create(fields).Error
	})
}

func (s *Store) UpdateEntry(ctx context.Context, actor auth.Principal, mode Mode, input EntryInput) error {
	if input.RowID == 0 {
		return ErrInput
	}
	candidate, err := s.entryCandidate(ctx, actor, input.RowID)
	if err != nil {
		return err
	}
	if candidate.RowId == 0 {
		return s.missing(ctx, actor, mode)
	}
	var peers []model.Peer
	if mode == Admin {
		peers, err = s.entryPeerCandidates(ctx, actor, candidate.Id)
		if err != nil {
			return err
		}
	}
	return s.run(ctx, actor, mode, entryPlan(entryScope(candidate), peers), func(tx *gorm.DB, locked lockedWrite) error {
		if err := entryIdentity(input, candidate); err != nil {
			return err
		}
		row, err := lockedEntry(tx, candidate)
		if err != nil {
			return err
		}
		var peer *model.Peer
		if mode == Admin {
			peer, err = checkedEntryPeer(tx, locked, peers, row.Id)
			if err != nil {
				return err
			}
		}
		fields, err := entryFields(input)
		if err != nil {
			return err
		}
		if err := tx.Model(&model.AddressBook{}).Where("row_id = ?", row.RowId).Updates(fields).Error; err != nil {
			return err
		}
		if peer != nil {
			return tx.Model(&model.Peer{}).Where("row_id = ?", peer.RowId).Updates(map[string]any{
				"ab_id": row.CollectionId, "alias": input.Alias, "username": input.Username, "hostname": input.Hostname,
			}).Error
		}
		return nil
	})
}

func (s *Store) DeleteEntry(ctx context.Context, actor auth.Principal, mode Mode, input EntryInput) error {
	if input.RowID == 0 {
		return ErrInput
	}
	candidate, err := s.entryCandidate(ctx, actor, input.RowID)
	if err != nil {
		return err
	}
	if candidate.RowId == 0 {
		return s.missing(ctx, actor, mode)
	}
	var peers []model.Peer
	if mode == Admin && candidate.CollectionId != 0 {
		peers, err = s.entryPeerCandidates(ctx, actor, candidate.Id)
		if err != nil {
			return err
		}
	}
	return s.run(ctx, actor, mode, entryPlan(entryScope(candidate), peers), func(tx *gorm.DB, locked lockedWrite) error {
		if err := entryIdentity(input, candidate); err != nil {
			return err
		}
		row, err := lockedEntry(tx, candidate)
		if err != nil {
			return err
		}
		var peer *model.Peer
		if mode == Admin && row.CollectionId != 0 {
			peer, err = checkedEntryPeer(tx, locked, peers, row.Id)
			if err != nil {
				return err
			}
		}
		result := tx.Where("row_id = ?", row.RowId).Delete(&model.AddressBook{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrNotFound
		}
		if peer != nil && peer.AbId == row.CollectionId {
			return tx.Model(&model.Peer{}).Where("row_id = ?", peer.RowId).Updates(map[string]any{"ab_id": uint(0), "alias": ""}).Error
		}
		return nil
	})
}
