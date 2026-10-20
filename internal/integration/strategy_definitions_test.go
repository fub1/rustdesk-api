package integration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/internal/strategydefinitions"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

func definitionAdmin(t *testing.T, db *gorm.DB, suffix string) auth.Principal {
	t.Helper()
	yes := true
	user := model.User{Username: "definition-admin-" + suffix, IsAdmin: &yes, Status: model.COMMON_STATUS_ENABLE}
	transactionCreate(t, db, &user)
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	session := model.UserToken{UserId: user.Id, Token: "definition-session-" + suffix, ExpiredAt: expires.Unix()}
	transactionCreate(t, db, &session)
	return auth.Principal{User: auth.User{ID: user.Id, Enabled: true, IsAdmin: true}, Session: auth.Session{ID: session.Id, UserID: user.Id, Token: session.Token, ExpiresAt: expires}}
}

func definitionStore(t *testing.T, db *gorm.DB) *strategydefinitions.Store {
	t.Helper()
	s, err := strategydefinitions.New(db, strategydefinitions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func definitionColumns(t *testing.T, f *fixture, db *gorm.DB) {
	t.Helper()
	columns, err := db.Migrator().ColumnTypes(&model.StrategyDefinition{})
	if err != nil {
		t.Fatal(err)
	}
	if len(columns) != 3 {
		t.Fatalf("definition must contain only three approved columns: %d", len(columns))
	}
	seen := map[string]bool{}
	for _, column := range columns {
		seen[column.Name()] = true
		if nullable, known := column.Nullable(); !known || nullable {
			t.Fatalf("column %s must be NOT NULL", column.Name())
		}
		if column.Name() == "name" {
			want := "bytea"
			if f.Engine == "mysql" {
				want = "varbinary"
			}
			if !strings.EqualFold(column.DatabaseTypeName(), want) {
				t.Fatalf("name physical type reverted: %s", column.DatabaseTypeName())
			}
			if f.Engine == "mysql" {
				if length, known := column.Length(); !known || length != 255 {
					t.Fatalf("name byte length=%d known=%t", length, known)
				}
			}
		}
	}
	if !seen["id"] || !seen["name"] || !seen["config_options"] {
		t.Fatalf("unexpected columns: %v", seen)
	}
}

func TestStrategyDefinitionsRealByteNamesAndLaterMigration(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		// Explicit target, independent of forEngines' historical 278 fixture.
		upgrade(t, f.DB, 280)
		definitionColumns(t, f, f.DB)
		actor := definitionAdmin(t, f.DB, "bytes")
		store := definitionStore(t, f.DB)
		names := []string{"Case", "case", "Cáse", "Case ", "Case\x00", "Case\x00x", "中文\x00名称"}
		created := []strategydefinitions.Definition{}
		for _, name := range names {
			input := strategydefinitions.Input{Name: name, ConfigOptions: map[string]string{"key": "value\x00原样"}}
			got, err := store.Create(context.Background(), actor, input)
			if err != nil || got.Name != name || !reflect.DeepEqual(got.ConfigOptions, input.ConfigOptions) {
				t.Fatalf("Store.Create failed exact byte roundtrip %q: %+v %v", name, got, err)
			}
			read, err := store.Get(context.Background(), actor, got.ID)
			if err != nil || !reflect.DeepEqual(got, read) {
				t.Fatalf("Store.Get changed name %q: %+v %v", name, read, err)
			}
			created = append(created, got)
			if _, err := store.Create(context.Background(), actor, input); !errors.Is(err, strategydefinitions.ErrConflict) {
				t.Fatalf("exact duplicate accepted: %q %v", name, err)
			}
		}
		// Exercise the UPDATE bind path with NUL too, rather than only raw SQL.
		changed, err := store.Update(context.Background(), actor, strategydefinitions.Input{ID: created[0].ID, Name: "renamed\x00中文 ", ConfigOptions: map[string]string{}})
		if err != nil || changed.Name != "renamed\x00中文 " {
			t.Fatal("NUL rename", err)
		}
		created[0] = changed
		for _, statement := range []string{"INSERT INTO strategy_definitions (name,config_options) VALUES (NULL,'{}')", "INSERT INTO strategy_definitions (name,config_options) VALUES (?,NULL)"} {
			var err error
			if strings.Contains(statement, "?") {
				err = f.DB.Exec(statement, []byte("nonnull-check")).Error
			} else {
				err = f.DB.Exec(statement).Error
			}
			if err == nil {
				t.Fatal("NOT NULL constraint missing")
			}
		}
		// A future target forces migrateSchema again; this does not define a 281 migration.
		upgrade(t, f.DB, 281)
		restarted := f.Reopen(t)
		upgrade(t, restarted, 281)
		definitionColumns(t, f, restarted)
		store = definitionStore(t, restarted)
		for _, before := range created {
			after, err := store.Get(context.Background(), actor, before.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("later schema pass changed byte name/data: %v", err)
			}
		}
	})
}

