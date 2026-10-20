// Package useradmin owns existing administrator account writes independently of legacy globals.
package useradmin

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionguard"
	"github.com/lejianwen/rustdesk-api/v2/internal/userguard"
	"github.com/lejianwen/rustdesk-api/v2/internal/userlifecycle"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/utils"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Options struct{ Now func() time.Time }
type Store struct {
	db  *gorm.DB
	now func() time.Time
}
type UserInput struct {
	ID                                        uint
	Username, Email, Nickname, Avatar, Remark string
	GroupID                                   uint
	IsAdmin                                   *bool
	Status                                    model.StatusCode
}
type PasswordInput struct {
	ID       uint
	Password string
}

var (
	ErrInput            = errors.New("invalid user input")
	ErrNotFound         = errors.New("user not found")
	ErrUnavailable      = errors.New("user storage unavailable")
	ErrOldPassword      = errors.New("old password is incorrect")
	ErrLastEnabledAdmin = userguard.ErrLastEnabledAdmin
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
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, collectionguard.ErrReferenced) {
		return collectionguard.ErrReferenced
	}
	for _, known := range []error{ErrInput, ErrNotFound, ErrUnavailable, ErrOldPassword, ErrLastEnabledAdmin, auth.ErrUnauthenticated, auth.ErrForbidden, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrUnavailable
}
func (in UserInput) model() model.User {
	return model.User{IdModel: model.IdModel{Id: in.ID}, Username: in.Username, Email: in.Email, Nickname: in.Nickname, Avatar: in.Avatar, Remark: in.Remark, GroupId: in.GroupID, IsAdmin: in.IsAdmin, Status: in.Status}
}

func (s *Store) run(ctx context.Context, actor auth.Principal, needAdmin bool, targetID uint, op func(*gorm.DB, map[uint]model.User, model.User) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if actor.User.ID == 0 || actor.Session.ID == 0 || actor.Session.UserID != actor.User.ID || actor.Session.Token == "" || !actor.Session.ExpiresAt.After(s.now()) {
		return auth.ErrUnauthenticated
	}
	work, cancel := context.WithDeadline(ctx, actor.Session.ExpiresAt)
	defer cancel()
	err := s.db.WithContext(work).Transaction(func(tx *gorm.DB) error {
		locked, err := userguard.LockUsers(tx, actor.User.ID, targetID)
		if err != nil {
			return err
		}
		current, ok := locked[actor.User.ID]
		if !ok || current.Status != model.COMMON_STATUS_ENABLE {
			return auth.ErrUnauthenticated
		}
		if needAdmin && (current.IsAdmin == nil || !*current.IsAdmin) {
			return auth.ErrForbidden
		}
		var session model.UserToken
		if err := userguard.Rows(tx).First(&session, actor.Session.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return auth.ErrUnauthenticated
			}
			return err
		}
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
		var target model.User
		if targetID != 0 {
			var exists bool
			target, exists = locked[targetID]
			if !exists {
				return ErrNotFound
			}
		}
		if err := op(tx, locked, target); err != nil {
			return err
		}
		// Valid self actions intentionally remove/disable/demote actor or revoke its
		// own row. The immutable pre-write proof remains held; only time/context can
		// invalidate authorization at this point.
		if err := work.Err(); err != nil {
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

func (s *Store) Create(ctx context.Context, actor auth.Principal, in UserInput) error {
	normalized := strings.ToLower(strings.ReplaceAll(in.Username, " ", ""))
	if in.ID != 0 || normalized == "" {
		return ErrInput
	}
	hash, err := utils.EncryptPassword(utils.DefaultInitPassword)
	if err != nil {
		return ErrUnavailable
	}
	return s.run(ctx, actor, true, 0, func(tx *gorm.DB, _ map[uint]model.User, _ model.User) error {
		user := in.model()
		user.Username = normalized
		user.Password = hash
		return tx.Create(&user).Error
	})
}
func (s *Store) Update(ctx context.Context, actor auth.Principal, in UserInput) error {
	if in.ID == 0 {
		return ErrInput
	}
	return s.run(ctx, actor, true, in.ID, func(tx *gorm.DB, locked map[uint]model.User, current model.User) error {
		next, values := userguard.Patch(current, in.model())
		if err := userguard.CheckRemoval(locked, current, &next); err != nil {
			return err
		}
		if len(values) > 0 {
			if err := tx.Model(&model.User{}).Where("id = ?", current.Id).Updates(values).Error; err != nil {
				return err
			}
		}
		// Upgrading an inactive account may encounter sessions predating the
		// disable policy; enabling it must require fresh login as well.
		if current.Status != model.COMMON_STATUS_ENABLE || next.Status != model.COMMON_STATUS_ENABLE {
			return userlifecycle.RevokeLocked(tx, current.Id)
		}
		return nil
	})
}
func (s *Store) Delete(ctx context.Context, actor auth.Principal, id uint) error {
	if id == 0 {
		return ErrInput
	}
	return s.run(ctx, actor, true, id, func(tx *gorm.DB, locked map[uint]model.User, current model.User) error {
		if err := userguard.CheckRemoval(locked, current, nil); err != nil {
			return err
		}
		return userlifecycle.DeleteLocked(tx, &current)
	})
}
func (s *Store) ResetPassword(ctx context.Context, actor auth.Principal, in PasswordInput) error {
	if in.ID == 0 || in.Password == "" {
		return ErrInput
	}
	hash, err := utils.EncryptPassword(in.Password)
	if err != nil {
		return ErrUnavailable
	}
	return s.run(ctx, actor, true, in.ID, func(tx *gorm.DB, _ map[uint]model.User, current model.User) error {
		return setPassword(tx, current, hash)
	})
}
func (s *Store) ChangePassword(ctx context.Context, actor auth.Principal, old, password string) error {
	if password == "" {
		return ErrInput
	}
	hash, err := utils.EncryptPassword(password)
	if err != nil {
		return ErrUnavailable
	}
	return s.run(ctx, actor, false, actor.User.ID, func(tx *gorm.DB, _ map[uint]model.User, current model.User) error {
		if current.Password != "" {
			ok, _, err := utils.VerifyPassword(current.Password, old)
			if err != nil || !ok {
				return ErrOldPassword
			}
		}
		return setPassword(tx, current, hash)
	})
}
func setPassword(tx *gorm.DB, current model.User, hash string) error {
	result := tx.Model(&model.User{}).Where("id = ? AND password = ?", current.Id, current.Password).Update("password", hash)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrUnavailable
	}
	return userlifecycle.RevokeLocked(tx, current.Id)
}
