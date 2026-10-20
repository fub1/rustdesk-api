// Package collectionadmin owns the existing management collection lifecycle.
// Collection owners are immutable; sharing a book does not grant metadata rights.
package collectionadmin

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionguard"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type Mode uint8

const (
	My Mode = iota + 1
	Admin
)

type Input struct {
	ID, UserID uint
	Name       string
}
type Page struct{ Current, Size int }
type Collection struct {
	ID        uint                  `json:"id"`
	UserID    uint                  `json:"user_id"`
	Name      string                `json:"name"`
	CreatedAt custom_types.AutoTime `json:"created_at"`
	UpdatedAt custom_types.AutoTime `json:"updated_at"`
}
type Options struct{ Now func() time.Time }
type Store struct {
	db  *gorm.DB
	now func() time.Time
}

var (
	ErrInput       = errors.New("invalid collection input")
	ErrNotFound    = errors.New("collection not found")
	ErrReferenced  = errors.New("collection is referenced")
	ErrUnavailable = errors.New("collection storage unavailable")
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
func projection(row model.AddressBookCollection) Collection {
	return Collection{ID: row.Id, UserID: row.UserId, Name: row.Name, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}
func proof(actor auth.Principal) bool {
	return actor.User.ID != 0 && actor.Session.ID != 0 && actor.Session.UserID == actor.User.ID && actor.Session.Token != ""
}
func classify(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrInput, ErrNotFound, ErrReferenced, auth.ErrUnauthenticated, auth.ErrForbidden, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	if errors.Is(err, collectionguard.ErrReferenced) {
		return ErrReferenced
	}
	return ErrUnavailable
}

// Candidate reads supply user lock identities only. Every row and its immutable
// owner are checked again after users/session locks inside the transaction.
func (s *Store) candidate(ctx context.Context, actor auth.Principal, id uint) (model.AddressBookCollection, error) {
	var row model.AddressBookCollection
	if err := ctx.Err(); err != nil {
		return row, err
	}
	if !proof(actor) || !actor.Session.ExpiresAt.After(s.now()) {
		return row, auth.ErrUnauthenticated
	}
	work, cancel := context.WithDeadline(ctx, actor.Session.ExpiresAt)
	defer cancel()
	err := s.db.WithContext(work).Select("id", "user_id").First(&row, id).Error
	if err != nil && work.Err() != nil {
		return row, work.Err()
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, nil
	}
	return row, classify(err)
}
func (s *Store) run(ctx context.Context, actor auth.Principal, mode Mode, ownerID uint, ownerRequired bool, operation func(*gorm.DB) error) error {
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
		ids := []uint{actor.User.ID}
		if ownerID != 0 && ownerID != actor.User.ID {
			ids = append(ids, ownerID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		var current, owner model.User
		for _, id := range ids {
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
			if id == ownerID {
				owner = user
			}
		}
		if current.Status != model.COMMON_STATUS_ENABLE {
			return auth.ErrUnauthenticated
		}
		if mode == Admin && (current.IsAdmin == nil || !*current.IsAdmin) {
			return auth.ErrForbidden
		}
		var sessions []model.UserToken
		if err := rowLock(tx).Where("token = ?", actor.Session.Token).Order("id ASC").Limit(2).Find(&sessions).Error; err != nil {
			return err
		}
		if len(sessions) != 1 || sessions[0].Id != actor.Session.ID || sessions[0].UserId != actor.User.ID || sessions[0].Token != actor.Session.Token || sessions[0].ExpiredAt <= s.now().Unix() || !actor.Session.ExpiresAt.After(s.now()) {
			return auth.ErrUnauthenticated
		}
		if ownerRequired && (owner.Id == 0 || owner.Status != model.COMMON_STATUS_ENABLE) {
			return ErrInput
		}
		if err := operation(tx); err != nil {
			return err
		}
		if sessions[0].ExpiredAt <= s.now().Unix() || !actor.Session.ExpiresAt.After(s.now()) {
			return auth.ErrUnauthenticated
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil && work.Err() != nil {
		return work.Err()
	}
	return classify(err)
}
func lockedCollection(tx *gorm.DB, id uint, candidate model.AddressBookCollection, actor auth.Principal, mode Mode) (model.AddressBookCollection, error) {
	var row model.AddressBookCollection
	if err := rowLock(tx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return row, ErrNotFound
		}
		return row, err
	}
	if row.UserId != candidate.UserId {
		return row, ErrInput
	}
	if mode == My && row.UserId != actor.User.ID {
		return row, auth.ErrForbidden
	}
	return row, nil
}
func (s *Store) Create(ctx context.Context, actor auth.Principal, mode Mode, input Input) (Collection, error) {
	if input.ID != 0 || input.Name == "" {
		return Collection{}, ErrInput
	}
	owner := input.UserID
	if mode == My {
		if owner != 0 && owner != actor.User.ID {
			return Collection{}, ErrInput
		}
		owner = actor.User.ID
	}
	if owner == 0 {
		return Collection{}, ErrInput
	}
	var result Collection
	err := s.run(ctx, actor, mode, owner, true, func(tx *gorm.DB) error {
		row := model.AddressBookCollection{UserId: owner, Name: input.Name}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		result = projection(row)
		return nil
	})
	if err != nil {
		return Collection{}, err
	}
	return result, nil
}
func (s *Store) Update(ctx context.Context, actor auth.Principal, mode Mode, input Input) (Collection, error) {
	if input.ID == 0 || input.Name == "" {
		return Collection{}, ErrInput
	}
	candidate, err := s.candidate(ctx, actor, input.ID)
	if err != nil {
		return Collection{}, err
	}
	var result Collection
	err = s.run(ctx, actor, mode, candidate.UserId, false, func(tx *gorm.DB) error {
		row, err := lockedCollection(tx, input.ID, candidate, actor, mode)
		if err != nil {
			return err
		}
		if input.UserID != 0 && input.UserID != row.UserId {
			return ErrInput
		}
		// Explicit columns: protected owner/ID/time fields never come from input.
		if err := tx.Model(&row).Update("name", input.Name).Error; err != nil {
			return err
		}
		result = projection(row)
		return nil
	})
	if err != nil {
		return Collection{}, err
	}
	return result, nil
}
func (s *Store) Get(ctx context.Context, actor auth.Principal, mode Mode, id uint) (Collection, error) {
	if id == 0 {
		return Collection{}, ErrInput
	}
	candidate, err := s.candidate(ctx, actor, id)
	if err != nil {
		return Collection{}, err
	}
	var result Collection
	err = s.run(ctx, actor, mode, candidate.UserId, false, func(tx *gorm.DB) error {
		row, err := lockedCollection(tx, id, candidate, actor, mode)
		if err != nil {
			return err
		}
		result = projection(row)
		return nil
	})
	if err != nil {
		return Collection{}, err
	}
	return result, nil
}
func (s *Store) List(ctx context.Context, actor auth.Principal, mode Mode, page Page, ownerFilter uint) (int64, []Collection, error) {
	if page.Current <= 0 || page.Size <= 0 || page.Size > 1000 || page.Current-1 > int(^uint(0)>>1)/page.Size {
		return 0, nil, ErrInput
	}
	result := []Collection{}
	var total int64
	err := s.run(ctx, actor, mode, 0, false, func(tx *gorm.DB) error {
		query := tx.Model(&model.AddressBookCollection{})
		if mode == My {
			query = query.Where("user_id = ?", actor.User.ID)
		} else if ownerFilter != 0 {
			query = query.Where("user_id = ?", ownerFilter)
		}
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		var rows []model.AddressBookCollection
		if err := query.Order("id ASC").Offset((page.Current - 1) * page.Size).Limit(page.Size).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			result = append(result, projection(row))
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return total, result, nil
}
func (s *Store) Delete(ctx context.Context, actor auth.Principal, mode Mode, input Input) error {
	if input.ID == 0 {
		return ErrInput
	}
	candidate, err := s.candidate(ctx, actor, input.ID)
	if err != nil {
		return err
	}
	return s.run(ctx, actor, mode, candidate.UserId, false, func(tx *gorm.DB) error {
		// A shared full-control grant cannot authorize collection metadata or delete.
		if mode == My && candidate.Id != 0 && candidate.UserId != actor.User.ID {
			return auth.ErrForbidden
		}
		if err := collectionguard.EnsureUnreferenced(tx, []uint{input.ID}); err != nil {
			return err
		}
		row, err := lockedCollection(tx, input.ID, candidate, actor, mode)
		if err != nil {
			return err
		}
		if input.UserID != 0 && input.UserID != row.UserId {
			return ErrInput
		}
		if err := collectionguard.EnsureUnreferenced(tx, []uint{row.Id}); err != nil {
			return err
		}
		return collectionguard.DeleteContents(tx, row.Id)
	})
}
