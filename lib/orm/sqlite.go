package orm

import (
	"fmt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
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
