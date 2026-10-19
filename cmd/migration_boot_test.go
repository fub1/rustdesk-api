package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Run the real entry point in a child test process so os.Exit and the failure to
// reach Run/RunE are verified without starting a server in the parent process.
func TestMigrationStartupHelper(t *testing.T) {
	if os.Getenv("RUSTDESK_MIGRATION_BOOT_HELPER") != "1" {
		return
	}
	rootCmd.SetArgs([]string{"--config", os.Getenv("RUSTDESK_MIGRATION_BOOT_CONFIG")})
	main()
}

func TestDatabaseFailureExitsBeforeHTTP(t *testing.T) {
	resources, err := filepath.Abs("../resources")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"connection", "migration"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			if scenario == "migration" {
				if err := os.Mkdir(filepath.Join(dir, "data"), 0700); err != nil {
					t.Fatal(err)
				}
				db := openBootDB(t, filepath.Join(dir, "data", "rustdeskapi.db"))
				if err := db.Exec("CREATE VIEW peers AS SELECT 1 AS row_id").Error; err != nil {
					t.Fatal(err)
				}
				sqlDB, _ := db.DB()
				if err := sqlDB.Close(); err != nil {
					t.Fatal(err)
				}
			}
			config := filepath.Join(dir, "config.yaml")
			// The connection case intentionally has no data directory. No network
			// services are needed: SQLite fails before HTTP or login workers.
			yaml := "lang: en\nlogger:\n  level: error\ngin:\n  api-addr: 127.0.0.1:0\n  resources-path: " + filepath.ToSlash(resources) + "\ngorm:\n  type: sqlite\n"
			if err := os.WriteFile(config, []byte(yaml), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMigrationStartupHelper$")
			command.Dir = dir
			command.Env = append(os.Environ(), "RUSTDESK_MIGRATION_BOOT_HELPER=1", "RUSTDESK_MIGRATION_BOOT_CONFIG="+config)
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("startup did not fail promptly: %v\n%s", ctx.Err(), output)
			}
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 {
				t.Fatalf("exit = %v, want status 1\n%s", err, output)
			}
			want := "initialize database"
			if scenario == "migration" {
				want = "migrate schema"
			}
			if !strings.Contains(string(output), want) || strings.Contains(string(output), "API SERVER START") {
				t.Fatalf("unexpected startup path: %s", output)
			}
			if scenario == "migration" {
				db := openBootDB(t, filepath.Join(dir, "data", "rustdeskapi.db"))
				sqlDB, _ := db.DB()
				t.Cleanup(func() { _ = sqlDB.Close() })
				if db.Migrator().HasTable(&model.Version{}) {
					var count int64
					if err := db.Model(&model.Version{}).Count(&count).Error; err != nil || count != 0 {
						t.Fatalf("failed startup recorded version: count=%d, error=%v", count, err)
					}
				}
			}
		})
	}
}

func openBootDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return db
}
