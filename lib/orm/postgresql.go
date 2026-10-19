package orm

import (
	"fmt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"time"
)

type PostgresqlConfig struct {
	Dsn          string
	MaxIdleConns int
	MaxOpenConns int
}

func NewPostgresql(conf *PostgresqlConfig, logwriter logger.Writer) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(conf.Dsn), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
		Logger: logger.New(
			logwriter, // io writer
			logger.Config{
				SlowThreshold: time.Second, // Slow SQL threshold
				LogLevel:      logger.Warn, // Log level
				//IgnoreRecordNotFoundError: true,        // Ignore ErrRecordNotFound error for logger
				ParameterizedQueries: true, // Don't include params in the SQL log
				Colorful:             true,
			},
		),
	})
	if err != nil {
		return nil, closeFailedOpen(db, fmt.Errorf("open postgresql database: %w", err))
	}
	return configureConnection(db, conf.MaxIdleConns, conf.MaxOpenConns)
}
