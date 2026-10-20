package clientassign_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientassign"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type fixture struct {
	db    *gorm.DB
	store *clientassign.Store
	actor auth.Principal
	owner model.User
	peer  model.Peer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "cli.db")+"?_txlock=immediate&_busy_timeout=5000"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&model.User{}, &model.UserToken{}, &model.Peer{}, &model.DeviceGroup{}); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	owner := model.User{IdModel: model.IdModel{Id: 1}, Username: "Exact Owner", IsAdmin: &no, Status: model.COMMON_STATUS_ENABLE}
	admin := model.User{IdModel: model.IdModel{Id: 9}, Username: "admin", IsAdmin: &yes, Status: model.COMMON_STATUS_ENABLE}
	for _, user := range []*model.User{&owner, &admin} {
		if err := db.Create(user).Error; err != nil {
			t.Fatal(err)
		}
	}
	expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	session := model.UserToken{UserId: admin.Id, Token: "private-raw-admin-session", ExpiredAt: expiry.Unix()}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	peer := model.Peer{Id: "device", Uuid: "uuid", Username: "before-user", Hostname: "before-host", Note: "before-note", Alias: "preserved-alias", UserId: 0}
	if err := db.Create(&peer).Error; err != nil {
		t.Fatal(err)
	}
	store, err := clientassign.New(db, clientassign.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{db: db, store: store, owner: owner, peer: peer, actor: auth.Principal{User: auth.User{ID: admin.Id, Enabled: true, IsAdmin: true}, Session: auth.Session{ID: session.Id, UserID: admin.Id, Token: session.Token, ExpiresAt: expiry}}}
}

func field(value string) clientassign.OptionalString {
	return clientassign.OptionalString{Present: true, Value: value}
}
func input() clientassign.Input {
	return clientassign.Input{ID: "device", UUID: "uuid", DeviceName: field("after-host"), Note: field("after-note")}
}
func readPeer(t *testing.T, f *fixture) model.Peer {
	t.Helper()
	var peer model.Peer
	if err := f.db.First(&peer, f.peer.RowId).Error; err != nil {
		t.Fatal(err)
	}
	return peer
}

func TestAssignmentRechecksAdministratorAndSession(t *testing.T) {
	for _, mode := range []string{"role", "disabled", "revoked", "database-expiry", "jwt-expiry", "duplicate-session", "uuid-change", "owner-disabled", "owner-exact", "group-ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			request := input()
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
			case "database-expiry":
				if err := f.db.Model(&model.UserToken{}).Where("id = ?", f.actor.Session.ID).Update("expired_at", time.Now().Add(-time.Minute).Unix()).Error; err != nil {
					t.Fatal(err)
				}
			case "jwt-expiry":
				f.actor.Session.ExpiresAt = time.Now().Add(-time.Second)
			case "duplicate-session":
				if err := f.db.Create(&model.UserToken{UserId: f.actor.User.ID, Token: f.actor.Session.Token, ExpiredAt: f.actor.Session.ExpiresAt.Unix()}).Error; err != nil {
					t.Fatal(err)
				}
			case "uuid-change":
				want = clientassign.ErrNotFound
				if err := f.db.Model(&f.peer).Update("uuid", "other").Error; err != nil {
					t.Fatal(err)
				}
			case "owner-disabled":
				want = clientassign.ErrInput
				request.UserName = field(f.owner.Username)
				if err := f.db.Model(&f.owner).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
					t.Fatal(err)
				}
			case "owner-exact":
				want = clientassign.ErrInput
				request.UserName = field("exact owner")
			case "group-ambiguous":
				want = clientassign.ErrInput
				request.DeviceGroupName = field("duplicate")
				if err := f.db.Create(&[]model.DeviceGroup{{Name: "duplicate"}, {Name: "duplicate"}}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := f.store.Assign(context.Background(), f.actor, request); !errors.Is(err, want) {
				t.Fatalf("expected %v, got %v", want, err)
			}
			peer := readPeer(t, f)
			if peer.Hostname != "before-host" || peer.Username != "before-user" || peer.Note != "before-note" || peer.UserId != 0 {
				t.Fatal("stale proof/invalid target caused partial assignment")
			}
		})
	}
}

func TestAssignmentInstancesAndExactLookups(t *testing.T) {
	a, b := newFixture(t), newFixture(t)
	oldGlobal, oldService := global.DB, service.DB
	t.Cleanup(func() { global.DB, service.DB = oldGlobal, oldService })
	global.DB, service.DB = b.db, b.db
	if err := a.store.Assign(context.Background(), a.actor, input()); err != nil {
		t.Fatal(err)
	}
	if readPeer(t, a).Hostname != "after-host" || readPeer(t, b).Hostname != "before-host" {
		t.Fatal("assignment followed another instance/global database")
	}
	for _, name := range []string{"Exact Owner ", "exact Owner", "Missing"} {
		request := input()
		request.UserName = field(name)
		if err := a.store.Assign(context.Background(), a.actor, request); !errors.Is(err, clientassign.ErrInput) {
			t.Fatal("non-exact user accepted", err)
		}
	}
	request := input()
	request.UserName = field(a.owner.Username)
	if err := a.store.Assign(context.Background(), a.actor, request); err != nil {
		t.Fatal(err)
	}
	if readPeer(t, a).UserId != a.owner.Id {
		t.Fatal("explicit owner was not applied")
	}
	// A missing selector must preserve even the unowned zero value.
	if readPeer(t, b).UserId != 0 {
		t.Fatal("unowned device acquired default owner")
	}
}

