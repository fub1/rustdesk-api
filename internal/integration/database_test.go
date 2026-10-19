// Package integration verifies production stores against disposable real
// MySQL/PostgreSQL databases. It never migrates or drops the supplied DSN's DB.
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/lib/orm"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type fixture struct {
	DB     *gorm.DB
	Engine string
	open   func() (*gorm.DB, error)
	mu     sync.Mutex
	pools  []*gorm.DB
}

func postgresDatabaseDSN(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return "", errors.New("invalid PostgreSQL URI")
	}
	u.Path = "/" + name
	u.RawPath = ""
	return u.String(), nil
}

func (f *fixture) Reopen(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := f.open()
	if err != nil {
		t.Fatal("open isolated test database failed (DSN redacted)")
	}
	f.mu.Lock()
	f.pools = append(f.pools, db)
	f.mu.Unlock()
	return db
}

func forEngines(t *testing.T, fn func(*testing.T, *fixture)) {
	t.Helper()
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			f := newFixture(t, engine)
			if err := database.Update(f.DB, 278, func(tx *gorm.DB) error {
				return database.SeedInitialData(tx, "Default", "Shared", "integration-only-password-hash")
			}); err != nil {
				t.Fatalf("production migration: %v", err)
			}
			fn(t, f)
		})
	}
}

func newFixture(t *testing.T, engine string) *fixture {
	t.Helper()
	dsn := os.Getenv("RUSTDESK_TEST_" + strings.ToUpper(engine) + "_DSN")
	if dsn == "" {
		if os.Getenv("RUSTDESK_TEST_DB_REQUIRED") == "1" {
			t.Fatal("missing required real database DSN for " + engine)
		}
		t.Skip("real " + engine + " integration DSN not supplied")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := "rd_it_" + hex.EncodeToString(random[:])
	writer := log.New(io.Discard, "", 0)
	connect := func(raw string) (*gorm.DB, error) {
		var db *gorm.DB
		var err error
		if engine == "mysql" {
			db, err = orm.NewMysql(&orm.MysqlConfig{Dsn: raw, MaxIdleConns: 8, MaxOpenConns: 16}, writer)
		} else {
			db, err = orm.NewPostgresql(&orm.PostgresqlConfig{Dsn: raw, MaxIdleConns: 8, MaxOpenConns: 16}, writer)
		}
		if err != nil {
			return nil, err
		}
		return db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}), nil
	}
	admin, err := connect(dsn)
	if err != nil {
		t.Fatal("connect integration database admin failed (DSN redacted)")
	}
	adminPool, _ := admin.DB()
	t.Cleanup(func() { _ = adminPool.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Only this locally generated fixed-prefix hex name reaches SQL identifiers.
	create := "CREATE DATABASE " + name
	if engine == "mysql" {
		create += " CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"
	}
	if err := admin.WithContext(ctx).Exec(create).Error; err != nil {
		t.Fatalf("create disposable database: %v", err)
	}
	f := &fixture{Engine: engine}
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, db := range f.pools {
			if pool, err := db.DB(); err == nil {
				_ = pool.Close()
			}
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if err := admin.WithContext(cleanupCtx).Exec("DROP DATABASE " + name).Error; err != nil {
			t.Errorf("drop owned disposable database: %v", err)
		}
	})
	var target string
	if engine == "mysql" {
		config, err := mysqldriver.ParseDSN(dsn)
		if err != nil {
			t.Fatal("invalid integration MySQL DSN")
		}
		config.DBName, config.ParseTime, config.Loc = name, true, time.UTC
		target = config.FormatDSN()
	} else {
		// Use a URI DSN so replacing only its database is unambiguous.
		var err error
		target, err = postgresDatabaseDSN(dsn, name)
		if err != nil {
			t.Fatal("integration PostgreSQL DSN must be postgres:// URI")
		}
	}
	f.open = func() (*gorm.DB, error) { return connect(target) }
	f.DB = f.Reopen(t)
	var version string
	if err := f.DB.Raw("SELECT version()").Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	t.Logf("real engine %s: %s", engine, version)
	return f
}