func TestStrategyDefinitionsRealMigration280FailureAndRetry(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		upgrade(t, f.DB, 279)
		if err := f.DB.Migrator().DropTable(&model.StrategyDefinition{}); err != nil {
			t.Fatal(err)
		}
		transactionExec(t, f.DB, "ALTER TABLE versions ADD CONSTRAINT reject_definition_280 CHECK (version <> 280)")
		if err := database.Update(f.DB, 280, nil); err == nil || !strings.Contains(err.Error(), "record database version 280") {
			t.Fatalf("final version failure not reached: %v", err)
		}
		var latest model.Version
		if err := f.Reopen(t).Last(&latest).Error; err != nil || latest.Version != 279 {
			t.Fatalf("failed 280 advanced version: %+v %v", latest, err)
		}
		if exists := f.DB.Migrator().HasTable(&model.StrategyDefinition{}); exists != (f.Engine == "mysql") {
			t.Fatalf("unexpected transactional DDL outcome: table exists=%t engine=%s", exists, f.Engine)
		}
		drop := "ALTER TABLE versions DROP CONSTRAINT reject_definition_280"
		if f.Engine == "mysql" {
			drop = "ALTER TABLE versions DROP CHECK reject_definition_280"
		}
		transactionExec(t, f.DB, drop)
		upgrade(t, f.DB, 280)
		definitionColumns(t, f, f.DB)
		actor := definitionAdmin(t, f.DB, "migration-retry")
		if _, err := definitionStore(t, f.DB).Create(context.Background(), actor, strategydefinitions.Input{Name: "retry\x00", ConfigOptions: map[string]string{}}); err != nil {
			t.Fatal("DDL retry left unusable schema", err)
		}
	})
}

func TestStrategyDefinitionsRealStaleAdmissionFailsClosed(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		upgrade(t, f.DB, 280)
		for _, mode := range []string{"role", "disabled", "revoked", "stored-expiry", "signed-expiry"} {
			t.Run(mode, func(t *testing.T) {
				actor := definitionAdmin(t, f.DB, mode)
				store := definitionStore(t, f.Reopen(t))
				before, err := store.Create(context.Background(), actor, strategydefinitions.Input{Name: mode, ConfigOptions: map[string]string{"before": "value"}})
				if err != nil {
					t.Fatal(err)
				}
				want := auth.ErrUnauthenticated
				switch mode {
				case "role":
					want = auth.ErrForbidden
					if err := f.DB.Model(&model.User{}).Where("id = ?", actor.User.ID).Update("is_admin", false).Error; err != nil {
						t.Fatal(err)
					}
				case "disabled":
					if err := f.DB.Model(&model.User{}).Where("id = ?", actor.User.ID).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
						t.Fatal(err)
					}
				case "revoked":
					if err := f.DB.Delete(&model.UserToken{}, actor.Session.ID).Error; err != nil {
						t.Fatal(err)
					}
				case "stored-expiry":
					if err := f.DB.Model(&model.UserToken{}).Where("id = ?", actor.Session.ID).Update("expired_at", time.Now().Add(-time.Minute).Unix()).Error; err != nil {
						t.Fatal(err)
					}
				case "signed-expiry":
					actor.Session.ExpiresAt = time.Now().Add(-time.Minute)
				}
				if _, err := store.Create(context.Background(), actor, strategydefinitions.Input{Name: mode + "-blocked", ConfigOptions: map[string]string{}}); !errors.Is(err, want) {
					t.Fatal("stale Create admission", err)
				}
				if _, err := store.Update(context.Background(), actor, strategydefinitions.Input{ID: before.ID, Name: mode + "-blocked", ConfigOptions: map[string]string{}}); !errors.Is(err, want) {
					t.Fatal("stale Update admission", err)
				}
				if err := store.Delete(context.Background(), actor, before.ID); !errors.Is(err, want) {
					t.Fatal("stale Delete admission", err)
				}
				if _, err := store.Get(context.Background(), actor, before.ID); !errors.Is(err, want) {
					t.Fatal("stale Get admission", err)
				}
				var stored model.StrategyDefinition
				if err := f.Reopen(t).First(&stored, before.ID).Error; err != nil || string(stored.Name) != before.Name || stored.ConfigOptions != `{"before":"value"}` {
					t.Fatal("stale principal mutated definition", err)
				}
			})
		}
	})
}

