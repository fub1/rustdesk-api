package database

import (
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

func oldAddressBookDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := openTestDB(t)
	if err := Update(db, 275, seedTestData); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AddressBook{Id: "old", UserId: 1, Note: "existing"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropColumn(&model.AddressBook{}, "Note"); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestUpgrade275AddressBookNoteAndPartialDDLRetry(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "old_schema", true: "nullable_partial_schema"}[partial], func(t *testing.T) {
			db := oldAddressBookDB(t)
			if partial {
				if err := db.Migrator().AddColumn(&nullableAddressBookNote{}, "Note"); err != nil {
					t.Fatal(err)
				}
				mustExec(t, db, "INSERT INTO address_books (id,user_id,tags,note) VALUES ('keep',1,'[]','保留备注')")
			}
			mustExec(t, db, "UPDATE address_books SET updated_at = '2025-01-02 03:04:05'")
			if err := Update(db, 276, seedTestData); err != nil {
				t.Fatal(err)
			}
			assertCurrentVersion(t, db, 276)
			var old model.AddressBook
			if err := db.Where("id = ?", "old").First(&old).Error; err != nil {
				t.Fatal(err)
			}
			if old.Note != "" || time.Time(old.UpdatedAt).UTC().Format("2006-01-02 15:04:05") != "2025-01-02 03:04:05" {
				t.Fatalf("backfill changed old row: %+v", old)
			}
			if partial {
				var keep model.AddressBook
				if err := db.Where("id = ?", "keep").First(&keep).Error; err != nil || keep.Note != "保留备注" {
					t.Fatalf("existing note lost: %+v / %v", keep, err)
				}
			}
			if err := db.Exec("UPDATE address_books SET note = NULL WHERE id = 'old'").Error; err == nil {
				t.Fatal("note must be NOT NULL after migration")
			}
			fresh := model.AddressBook{Id: "fresh", UserId: 1}
			if err := db.Create(&fresh).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.First(&fresh, fresh.RowId).Error; err != nil || fresh.Note != "" {
				t.Fatalf("application empty note default: %+v / %v", fresh, err)
			}
			if err := Update(db, 276, seedTestData); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNoteMigrationVersionFailureRollsBackSchemaAndCanRetry(t *testing.T) {
	db := oldAddressBookDB(t)
	mustExec(t, db, "CREATE TRIGGER fail_note_version BEFORE INSERT ON versions BEGIN SELECT RAISE(ABORT, 'version failure'); END")
	if err := Update(db, 276, seedTestData); err == nil {
		t.Fatal("version failure must fail migration")
	}
	assertCurrentVersion(t, db, 275)
	if db.Migrator().HasColumn(&model.AddressBook{}, "Note") {
		t.Fatal("SQLite schema committed before version")
	}
	mustExec(t, db, "DROP TRIGGER fail_note_version")
	if err := Update(db, 276, seedTestData); err != nil {
		t.Fatal(err)
	}
	assertCurrentVersion(t, db, 276)
}
