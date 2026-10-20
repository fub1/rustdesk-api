// Package userlifecycle shares checked account cleanup within a caller-owned transaction.
package userlifecycle

import (
	"context"
	"errors"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionguard"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"sort"
)

// DeleteLocked requires the current user PK lock and an already checked role guard.
func DeleteLocked(tx *gorm.DB, current *model.User) error {
	// Login can hold an identity before waiting for this user. Never wait
	// for that identity in the opposite order: fail this whole deletion.
	for _, rows := range []any{&[]model.OIDCIdentity{}, &[]model.UserThird{}} {
		query := tx.Select("id").Where("user_id = ?", current.Id).Order("id ASC")
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"})
		}
		if err := query.Find(rows).Error; err != nil {
			return Classify(tx, err)
		}
	}

	var candidates []model.AddressBookCollection
	if err := tx.Select("id,user_id").Where("user_id = ?", current.Id).Order("id ASC").Find(&candidates).Error; err != nil {
		return err
	}
	cids := make([]uint, 0, len(candidates))
	for _, row := range candidates {
		cids = append(cids, row.Id)
	}
	if err := collectionguard.EnsureUnreferenced(tx, cids); err != nil {
		return err
	}
	sessionIDs, err := lockSessions(tx, current.Id)
	if err != nil {
		return err
	}
	var peers []model.Peer
	if err := tx.Select("row_id").Where("user_id = ?", current.Id).Order("row_id ASC").Find(&peers).Error; err != nil {
		return err
	}
	peerIDs := make([]uint, 0, len(peers))
	for _, candidate := range peers {
		var peer model.Peer
		if err := primaryRows(tx, "peers").Select("row_id,user_id").First(&peer, candidate.RowId).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return err
		}
		if peer.UserId == current.Id {
			peerIDs = append(peerIDs, peer.RowId)
		}
	}
	for _, cid := range cids {
		var collection model.AddressBookCollection
		if err := rows(tx).First(&collection, cid).Error; err != nil {
			return err
		}
		if collection.UserId != current.Id {
			return collectionguard.ErrInput
		}
	}
	if err := collectionguard.EnsureUnreferenced(tx, cids); err != nil {
		return err
	}
	for _, cid := range cids {
		if err := collectionguard.DeleteContents(tx, cid); err != nil {
			return err
		}
	}
	for _, row := range []interface{}{&model.OIDCIdentity{}, &model.UserThird{},
		&model.AddressBook{}, &model.AddressBookCollectionRule{}, &model.Tag{}} {
		if err := tx.Where("user_id = ?", current.Id).Delete(row).Error; err != nil {
			return err
		}
	}
	if err := deleteSessions(tx, sessionIDs); err != nil {
		return err
	}
	for _, id := range peerIDs {
		if err := tx.Model(&model.Peer{}).Where("row_id = ?", id).Update("user_id", 0).Error; err != nil {
			return err
		}
	}
	deleted := tx.Delete(current)
	if deleted.Error != nil {
		return deleted.Error
	}
	if deleted.RowsAffected != 1 {
		return collectionguard.ErrUnavailable
	}
	return nil
}
func lockSessions(tx *gorm.DB, userID uint) ([]uint, error) {
	// Revoke locks the token index before the session PK. Use that same
	// access path, with NOWAIT so a busy group rolls back this whole delete.
	var sessions []model.UserToken
	if err := tx.Select("id,token").Where("user_id = ?", userID).Order("id ASC").Find(&sessions).Error; err != nil {
		return nil, err
	}
	groups := make(map[string]bool, len(sessions))
	candidatesByID := make(map[uint]string, len(sessions))
	for _, candidate := range sessions {
		groups[candidate.Token] = true
		candidatesByID[candidate.Id] = candidate.Token
	}
	tokens := make([]string, 0, len(groups))
	for token := range groups {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)
	lockedSessions := map[uint]bool{}
	for _, token := range tokens {
		var rows []model.UserToken
		if err := sessionGroup(tx, token).Select("id,user_id,token").Find(&rows).Error; err != nil {
			return nil, Classify(tx, err)
		}
		for _, row := range rows {
			if expected, exists := candidatesByID[row.Id]; exists && row.UserId == userID && row.Token == expected {
				lockedSessions[row.Id] = true
			}
		}
	}
	// A revoked row may disappear. A still-owned candidate whose token
	// changed cannot be silently left behind by this deletion.
	for _, candidate := range sessions {
		if lockedSessions[candidate.Id] {
			continue
		}
		var remaining model.UserToken
		if err := tx.Select("id,user_id,token").First(&remaining, candidate.Id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return nil, err
		}
		if remaining.UserId == userID {
			return nil, collectionguard.ErrUnavailable
		}
	}
	ids := make([]uint, 0, len(lockedSessions))
	for id := range lockedSessions {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}
func rows(tx *gorm.DB) *gorm.DB {
	if tx.Dialector.Name() != "sqlite" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return tx
}

// Lock a known peer PK without first taking a user_id secondary-index lock.
// The caller passes a fixed table name, never request input.
func primaryRows(tx *gorm.DB, table string) *gorm.DB {
	query := rows(tx)
	if tx.Dialector.Name() == "mysql" {
		query = query.Clauses(clause.From{Tables: []clause.Table{{Name: table + " FORCE INDEX (PRIMARY)", Raw: true}}})
	}
	return query
}

func sessionGroup(tx *gorm.DB, token string) *gorm.DB {
	query := tx.Where("token = ?", token).Order("id ASC")
	if tx.Dialector.Name() != "sqlite" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"})
	}
	if tx.Dialector.Name() == "mysql" {
		query = query.Clauses(clause.From{Tables: []clause.Table{{Name: "user_tokens FORCE INDEX (idx_user_tokens_token)", Raw: true}}})
	}
	return query
}

// Caller already owns the target user PK lock. New issuance cannot cross it.
func RevokeLocked(tx *gorm.DB, userID uint) error {
	ids, err := lockSessions(tx, userID)
	if err != nil {
		return err
	}
	return deleteSessions(tx, ids)
}
func deleteSessions(tx *gorm.DB, ids []uint) error {
	for _, id := range ids {
		result := tx.Where("id = ?", id).Delete(&model.UserToken{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return collectionguard.ErrUnavailable
		}
	}
	return nil
}
func Classify(db *gorm.DB, err error) error {
	if err == nil {
		return nil
	}
	if db != nil && db.Statement != nil && db.Statement.Context != nil {
		if cause := db.Statement.Context.Err(); cause != nil {
			return cause
		}
	}
	for _, known := range []error{collectionguard.ErrInput, collectionguard.ErrReferenced, collectionguard.ErrUnavailable, gorm.ErrRecordNotFound, auth.ErrUnauthenticated, auth.ErrForbidden, auth.ErrInvalidInput, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	return collectionguard.ErrUnavailable
}
