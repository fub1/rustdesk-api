package database

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

func peerDefinitionLegacy280(t *testing.T) *gorm.DB {
	t.Helper()
	db := openTestDB(t)
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	mustExec(t, db, "PRAGMA foreign_keys=ON")
	if err := Update(db, 280, seedTestData); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestPeerDefinitionHistoricalTargetsDoNotAddReference(t *testing.T) {
	for _, target := range []uint{278, 280} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			db := openTestDB(t)
			if err := Update(db, target, seedTestData); err != nil {
				t.Fatal(err)
			}
			if present, err := peerDefinitionColumnPresent(db); err != nil || present {
				t.Fatal("historical target gained a future reference", err)
			}
			if err := db.Omit("StrategyDefinitionID").Create(&model.Peer{Id: "historical", Uuid: "uuid"}).Error; err != nil {
				t.Fatal("historical peer shape insertion failed", err)
			}
		})
	}
}

func TestPeerDefinitionMigration281PreservesRowsAndReferenceRules(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgrade280"}[legacy], func(t *testing.T) {
			var db *gorm.DB
			var legacyBefore model.Peer
			if legacy {
				db = peerDefinitionLegacy280(t)
			} else {
				db = openTestDB(t)
				pool, err := db.DB()
				if err != nil {
					t.Fatal(err)
				}
				pool.SetMaxOpenConns(1)
				mustExec(t, db, "PRAGMA foreign_keys=ON")
			}
			if legacy {
				mustExec(t, db, "INSERT INTO peers(id,uuid,note) VALUES('legacy','uuid','keep')")
				if err := db.Table("peers").Where("id = ?", "legacy").Updates(map[string]any{
					"note": "keep\x00备注", "cpu": "legacy-cpu", "hostname": "legacy-host", "memory": "legacy-memory", "os": "legacy-os", "username": "legacy-user", "version": "legacy-version", "user_id": 27, "last_online_time": 123, "last_online_ip": "10.0.0.7", "group_id": 28, "alias": "legacy-alias", "ab_id": 29, "preset_ab_id": 30, "preset_ab_alias": "legacy-book", "preset_dev_usr_name": "legacy-preset-user", "preset_dev_name": "legacy-preset-name", "preset_modified_at": 456,
				}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Where("id = ?", "legacy").First(&legacyBefore).Error; err != nil {
					t.Fatal(err)
				}
				mustExec(t, db, "INSERT INTO peer_strategy_states(peer_row_id,managed_keys) SELECT row_id,'[\"old\"]' FROM peers WHERE id='legacy'")
				mustExec(t, db, "CREATE INDEX preserve_custom_peer_index ON peers(username)")
				mustExec(t, db, "CREATE TRIGGER preserve_peer_trigger AFTER UPDATE OF hostname ON peers BEGIN UPDATE peers SET cpu='trigger-fired' WHERE row_id=NEW.row_id; END")
			}
			if err := Update(db, 281, seedTestData); err != nil {
				t.Fatal(err)
			}
			assertCurrentVersion(t, db, 281)
			if legacy {
				var old model.Peer
				if err := db.Where("id=?", "legacy").First(&old).Error; err != nil || old.StrategyDefinitionID != nil || !reflect.DeepEqual(old, legacyBefore) {
					t.Fatal("legacy peer changed", err)
				}
				if rowCount(t, db, &model.PeerStrategyState{}) != 1 {
					t.Fatal("migration cascaded existing managed keys")
				}
				mustExec(t, db, "UPDATE peers SET hostname='updated' WHERE id='legacy'")
				if err := db.Where("id=?", "legacy").First(&old).Error; err != nil || old.Cpu != "trigger-fired" {
					t.Fatal("custom trigger was lost", err)
				}
				legacyBefore = old
				var count int64
				if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='preserve_custom_peer_index'").Scan(&count).Error; err != nil || count != 1 {
					t.Fatal("custom index was lost", err)
				}
			}
			definition := model.StrategyDefinition{Name: model.StrategyName("a\x00b "), ConfigOptions: "{}"}
			if err := db.Create(&definition).Error; err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"first", "second"} {
				peer := model.Peer{Id: id, Uuid: id, StrategyDefinitionID: &definition.Id}
				if err := db.Create(&peer).Error; err != nil {
					t.Fatal("multiple devices must be allowed to share a definition", err)
				}
			}
			missing := definition.Id + 100
			if err := db.Create(&model.Peer{Id: "orphan", Uuid: "orphan", StrategyDefinitionID: &missing}).Error; err == nil {
				t.Fatal("orphan definition accepted")
			}
			if err := db.Delete(&definition).Error; err == nil {
				t.Fatal("referenced definition deleted")
			}
			if err := db.Model(&model.Peer{}).Where("strategy_definition_id=?", definition.Id).UpdateColumn("strategy_definition_id", nil).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Delete(&definition).Error; err != nil {
				t.Fatal("unreferenced definition rejected", err)
			}
			if err := Update(db, 282, seedTestData); err != nil {
				t.Fatal(err)
			}
			if _, err := peerDefinitionColumnPresent(db); err != nil {
				t.Fatal(err)
			}
			if valid, err := peerDefinitionForeignKey(db); err != nil || !valid {
				t.Fatal("future migration changed reference", err)
			}
			if legacy {
				var after model.Peer
				if err := db.Where("id = ?", "legacy").First(&after).Error; err != nil || !reflect.DeepEqual(legacyBefore, after) {
					t.Fatal("future migration changed existing peer", err)
				}
				var state model.PeerStrategyState
				if err := db.First(&state, legacyBefore.RowId).Error; err != nil || state.ManagedKeys != `["old"]` {
					t.Fatal("future migration changed existing managed keys", err)
				}
				mustExec(t, db, "UPDATE peers SET hostname='after-future-upgrade',cpu='before-trigger' WHERE id='legacy'")
				if err := db.Where("id = ?", "legacy").First(&after).Error; err != nil || after.Cpu != "trigger-fired" {
					t.Fatal("future migration lost custom trigger", err)
				}
				var count int64
				if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='preserve_custom_peer_index'").Scan(&count).Error; err != nil || count != 1 {
					t.Fatal("future migration lost custom index", err)
				}
			}
		})
	}
}

