package model

import (
	"database/sql/driver"
	"fmt"
)

// StrategyName binds names as bytes on every database. The public DTO keeps
// a string, including valid UTF8 NUL; text collations and encodings cannot
// silently trim, fold or reject one member of that approved logical domain.
type StrategyName string

func (name StrategyName) Value() (driver.Value, error) { return []byte(name), nil }
func (name *StrategyName) Scan(value any) error {
	switch value := value.(type) {
	case []byte:
		*name = StrategyName(string(value))
		return nil
	case string:
		*name = StrategyName(value)
		return nil
	default:
		return fmt.Errorf("invalid stored strategy name type %T", value)
	}
}

// StrategyDefinition contains only the three approved fields. Its explicit
// dialect migration retains binary uniqueness across every later upgrade.
type StrategyDefinition struct {
	Id            uint         `json:"id" gorm:"primaryKey"`
	Name          StrategyName `json:"name" gorm:"not null"`
	ConfigOptions string       `json:"-" gorm:"type:text;not null"`
}

func (StrategyDefinition) TableName() string { return "strategy_definitions" }
