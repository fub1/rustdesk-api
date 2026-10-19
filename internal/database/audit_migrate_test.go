package database

import (
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/model"
)

func TestUpgrade276AddsReceiptsWithoutInferringHistoricalDeliveries(t *testing.T) {
	for _, failVersion := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "failed_version_retry"}[failVersion], func(t *testing.T) {
			db := openTestDB(t)
			if err := Update(db, 276, seedTestData); err != nil {
				t.Fatal(err)
			}
			if err := db.Migrator().DropTable(&model.AuditDeliveryReceipt{}); err != nil {
				t.Fatal(err)
			}
			legacy := model.AuditConn{PeerId: "old", SessionId: "9007199254740992", Nonce: "old-nonce"}
			if err := db.Create(&legacy).Error; err != nil {
				t.Fatal(err)
			}
			if failVersion {
				mustExec(t, db, "CREATE TRIGGER fail_receipt_version BEFORE INSERT ON versions BEGIN SELECT RAISE(ABORT, 'version failure'); END")
				if err := Update(db, 277, seedTestData); err == nil {
					t.Fatal("migration acknowledged version failure")
				}
				assertCurrentVersion(t, db, 276)
				if db.Migrator().HasTable(&model.AuditDeliveryReceipt{}) {
					t.Fatal("receipt DDL committed before version")
				}
				mustExec(t, db, "DROP TRIGGER fail_receipt_version")
			}
			if err := Update(db, 277, seedTestData); err != nil {
				t.Fatal(err)
			}
			assertCurrentVersion(t, db, 277)
			if rowCount(t, db, &model.AuditDeliveryReceipt{}) != 0 {
				t.Fatal("inferred receipts from historical events")
			}
			var stored model.AuditConn
			if err := db.First(&stored, legacy.Id).Error; err != nil || stored.SessionId != legacy.SessionId {
				t.Fatalf("historical audit modified: %+v / %v", stored, err)
			}
			if err := Update(db, 277, seedTestData); err != nil {
				t.Fatal(err)
			}
		})
	}
}
