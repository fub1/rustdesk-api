package clientsysinfo

import (
	"context"
	"errors"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrInput       = errors.New("invalid sysinfo input")
	ErrUnavailable = errors.New("sysinfo storage unavailable")
)

type Store struct{ db *gorm.DB }

func New(db *gorm.DB) (*Store, error) {
	if db == nil || db.Error != nil {
		return nil, ErrUnavailable
	}
	return &Store{db: db}, nil
}

func (s *Store) Report(ctx context.Context, input Input) error {
	if input.ID == "" {
		return ErrInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.db == nil || s.db.Error != nil {
		return ErrUnavailable
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var peer model.Peer
		lookup := tx.Where("id = ?", input.ID)
		// Serialize reports with deletion of an existing target. Do not use a
		// changed-row count as existence proof: MySQL may report zero for an
		// identical snapshot. SQLite's transaction conflict fails and rolls back.
		if tx.Dialector.Name() != "sqlite" {
			lookup = lookup.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		err := lookup.First(&peer).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Preserve the legacy registration association. This is not a new
			// authentication rule, nor is a report allowed to reassign an owner.
			var login model.LoginLog
			if err := tx.Select("user_id").Where("uuid = ? AND device_id = ?", input.UUID.Value, input.ID).Order("id DESC").First(&login).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			peer = model.Peer{Id: input.ID, Cpu: input.CPU.Value, Hostname: input.Hostname.Value, Memory: input.Memory.Value, Os: input.OS.Value, Username: input.Username.Value, Uuid: input.UUID.Value, Version: input.Version.Value, UserId: login.UserId}
			if err := tx.Create(&peer).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Model(&model.Peer{}).Where("row_id = ?", peer.RowId).Updates(input.values()).Error; err != nil {
				return err
			}
		}
		return legacyPreset(tx, peer.RowId, input)
	})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrUnavailable
}
