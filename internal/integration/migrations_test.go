package integration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

func upgrade(t *testing.T, db *gorm.DB, version uint) {
	t.Helper()
	if err := database.Update(db, version, nil); err != nil {
		t.Fatalf("upgrade to %d: %v", version, err)
	}
	var latest model.Version
	if err := db.Last(&latest).Error; err != nil || latest.Version != version {
		t.Fatalf("version = %+v / %v", latest, err)
	}
}

func rows(t *testing.T, db *gorm.DB, value any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(value).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRealDatabaseFreshMigrationAndRepeatedStartup(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		upgrade(t, f.DB, 278)
		if rows(t, f.DB, &model.Version{}) != 1 || rows(t, f.DB, &model.User{}) != 1 || rows(t, f.DB, &model.Group{}) != 2 {
			t.Fatal("repeated startup duplicated seed/version")
		}
		for _, model := range []any{&model.OIDCIdentity{}, &model.AuditDeliveryReceipt{}, &model.PeerStrategyState{}} {
			if !f.DB.Migrator().HasTable(model) {
				t.Fatalf("missing migrated table %T", model)
			}
		}
	})
}

func TestRealDatabaseUpgrade275Through278(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		db := f.DB
		if err := db.Migrator().DropTable(&model.PeerStrategyState{}, &model.AuditDeliveryReceipt{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Migrator().DropColumn(&model.AddressBook{}, "Note"); err != nil {
			t.Fatal(err)
		}
		if err := db.Model(&model.Version{}).Where("version = ?", 278).UpdateColumn("version", 275).Error; err != nil {
			t.Fatal(err)
		}
		// Write an actual old row without using the current model's new column.
		if err := db.Exec("INSERT INTO address_books (id,user_id,tags,created_at,updated_at) VALUES (?,1,?, ?, ?)", "old", "[]", "2025-01-02 03:04:05", "2025-01-02 03:04:05").Error; err != nil {
			t.Fatal(err)
		}
		upgrade(t, db, 276)
		var old model.AddressBook
		if err := db.Where("id = ?", "old").First(&old).Error; err != nil || old.Note != "" {
			t.Fatalf("backfill: %+v / %v", old, err)
		}
		if err := db.Model(&old).UpdateColumn("note", nil).Error; err == nil {
			t.Fatal("note NOT NULL not enforced")
		}
		if err := db.Model(&old).UpdateColumn("note", "保留\n备注").Error; err != nil {
			t.Fatal(err)
		}
		upgrade(t, db, 277)
		if rows(t, db, &model.AuditDeliveryReceipt{}) != 0 {
			t.Fatal("inferred legacy receipts")
		}
		upgrade(t, db, 278)
		if rows(t, db, &model.PeerStrategyState{}) != 0 {
			t.Fatal("inferred legacy managed keys")
		}
		if err := db.First(&old, old.RowId).Error; err != nil || old.Note != "保留\n备注" {
			t.Fatalf("note lost after later upgrades: %v", err)
		}
		if !db.Migrator().HasConstraint(&model.PeerStrategyState{}, "Peer") {
			t.Fatal("missing peer FK")
		}
		if err := db.Create(&model.PeerStrategyState{PeerRowID: 999999, ManagedKeys: "[]"}).Error; err == nil {
			t.Fatal("orphan state accepted")
		}
		peer := model.Peer{Id: "fk-peer", Uuid: "uuid"}
		if err := db.Create(&peer).Error; err != nil {
			t.Fatal(err)
		}
		state := model.PeerStrategyState{PeerRowID: peer.RowId, ManagedKeys: `["key"]`}
		if err := db.Create(&state).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Delete(&peer).Error; err != nil {
			t.Fatal(err)
		}
		if rows(t, db, &model.PeerStrategyState{}) != 0 {
			t.Fatal("state not cascaded")
		}
	})
}

