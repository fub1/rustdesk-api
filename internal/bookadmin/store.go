// Package bookadmin owns the existing management address-book child writes.
package bookadmin

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type Mode uint8

const (
	My Mode = iota + 1
	Admin
)

type Options struct{ Now func() time.Time }
type Store struct {
	db  *gorm.DB
	now func() time.Time
}
type TagInput struct {
	ID, UserID, CollectionID uint
	Name                     string
	Color                    uint
}
type RuleInput struct {
	ID, UserID, CollectionID, ToID uint
	Type, Rule                     int
}

var (
	ErrInput       = errors.New("invalid address-book input")
	ErrNotFound    = errors.New("address-book item not found")
	ErrUnavailable = errors.New("address-book storage unavailable")
)

func New(db *gorm.DB, options Options) (*Store, error) {
	if db == nil || db.Error != nil {
		return nil, ErrUnavailable
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Store{db: db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}), now: options.Now}, nil
}
func rowLock(tx *gorm.DB) *gorm.DB {
	if tx.Dialector.Name() != "sqlite" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return tx
}
func proof(actor auth.Principal) bool {
	return actor.User.ID != 0 && actor.Session.ID != 0 && actor.Session.UserID == actor.User.ID && actor.Session.Token != ""
}
func classify(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrInput, ErrNotFound, auth.ErrUnauthenticated, auth.ErrForbidden, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrUnavailable
}

type bookScope struct{ OwnerID, CollectionID uint }
type writePlan struct {
	Scopes  []bookScope
	PeerIDs []uint
	UserIDs []uint
}
type lockedWrite struct {
	Peers       map[uint]model.Peer
	Collections map[uint]model.AddressBookCollection
}

