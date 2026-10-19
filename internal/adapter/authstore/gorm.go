// Package authstore adapts legacy GORM records to framework-independent auth
// values. Each Store owns its database reference and never reads application globals.
package authstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type Store struct{ db *gorm.DB }

var _ auth.Store = (*Store)(nil)

func New(db *gorm.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("auth database is required")
	}
	if db.Error != nil {
		return nil, fmt.Errorf("auth database: %w", db.Error)
	}
	// Authentication SQL contains raw credentials in WHERE clauses. Do not rely
	// on the application's logger configuration to redact them.
	return &Store{db: db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})}, nil
}

func (s *Store) DeleteSessions(ctx context.Context, ids []uint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if id == 0 {
			return auth.ErrInvalidInput
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Where("id IN ?", ids).Delete(&model.UserToken{}).Error
}

func (s *Store) Lookup(ctx context.Context, token string) (auth.User, auth.Session, error) {
	if err := ctx.Err(); err != nil {
		return auth.User{}, auth.Session{}, err
	}
	row, err := uniqueSession(s.db.WithContext(ctx), token, false)
	if err != nil {
		return auth.User{}, auth.Session{}, err
	}
	var user model.User
	if err := s.db.WithContext(ctx).First(&user, row.UserId).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return auth.User{}, auth.Session{}, auth.ErrUnauthenticated
		}
		return auth.User{}, auth.Session{}, err
	}
	return userValue(user), sessionValue(row), nil
}

func (s *Store) Renew(ctx context.Context, session auth.Session, expiresAt, now time.Time) (auth.Session, error) {
	if err := ctx.Err(); err != nil {
		return auth.Session{}, err
	}
	var result auth.Session
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := uniqueSession(tx, session.Token, true)
		if err != nil {
			return err
		}
		if row.Id != session.ID || row.UserId != session.UserID || row.ExpiredAt <= now.Unix() {
			return auth.ErrUnauthenticated
		}
		// A concurrent renewal may already have extended this row. Never shorten
		// it or recreate it. The caller independently enforces any JWT lifetime.
		if row.ExpiredAt >= expiresAt.Unix() {
			result = sessionValue(row)
			return nil
		}
		update := tx.Model(&model.UserToken{}).
			Where("id = ? AND user_id = ? AND token = ? AND expired_at = ? AND expired_at > ?", row.Id, row.UserId, row.Token, row.ExpiredAt, now.Unix()).
			Update("expired_at", expiresAt.Unix())
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return auth.ErrUnauthenticated
		}
		row.ExpiredAt = expiresAt.Unix()
		result = sessionValue(row)
		return nil
	})
	return result, err
}

func (s *Store) Revoke(ctx context.Context, session auth.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := uniqueSession(tx, session.Token, true)
		if err != nil {
			return err
		}
		if row.Id != session.ID || row.UserId != session.UserID {
			return auth.ErrUnauthenticated
		}
		deleted := tx.Where("id = ? AND user_id = ? AND token = ?", row.Id, row.UserId, row.Token).Delete(&model.UserToken{})
		if deleted.Error != nil {
			return deleted.Error
		}
		if deleted.RowsAffected != 1 {
			return auth.ErrUnauthenticated
		}
		if row.DeviceUuid == "" {
			return nil
		}
		// Preserve UuidUnbindUserId's first matching peer semantics, but read
		// the UUID from the stored session and keep both writes atomic.
		var peer model.Peer
		query := tx.Where("uuid = ? AND user_id = ?", row.DeviceUuid, row.UserId)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.First(&peer).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		return tx.Model(&model.Peer{}).Where("row_id = ? AND uuid = ? AND user_id = ?", peer.RowId, row.DeviceUuid, row.UserId).Update("user_id", 0).Error
	})
}

func uniqueSession(db *gorm.DB, token string, lock bool) (model.UserToken, error) {
	if token == "" {
		return model.UserToken{}, auth.ErrUnauthenticated
	}
	query := db.Where("token = ?", token).Order("id").Limit(2)
	if lock && db.Dialector.Name() != "sqlite" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var rows []model.UserToken
	if err := query.Find(&rows).Error; err != nil {
		return model.UserToken{}, err
	}
	if len(rows) != 1 || rows[0].Id == 0 || rows[0].UserId == 0 {
		return model.UserToken{}, auth.ErrUnauthenticated
	}
	return rows[0], nil
}

func userValue(row model.User) auth.User {
	return auth.User{
		ID: row.Id, Username: row.Username, Email: row.Email, Nickname: row.Nickname,
		Avatar: row.Avatar, GroupID: row.GroupId, IsAdmin: row.IsAdmin != nil && *row.IsAdmin,
		Enabled: row.Status == model.COMMON_STATUS_ENABLE, PasswordHash: row.Password, Remark: row.Remark,
		CreatedAt: time.Time(row.CreatedAt).UTC(), UpdatedAt: time.Time(row.UpdatedAt).UTC(),
	}
}

func sessionValue(row model.UserToken) auth.Session {
	return auth.Session{
		ID: row.Id, UserID: row.UserId, Token: row.Token, DeviceUUID: row.DeviceUuid,
		DeviceID: row.DeviceId, ExpiresAt: time.Unix(row.ExpiredAt, 0).UTC(),
	}
}
