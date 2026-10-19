package clientcompat_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSQLiteTagIndependentPoolsSerializeReferences(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-target=%t", conflict), func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "tags.db") + "?_txlock=immediate&_journal_mode=WAL&_busy_timeout=5000"
			open := func() *gorm.DB {
				db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
				if err != nil {
					t.Fatal(err)
				}
				pool, err := db.DB()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = pool.Close() })
				return db
			}
			db := open()
			if err := db.AutoMigrate(&model.User{}, &model.AddressBook{}, &model.Tag{}); err != nil {
				t.Fatal(err)
			}
			owner := model.User{Username: "owner", GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
			if err := db.Create(&owner).Error; err != nil {
				t.Fatal(err)
			}
			labels := []model.Tag{{UserId: owner.Id, Name: "old-a", Color: 1}, {UserId: owner.Id, Name: "old-b", Color: 2}}
			if err := db.Create(&labels).Error; err != nil {
				t.Fatal(err)
			}
			row := model.AddressBook{Id: "both", UserId: owner.Id, Tags: custom_types.AutoJson(`["old-a","old-b"]`), Note: "kept"}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				index int
				err   error
			}
			start, results := make(chan struct{}), make(chan outcome, 2)
			for i := range labels {
				store, err := clientcompat.New(open())
				if err != nil {
					t.Fatal(err)
				}
				go func(i int, store *clientcompat.Store) {
					<-start
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					name := fmt.Sprintf("new-%c", 'a'+i)
					if conflict {
						name = "same"
					}
					results <- outcome{i, store.RenameTag(ctx, clientcompat.Actor{ID: owner.Id}, fmt.Sprintf("1-%d-0", owner.Id), labels[i].Name, name)}
				}(i, store)
			}
			close(start)
			winner, failures := -1, 0
			for i := 0; i < 2; i++ {
				result := <-results
				if result.err == nil {
					winner = result.index
				} else {
					failures++
					if !conflict || !errors.Is(result.err, clientcompat.ErrInput) {
						t.Fatal("unexpected SQLite competition failure", result.err)
					}
				}
			}
			want := `["new-a","new-b"]`
			if conflict {
				if winner < 0 || failures != 1 {
					t.Fatalf("winner=%d failures=%d", winner, failures)
				}
				if winner == 0 {
					want = `["same","old-b"]`
				} else {
					want = `["old-a","same"]`
				}
			}
			if err := db.First(&row, row.RowId).Error; err != nil || string(row.Tags) != want || row.Note != "kept" {
				t.Fatalf("SQLite lost reference/metadata: %s %v", row.Tags, err)
			}
		})
	}
}
