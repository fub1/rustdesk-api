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
		if err := db.AutoMigrate(&model.Peer{}, &model.LoginLog{}, &model.AddressBookCollection{}, &model.AddressBook{}); err != nil {
			t.Fatal(err)
		}
		// Exercise peer creation, login association, preset lookup/create and
		// cleanup through each instance while globals point at the other DB.
		if err := db.Create(&model.LoginLog{UserId: uint(11 + i), DeviceId: "device", Uuid: "uuid"}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.AddressBookCollection{UserId: 1, Name: "target"}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.AddressBook{UserId: 1, Id: "device"}).Error; err != nil {
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
		input := clientsysinfo.Input{ID: "device", UUID: field("uuid"), Username: field(fmt.Sprintf("user-%d", i)), PresetAddressBookName: field("target"), PresetAddressBookAlias: field(fmt.Sprintf("alias-%d", i))}
		if err := store.Report(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	for i, db := range databases {
		var peer model.Peer
		if err := db.Where("id = ?", "device").First(&peer).Error; err != nil {
			t.Fatal(err)
		}
		if peer.UserId != uint(11+i) || peer.Username != fmt.Sprintf("user-%d", i) || peer.Alias != fmt.Sprintf("alias-%d", i) {
			t.Fatalf("instance %d followed global database: %+v", i, peer)
		}
		var rows []model.AddressBook
		if err := db.Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].CollectionId == 0 || rows[0].Username != peer.Username || rows[0].Alias != peer.Alias {
			t.Fatalf("instance %d preset escaped transaction/database: %+v", i, rows)
		}
	}
}

func field(value string) clientsysinfo.OptionalString {
	return clientsysinfo.OptionalString{Present: true, Value: value}
}
