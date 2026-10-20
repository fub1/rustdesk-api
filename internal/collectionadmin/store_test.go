package collectionadmin_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionadmin"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type fixture struct {
	db                  *gorm.DB
	store               *collectionadmin.Store
	admin, owner, other auth.Principal
}

func createUser(t *testing.T, db *gorm.DB, name string, admin bool) auth.Principal {
	t.Helper()
	u := model.User{Username: name, Status: model.COMMON_STATUS_ENABLE, IsAdmin: &admin}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	session := model.UserToken{UserId: u.Id, Token: "private-" + name, ExpiredAt: end.Unix()}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	return auth.Principal{User: auth.User{ID: u.Id, IsAdmin: admin, Enabled: true}, Session: auth.Session{ID: session.Id, UserID: u.Id, Token: session.Token, ExpiresAt: end}}
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "collections.db")+"?_foreign_keys=on&_txlock=immediate&_busy_timeout=5000"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := database.Update(db, 281, nil); err != nil {
		t.Fatal(err)
	}
	store, err := collectionadmin.New(db, collectionadmin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{db: db, store: store, admin: createUser(t, db, "admin", true), owner: createUser(t, db, "owner", false), other: createUser(t, db, "other", false)}
}
func create(t *testing.T, f *fixture, actor auth.Principal, mode collectionadmin.Mode, owner uint, name string) collectionadmin.Collection {
	t.Helper()
	row, err := f.store.Create(context.Background(), actor, mode, collectionadmin.Input{UserID: owner, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return row
}
func read(t *testing.T, f *fixture, id uint) model.AddressBookCollection {
	t.Helper()
	var row model.AddressBookCollection
	if err := f.db.First(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	return row
}
func count(t *testing.T, f *fixture, row any) int64 {
	t.Helper()
	var n int64
	if err := f.db.Model(row).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCollectionsFixedOwnerLifecycleAndExplicitProjection(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	own := create(t, f, f.owner, collectionadmin.My, 0, " own ")
	create(t, f, f.owner, collectionadmin.My, f.owner.User.ID, " own ") // no new uniqueness policy
	foreign := create(t, f, f.admin, collectionadmin.Admin, f.other.User.ID, "foreign")
	if err := f.db.Create(&model.AddressBookCollectionRule{UserId: foreign.UserID, CollectionId: foreign.ID, Type: model.ShareAddressBookRuleTypePersonal, Rule: model.ShareAddressBookRuleRuleFullControl, ToId: f.owner.User.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if own.UserID != f.owner.User.ID || own.Name != " own " {
		t.Fatal("my creation changed owner/name")
	}
	for _, user := range []uint{0, f.owner.User.ID} {
		before := read(t, f, own.ID)
		got, err := f.store.Update(ctx, f.owner, collectionadmin.My, collectionadmin.Input{ID: own.ID, UserID: user, Name: "renamed"})
		if err != nil || got.UserID != f.owner.User.ID {
			t.Fatal(err)
		}
		after := read(t, f, own.ID)
		if after.UserId != before.UserId || after.CreatedAt != before.CreatedAt {
			t.Fatal("protected fields changed")
		}
	}
	before := read(t, f, own.ID)
	for _, mode := range []collectionadmin.Mode{collectionadmin.My, collectionadmin.Admin} {
		actor := f.owner
		if mode == collectionadmin.Admin {
			actor = f.admin
		}
		if got, err := f.store.Update(ctx, actor, mode, collectionadmin.Input{ID: own.ID, UserID: f.other.User.ID, Name: "transfer"}); !errors.Is(err, collectionadmin.ErrInput) || got.ID != 0 {
			t.Fatal("owner transfer accepted", err)
		}
	}
	if !reflect.DeepEqual(before, read(t, f, own.ID)) {
		t.Fatal("rejected transfer retained a write")
	}
	for _, operation := range []string{"get", "update", "delete"} {
		var err error
		switch operation {
		case "get":
			_, err = f.store.Get(ctx, f.owner, collectionadmin.My, foreign.ID)
		case "update":
			_, err = f.store.Update(ctx, f.owner, collectionadmin.My, collectionadmin.Input{ID: foreign.ID, Name: "forbidden"})
		case "delete":
			err = f.store.Delete(ctx, f.owner, collectionadmin.My, collectionadmin.Input{ID: foreign.ID})
		}
		if !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("my accessed another owner", operation, err)
		}
	}
	total, rows, err := f.store.List(ctx, f.owner, collectionadmin.My, collectionadmin.Page{Current: 1, Size: 20}, f.other.User.ID)
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatal("my list was filtered into another owner", err)
	}
	total, rows, err = f.store.List(ctx, f.admin, collectionadmin.Admin, collectionadmin.Page{Current: 1, Size: 1}, f.other.User.ID)
	if err != nil || total != 1 || len(rows) != 1 || rows[0].ID != foreign.ID {
		t.Fatal("admin filter failed", err)
	}
	if err := f.store.Delete(ctx, f.owner, collectionadmin.My, collectionadmin.Input{ID: own.ID, UserID: f.owner.User.ID, Name: "ignored"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Get(ctx, f.admin, collectionadmin.Admin, own.ID); !errors.Is(err, collectionadmin.ErrNotFound) {
		t.Fatal("deleted collection remained", err)
	}
}

func TestCollectionsValidateCreationAndCurrentOwner(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		actor auth.Principal
		mode  collectionadmin.Mode
		input collectionadmin.Input
	}{
		{f.owner, collectionadmin.My, collectionadmin.Input{UserID: f.other.User.ID, Name: "bad"}},
		{f.owner, collectionadmin.My, collectionadmin.Input{ID: 999, Name: "bad"}},
		{f.owner, collectionadmin.My, collectionadmin.Input{Name: ""}},
		{f.admin, collectionadmin.Admin, collectionadmin.Input{Name: "bad"}},
		{f.admin, collectionadmin.Admin, collectionadmin.Input{UserID: 99999, Name: "bad"}},
	} {
		if got, err := f.store.Create(ctx, tc.actor, tc.mode, tc.input); !errors.Is(err, collectionadmin.ErrInput) || got.ID != 0 {
			t.Fatal("bad create accepted", err)
		}
	}
	if err := f.db.Model(&model.User{}).Where("id=?", f.other.User.ID).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Create(ctx, f.admin, collectionadmin.Admin, collectionadmin.Input{UserID: f.other.User.ID, Name: "disabled-owner"}); !errors.Is(err, collectionadmin.ErrInput) {
		t.Fatal("disabled owner accepted", err)
	}
	if count(t, f, &model.AddressBookCollection{}) != 0 {
		t.Fatal("invalid create retained data")
	}
}

func TestCollectionsAllOperationsRecheckCurrentAdministratorAndUniqueSession(t *testing.T) {
	for _, change := range []string{"disabled", "demoted", "revoked", "db-expired", "jwt-expired", "duplicate-session"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			row := create(t, f, f.owner, collectionadmin.My, 0, "keep")
			actor := f.admin
			switch change {
			case "disabled":
				if err := f.db.Model(&model.User{}).Where("id=?", actor.User.ID).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
					t.Fatal(err)
				}
			case "demoted":
				if err := f.db.Model(&model.User{}).Where("id=?", actor.User.ID).UpdateColumn("is_admin", false).Error; err != nil {
					t.Fatal(err)
				}
			case "revoked":
				if err := f.db.Where("id=?", actor.Session.ID).Delete(&model.UserToken{}).Error; err != nil {
					t.Fatal(err)
				}
			case "db-expired":
				if err := f.db.Model(&model.UserToken{}).Where("id=?", actor.Session.ID).UpdateColumn("expired_at", time.Now().Add(-time.Hour).Unix()).Error; err != nil {
					t.Fatal(err)
				}
			case "jwt-expired":
				actor.Session.ExpiresAt = time.Now().Add(-time.Hour)
			case "duplicate-session":
				if err := f.db.Create(&model.UserToken{UserId: actor.User.ID, Token: actor.Session.Token, ExpiredAt: actor.Session.ExpiresAt.Unix()}).Error; err != nil {
					t.Fatal(err)
				}
			}
			want := auth.ErrUnauthenticated
			if change == "demoted" {
				want = auth.ErrForbidden
			}
			before := read(t, f, row.ID)
			operations := []func() error{
				func() error {
					got, err := f.store.Create(context.Background(), actor, collectionadmin.Admin, collectionadmin.Input{UserID: f.owner.User.ID, Name: "forbidden"})
					if got.ID != 0 {
						t.Fatal("failed create returned data")
					}
					return err
				},
				func() error {
					got, err := f.store.Update(context.Background(), actor, collectionadmin.Admin, collectionadmin.Input{ID: row.ID, Name: "forbidden"})
					if got.ID != 0 {
						t.Fatal("failed update returned data")
					}
					return err
				},
				func() error {
					got, err := f.store.Get(context.Background(), actor, collectionadmin.Admin, row.ID)
					if got.ID != 0 {
						t.Fatal("failed get returned data")
					}
					return err
				},
				func() error {
					n, got, err := f.store.List(context.Background(), actor, collectionadmin.Admin, collectionadmin.Page{Current: 1, Size: 20}, 0)
					if n != 0 || got != nil {
						t.Fatal("failed list returned data")
					}
					return err
				},
				func() error {
					return f.store.Delete(context.Background(), actor, collectionadmin.Admin, collectionadmin.Input{ID: row.ID})
				},
			}
			for _, op := range operations {
				if err := op(); !errors.Is(err, want) {
					t.Fatal("stale principal accepted", err)
				}
			}
			if !reflect.DeepEqual(before, read(t, f, row.ID)) || count(t, f, &model.AddressBookCollection{}) != 1 {
				t.Fatal("failed actor retained collection writes")
			}
		})
	}
}

func TestCollectionsDeleteRejectsActualPeerReferenceAndRemovesScopedChildren(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	row := create(t, f, f.owner, collectionadmin.My, 0, "target")
	sentinel := create(t, f, f.other, collectionadmin.My, 0, "sentinel")
	for _, book := range []collectionadmin.Collection{row, sentinel} {
		for _, entity := range []any{&model.AddressBook{Id: "device", UserId: book.UserID, CollectionId: book.ID}, &model.Tag{UserId: book.UserID, CollectionId: book.ID, Name: "tag"}, &model.AddressBookCollectionRule{UserId: book.UserID, CollectionId: book.ID, Type: 1, Rule: 3, ToId: f.admin.User.ID}} {
			if err := f.db.Create(entity).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	peer := model.Peer{Id: "live-device", Uuid: "uuid", AbId: row.ID}
	if err := f.db.Create(&peer).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(ctx, f.owner, collectionadmin.My, collectionadmin.Input{ID: row.ID}); !errors.Is(err, collectionadmin.ErrReferenced) {
		t.Fatal("referenced collection deleted", err)
	}
	for _, entity := range []any{&model.AddressBook{}, &model.Tag{}, &model.AddressBookCollectionRule{}, &model.AddressBookCollection{}} {
		if count(t, f, entity) != 2 {
			t.Fatal("reference failure retained deletions")
		}
	}
	if err := f.db.Model(&peer).Updates(map[string]any{"ab_id": 0, "preset_ab_id": row.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(ctx, f.owner, collectionadmin.My, collectionadmin.Input{ID: row.ID}); err != nil {
		t.Fatal("non-consumed preset cache blocked delete", err)
	}
	for _, entity := range []any{&model.AddressBook{}, &model.Tag{}, &model.AddressBookCollectionRule{}, &model.AddressBookCollection{}} {
		if count(t, f, entity) != 1 {
			t.Fatal("delete left scoped children or deleted sentinel")
		}
	}
	var stored model.Peer
	if err := f.db.First(&stored, peer.RowId).Error; err != nil || stored.PresetAbId != row.ID {
		t.Fatal("delete guessed cache repair", err)
	}
}

func TestCollectionsOwnerAndSessionChangedAfterCandidateReadCannotWrite(t *testing.T) {
	for _, change := range []string{"owner", "revoke"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			row := create(t, f, f.owner, collectionadmin.My, 0, "keep")
			fired := false
			if err := f.db.Callback().Query().After("gorm:query").Register("collection:candidate-change", func(tx *gorm.DB) {
				if fired || tx.Statement.Table != "address_book_collections" {
					return
				}
				fired = true
				var err error
				if change == "owner" {
					err = tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE address_book_collections SET user_id=? WHERE id=?", f.other.User.ID, row.ID).Error
				} else {
					err = tx.Session(&gorm.Session{NewDB: true}).Exec("DELETE FROM user_tokens WHERE id=?", f.owner.Session.ID).Error
				}
				if err != nil {
					tx.AddError(err)
				}
			}); err != nil {
				t.Fatal(err)
			}
			got, err := f.store.Update(context.Background(), f.owner, collectionadmin.My, collectionadmin.Input{ID: row.ID, Name: "must-not-write"})
			if err == nil || got.ID != 0 || !fired {
				t.Fatal("candidate race accepted", err)
			}
			if err := f.db.Callback().Query().Remove("collection:candidate-change"); err != nil {
				t.Fatal(err)
			}
			if read(t, f, row.ID).Name != "keep" {
				t.Fatal("candidate race retained name update")
			}
		})
	}
}

type commitPool struct {
	gorm.ConnPool
	db      *sql.DB
	reached *bool
}

func (p commitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &commitTx{Tx: tx, reached: p.reached}, nil
}

type commitTx struct {
	*sql.Tx
	reached *bool
}

func (tx *commitTx) Commit() error {
	*tx.reached = true
	_ = tx.Tx.Rollback()
	return errors.New("private commit error")
}

func TestCollectionsCommitAndChildFailureRollBackAndRetry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	row := create(t, f, f.owner, collectionadmin.My, 0, "keep")
	if err := f.db.Create(&model.AddressBook{Id: "device", UserId: row.UserID, CollectionId: row.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&model.Tag{UserId: row.UserID, CollectionId: row.ID, Name: "keep"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec("CREATE TRIGGER reject_tag_deletion BEFORE DELETE ON tags BEGIN SELECT RAISE(ABORT,'private deletion failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(ctx, f.owner, collectionadmin.My, collectionadmin.Input{ID: row.ID}); !errors.Is(err, collectionadmin.ErrUnavailable) {
		t.Fatal("child failure acknowledged", err)
	}
	if count(t, f, &model.AddressBook{}) != 1 || count(t, f, &model.Tag{}) != 1 || count(t, f, &model.AddressBookCollection{}) != 1 {
		t.Fatal("child failure partially committed")
	}
	if err := f.db.Exec("DROP TRIGGER reject_tag_deletion").Error; err != nil {
		t.Fatal(err)
	}
	pool, err := f.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	reached := false
	faultDB, err := gorm.Open(sqlite.Dialector{Conn: commitPool{ConnPool: pool, db: pool, reached: &reached}}, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	store, err := collectionadmin.New(faultDB, collectionadmin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Update(ctx, f.owner, collectionadmin.My, collectionadmin.Input{ID: row.ID, Name: "failed"})
	if !errors.Is(err, collectionadmin.ErrUnavailable) || got.ID != 0 || !reached || read(t, f, row.ID).Name != "keep" {
		t.Fatal("actual commit failure acknowledged or retained data", err)
	}
	if err := f.store.Delete(ctx, f.owner, collectionadmin.My, collectionadmin.Input{ID: row.ID}); err != nil {
		t.Fatal("delete retry failed", err)
	}
}

func TestCollectionsExpiryAfterWriteAndCanceledContextNeverPublish(t *testing.T) {
	f := newFixture(t)
	clock := time.Now().UTC()
	store, err := collectionadmin.New(f.db, collectionadmin.Options{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	reached := false
	if err := f.db.Callback().Create().After("gorm:create").Register("collection:expire", func(tx *gorm.DB) {
		if tx.Statement.Table == "address_book_collections" {
			reached = true
			clock = f.owner.Session.ExpiresAt.Add(time.Second)
		}
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Create(context.Background(), f.owner, collectionadmin.My, collectionadmin.Input{Name: "must-rollback"})
	if !errors.Is(err, auth.ErrUnauthenticated) || got.ID != 0 || !reached || count(t, f, &model.AddressBookCollection{}) != 0 {
		t.Fatal("elapsed session retained write/result", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := f.store.Create(ctx, f.owner, collectionadmin.My, collectionadmin.Input{Name: "canceled"}); !errors.Is(err, context.Canceled) || got.ID != 0 {
		t.Fatal("canceled create accepted", err)
	}
}

func TestCollectionsInstanceSilentSQLAndQueryFailureBoundaries(t *testing.T) {
	first, second := newFixture(t), newFixture(t)
	original := create(t, first, first.owner, collectionadmin.My, 0, "first-instance")
	other := create(t, second, second.owner, collectionadmin.My, 0, "second-instance")
	var output bytes.Buffer
	first.db.Logger = logger.New(log.New(&output, "", 0), logger.Config{LogLevel: logger.Info})
	store, err := collectionadmin.New(first.db, collectionadmin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Update(context.Background(), first.owner, collectionadmin.My, collectionadmin.Input{ID: original.ID, Name: "first-only"})
	if err != nil || got.Name != "first-only" {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatal("store used caller SQL logger; credentials may leak")
	}
	if read(t, second, other.ID).Name != "second-instance" {
		t.Fatal("store used another instance DB")
	}
	first.db.Logger = logger.Default.LogMode(logger.Silent)
	if err := first.db.Callback().Query().Before("gorm:query").Register("collection:query-error", func(tx *gorm.DB) {
		if tx.Statement.Table == "address_book_collections" {
			tx.AddError(errors.New("private database cause"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	result, err := store.Get(context.Background(), first.owner, collectionadmin.My, original.ID)
	if !errors.Is(err, collectionadmin.ErrUnavailable) || !reflect.DeepEqual(result, collectionadmin.Collection{}) {
		t.Fatal("query failure leaked cause/result", err)
	}
	total, rows, err := store.List(context.Background(), first.admin, collectionadmin.Admin, collectionadmin.Page{Current: 1, Size: 20}, 0)
	if !errors.Is(err, collectionadmin.ErrUnavailable) || total != 0 || rows != nil {
		t.Fatal("failed list returned partial success", err)
	}
	if _, err := collectionadmin.New(nil, collectionadmin.Options{}); !errors.Is(err, collectionadmin.ErrUnavailable) {
		t.Fatal("nil database accepted", err)
	}
}

func TestCollectionsCandidateReadHonorsEffectiveJWTDeadline(t *testing.T) {
	f := newFixture(t)
	row := create(t, f, f.owner, collectionadmin.My, 0, "keep")
	actor := f.owner
	actor.Session.ExpiresAt = time.Now().Add(100 * time.Millisecond)
	fired := false
	if err := f.db.Callback().Query().Before("gorm:query").Register("collection:wait-candidate", func(tx *gorm.DB) {
		if tx.Statement.Table == "address_book_collections" {
			fired = true
			time.Sleep(200 * time.Millisecond)
		}
	}); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Update(context.Background(), actor, collectionadmin.My, collectionadmin.Input{ID: row.ID, Name: "must-not-write"})
	if !errors.Is(err, context.DeadlineExceeded) || got.ID != 0 || !fired {
		t.Fatal("candidate read escaped effective expiry deadline", err)
	}
	if err := f.db.Callback().Query().Remove("collection:wait-candidate"); err != nil {
		t.Fatal(err)
	}
	if read(t, f, row.ID).Name != "keep" {
		t.Fatal("expired candidate read retained write")
	}
}

func TestCollectionsAdministratorMayMaintainHistoricalOwnerZero(t *testing.T) {
	f := newFixture(t)
	legacy := model.AddressBookCollection{Name: "historical", UserId: 0}
	if err := f.db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	peer := model.Peer{Id: "historical-reference", Uuid: "uuid", AbId: legacy.Id}
	if err := f.db.Create(&peer).Error; err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"get", "update", "delete"} {
		var err error
		switch op {
		case "get":
			_, err = f.store.Get(context.Background(), f.owner, collectionadmin.My, legacy.Id)
		case "update":
			_, err = f.store.Update(context.Background(), f.owner, collectionadmin.My, collectionadmin.Input{ID: legacy.Id, Name: "forbidden"})
		case "delete":
			err = f.store.Delete(context.Background(), f.owner, collectionadmin.My, collectionadmin.Input{ID: legacy.Id})
		}
		if !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("my accessed owner-zero history", op, err)
		}
	}
	var unchanged model.Peer
	if err := f.db.First(&unchanged, peer.RowId).Error; err != nil || unchanged.AbId != legacy.Id || read(t, f, legacy.Id).Name != "historical" {
		t.Fatal("my owner-zero rejection changed historical data", err)
	}
	if err := f.db.Model(&peer).UpdateColumn("ab_id", 0).Error; err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Get(context.Background(), f.admin, collectionadmin.Admin, legacy.Id)
	if err != nil || got.UserID != 0 {
		t.Fatal("admin cannot inspect historical owner-zero", err)
	}
	got, err = f.store.Update(context.Background(), f.admin, collectionadmin.Admin, collectionadmin.Input{ID: legacy.Id, Name: "historical-renamed"})
	if err != nil || got.UserID != 0 || got.Name != "historical-renamed" {
		t.Fatal("admin historical rename failed", err)
	}
	if _, err := f.store.Create(context.Background(), f.admin, collectionadmin.Admin, collectionadmin.Input{Name: "new-owner-zero"}); !errors.Is(err, collectionadmin.ErrInput) {
		t.Fatal("historical compatibility allowed new owner-zero", err)
	}
	if err := f.store.Delete(context.Background(), f.admin, collectionadmin.Admin, collectionadmin.Input{ID: legacy.Id}); err != nil {
		t.Fatal("admin historical cleanup failed", err)
	}
}
