package clientcompat_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSharedWritesReportCommitFailureAndRollback(t *testing.T) {
	for _, action := range []string{"add", "rename", "color", "tags-delete", "peers-delete"} {
		t.Run(action, func(t *testing.T) {
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
			if err := db.AutoMigrate(&model.User{}, &model.Tag{}, &model.AddressBook{}); err != nil {
				t.Fatal(err)
			}
			u := model.User{Username: "actor", GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
			if err := db.Create(&u).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.Tag{UserId: u.Id, Name: "old", Color: 7}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.AddressBook{UserId: u.Id, Id: "peer"}).Error; err != nil {
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
			ctx, actor, raw := context.Background(), clientcompat.Actor{ID: u.Id}, fmt.Sprintf("1-%d-0", u.Id)
			switch action {
			case "add":
				err = store.AddTag(ctx, actor, raw, clientcompat.Tag{Name: "new", Color: 8})
			case "rename":
				err = store.RenameTag(ctx, actor, raw, "old", "new")
			case "color":
				err = store.ColorTag(ctx, actor, raw, clientcompat.Tag{Name: "old", Color: 0})
			case "tags-delete":
				err = store.DeleteTags(ctx, actor, raw, []string{"old"})
			case "peers-delete":
				err = store.DeletePeers(ctx, actor, raw, []string{"peer"})
			}
			if !errors.Is(err, clientcompat.ErrUnavailable) {
				t.Fatalf("commit failed without unavailable result: %v", err)
			}
			var tags []model.Tag
			if err := db.Find(&tags).Error; err != nil {
				t.Fatal(err)
			}
			if len(tags) != 1 || tags[0].Name != "old" || tags[0].Color != 7 {
				t.Fatalf("tag write escaped failed commit: %+v", tags)
			}
			var count int64
			if err := db.Model(&model.AddressBook{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatal("peer delete escaped failed commit")
			}
		})
	}
}
