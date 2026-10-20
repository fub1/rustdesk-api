package database

import (
	"github.com/lejianwen/rustdesk-api/v2/model"
	"testing"
)

func TestPeerNoteMigrationFreshAndUpgrade279(t *testing.T) {
	for _, mode := range []string{"fresh", "legacy", "partial"} {
		t.Run(mode, func(t *testing.T) {
			db := openTestDB(t)
			if mode != "fresh" {
				if err := Update(db, 278, seedTestData); err != nil {
					t.Fatal(err)
				}
				if err := db.Omit("StrategyDefinitionID").Create(&model.Peer{Id: "old", Uuid: "uuid"}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Migrator().DropColumn(&model.Peer{}, "Note"); err != nil {
					t.Fatal(err)
				}
				if mode == "partial" {
					if err := db.Migrator().AddColumn(&nullablePeerNote{}, "Note"); err != nil {
						t.Fatal(err)
					}
					mustExec(t, db, "INSERT INTO peers (id,note) VALUES ('keep','已有备注')")
				}
				mustExec(t, db, "UPDATE peers SET updated_at='2025-01-02 03:04:05'")
			}
			if err := Update(db, 279, seedTestData); err != nil {
				t.Fatal(err)
			}
			assertCurrentVersion(t, db, 279)
			mustExec(t, db, "INSERT INTO peers (id) VALUES ('default-note')")
			var peer model.Peer
			if err := db.Where("id = ?", "default-note").First(&peer).Error; err != nil || peer.Note != "" {
				t.Fatal("database note default missing", err)
			}
			if err := db.Exec("UPDATE peers SET note=NULL WHERE id='default-note'").Error; err == nil {
				t.Fatal("peer note must be NOT NULL")
			}
			if mode != "fresh" {
				var raw struct{ Note, UpdatedAt string }
				if err := db.Raw("SELECT note, strftime('%Y-%m-%d %H:%M:%S',updated_at) AS updated_at FROM peers WHERE id='old'").Scan(&raw).Error; err != nil || raw.Note != "" || raw.UpdatedAt != "2025-01-02 03:04:05" {
					t.Fatal("backfill changed existing peer", err)
				}
			}
			if mode == "partial" {
				if err := db.Where("id = ?", "keep").First(&model.Peer{}).Error; err != nil {
					t.Fatal(err)
				}
				var note string
				if err := db.Raw("SELECT note FROM peers WHERE id='keep'").Scan(&note).Error; err != nil || note != "已有备注" {
					t.Fatal("partial migration lost note", err)
				}
			}
			if err := Update(db, 279, seedTestData); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPeerNoteVersionFailureRollsBackAndRetries(t *testing.T) {
	db := openTestDB(t)
	if err := Update(db, 278, seedTestData); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropColumn(&model.Peer{}, "Note"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TRIGGER fail_peer_note_version BEFORE INSERT ON versions BEGIN SELECT RAISE(ABORT,'version failure'); END")
	if err := Update(db, 279, seedTestData); err == nil {
		t.Fatal("migration falsely acknowledged")
	}
	assertCurrentVersion(t, db, 278)
	if db.Migrator().HasColumn(&model.Peer{}, "Note") {
		t.Fatal("SQLite note schema committed before version")
	}
	mustExec(t, db, "DROP TRIGGER fail_peer_note_version")
	if err := Update(db, 279, seedTestData); err != nil {
		t.Fatal(err)
	}
	assertCurrentVersion(t, db, 279)
}