func TestStrategyDefinitionsRealIndependentPoolsUniqueAndRenameRollback(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		upgrade(t, f.DB, 280)
		actors := []auth.Principal{definitionAdmin(t, f.DB, "concurrent-a"), definitionAdmin(t, f.DB, "concurrent-b")}
		stores := []*strategydefinitions.Store{definitionStore(t, f.Reopen(t)), definitionStore(t, f.Reopen(t))}
		start, results := make(chan struct{}), make(chan error, 2)
		var group sync.WaitGroup
		for i := range actors {
			group.Add(1)
			go func(i int) {
				defer group.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, err := stores[i].Create(ctx, actors[i], strategydefinitions.Input{Name: "unique\x00", ConfigOptions: map[string]string{"writer": fmt.Sprint(i)}})
				results <- err
			}(i)
		}
		close(start)
		group.Wait()
		close(results)
		success, conflict := 0, 0
		for err := range results {
			if err == nil {
				success++
			} else if errors.Is(err, strategydefinitions.ErrConflict) {
				conflict++
			} else {
				t.Fatal("independent create", err)
			}
		}
		if success != 1 || conflict != 1 || transactionCount(t, f.DB, &model.StrategyDefinition{}) != 1 {
			t.Fatalf("uniqueness did not arbitrate: success=%d conflict=%d", success, conflict)
		}
		before, err := stores[0].Create(context.Background(), actors[0], strategydefinitions.Input{Name: "other", ConfigOptions: map[string]string{"keep": "original"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stores[0].Update(context.Background(), actors[0], strategydefinitions.Input{ID: before.ID, Name: "unique\x00", ConfigOptions: map[string]string{"must": "rollback"}}); !errors.Is(err, strategydefinitions.ErrConflict) {
			t.Fatal("rename conflict accepted", err)
		}
		after, err := stores[1].Get(context.Background(), actors[1], before.ID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("rename conflict partially replaced config/name", err)
		}
	})
}

// No transaction SQL is issued after successful final definition DML. MySQL's
// connection identifier is read BEFORE that DML; only the independent control
// pool sends KILL afterwards. PostgreSQL defers its constraint trigger failure.
func failDefinitionCommit(t *testing.T, f *fixture, operation string) func() {
	t.Helper()
	if f.Engine == "postgres" {
		transactionExec(t, f.DB, "CREATE FUNCTION definition_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'definition deferred commit failure'; END $$")
		transactionExec(t, f.DB, "CREATE CONSTRAINT TRIGGER definition_fail_commit AFTER "+strings.ToUpper(operation)+" ON strategy_definitions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION definition_fail_commit()")
		var reached atomic.Bool
		mark := func(tx *gorm.DB) {
			if tx.Statement.Table == "strategy_definitions" && tx.Error == nil {
				reached.Store(true)
			}
		}
		registerDefinitionCallbacks(t, f.DB, operation, nil, mark)
		return func() {
			if !reached.Load() {
				t.Error("successful last definition SQL was not reached before COMMIT")
			}
			removeDefinitionCallbacks(t, f.DB, operation)
			transactionExec(t, f.DB, "DROP TRIGGER definition_fail_commit ON strategy_definitions")
			transactionExec(t, f.DB, "DROP FUNCTION definition_fail_commit()")
		}
	}
	control := f.Reopen(t)
	var id uint64
	var reached, killed atomic.Bool
	before := func(tx *gorm.DB) {
		if tx.Statement.Table == "strategy_definitions" {
			if err := tx.Statement.ConnPool.QueryRowContext(tx.Statement.Context, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
				tx.AddError(err)
			}
		}
	}
	after := func(tx *gorm.DB) {
		if tx.Statement.Table != "strategy_definitions" || tx.Error != nil {
			return
		}
		reached.Store(true)
		if err := control.Exec(fmt.Sprintf("KILL CONNECTION %d", id)).Error; err != nil {
			tx.AddError(err)
			return
		}
		killed.Store(true)
	}
	registerDefinitionCallbacks(t, f.DB, operation, before, after)
	return func() {
		if !reached.Load() || !killed.Load() {
			t.Error("successful last definition SQL/connection termination not reached")
		}
		removeDefinitionCallbacks(t, f.DB, operation)
	}
}

func registerDefinitionCallbacks(t *testing.T, db *gorm.DB, operation string, before, after func(*gorm.DB)) {
	t.Helper()
	var errs []error
	switch operation {
	case "insert":
		if before != nil {
			errs = append(errs, db.Callback().Create().Before("gorm:create").Register("definition:before-commit", before))
		}
		errs = append(errs, db.Callback().Create().After("gorm:create").Register("definition:fail-commit", after))
	case "update":
		if before != nil {
			errs = append(errs, db.Callback().Update().Before("gorm:update").Register("definition:before-commit", before))
		}
		errs = append(errs, db.Callback().Update().After("gorm:update").Register("definition:fail-commit", after))
	case "delete":
		if before != nil {
			errs = append(errs, db.Callback().Delete().Before("gorm:delete").Register("definition:before-commit", before))
		}
		errs = append(errs, db.Callback().Delete().After("gorm:delete").Register("definition:fail-commit", after))
	default:
		t.Fatal("invalid definition operation")
	}
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func removeDefinitionCallbacks(t *testing.T, db *gorm.DB, operation string) {
	t.Helper()
	for _, name := range []string{"definition:before-commit", "definition:fail-commit"} {
		var err error
		switch operation {
		case "insert":
			err = db.Callback().Create().Remove(name)
		case "update":
			err = db.Callback().Update().Remove(name)
		case "delete":
			err = db.Callback().Delete().Remove(name)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestStrategyDefinitionsRealCommitRollbackAndRetry(t *testing.T) {
	for _, operation := range []string{"insert", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			forEngines(t, func(t *testing.T, f *fixture) {
				upgrade(t, f.DB, 280)
				actor := definitionAdmin(t, f.DB, operation)
				store := definitionStore(t, f.DB)
				input := strategydefinitions.Input{Name: "commit\x00", ConfigOptions: map[string]string{"after": "value"}}
				var before strategydefinitions.Definition
				if operation != "insert" {
					var err error
					before, err = store.Create(context.Background(), actor, strategydefinitions.Input{Name: "before", ConfigOptions: map[string]string{"before": "value"}})
					if err != nil {
						t.Fatal(err)
					}
					input.ID = before.ID
				}
				run := func(s *strategydefinitions.Store) error {
					switch operation {
					case "insert":
						_, err := s.Create(context.Background(), actor, input)
						return err
					case "update":
						_, err := s.Update(context.Background(), actor, input)
						return err
					default:
						return s.Delete(context.Background(), actor, before.ID)
					}
				}
				remove := failDefinitionCommit(t, f, operation)
				err := run(store)
				remove()
				if !errors.Is(err, strategydefinitions.ErrUnavailable) {
					t.Fatalf("failed real COMMIT acknowledged: %v", err)
				}
				fresh := f.Reopen(t)
				retry := definitionStore(t, fresh)
				if operation == "insert" {
					if transactionCount(t, fresh, &model.StrategyDefinition{}) != 0 {
						t.Fatal("failed create COMMIT retained row")
					}
				} else {
					got, err := retry.Get(context.Background(), actor, before.ID)
					if err != nil || !reflect.DeepEqual(before, got) {
						t.Fatal("failed COMMIT retained mutation", err)
					}
				}
				if err := run(retry); err != nil {
					t.Fatal("retry after real COMMIT failure", err)
				}
				if operation == "delete" {
					if transactionCount(t, fresh, &model.StrategyDefinition{}) != 0 {
						t.Fatal("successful retry left definition")
					}
				} else {
					_, got, err := retry.List(context.Background(), actor, strategydefinitions.Page{Current: 1, Size: 10})
					if err != nil || len(got) != 1 || got[0].Name != input.Name || !reflect.DeepEqual(got[0].ConfigOptions, input.ConfigOptions) {
						t.Fatal("retry did not persist intended definition", err)
					}
				}
			})
		})
	}
}
