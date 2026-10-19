// Package database applies startup migrations without publishing a version until
// schema, historical data updates and initial records have all succeeded.
package database

import (
	"errors"
	"fmt"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

// Update preserves the existing version history and runs seed only when there
// is no completed version. Callers must serialize application migrations.
// SQLite/PostgreSQL include schema changes in the transaction. MySQL DDL can
// implicitly commit, so only data changes and the version record are atomic
// there; a failed schema migration can leave changes for a later retry.
func Update(db *gorm.DB, target uint, seed func(*gorm.DB) error) error {
	if db == nil {
		return errors.New("migrate database: nil connection")
	}
	if db.Error != nil {
		return fmt.Errorf("migrate database: %w", db.Error)
	}
	current, fresh, err := currentVersion(db)
	if err != nil {
		return err
	}
	if !fresh && current >= target {
		return nil
	}

	apply := func(tx *gorm.DB) error {
		if db.Dialector.Name() != "mysql" {
			if err := migrateSchema(tx); err != nil {
				return err
			}
		}
		if !fresh {
			if err := migrateHistoricalData(tx, current); err != nil {
				return err
			}
		} else if seed != nil {
			if err := seed(tx); err != nil {
				return fmt.Errorf("seed initial records: %w", err)
			}
		}
		if err := tx.Create(&model.Version{Version: target}).Error; err != nil {
			return fmt.Errorf("record database version %d: %w", target, err)
		}
		return nil
	}
	if db.Dialector.Name() == "mysql" {
		if err := migrateSchema(db); err != nil {
			return err
		}
	}
	if err := db.Transaction(apply); err != nil {
		return fmt.Errorf("migrate database to %d: %w", target, err)
	}
	return nil
}

func currentVersion(db *gorm.DB) (uint, bool, error) {
	// HasTable only returns bool and can hide a failed metadata query. GetTables
	// lets a closed/unavailable database fail instead of being treated as empty.
	tables, err := db.Migrator().GetTables()
	if err != nil {
		return 0, false, fmt.Errorf("inspect database tables: %w", err)
	}
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(&model.Version{}); err != nil {
		return 0, false, fmt.Errorf("resolve version table: %w", err)
	}
	for _, table := range tables {
		if table != stmt.Schema.Table {
			continue
		}
		var version model.Version
		err := db.Last(&version).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, true, nil
		}
		if err != nil {
			return 0, false, fmt.Errorf("read database version: %w", err)
		}
		return version.Version, false, nil
	}
	return 0, true, nil
}

func migrateSchema(db *gorm.DB) error {
	if err := prepareAddressBookNote(db); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	if err := db.AutoMigrate(
		&model.Version{}, &model.User{}, &model.UserToken{}, &model.Tag{},
		&model.AddressBook{}, &model.Peer{}, &model.Group{}, &model.UserThird{},
		&model.Oauth{}, &model.OIDCIdentity{}, &model.LoginLog{}, &model.ShareRecord{}, &model.AuditConn{},
		&model.AuditFile{}, &model.AuditDeliveryReceipt{}, &model.AddressBookCollection{}, &model.AddressBookCollectionRule{},
		&model.ServerCmd{}, &model.DeviceGroup{}, &model.PeerStrategy{}, &model.PeerStrategyState{}, &model.OperationLog{},
	); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	if db.Dialector.Name() == "mysql" {
		// OIDC issuer and subject are byte-exact, independently of the
		// installation's collation or trailing-space comparison rules. This new table has no legacy
		// bindings to infer or backfill.
		if err := db.Exec("ALTER TABLE oidc_identities MODIFY issuer VARBINARY(512) NOT NULL, MODIFY sub VARBINARY(255) NOT NULL").Error; err != nil {
			return fmt.Errorf("set OIDC identity collation: %w", err)
		}
	}
	if !db.Migrator().HasConstraint(&model.OIDCIdentity{}, "User") {
		if err := db.Migrator().CreateConstraint(&model.OIDCIdentity{}, "User"); err != nil {
			return fmt.Errorf("create OIDC identity user foreign key: %w", err)
		}
	}
	if !db.Migrator().HasConstraint(&model.PeerStrategyState{}, "Peer") {
		if err := db.Migrator().CreateConstraint(&model.PeerStrategyState{}, "Peer"); err != nil {
			return fmt.Errorf("create strategy state peer foreign key: %w", err)
		}
	}
	return nil
}

// An existing table needs a nullable column first so rows can be backfilled
// before enforcing NOT NULL. Avoid TEXT DEFAULT syntax unsupported by older
// MySQL releases. Application creates explicitly write the string zero value.
// Also repair NULLs after a partially completed MySQL DDL migration retry.
type nullableAddressBookNote struct {
	Note *string `gorm:"type:text"`
}

func (nullableAddressBookNote) TableName() string { return "address_books" }

func prepareAddressBookNote(db *gorm.DB) error {
	if !db.Migrator().HasTable(&model.AddressBook{}) {
		return nil
	}
	if !db.Migrator().HasColumn(&model.AddressBook{}, "Note") {
		if err := db.Migrator().AddColumn(&nullableAddressBookNote{}, "Note"); err != nil {
			return fmt.Errorf("add address book note: %w", err)
		}
	}
	if err := db.Model(&nullableAddressBookNote{}).Where("note IS NULL").UpdateColumn("note", "").Error; err != nil {
		return fmt.Errorf("backfill address book note: %w", err)
	}
	return nil
}

func migrateHistoricalData(db *gorm.DB, current uint) error {
	if current < 245 {
		updates := []string{
			"update oauths set oauth_type = op",
			"update oauths set issuer = 'https://accounts.google.com' where op = 'google'",
			"update user_thirds set oauth_type = third_type, op = third_type",
		}
		for _, query := range updates {
			if err := db.Exec(query).Error; err != nil {
				return fmt.Errorf("migrate legacy OAuth data (<245): %w", err)
			}
		}
		var identities []model.UserThird
		if err := db.Where("oauth_type = ?", "google").Find(&identities).Error; err != nil {
			return fmt.Errorf("read legacy Google identities: %w", err)
		}
		for _, identity := range identities {
			if identity.UserId == 0 {
				continue
			}
			if err := db.Model(&model.User{}).Where("id = ?", identity.UserId).Update("email", identity.OpenId).Error; err != nil {
				return fmt.Errorf("migrate legacy Google user %d: %w", identity.UserId, err)
			}
		}
	}
	if current < 246 {
		if err := db.Exec("update oauths set issuer = 'https://accounts.google.com' where op = 'google' and issuer is null").Error; err != nil {
			return fmt.Errorf("migrate legacy Google issuer (<246): %w", err)
		}
	}
	return nil
}
