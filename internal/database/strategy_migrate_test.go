package database

import (
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/model"
)

func TestUpgrade277StrategyStateIsEmptyAndHasPeerForeignKey(t *testing.T) {
	for _, failVersion := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "failed_version_retry"}[failVersion], func(t *testing.T) {
			db := openTestDB(t)
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			mustExec(t, db, "PRAGMA foreign_keys = ON")
			if err := Update(db, 277, seedTestData); err != nil {
				t.Fatal(err)
			}
			if err := db.Migrator().DropTable(&model.PeerStrategyState{}); err != nil {
				t.Fatal(err)
			}
			peer := model.Peer{Id: "existing", Uuid: "uuid"}
			if err := db.Omit("StrategyDefinitionID").Create(&peer).Error; err != nil {
				t.Fatal(err)
			}
			if failVersion {
				mustExec(t, db, "CREATE TRIGGER fail_strategy_version BEFORE INSERT ON versions BEGIN SELECT RAISE(ABORT, 'version failure'); END")
				if err := Update(db, 278, seedTestData); err == nil {
					t.Fatal("migration acknowledged version failure")
				}
				assertCurrentVersion(t, db, 277)
				if db.Migrator().HasTable(&model.PeerStrategyState{}) {
					t.Fatal("state DDL committed before version")
				}
				mustExec(t, db, "DROP TRIGGER fail_strategy_version")
			}
			if err := Update(db, 278, seedTestData); err != nil {
				t.Fatal(err)
			}
			assertCurrentVersion(t, db, 278)
			if rowCount(t, db, &model.PeerStrategyState{}) != 0 {
				t.Fatal("inferred previously delivered keys")
			}
			if !db.Migrator().HasConstraint(&model.PeerStrategyState{}, "Peer") {
				t.Fatal("missing peer foreign key")
			}
			if err := db.Create(&model.PeerStrategyState{PeerRowID: peer.RowId + 99, ManagedKeys: "[]"}).Error; err == nil {
				t.Fatal("orphan state accepted")
			}
			state := model.PeerStrategyState{PeerRowID: peer.RowId, ManagedKeys: `["managed"]`}
			if err := db.Create(&state).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&state).Error; err == nil {
				t.Fatal("duplicate peer state accepted")
			}
			if err := Update(db, 278, seedTestData); err != nil {
				t.Fatal(err)
			}
			var stored model.PeerStrategyState
			if err := db.First(&stored, peer.RowId).Error; err != nil || stored.ManagedKeys != state.ManagedKeys {
				t.Fatalf("repeat migration changed state: %+v / %v", stored, err)
			}
			if err := db.Delete(&peer).Error; err != nil {
				t.Fatal(err)
			}
			if rowCount(t, db, &model.PeerStrategyState{}) != 0 {
				t.Fatal("peer deletion did not cascade")
			}
		})
	}
}
