// Package collectionguard contains transaction-local collection lifecycle
// operations. Callers own authorization, owner/collection locks and COMMIT.
package collectionguard

import (
	"context"
	"errors"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

var (
	ErrInput       = errors.New("invalid collection lifecycle input")
	ErrReferenced  = errors.New("collection is referenced by a primary address book")
	ErrUnavailable = errors.New("collection storage unavailable")
)

func ready(tx *gorm.DB) error {
	if tx == nil || tx.Statement == nil {
		return ErrUnavailable
	}
	if tx.Statement.Context != nil {
		if err := tx.Statement.Context.Err(); err != nil {
			return err
		}
	}
	if tx.Error != nil {
		return failure(tx, tx.Error)
	}
	return nil
}

func failure(tx *gorm.DB, err error) error {
	if err == nil {
		return nil
	}
	if tx != nil && tx.Statement != nil && tx.Statement.Context != nil {
		if canceled := tx.Statement.Context.Err(); canceled != nil {
			return canceled
		}
	}
	for _, known := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrUnavailable
}

// EnsureUnreferenced performs an ordinary current COUNT, never a peer row lock.
// All primary-binding writers must share the caller's owner-lock protocol. A
// COUNT alone cannot protect this legacy relation against unrelated writers.
func EnsureUnreferenced(tx *gorm.DB, cids []uint) error {
	if err := ready(tx); err != nil {
		return err
	}
	if len(cids) == 0 {
		return nil
	}
	ids := make([]uint, 0, len(cids))
	seen := map[uint]bool{}
	for _, id := range cids {
		if id == 0 {
			return ErrInput
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	var count int64
	if err := tx.Model(&model.Peer{}).Where("ab_id IN ?", ids).Count(&count).Error; err != nil {
		return failure(tx, err)
	}
	if count != 0 {
		return ErrReferenced
	}
	return nil
}

// DeleteContents removes one already-locked, checked collection and its owned
// child scopes. It neither deletes nor rewrites any peer or preset cache.
func DeleteContents(tx *gorm.DB, cid uint) error {
	if err := ready(tx); err != nil {
		return err
	}
	if cid == 0 {
		return ErrInput
	}
	for _, row := range []any{&model.AddressBookCollectionRule{}, &model.AddressBook{}, &model.Tag{}} {
		if err := tx.Where("collection_id = ?", cid).Delete(row).Error; err != nil {
			return failure(tx, err)
		}
	}
	result := tx.Where("id = ?", cid).Delete(&model.AddressBookCollection{})
	if result.Error != nil {
		return failure(tx, result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrUnavailable
	}
	return nil
}
