package clientassign

import (
	"unicode/utf8"

	"github.com/lejianwen/rustdesk-api/v2/internal/strategydefinitions"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

// Caller already holds users/session/peer. A detach requires no definition
// lock; a link holds the matching definition until the same transaction commits.
func assignmentStrategy(tx *gorm.DB, name string) (*uint, error) {
	if name == "" {
		return nil, nil
	}
	if !utf8.ValidString(name) || len(name) > 255 {
		return nil, ErrInput
	}
	var rows []model.StrategyDefinition
	// StrategyName's driver binding preserves bytea/blob/varbinary comparison.
	if err := locked(tx).Where("name = ?", model.StrategyName(name)).Order("id ASC").Limit(2).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) != 1 || rows[0].Id == 0 || string(rows[0].Name) != name {
		return nil, ErrInput
	}
	if _, err := strategydefinitions.DecodeOptions([]byte(rows[0].ConfigOptions)); err != nil {
		return nil, ErrUnavailable
	}
	return &rows[0].Id, nil
}
