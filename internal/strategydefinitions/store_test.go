package strategydefinitions_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/internal/strategydefinitions"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type fixture struct {
	db    *gorm.DB
	store *strategydefinitions.Store
	actor auth.Principal
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "definitions.db")+"?_txlock=immediate&_busy_timeout=5000"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := database.Update(db, 280, nil); err != nil {
		t.Fatal(err)
	}
	yes := true
	user := model.User{Username: "definition-admin", IsAdmin: &yes, Status: model.COMMON_STATUS_ENABLE}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	session := model.UserToken{UserId: user.Id, Token: "private-definition-token", ExpiredAt: expiry.Unix()}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	store, err := strategydefinitions.New(db, strategydefinitions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{db, store, auth.Principal{User: auth.User{ID: user.Id, Enabled: true, IsAdmin: true}, Session: auth.Session{ID: session.Id, UserID: user.Id, Token: session.Token, ExpiresAt: expiry}}}
}
func input(name string) strategydefinitions.Input {
	return strategydefinitions.Input{Name: name, ConfigOptions: map[string]string{"custom-key": "value"}}
}

func TestDefinitionDecodeStrictObjectContract(t *testing.T) {
	valid := []string{`{"name":"Name","config_options":{}}`, `{"name":"name ","config_options":{"custom":""," ":"yes"}}`, `{"id":7,"name":"Name","config_options":{"x":"y"}}`}
	for _, raw := range valid {
		if _, err := strategydefinitions.Decode([]byte(raw)); err != nil {
			t.Fatal("valid definition rejected", err)
		}
	}
	invalid := []string{`null`, `[]`, `{}`, `{"name":"","config_options":{}}`, `{"name":null,"config_options":{}}`, `{"name":"x"}`, `{"name":"x","config_options":null}`, `{"name":"x","config_options":[]}`, `{"name":"x","config_options":{"":"v"}}`, `{"name":"x","config_options":{"x":null}}`, `{"name":"x","config_options":{"x":1}}`, `{"name":"x","config_options":{"x":true}}`, `{"name":"x","config_options":{"x":{}}}`, `{"name":"x","name":"y","config_options":{}}`, `{"name":"x","config_options":{"x":"1","x":"2"}}`, `{"name":"x","config_options":{},"owner":1}`, `{"name":"x","config_options":{}} {}`, `{"id":null,"name":"x","config_options":{}}`, `{"id":-1,"name":"x","config_options":{}}`}
	for _, raw := range invalid {
		if _, err := strategydefinitions.Decode([]byte(raw)); !errors.Is(err, strategydefinitions.ErrInput) {
			t.Fatalf("invalid definition accepted: %s", raw)
		}
	}
	if _, err := strategydefinitions.Decode([]byte("{\"name\":\"" + strings.Repeat("界", 85) + "\",\"config_options\":{}}")); err != nil {
		t.Fatal("255 UTF8 bytes rejected", err)
	}
	if _, err := strategydefinitions.Decode([]byte("{\"name\":\"" + strings.Repeat("界", 85) + "x\",\"config_options\":{}}")); !errors.Is(err, strategydefinitions.ErrInput) {
		t.Fatal("256 UTF8 bytes accepted")
	}
	if _, err := strategydefinitions.Decode([]byte{'{', '"', 'n', 'a', 'm', 'e', '"', ':', '"', 0xff, '"', '}'}); !errors.Is(err, strategydefinitions.ErrInput) {
		t.Fatal("invalid UTF8 accepted")
	}
}

