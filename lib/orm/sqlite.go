package orm

import (
	"fmt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net/url"
	"strings"
	"time"
)

type SqliteConfig struct {
	// Dsn defaults to the existing application database; tests can use isolated files.
	Dsn          string
	MaxIdleConns int
	MaxOpenConns int
}

func NewSqlite(sqliteConf *SqliteConfig, logwriter logger.Writer) (*gorm.DB, error) {
	dsn := sqliteConf.Dsn
	if dsn == "" {
		dsn = "./data/rustdeskapi.db?_txlock=immediate&_journal_mode=WAL&_busy_timeout=5000"
	}
	path, query, _ := strings.Cut(dsn, "?")
	options, err := url.ParseQuery(query)
	if err != nil {
		return nil, fmt.Errorf("invalid sqlite connection options")
	}
	// The OIDC identity link has a real user FK. Enable enforcement on every
	// pooled connection, rather than issuing a connection-local PRAGMA once.
	options.Set("_foreign_keys", "on")
	dsn = path + "?" + options.Encode()
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
		Logger: logger.New(
			logwriter, // io writer
			logger.Config{
				SlowThreshold:             time.Second, // Slow SQL threshold
				LogLevel:                  logger.Warn, // Log level
				IgnoreRecordNotFoundError: true,        // Ignore ErrRecordNotFound error for logger
				ParameterizedQueries:      true,        // Don't include params in the SQL log
				Colorful:                  true,
			},
		),
	})
	if err != nil {
		return nil, closeFailedOpen(db, fmt.Errorf("open sqlite database: %w", err))
	}
	return configureConnection(db, sqliteConf.MaxIdleConns, sqliteConf.MaxOpenConns)
}