func TestRealDatabaseNotePartialDDLRetryAndVersionFailure(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		db := f.DB
		if err := db.Model(&model.Version{}).Where("version = ?", 278).UpdateColumn("version", 275).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Migrator().DropColumn(&model.AddressBook{}, "Note"); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("ALTER TABLE address_books ADD COLUMN note TEXT NULL").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO address_books (id,user_id,tags,note) VALUES (?,1,?,NULL),(?,1,?,?)", "null-note", "[]", "keep-note", "[]", "existing").Error; err != nil {
			t.Fatal(err)
		}
		// A real constraint violation at the final version write, not a mocked DB.
		constraint := "ALTER TABLE versions ADD CONSTRAINT reject_target_version CHECK (version <> 278)"
		if err := db.Exec(constraint).Error; err != nil {
			t.Fatal(err)
		}
		if err := database.Update(db, 278, nil); err == nil || !strings.Contains(err.Error(), "record database version") {
			t.Fatalf("missing version failure: %v", err)
		}
		var latest model.Version
		if err := db.Last(&latest).Error; err != nil || latest.Version != 275 {
			t.Fatal("failed migration published version")
		}
		var nullable bool
		cols, err := db.Migrator().ColumnTypes(&model.AddressBook{})
		if err != nil {
			t.Fatal(err)
		}
		for _, col := range cols {
			if col.Name() == "note" {
				nullable, _ = col.Nullable()
			}
		}
		if f.Engine == "postgres" && !nullable {
			t.Fatal("PostgreSQL DDL was not rolled back")
		}
		if f.Engine == "mysql" && nullable {
			t.Fatal("expected actual partial MySQL DDL to remain")
		}
		drop := "ALTER TABLE versions DROP CONSTRAINT reject_target_version"
		if f.Engine == "mysql" {
			drop = "ALTER TABLE versions DROP CHECK reject_target_version"
		}
		if err := db.Exec(drop).Error; err != nil {
			t.Fatal(err)
		}
		upgrade(t, db, 278)
		var notes []model.AddressBook
		if err := db.Order("id").Find(&notes).Error; err != nil {
			t.Fatal(err)
		}
		if len(notes) != 2 || notes[0].Note != "existing" || notes[1].Note != "" {
			t.Fatalf("retry corrupted notes: %+v", notes)
		}
	})
}

func TestRealDatabaseUpgrade276And277PreservesExistingData(t *testing.T) {
	for _, prior := range []uint{276, 277} {
		t.Run(fmt.Sprint(prior), func(t *testing.T) {
			forEngines(t, func(t *testing.T, f *fixture) {
				db := f.DB
				if err := db.Migrator().DropTable(&model.PeerStrategyState{}); err != nil {
					t.Fatal(err)
				}
				if prior == 276 {
					if err := db.Migrator().DropTable(&model.AuditDeliveryReceipt{}); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.Model(&model.Version{}).Where("version = ?", 278).UpdateColumn("version", prior).Error; err != nil {
					t.Fatal(err)
				}
				book := model.AddressBook{Id: "keep", UserId: 1, Note: "existing note"}
				if err := db.Create(&book).Error; err != nil {
					t.Fatal(err)
				}
				if prior == 277 {
					if err := db.Create(&model.AuditDeliveryReceipt{Key: strings.Repeat("a", 64), PayloadHash: strings.Repeat("b", 64)}).Error; err != nil {
						t.Fatal(err)
					}
				}
				upgrade(t, db, 278)
				if err := db.First(&book, book.RowId).Error; err != nil || book.Note != "existing note" {
					t.Fatalf("upgrade changed note: %v", err)
				}
				want := int64(0)
				if prior == 277 {
					want = 1
				}
				if rows(t, db, &model.AuditDeliveryReceipt{}) != want || rows(t, db, &model.PeerStrategyState{}) != 0 {
					t.Fatal("upgrade changed/inferred delivery history")
				}
			})
		})
	}
}

func TestRealDatabaseLegacy244DataAndIdentitySeparation(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		db := f.DB
		if err := db.Model(&model.Version{}).Where("version = ?", 278).UpdateColumn("version", 244).Error; err != nil {
			t.Fatal(err)
		}
		provider := model.Oauth{Op: "google", OauthType: "legacy"}
		if err := db.Create(&provider).Error; err != nil {
			t.Fatal(err)
		}
		legacy := model.UserThird{UserId: 1, ThirdType: "google", OauthType: "legacy", Op: "old", OauthUser: model.OauthUser{OpenId: "legacy@example.test"}}
		if err := db.Create(&legacy).Error; err != nil {
			t.Fatal(err)
		}
		upgrade(t, db, 278)
		var user model.User
		if err := db.First(&user, 1).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.First(&provider, provider.Id).Error; err != nil {
			t.Fatal(err)
		}
		if user.Email != "legacy@example.test" || provider.OauthType != "google" || provider.Issuer != model.IssuerGoogle {
			t.Fatal("legacy data upgrade incomplete")
		}
		if rows(t, db, &model.OIDCIdentity{}) != 0 {
			t.Fatal("legacy email/op inferred an external identity")
		}
	})
}
