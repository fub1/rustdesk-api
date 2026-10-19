package clientcompat_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type failingCommitPool struct {
	gorm.ConnPool
	db *sql.DB
}

func (p failingCommitPool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.db.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &failingCommitTx{Tx: tx}, nil
}

type failingCommitTx struct{ *sql.Tx }

func (tx failingCommitTx) Commit() error {
	_ = tx.Tx.Rollback()
	return errors.New("private commit fault")
}

func TestPersonalReplaceReportsCommitFailureWithoutPartialWrites(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "commit.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	pool.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.User{}, &model.AddressBook{}, &model.Tag{}, &model.Peer{}); err != nil {
		t.Fatal(err)
	}
	u := model.User{Username: "actor", GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	row := model.AddressBook{Id: "existing", UserId: u.Id, Alias: "before", Note: "existing-note", Tags: custom_types.AutoJson(`[]`)}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	tag := model.Tag{UserId: u.Id, Name: "before"}
	if err := db.Create(&tag).Error; err != nil {
		t.Fatal(err)
	}
	faultDB, err := gorm.Open(sqlite.Dialector{Conn: failingCommitPool{ConnPool: pool, db: pool}}, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	store, err := clientcompat.New(faultDB)
	if err != nil {
		t.Fatal(err)
	}
	doc := clientcompat.Document{Peers: []clientcompat.PeerInput{{ID: "existing", Alias: clientcompat.StringField{Present: true, Value: "after"}, Note: clientcompat.StringField{Present: true, Value: "new-note"}}}, Tags: []string{"after"}, TagColors: `{"after":1}`}
	if err := store.ReplacePersonal(context.Background(), clientcompat.Actor{ID: u.Id}, doc); !errors.Is(err, clientcompat.ErrUnavailable) {
		t.Fatal("commit failure acknowledged", err)
	}
	var got model.AddressBook
	if err := db.First(&got, row.RowId).Error; err != nil {
		t.Fatal(err)
	}
	if got.Alias != "before" || got.Note != "existing-note" {
		t.Fatal("failed commit left updated peer")
	}
	var tags []model.Tag
	if err := db.Find(&tags).Error; err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Name != "before" {
		t.Fatal("failed commit left changed tags")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.ReplacePersonal(ctx, clientcompat.Actor{ID: u.Id}, doc); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not propagated", err)
	}
}