func TestDefinitionCRUDExactNamesAndReplacement(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	names := []string{"Name", "name", "name ", "é", "e\u0301", "a\x00b", "a", "É"}
	ids := []uint{}
	for _, name := range names {
		row, err := f.store.Create(ctx, f.actor, input(name))
		if err != nil || row.ID == 0 || row.Name != name {
			t.Fatal("distinct exact name failed", err)
		}
		ids = append(ids, row.ID)
	}
	if result, err := f.store.Create(ctx, f.actor, input("Name")); !errors.Is(err, strategydefinitions.ErrConflict) || result.ID != 0 {
		t.Fatal("exact duplicate not classified", err)
	}
	total, rows, err := f.store.List(ctx, f.actor, strategydefinitions.Page{Current: 2, Size: 2})
	if err != nil || total != int64(len(names)) || len(rows) != 2 || rows[0].Name != names[2] || rows[1].Name != names[3] {
		t.Fatal("stable pagination failed", err)
	}
	row, err := f.store.Update(ctx, f.actor, strategydefinitions.Input{ID: ids[0], Name: "Renamed", ConfigOptions: map[string]string{}})
	if err != nil || row.ID != ids[0] || len(row.ConfigOptions) != 0 {
		t.Fatal("rename/replacement failed", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := f.store.Update(ctx, f.actor, strategydefinitions.Input{ID: ids[0], Name: "Renamed", ConfigOptions: map[string]string{}}); err != nil {
			t.Fatal("same-value update failed", err)
		}
	}
	if _, err := f.store.Update(ctx, f.actor, strategydefinitions.Input{ID: ids[0], Name: names[1], ConfigOptions: map[string]string{"wrong": "change"}}); !errors.Is(err, strategydefinitions.ErrConflict) {
		t.Fatal("rename duplicate not classified", err)
	}
	kept, err := f.store.Get(ctx, f.actor, ids[0])
	if err != nil || kept.Name != "Renamed" || len(kept.ConfigOptions) != 0 {
		t.Fatal("failed rename partially saved", err)
	}
	if err := f.store.Delete(ctx, f.actor, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Get(ctx, f.actor, ids[0]); !errors.Is(err, strategydefinitions.ErrNotFound) {
		t.Fatal("deleted definition remained", err)
	}
	if err := f.store.Delete(ctx, f.actor, ids[0]); !errors.Is(err, strategydefinitions.ErrNotFound) {
		t.Fatal("missing delete not reported", err)
	}
}

func TestDefinitionCurrentAdministratorAndSessionRequired(t *testing.T) {
	for _, mode := range []string{"role", "disabled", "revoked", "db-expiry", "jwt-expiry", "duplicate-session"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			want := auth.ErrUnauthenticated
			switch mode {
			case "role":
				want = auth.ErrForbidden
				if err := f.db.Model(&model.User{}).Where("id = ?", f.actor.User.ID).Update("is_admin", false).Error; err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := f.db.Model(&model.User{}).Where("id = ?", f.actor.User.ID).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
					t.Fatal(err)
				}
			case "revoked":
				if err := f.db.Where("id = ?", f.actor.Session.ID).Delete(&model.UserToken{}).Error; err != nil {
					t.Fatal(err)
				}
			case "db-expiry":
				if err := f.db.Model(&model.UserToken{}).Where("id = ?", f.actor.Session.ID).Update("expired_at", time.Now().Add(-time.Minute).Unix()).Error; err != nil {
					t.Fatal(err)
				}
			case "jwt-expiry":
				f.actor.Session.ExpiresAt = time.Now().Add(-time.Second)
			case "duplicate-session":
				if err := f.db.Create(&model.UserToken{UserId: f.actor.User.ID, Token: f.actor.Session.Token, ExpiredAt: f.actor.Session.ExpiresAt.Unix()}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if result, err := f.store.Create(context.Background(), f.actor, input("rejected")); !errors.Is(err, want) || result.ID != 0 {
				t.Fatal("stale proof accepted", err)
			}
			if _, _, err := f.store.List(context.Background(), f.actor, strategydefinitions.Page{Current: 1, Size: 10}); !errors.Is(err, want) {
				t.Fatal("stale proof could read definitions", err)
			}
			var count int64
			if err := f.db.Model(&model.StrategyDefinition{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatal("rejected proof wrote definitions", err)
			}
		})
	}
}

func TestDefinitionBadStorageAndFailedOperationsNeverPartiallyReturn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, err := f.store.Create(ctx, f.actor, input("first"))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`null`, `[]`, `{"":"x"}`, `{"key":null}`, `{"key":"a","key":"b"}`, `broken`} {
		if err := f.db.Exec("UPDATE strategy_definitions SET config_options=? WHERE id=?", raw, first.ID).Error; err != nil {
			t.Fatal(err)
		}
		if got, err := f.store.Get(ctx, f.actor, first.ID); !errors.Is(err, strategydefinitions.ErrUnavailable) || !reflect.DeepEqual(got, strategydefinitions.Definition{}) {
			t.Fatal("bad stored document returned", err)
		}
		total, rows, err := f.store.List(ctx, f.actor, strategydefinitions.Page{Current: 1, Size: 10})
		if !errors.Is(err, strategydefinitions.ErrUnavailable) || total != 0 || rows != nil {
			t.Fatal("bad list returned partially", err)
		}
	}
	if err := f.db.Exec("UPDATE strategy_definitions SET config_options='{}' WHERE id=?", first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Callback().Update().Before("gorm:update").Register("fail-definition-update", func(tx *gorm.DB) {
		if tx.Statement.Table == "strategy_definitions" {
			tx.AddError(errors.New("private storage fault"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := f.store.Update(ctx, f.actor, strategydefinitions.Input{ID: first.ID, Name: "changed", ConfigOptions: map[string]string{}}); !errors.Is(err, strategydefinitions.ErrUnavailable) || got.ID != 0 {
		t.Fatal("failed update acknowledged", err)
	}
	if err := f.db.Callback().Update().Remove("fail-definition-update"); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Get(ctx, f.actor, first.ID)
	if err != nil || got.Name != "first" {
		t.Fatal("failed update persisted", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.store.Create(canceled, f.actor, input("canceled")); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled operation acknowledged", err)
	}
}

func TestDefinitionInstanceAndCredentialLoggingBoundaries(t *testing.T) {
	a, b := newFixture(t), newFixture(t)
	if _, err := a.store.Create(context.Background(), a.actor, input("only-A")); err != nil {
		t.Fatal(err)
	}
	total, rows, err := b.store.List(context.Background(), b.actor, strategydefinitions.Page{Current: 1, Size: 10})
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatal("cross-instance definition leakage", err)
	}
	var logs bytes.Buffer
	a.db.Logger = logger.New(log.New(&logs, "", 0), logger.Config{LogLevel: logger.Info})
	store, err := strategydefinitions.New(a.db, strategydefinitions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), a.actor, input("silent")); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatal("caller logger observed token/config SQL")
	}
}

type commitPool struct {
	gorm.ConnPool
	db      *sql.DB
	reached *bool
}

func (p commitPool) BeginTx(ctx context.Context, o *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.db.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &commitTx{Tx: tx, reached: p.reached}, nil
}

type commitTx struct {
	*sql.Tx
	reached *bool
}

func (tx commitTx) Commit() error {
	*tx.reached = true
	_ = tx.Tx.Rollback()
	return errors.New("private commit failure")
}
func TestDefinitionCommitFailureAndExpiryRollBack(t *testing.T) {
	f := newFixture(t)
	pool, err := f.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	reached := false
	faultDB, err := gorm.Open(sqlite.Dialector{Conn: commitPool{ConnPool: pool, db: pool, reached: &reached}}, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	store, err := strategydefinitions.New(faultDB, strategydefinitions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.Create(context.Background(), f.actor, input("failed")); !errors.Is(err, strategydefinitions.ErrUnavailable) || got.ID != 0 || !reached {
		t.Fatal("commit boundary not rejected", err)
	}
	var count int64
	if err := f.db.Model(&model.StrategyDefinition{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("commit failure persisted", err)
	}
	clock := time.Now().UTC()
	store, err = strategydefinitions.New(f.db, strategydefinitions.Options{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	observed := false
	if err := f.db.Callback().Create().After("gorm:create").Register("expire-after-definition-create", func(tx *gorm.DB) {
		if tx.Statement.Table == "strategy_definitions" {
			observed = true
			clock = f.actor.Session.ExpiresAt.Add(time.Second)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Create(context.Background(), f.actor, input("expired")); !errors.Is(err, auth.ErrUnauthenticated) || got.ID != 0 || !observed {
		t.Fatal("post-wait expiry not revalidated", err)
	}
	if err := f.db.Model(&model.StrategyDefinition{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("expired operation persisted", err)
	}
}
