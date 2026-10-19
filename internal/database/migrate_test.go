package database

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const testVersion uint = 278

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "migration.db")
	db, err := gorm.Open(sqlite.Open(path+"?_txlock=immediate&_busy_timeout=5000"), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
		Logger:                                   logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func seedTestData(tx *gorm.DB) error {
	return SeedInitialData(tx, "Default", "Shared", "test-password-hash")
}

func mustExec(t *testing.T, db *gorm.DB, sql string) {
	t.Helper()
	if err := db.Exec(sql).Error; err != nil {
		t.Fatal(err)
	}
}

func rowCount(t *testing.T, db *gorm.DB, value interface{}) int64 {
	t.Helper()
	var count int64
	if err := db.Model(value).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func assertCurrentVersion(t *testing.T, db *gorm.DB, want uint) {
	t.Helper()
	got, fresh, err := currentVersion(db)
	if err != nil || fresh || got != want {
		t.Fatalf("current version = (%d, fresh=%v, %v), want %d", got, fresh, err, want)
	}
}

func TestFreshMigrationAndRepeatedStartup(t *testing.T) {
	db := openTestDB(t)
	if err := Update(db, testVersion, seedTestData); err != nil {
		t.Fatal(err)
	}
	assertCurrentVersion(t, db, testVersion)
	var admin model.User
	if err := db.Where("username = ?", "admin").First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	var group model.Group
	if err := db.First(&group, admin.GroupId).Error; err != nil {
		t.Fatal(err)
	}
	if admin.Password != "test-password-hash" || admin.IsAdmin == nil || !*admin.IsAdmin || group.Type != model.GroupTypeDefault {
		t.Fatalf("initial admin/group not initialized correctly: %+v / %+v", admin, group)
	}
	if err := Update(db, testVersion, func(*gorm.DB) error {
		t.Fatal("must not seed an initialized database")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rowCount(t, db, &model.Version{}) != 1 || rowCount(t, db, &model.User{}) != 1 || rowCount(t, db, &model.Group{}) != 2 {
		t.Fatal("repeated startup duplicated version or initial records")
	}
}

func legacyDB(t *testing.T, version uint) *gorm.DB {
	t.Helper()
	db := openTestDB(t)
	if err := migrateSchema(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Version{Version: version}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.User{Username: "legacy", Email: "old@example.test"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Oauth{Op: "google", OauthType: "legacy"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserThird{
		UserId: 1, ThirdType: "google", OauthType: "legacy", Op: "old",
		OauthUser: model.OauthUser{OpenId: "migrated@example.test"},
	}).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func TestUpgrade244AppliesDataBeforeRecordingVersion(t *testing.T) {
	db := legacyDB(t, 244)
	if err := Update(db, testVersion, func(*gorm.DB) error {
		t.Fatal("upgrade must not create initial records")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertCurrentVersion(t, db, testVersion)
	var provider model.Oauth
	var identity model.UserThird
	var user model.User
	for _, value := range []interface{}{&provider, &identity, &user} {
		if err := db.First(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if provider.OauthType != "google" || provider.Issuer != model.IssuerGoogle || identity.Op != "google" || identity.OauthType != "google" || user.Email != "migrated@example.test" {
		t.Fatalf("legacy data not migrated: %+v / %+v / %+v", provider, identity, user)
	}
	if rowCount(t, db, &model.Version{}) != 2 || rowCount(t, db, &model.User{}) != 1 {
		t.Fatal("upgrade changed initial records or version history unexpectedly")
	}
}

func TestUpgrade245RepairsNullIssuer(t *testing.T) {
	db := legacyDB(t, 245)
	mustExec(t, db, "UPDATE oauths SET issuer = NULL")
	if err := Update(db, testVersion, seedTestData); err != nil {
		t.Fatal(err)
	}
	var provider model.Oauth
	if err := db.First(&provider).Error; err != nil {
		t.Fatal(err)
	}
	if provider.Issuer != model.IssuerGoogle {
		t.Fatalf("issuer = %q", provider.Issuer)
	}
}

func TestSchemaFailureDoesNotRecordVersionAndCanRetry(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db, "CREATE VIEW peers AS SELECT 1 AS row_id")
	err := Update(db, testVersion, seedTestData)
	if err == nil || !strings.Contains(err.Error(), "migrate schema") {
		t.Fatalf("expected schema failure, got %v", err)
	}
	if _, fresh, err := currentVersion(db); err != nil || !fresh {
		t.Fatalf("failed schema was recorded: fresh=%v, err=%v", fresh, err)
	}
	mustExec(t, db, "DROP VIEW peers")
	if err := Update(db, testVersion, seedTestData); err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertCurrentVersion(t, db, testVersion)
}

func TestSeedFailuresRollbackAndRetry(t *testing.T) {
	for _, test := range []struct{ name, table, condition, message string }{
		{"default_group", "groups", "NEW.type = 1", "create default group"},
		{"shared_group", "groups", "NEW.type = 2", "create shared group"},
		{"admin", "users", "1 = 1", "create initial admin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openTestDB(t)
			if err := migrateSchema(db); err != nil {
				t.Fatal(err)
			}
			mustExec(t, db, "CREATE TRIGGER fail_seed BEFORE INSERT ON "+test.table+" WHEN "+test.condition+" BEGIN SELECT RAISE(ABORT, 'injected seed failure'); END")
			err := Update(db, testVersion, seedTestData)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("expected %s error, got %v", test.message, err)
			}
			if rowCount(t, db, &model.Version{}) != 0 || rowCount(t, db, &model.Group{}) != 0 || rowCount(t, db, &model.User{}) != 0 {
				t.Fatal("failed seed left version or partial initial records")
			}
			mustExec(t, db, "DROP TRIGGER fail_seed")
			if err := Update(db, testVersion, seedTestData); err != nil {
				t.Fatalf("retry: %v", err)
			}
			assertCurrentVersion(t, db, testVersion)
		})
	}
}

func TestLegacyWriteFailuresDoNotAdvanceVersionAndCanRetry(t *testing.T) {
	for _, test := range []struct {
		name, table, column string
		version             uint
	}{
		{"provider_type_245", "oauths", "oauth_type", 244},
		{"provider_issuer_245", "oauths", "issuer", 244},
		{"identity_245", "user_thirds", "oauth_type", 244},
		{"email_245", "users", "email", 244},
		{"issuer_246", "oauths", "issuer", 245},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := legacyDB(t, test.version)
			if test.version == 245 {
				mustExec(t, db, "UPDATE oauths SET issuer = NULL")
			}
			mustExec(t, db, "CREATE TRIGGER fail_legacy BEFORE UPDATE OF "+test.column+" ON "+test.table+" BEGIN SELECT RAISE(ABORT, 'injected data failure'); END")
			if err := Update(db, testVersion, seedTestData); err == nil {
				t.Fatal("expected legacy update failure")
			}
			assertCurrentVersion(t, db, test.version)
			if rowCount(t, db, &model.Version{}) != 1 {
				t.Fatal("failed update appended a version")
			}
			mustExec(t, db, "DROP TRIGGER fail_legacy")
			if err := Update(db, testVersion, seedTestData); err != nil {
				t.Fatalf("retry: %v", err)
			}
			assertCurrentVersion(t, db, testVersion)
		})
	}
}

func TestVersionInsertFailureRollsBackDataAndCanRetry(t *testing.T) {
	db := legacyDB(t, 244)
	mustExec(t, db, "CREATE TRIGGER fail_version BEFORE INSERT ON versions BEGIN SELECT RAISE(ABORT, 'injected version failure'); END")
	err := Update(db, testVersion, seedTestData)
	if err == nil || !strings.Contains(err.Error(), "record database version") {
		t.Fatalf("expected version error, got %v", err)
	}
	assertCurrentVersion(t, db, 244)
	var user model.User
	if err := db.First(&user).Error; err != nil {
		t.Fatal(err)
	}
	if user.Email != "old@example.test" {
		t.Fatal("legacy data committed before version insert succeeded")
	}
	mustExec(t, db, "DROP TRIGGER fail_version")
	if err := Update(db, testVersion, seedTestData); err != nil {
		t.Fatal(err)
	}
	assertCurrentVersion(t, db, testVersion)
}

func TestReadFailuresPropagate(t *testing.T) {
	for _, table := range []string{"versions", "user_thirds"} {
		t.Run(table, func(t *testing.T) {
			db := legacyDB(t, 244)
			injected := errors.New("injected read failure")
			if err := db.Callback().Query().Before("gorm:query").Register("test:fail_read", func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(injected)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := Update(db, testVersion, seedTestData); !errors.Is(err, injected) {
				t.Fatalf("expected read failure, got %v", err)
			}
			if err := db.Callback().Query().Remove("test:fail_read"); err != nil {
				t.Fatal(err)
			}
			assertCurrentVersion(t, db, 244)
		})
	}
}

func TestUnavailableDatabaseReturnsError(t *testing.T) {
	if err := Update(nil, testVersion, seedTestData); err == nil {
		t.Fatal("nil database must fail")
	}
	db := openTestDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Update(db, testVersion, seedTestData); err == nil {
		t.Fatal("closed database must not be interpreted as a fresh database")
	}
}
