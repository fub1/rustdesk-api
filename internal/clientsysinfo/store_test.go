package clientsysinfo_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientsysinfo"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSysInfoStoreInstancesNeverFollowLegacyGlobalDatabase(t *testing.T) {
	previousGlobal, previousService := global.DB, service.DB
	t.Cleanup(func() { global.DB, service.DB = previousGlobal, previousService })
	var stores [2]*clientsysinfo.Store
	var databases [2]*gorm.DB
	for i := range stores {
		db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "store.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
		if err != nil {
			t.Fatal(err)
		}
		pool, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pool.Close() })
		if err := db.AutoMigrate(&model.Peer{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.Peer{Id: "device", UserId: uint(11 + i), Alias: fmt.Sprintf("managed-%d", i)}).Error; err != nil {
			t.Fatal(err)
		}
		stores[i], err = clientsysinfo.New(db)
		if err != nil {
			t.Fatal(err)
		}
		databases[i] = db
	}
	for i, store := range stores {
		global.DB, service.DB = databases[1-i], databases[1-i]
		input := clientsysinfo.Input{ID: "device", UUID: field("uuid"), Username: field(fmt.Sprintf("user-%d", i))}
		if err := store.Report(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		if err := store.Report(context.Background(), clientsysinfo.Input{ID: "new", Username: field(fmt.Sprintf("new-%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	for i, db := range databases {
		var peer model.Peer
		if err := db.Where("id = ?", "device").First(&peer).Error; err != nil {
			t.Fatal(err)
		}
		if peer.UserId != uint(11+i) || peer.Username != fmt.Sprintf("user-%d", i) || peer.Alias != fmt.Sprintf("managed-%d", i) {
			t.Fatalf("instance %d followed global database: %+v", i, peer)
		}
		var created model.Peer
		if err := db.Where("id = ?", "new").First(&created).Error; err != nil {
			t.Fatal(err)
		}
		if created.UserId != 0 || created.Username != fmt.Sprintf("new-%d", i) {
			t.Fatalf("instance %d creation followed globals or assigned owner: %+v", i, created)
		}
	}
}

func field(value string) clientsysinfo.OptionalString {
	return clientsysinfo.OptionalString{Present: true, Value: value}
}
