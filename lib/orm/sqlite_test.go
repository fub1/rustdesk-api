package orm

import (
	"io"
	"log"
	"path/filepath"
	"testing"
)

func TestSqliteConnectionUsesIsolatedPath(t *testing.T) {
	db, err := NewSqlite(&SqliteConfig{
		Dsn: filepath.Join(t.TempDir(), "isolated.db") + "?_txlock=immediate",
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := sqlDB.Ping(); err != nil {
		t.Fatal(err)
	}
}

func TestSqliteOpenFailureReturnsError(t *testing.T) {
	db, err := NewSqlite(&SqliteConfig{
		Dsn: filepath.Join(t.TempDir(), "missing", "db.sqlite"),
	}, log.New(io.Discard, "", 0))
	if err == nil || db != nil {
		t.Fatalf("invalid path returned (%v, %v), want nil database and error", db, err)
	}
}

func TestClosedConnectionReturnsError(t *testing.T) {
	db, err := NewSqlite(&SqliteConfig{Dsn: filepath.Join(t.TempDir(), "closed.db")}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := configureConnection(db, 1, 1)
	if got != nil || err == nil {
		t.Fatalf("closed connection returned (%v, %v)", got, err)
	}
}

func TestInvalidNetworkDatabaseDSNsReturnErrorsWithoutConnecting(t *testing.T) {
	writer := log.New(io.Discard, "", 0)
	if db, err := NewMysql(&MysqlConfig{Dsn: "invalid-dsn-without-database-separator"}, writer); db != nil || err == nil {
		t.Fatalf("invalid mysql DSN returned (%v, %v)", db, err)
	}
	if db, err := NewPostgresql(&PostgresqlConfig{Dsn: "postgres://%"}, writer); db != nil || err == nil {
		t.Fatalf("invalid postgresql DSN returned (%v, %v)", db, err)
	}
}
