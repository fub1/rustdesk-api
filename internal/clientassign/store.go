package clientassign

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var (
	ErrInput       = errors.New("invalid device assignment")
	ErrNotFound    = errors.New("assignment target not found")
	ErrUnavailable = errors.New("assignment storage unavailable")
)

type Options struct{ Now func() time.Time }
type Store struct {
	db  *gorm.DB
	now func() time.Time
}

func New(db *gorm.DB, options Options) (*Store, error) {
	if db == nil || db.Error != nil {
		return nil, ErrUnavailable
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	// Session SQL contains a raw bearer credential, including on failed queries.
	return &Store{db: db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}), now: options.Now}, nil
}

func locked(tx *gorm.DB) *gorm.DB {
	if tx.Dialector.Name() != "sqlite" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return tx
}

func exact(tx *gorm.DB, column, value string) *gorm.DB {
	switch tx.Dialector.Name() {
	case "mysql":
		return tx.Where("BINARY "+column+" = ?", value)
	case "sqlite":
		return tx.Where(column+" COLLATE BINARY = ?", value)
	default:
		return tx.Where(column+" = ?", value)
	}
}

func (s *Store) Assign(ctx context.Context, actor auth.Principal, input Input) error {
	if !input.valid() {
		return ErrInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if actor.User.ID == 0 || actor.Session.ID == 0 || actor.Session.UserID != actor.User.ID || actor.Session.Token == "" || !actor.Session.ExpiresAt.After(s.now()) {
		return auth.ErrUnauthenticated
	}
	// Bound lock waits and database work by the middleware's effective expiry,
	// which includes JWT expiry even if the stored session lasts longer.
	work, cancel := context.WithDeadline(ctx, actor.Session.ExpiresAt)
	defer cancel()
	err := s.db.WithContext(work).Transaction(func(tx *gorm.DB) error {
		ids := []uint{actor.User.ID}
		var targetUser model.User
		var ownerCandidate model.Peer
		if input.UserName.Present {
			var candidates []model.User
			if err := exact(tx, "username", input.UserName.Value).Order("id ASC").Limit(2).Find(&candidates).Error; err != nil {
				return err
			}
			if len(candidates) != 1 || candidates[0].Username != input.UserName.Value {
				return ErrInput
			}
			targetUser = candidates[0]
		} else if input.AddressBookName.Present {
			// This unlocked read supplies only the user-lock candidate. Recheck
			// it after locking users/session/peer; never acquire a new owner lock
			// in reverse order while already holding the device row.
			var candidates []model.Peer
			if err := exact(tx, "id", input.ID).Select("row_id, id, uuid, user_id").Order("row_id ASC").Limit(2).Find(&candidates).Error; err != nil {
				return err
			}
			if len(candidates) != 1 || candidates[0].Id != input.ID || candidates[0].Uuid != input.UUID {
				return ErrNotFound
			}
			ownerCandidate = candidates[0]
			if ownerCandidate.UserId == 0 {
				return ErrInput
			}
			targetUser.Id = ownerCandidate.UserId
		}
		if targetUser.Id != 0 && targetUser.Id != actor.User.ID {
			ids = append(ids, targetUser.Id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			var user model.User
			if err := locked(tx).First(&user, id).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					if id != actor.User.ID {
						return ErrInput
					}
					return auth.ErrUnauthenticated
				}
				return err
			}
			if user.Id == actor.User.ID {
				if user.Status != model.COMMON_STATUS_ENABLE {
					return auth.ErrUnauthenticated
				}
				if user.IsAdmin == nil || !*user.IsAdmin {
					return auth.ErrForbidden
				}
			}
			if user.Id == targetUser.Id {
				if input.UserName.Present && user.Username != input.UserName.Value || user.Status != model.COMMON_STATUS_ENABLE {
					return ErrInput
				}
				targetUser = user
			}
		}
		// Recheck the unique live session, rather than trusting a pre-transaction
		// middleware principal after a concurrent revocation/role change.
		var sessions []model.UserToken
		if err := locked(tx).Where("token = ?", actor.Session.Token).Order("id ASC").Limit(2).Find(&sessions).Error; err != nil {
			return err
		}
		if len(sessions) != 1 || sessions[0].Id != actor.Session.ID || sessions[0].UserId != actor.User.ID || sessions[0].Token != actor.Session.Token || sessions[0].ExpiredAt <= s.now().Unix() {
			return auth.ErrUnauthenticated
		}
		var peers []model.Peer
		if err := exact(locked(tx), "id", input.ID).Order("row_id ASC").Limit(2).Find(&peers).Error; err != nil {
			return err
		}
		if len(peers) != 1 || peers[0].Id != input.ID || peers[0].Uuid != input.UUID {
			return ErrNotFound
		}
		if input.AddressBookName.Present && !input.UserName.Present && (peers[0].UserId != targetUser.Id || peers[0].RowId != ownerCandidate.RowId) {
			return ErrInput
		}
		values := map[string]any{}
		if input.StrategyName.Present {
			strategyID, err := assignmentStrategy(tx, input.StrategyName.Value)
			if err != nil {
				return err
			}
			values["strategy_definition_id"] = strategyID
		}
		if input.UserName.Present {
			values["user_id"] = targetUser.Id
		}
		if input.DeviceGroupName.Present {
			var groups []model.DeviceGroup
			if err := exact(locked(tx), "name", input.DeviceGroupName.Value).Order("id ASC").Limit(2).Find(&groups).Error; err != nil {
				return err
			}
			if len(groups) != 1 || groups[0].Name != input.DeviceGroupName.Value {
				return ErrInput
			}
			values["group_id"] = groups[0].Id
		}
		for column, field := range map[string]OptionalString{"hostname": input.DeviceName, "username": input.DeviceUsername, "note": input.Note} {
			if field.Present {
				values[column] = field.Value
			}
		}
		// The row is already locked and exists. A MySQL same-value UPDATE may
		// report zero changed rows, which does not make this a missing target.
		if len(values) != 0 {
			if err := tx.Model(&model.Peer{}).Where("row_id = ?", peers[0].RowId).Updates(values).Error; err != nil {
				return err
			}
		}
		if input.AddressBookName.Present {
			peer := peers[0]
			if input.DeviceName.Present {
				peer.Hostname = input.DeviceName.Value
			}
			if input.DeviceUsername.Present {
				peer.Username = input.DeviceUsername.Value
			}
			if err := assignAddressBook(tx, targetUser.Id, peer, input); err != nil {
				return err
			}
		}
		if !actor.Session.ExpiresAt.After(s.now()) || sessions[0].ExpiredAt <= s.now().Unix() {
			return auth.ErrUnauthenticated
		}
		return nil
	})
	if err == nil {
		return nil
	}
	if work.Err() != nil {
		return work.Err()
	}
	for _, known := range []error{ErrInput, ErrNotFound, auth.ErrUnauthenticated, auth.ErrForbidden, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrUnavailable
}
