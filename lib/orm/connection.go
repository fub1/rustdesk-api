package orm

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

func closeFailedOpen(db *gorm.DB, cause error) error {
	if db == nil {
		return cause
	}
	if sqlDB, err := db.DB(); err == nil && sqlDB != nil {
		return errors.Join(cause, sqlDB.Close())
	}
	return cause
}

func configureConnection(db *gorm.DB, maxIdle, maxOpen int) (*gorm.DB, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get database connection: %w", err)
	}
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetMaxOpenConns(maxOpen)
	if err := sqlDB.Ping(); err != nil {
		return nil, errors.Join(fmt.Errorf("ping database: %w", err), sqlDB.Close())
	}
	return db, nil
}
