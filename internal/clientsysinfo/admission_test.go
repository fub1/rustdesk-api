package clientsysinfo_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/internal/clientsysinfo"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSysInfoStoreRejectsTypedAndDecodedManagementBeforeDatabaseAccess(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "admission.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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
	// Use the runtime schema for current-model peer writes.
	if err := database.Update(db, 281, nil); err != nil {
		t.Fatal(err)
	}
	store, err := clientsysinfo.New(db)
	if err != nil {
		t.Fatal(err)
	}
	inputs := []clientsysinfo.Input{{ID: "name", PresetAddressBookName: field("private")}, {ID: "alias", PresetAddressBookAlias: field("private")}}
	var decoded clientsysinfo.Input
	if err := json.Unmarshal([]byte(`{"id":"decoded","preset-future-option":"private"}`), &decoded); err != nil {
		t.Fatal(err)
	}
	inputs = append(inputs, decoded)
	queries := 0
	if err := db.Callback().Query().Before("gorm:query").Register("admission-query-observer", func(*gorm.DB) { queries++ }); err != nil {
		t.Fatal(err)
	}
	for _, input := range inputs {
		if err := store.Report(context.Background(), input); !errors.Is(err, clientsysinfo.ErrManagementPreset) {
			t.Errorf("direct Store accepted management input: %s", input.ID)
		}
	}
	if queries != 0 {
		t.Fatalf("rejected input reached database %d times", queries)
	}
}