func uniqueIDs(ids []uint) []uint {
	seen := map[uint]bool{}
	result := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id != 0 && !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// Candidates supply lock identities only. After the user/session/parent locks,
// callers must re-read the child and compare its immutable scope before writes.
func (s *Store) candidate(ctx context.Context, actor auth.Principal, dest any, id uint, extra ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !proof(actor) || !actor.Session.ExpiresAt.After(s.now()) {
		return auth.ErrUnauthenticated
	}
	work, cancel := context.WithDeadline(ctx, actor.Session.ExpiresAt)
	defer cancel()
	columns := append([]string{"id", "user_id", "collection_id"}, extra...)
	err := s.db.WithContext(work).Select(columns).First(dest, id).Error
	if err != nil && work.Err() != nil {
		return work.Err()
	}
	// Missing rows are resolved after current authentication is rechecked.
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	return classify(err)
}
func (s *Store) createScope(ctx context.Context, actor auth.Principal, mode Mode, ownerID, cid uint) (bookScope, error) {
	if mode == My {
		if ownerID != 0 && ownerID != actor.User.ID {
			return bookScope{}, ErrInput
		}
		ownerID = actor.User.ID
	}
	if cid == 0 {
		if ownerID == 0 {
			return bookScope{}, ErrInput
		}
		return bookScope{ownerID, 0}, nil
	}
	if err := ctx.Err(); err != nil {
		return bookScope{}, err
	}
	if !proof(actor) || !actor.Session.ExpiresAt.After(s.now()) {
		return bookScope{}, auth.ErrUnauthenticated
	}
	work, cancel := context.WithDeadline(ctx, actor.Session.ExpiresAt)
	defer cancel()
	var parent model.AddressBookCollection
	err := s.db.WithContext(work).Select("id", "user_id").First(&parent, cid).Error
	if err != nil && work.Err() != nil {
		return bookScope{}, work.Err()
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return bookScope{ownerID, cid}, nil
	}
	if err != nil {
		return bookScope{}, classify(err)
	}
	if parent.UserId == 0 || (ownerID != 0 && ownerID != parent.UserId) {
		return bookScope{}, ErrInput
	}
	return bookScope{parent.UserId, cid}, nil
}
func (s *Store) run(ctx context.Context, actor auth.Principal, mode Mode, plan writePlan, operation func(*gorm.DB, lockedWrite) error) error {
	if mode != My && mode != Admin {
		return ErrInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !proof(actor) || !actor.Session.ExpiresAt.After(s.now()) {
		return auth.ErrUnauthenticated
	}
	work, cancel := context.WithDeadline(ctx, actor.Session.ExpiresAt)
	defer cancel()
	err := s.db.WithContext(work).Transaction(func(tx *gorm.DB) error {
		ids := append([]uint{actor.User.ID}, plan.UserIDs...)
		for _, scope := range plan.Scopes {
			ids = append(ids, scope.OwnerID)
		}
		var current model.User
		for _, id := range uniqueIDs(ids) {
			var user model.User
			if err := rowLock(tx).First(&user, id).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					if id == actor.User.ID {
						return auth.ErrUnauthenticated
					}
					return ErrInput
				}
				return err
			}
			if id == actor.User.ID {
				current = user
			}
		}
		if current.Status != model.COMMON_STATUS_ENABLE {
			return auth.ErrUnauthenticated
		}
		if mode == Admin && (current.IsAdmin == nil || !*current.IsAdmin) {
			return auth.ErrForbidden
		}
		var session model.UserToken
		if err := rowLock(tx).First(&session, actor.Session.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return auth.ErrUnauthenticated
			}
			return err
		}
		// Do not take token-secondary locks after a session PK lock. Raw-token
		// ambiguity is checked by an ordinary read; legitimate issuance takes
		// the actor user lock already held by this transaction.
		var matches []model.UserToken
		if err := tx.Select("id").Where("token = ?", actor.Session.Token).Limit(2).Find(&matches).Error; err != nil {
			return err
		}
		valid := func() bool {
			return len(matches) == 1 && matches[0].Id == actor.Session.ID && session.Id == actor.Session.ID && session.UserId == actor.User.ID && session.Token == actor.Session.Token && session.ExpiredAt > s.now().Unix() && actor.Session.ExpiresAt.After(s.now())
		}
		if !valid() {
			return auth.ErrUnauthenticated
		}
		for _, scope := range plan.Scopes {
			if mode == My && scope.OwnerID != actor.User.ID {
				return auth.ErrForbidden
			}
		}
		locked := lockedWrite{Peers: map[uint]model.Peer{}, Collections: map[uint]model.AddressBookCollection{}}
		for _, id := range uniqueIDs(plan.PeerIDs) {
			var peer model.Peer
			if err := rowLock(tx).First(&peer, id).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrNotFound
				}
				return err
			}
			locked.Peers[id] = peer
		}
		var cids []uint
		owners := map[uint]uint{}
		for _, scope := range plan.Scopes {
			if scope.CollectionID != 0 {
				if owner, ok := owners[scope.CollectionID]; ok && owner != scope.OwnerID {
					return ErrInput
				}
				owners[scope.CollectionID] = scope.OwnerID
				cids = append(cids, scope.CollectionID)
			}
		}
		for _, cid := range uniqueIDs(cids) {
			var parent model.AddressBookCollection
			if err := rowLock(tx).First(&parent, cid).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrNotFound
				}
				return err
			}
			if parent.UserId != owners[cid] {
				return ErrInput
			}
			locked.Collections[cid] = parent
		}
		if !valid() {
			return auth.ErrUnauthenticated
		}
		if err := operation(tx, locked); err != nil {
			return err
		}
		if !valid() {
			return auth.ErrUnauthenticated
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil && work.Err() != nil {
		return work.Err()
	}
	return classify(err)
}
func fixedScope(inputOwner, inputCID uint, actual bookScope) error {
	if (inputOwner != 0 && inputOwner != actual.OwnerID) || (inputCID != 0 && inputCID != actual.CollectionID) {
		return ErrInput
	}
	return nil
}
func sameScope(candidate, actual bookScope) bool { return candidate == actual }

func (s *Store) missing(ctx context.Context, actor auth.Principal, mode Mode) error {
	return s.run(ctx, actor, mode, writePlan{}, func(*gorm.DB, lockedWrite) error { return ErrNotFound })
}