func TestPeerDefinitionVersionFailureRollsBackAndRetries(t *testing.T) {
	db := peerDefinitionLegacy280(t)
	mustExec(t, db, "INSERT INTO peers(id,uuid,note) VALUES('legacy','uuid','unchanged')")
	mustExec(t, db, "CREATE TRIGGER reject_peer_definition_version BEFORE INSERT ON versions BEGIN SELECT RAISE(ABORT,'version failure'); END")
	if err := Update(db, 281, seedTestData); err == nil {
		t.Fatal("version failure acknowledged")
	}
	assertCurrentVersion(t, db, 280)
	if present, err := peerDefinitionColumnPresent(db); err != nil || present {
		t.Fatal("SQLite reference schema committed before version", err)
	}
	mustExec(t, db, "DROP TRIGGER reject_peer_definition_version")
	if err := Update(db, 281, seedTestData); err != nil {
		t.Fatal(err)
	}
	assertCurrentVersion(t, db, 281)
	var peer model.Peer
	if err := db.Where("id=?", "legacy").First(&peer).Error; err != nil || peer.Note != "unchanged" || peer.StrategyDefinitionID != nil {
		t.Fatal("retry changed legacy peer", err)
	}
}

func TestPeerDefinitionMalformedExistingColumnFailsClosed(t *testing.T) {
	for _, column := range []string{
		"strategy_definition_id INTEGER NULL",
		"strategy_definition_id TEXT NULL REFERENCES strategy_definitions(id) ON UPDATE RESTRICT ON DELETE RESTRICT",
		"strategy_definition_id INTEGER NULL REFERENCES strategy_definitions(id) ON UPDATE RESTRICT ON DELETE CASCADE",
		"strategy_definition_id INTEGER NULL REFERENCES users(id) ON UPDATE RESTRICT ON DELETE RESTRICT",
	} {
		t.Run(column, func(t *testing.T) {
			db := peerDefinitionLegacy280(t)
			mustExec(t, db, "ALTER TABLE peers ADD COLUMN "+column)
			var before string
			if err := db.Raw("SELECT sql FROM sqlite_master WHERE type='table' AND name='peers'").Scan(&before).Error; err != nil {
				t.Fatal(err)
			}
			if err := Update(db, 281, seedTestData); err == nil {
				t.Fatal("malformed existing reference accepted")
			}
			assertCurrentVersion(t, db, 280)
			var after string
			if err := db.Raw("SELECT sql FROM sqlite_master WHERE type='table' AND name='peers'").Scan(&after).Error; err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("unknown schema silently rewritten")
			}
		})
	}
}

func TestPeerDefinitionMalformedExistingIndexFailsClosed(t *testing.T) {
	for _, statement := range []string{
		"CREATE UNIQUE INDEX unexpected_unique_definition ON peers(strategy_definition_id)",
		"CREATE INDEX partial_definition ON peers(strategy_definition_id) WHERE row_id > 10",
		"CREATE INDEX idx_peers_strategy_definition_id ON peers(uuid)",
	} {
		t.Run(statement, func(t *testing.T) {
			db := peerDefinitionLegacy280(t)
			mustExec(t, db, "ALTER TABLE peers ADD COLUMN strategy_definition_id INTEGER NULL REFERENCES strategy_definitions(id) ON UPDATE RESTRICT ON DELETE RESTRICT")
			mustExec(t, db, statement)
			if err := Update(db, 281, seedTestData); err == nil {
				t.Fatal("malformed reference index acknowledged")
			}
			assertCurrentVersion(t, db, 280)
		})
	}
}
