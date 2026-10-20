package strategydefinitions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var (
	ErrInput       = errors.New("invalid strategy definition")
	ErrNotFound    = errors.New("strategy definition not found")
	ErrConflict    = errors.New("strategy definition name already exists")
	ErrReferenced  = errors.New("strategy definition is referenced")
	ErrUnavailable = errors.New("strategy definition storage unavailable")
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
	return &Store{db: db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}), now: options.Now}, nil
}

func lock(tx *gorm.DB) *gorm.DB {
	if tx.Dialector.Name() != "sqlite" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return tx
}

func (s *Store) transaction(ctx context.Context, actor auth.Principal, operation func(*gorm.DB) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if actor.User.ID == 0 || actor.Session.ID == 0 || actor.Session.UserID != actor.User.ID || actor.Session.Token == "" || !actor.Session.ExpiresAt.After(s.now()) {
		return auth.ErrUnauthenticated
	}
	work, cancel := context.WithDeadline(ctx, actor.Session.ExpiresAt)
	defer cancel()
	err := s.db.WithContext(work).Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := lock(tx).First(&user, actor.User.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return auth.ErrUnauthenticated
			}
			return err
		}
		if user.Status != model.COMMON_STATUS_ENABLE {
			return auth.ErrUnauthenticated
		}
		if user.IsAdmin == nil || !*user.IsAdmin {
			return auth.ErrForbidden
		}
		var sessions []model.UserToken
		if err := lock(tx).Where("token = ?", actor.Session.Token).Order("id ASC").Limit(2).Find(&sessions).Error; err != nil {
			return err
		}
		if len(sessions) != 1 || sessions[0].Id != actor.Session.ID || sessions[0].UserId != actor.User.ID || sessions[0].Token != actor.Session.Token || sessions[0].ExpiredAt <= s.now().Unix() || !actor.Session.ExpiresAt.After(s.now()) {
			return auth.ErrUnauthenticated
		}
		if err := operation(tx); err != nil {
			return err
		}
		// This second clock check includes time spent waiting for the definition
		// lock. A failure rolls back any mutation made by operation.
		if sessions[0].ExpiredAt <= s.now().Unix() || !actor.Session.ExpiresAt.After(s.now()) {
			return auth.ErrUnauthenticated
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err == nil {
		return nil
	}
	if work.Err() != nil {
		return work.Err()
	}
	for _, known := range []error{auth.ErrUnauthenticated, auth.ErrForbidden, ErrInput, ErrNotFound, ErrConflict, ErrReferenced, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	if translator, ok := s.db.Dialector.(interface{ Translate(error) error }); ok {
		err = translator.Translate(err)
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrConflict
	}
	if errors.Is(err, gorm.ErrForeignKeyViolated) {
		return ErrReferenced
	}
	return ErrUnavailable
}

func dto(row model.StrategyDefinition) (Definition, error) {
	options, err := parseOptions([]byte(row.ConfigOptions))
	if err != nil || !validName(string(row.Name)) {
		return Definition{}, ErrUnavailable
	}
	return Definition{ID: row.Id, Name: string(row.Name), ConfigOptions: options}, nil
}

func (s *Store) Create(ctx context.Context, actor auth.Principal, input Input) (Definition, error) {
	if input.ID != 0 || input.IDPresent || !input.valid() {
		return Definition{}, ErrInput
	}
	var result Definition
	err := s.transaction(ctx, actor, func(tx *gorm.DB) error {
		raw, err := json.Marshal(input.ConfigOptions)
		if err != nil {
			return ErrInput
		}
		row := model.StrategyDefinition{Name: model.StrategyName(input.Name), ConfigOptions: string(raw)}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		result, err = dto(row)
		return err
	})
	if err != nil {
		return Definition{}, err
	}
	return result, nil
}

func (s *Store) Update(ctx context.Context, actor auth.Principal, input Input) (Definition, error) {
	if input.ID == 0 || !input.valid() {
		return Definition{}, ErrInput
	}
	var result Definition
	err := s.transaction(ctx, actor, func(tx *gorm.DB) error {
		var row model.StrategyDefinition
		if err := lock(tx).First(&row, input.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		raw, err := json.Marshal(input.ConfigOptions)
		if err != nil {
			return ErrInput
		}
		if err := tx.Model(&row).Updates(map[string]any{"name": model.StrategyName(input.Name), "config_options": string(raw)}).Error; err != nil {
			return err
		}
		row.Name, row.ConfigOptions = model.StrategyName(input.Name), string(raw)
		result, err = dto(row)
		return err
	})
	if err != nil {
		return Definition{}, err
	}
	return result, nil
}

func (s *Store) Get(ctx context.Context, actor auth.Principal, id uint) (Definition, error) {
	if id == 0 {
		return Definition{}, ErrInput
	}
	var result Definition
	err := s.transaction(ctx, actor, func(tx *gorm.DB) error {
		var row model.StrategyDefinition
		if err := tx.First(&row, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		var err error
		result, err = dto(row)
		return err
	})
	if err != nil {
		return Definition{}, err
	}
	return result, nil
}

func (s *Store) List(ctx context.Context, actor auth.Principal, page Page) (int64, []Definition, error) {
	if !page.valid() {
		return 0, nil, ErrInput
	}
	var total int64
	result := []Definition{}
	err := s.transaction(ctx, actor, func(tx *gorm.DB) error {
		if err := tx.Model(&model.StrategyDefinition{}).Count(&total).Error; err != nil {
			return err
		}
		var rows []model.StrategyDefinition
		if err := tx.Order("id ASC").Offset((page.Current - 1) * page.Size).Limit(page.Size).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			item, err := dto(row)
			if err != nil {
				return err
			}
			result = append(result, item)
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return total, result, nil
}

func (s *Store) Delete(ctx context.Context, actor auth.Principal, id uint) error {
	if id == 0 {
		return ErrInput
	}
	return s.transaction(ctx, actor, func(tx *gorm.DB) error {
		var row model.StrategyDefinition
		if err := lock(tx).First(&row, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		// No device relation exists in this slice. A later approved FK will
		// enforce RESTRICT; its violation is already classified as ErrReferenced.
		return tx.Delete(&row).Error
	})
}
