package authstore

import (
	"context"
	"errors"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func lockRow(tx *gorm.DB) *gorm.DB {
	if tx.Dialector.Name() != "sqlite" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return tx
}

func (s *Store) Issue(ctx context.Context, input auth.IssuedSession) (auth.Principal, error) {
	var result auth.Principal
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		result, err = IssueInTransaction(tx, input)
		return err
	})
	if err != nil {
		return auth.Principal{}, err
	}
	return result, nil
}

// IssueInTransaction is also used by identity admission, so no helper escapes
// the caller's transaction while creating a user, identity, session or peer link.
func IssueInTransaction(tx *gorm.DB, input auth.IssuedSession) (auth.Principal, error) {
	r := input.Request
	if r.UserID == 0 || input.Token == "" || !input.ExpiresAt.After(input.Now) {
		return auth.Principal{}, auth.ErrInvalidInput
	}
	var user model.User
	if err := lockRow(tx).First(&user, r.UserID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return auth.Principal{}, auth.ErrUnauthenticated
		}
		return auth.Principal{}, err
	}
	if user.Status != model.COMMON_STATUS_ENABLE {
		return auth.Principal{}, auth.ErrDisabled
	}
	if r.ExpectedPassword != nil && user.Password != *r.ExpectedPassword {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if source := r.SourceSession; source != nil {
		if source.UserID != user.Id || source.ID == 0 || !source.ExpiresAt.After(input.Now) {
			return auth.Principal{}, auth.ErrUnauthenticated
		}
		stored, err := uniqueSession(tx, source.Token, true)
		if err != nil {
			return auth.Principal{}, err
		}
		if stored.Id != source.ID || stored.UserId != user.Id || stored.ExpiredAt <= input.Now.Unix() {
			return auth.Principal{}, auth.ErrUnauthenticated
		}
	}
	var count int64
	if err := tx.Model(&model.UserToken{}).Where("token = ?", input.Token).Count(&count).Error; err != nil {
		return auth.Principal{}, err
	}
	if count != 0 {
		return auth.Principal{}, auth.ErrUnavailable
	}
	row := model.UserToken{UserId: user.Id, Token: input.Token, DeviceUuid: r.DeviceUUID, DeviceId: r.DeviceID, ExpiredAt: input.ExpiresAt.Unix()}
	if err := tx.Create(&row).Error; err != nil {
		return auth.Principal{}, err
	}
	log := model.LoginLog{UserId: user.Id, UserTokenId: row.Id, Client: r.Client, DeviceId: r.DeviceID, Uuid: r.DeviceUUID, Ip: r.IP, Type: r.Method, Platform: r.Platform}
	if err := tx.Create(&log).Error; err != nil {
		return auth.Principal{}, err
	}
	if r.DeviceUUID != "" {
		var peer model.Peer
		err := lockRow(tx).Where("uuid = ?", r.DeviceUUID).First(&peer).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return auth.Principal{}, err
		}
		if err == nil {
			if err := tx.Model(&model.Peer{}).Where("row_id = ?", peer.RowId).Update("user_id", user.Id).Error; err != nil {
				return auth.Principal{}, err
			}
		}
	}
	return auth.Principal{User: userValue(user), Session: sessionValue(row)}, nil
}