func TestAssignmentFailureAndContextDoNotAcknowledge(t *testing.T) {
	f := newFixture(t)
	if err := f.db.Callback().Update().Before("gorm:update").Register("fail-cli-write", func(tx *gorm.DB) {
		if tx.Statement.Table == "peers" {
			tx.AddError(errors.New("private assignment write failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Assign(context.Background(), f.actor, input()); !errors.Is(err, clientassign.ErrUnavailable) {
		t.Fatal("write fault acknowledged", err)
	}
	if readPeer(t, f).Hostname != "before-host" {
		t.Fatal("failed write partially persisted")
	}
	if err := f.db.Callback().Update().Remove("fail-cli-write"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.store.Assign(ctx, f.actor, input()); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled assignment acknowledged", err)
	}
	if err := f.store.Assign(context.Background(), f.actor, input()); err != nil {
		t.Fatal("retry failed", err)
	}
}

func TestAssignmentStoreNeverLogsCredentialSQL(t *testing.T) {
	f := newFixture(t)
	var buffer bytes.Buffer
	f.db.Logger = logger.New(log.New(&buffer, "", 0), logger.Config{LogLevel: logger.Info})
	store, err := clientassign.New(f.db, clientassign.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Assign(context.Background(), f.actor, input()); err != nil {
		t.Fatal(err)
	}
	if buffer.Len() != 0 {
		t.Fatal("store emitted SQL through the caller's logger")
	}
}

type commitPool struct {
	gorm.ConnPool
	db      *sql.DB
	reached *bool
}

func (p commitPool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.db.BeginTx(ctx, options)
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

func TestAssignmentCommitFailureRollsBack(t *testing.T) {
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
	store, err := clientassign.New(faultDB, clientassign.Options{})
	if err != nil {
		t.Fatal(err)
	}
	request := input()
	request.UserName = field(f.owner.Username)
	if err := store.Assign(context.Background(), f.actor, request); !errors.Is(err, clientassign.ErrUnavailable) || !reached {
		t.Fatal("real commit boundary was not reached/rejected", err)
	}
	peer := readPeer(t, f)
	if peer.Hostname != "before-host" || peer.Note != "before-note" || peer.UserId != 0 {
		t.Fatal("commit failure persisted partial assignment")
	}
}

func TestAssignmentRechecksOwnerNameAndExpiryAfterLookup(t *testing.T) {
	for _, mode := range []string{"owner-renamed", "expiry-after-peer-read", "session-query-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			clock := time.Now().UTC()
			store, err := clientassign.New(f.db, clientassign.Options{Now: func() time.Time { return clock }})
			if err != nil {
				t.Fatal(err)
			}
			triggered := false
			if err := f.db.Callback().Query().After("gorm:query").Register("cli-after-lookup", func(tx *gorm.DB) {
				if triggered {
					return
				}
				switch {
				case mode == "owner-renamed" && tx.Statement.Table == "users" && strings.Contains(tx.Statement.SQL.String(), "username"):
					triggered = true
					changed := tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE users SET username = ? WHERE id = ?", "renamed-owner", f.owner.Id)
					if changed.Error != nil || changed.RowsAffected != 1 {
						tx.AddError(errors.New("owner transition injection failed"))
					}
				case mode == "expiry-after-peer-read" && tx.Statement.Table == "peers":
					triggered = true
					clock = f.actor.Session.ExpiresAt.Add(time.Second)
				case mode == "session-query-failure" && tx.Statement.Table == "user_tokens":
					triggered = true
					tx.AddError(errors.New("private assignment session query failure"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			request := input()
			want := auth.ErrUnauthenticated
			if mode == "owner-renamed" {
				request.UserName = field(f.owner.Username)
				want = clientassign.ErrInput
			}
			if mode == "session-query-failure" {
				want = clientassign.ErrUnavailable
			}
			if err := store.Assign(context.Background(), f.actor, request); !errors.Is(err, want) || !triggered {
				t.Fatalf("lookup transition was not revalidated: %v, triggered=%t", err, triggered)
			}
			if peer := readPeer(t, f); peer.Note != "before-note" || peer.Hostname != "before-host" || peer.UserId != 0 {
				t.Fatal("lookup transition produced a partial assignment")
			}
		})
	}
}
