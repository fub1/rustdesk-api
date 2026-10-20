package clientstrategy

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

func namedFixture(t *testing.T) (*gorm.DB, *Store, model.Peer, model.StrategyDefinition) {
	t.Helper()
	db, store, peer := fixture(t)
	if err := database.Update(db, 281, nil); err != nil {
		t.Fatal(err)
	}
	definition := model.StrategyDefinition{Name: model.StrategyName("Live named"), ConfigOptions: `{"shared":"named","named-only":"live","fallback":"named"}`}
	if err := db.Create(&definition).Error; err != nil {
		t.Fatal(err)
	}
	peer.StrategyDefinitionID = &definition.Id
	if err := db.Model(&peer).UpdateColumn("strategy_definition_id", definition.Id).Error; err != nil {
		t.Fatal(err)
	}
	return db, store, peer, definition
}

func TestNamedPolicyLivePrecedenceRevisionAndDetachHistory(t *testing.T) {
	db, store, peer, definition := namedFixture(t)
	policy(t, db, "", `{"shared":"default","default-only":"D","fallback":"default"}`, 1)
	policy(t, db, peer.Id, `{"shared":"device","device-only":"V","preset-device-name":"device-policy"}`, 1)
	book := model.AddressBookCollection{Name: "real-book"}
	if err := db.Create(&book).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&peer).Updates(map[string]any{"ab_id": book.Id, "hostname": "real-host", "username": "real-user"}).Error; err != nil {
		t.Fatal(err)
	}
	var beforePeers []model.Peer
	if err := db.Find(&beforePeers).Error; err != nil {
		t.Fatal(err)
	}
	first := snapshot(t, store, peer, 0)
	for key, want := range map[string]string{"shared": "device", "default-only": "D", "named-only": "live", "device-only": "V", "fallback": "named", "preset-device-name": "real-host", "preset-address-book-name": "real-book"} {
		if first.ConfigOptions[key] != want {
			t.Fatalf("wrong precedence %s: %v", key, first.ConfigOptions)
		}
	}
	if err := db.Model(&definition).UpdateColumn("name", model.StrategyName("renamed-only")).Error; err != nil {
		t.Fatal(err)
	}
	if snapshot(t, store, peer, first.Revision).Changed {
		t.Fatal("definition rename changed unchanged payload revision")
	}
	if err := db.Model(&definition).UpdateColumn("config_options", `{"new-key":"new","shared":"new-named"}`).Error; err != nil {
		t.Fatal(err)
	}
	changed := snapshot(t, store, peer, first.Revision)
	if !changed.Changed || changed.ConfigOptions["new-key"] != "new" || changed.ConfigOptions["named-only"] != "" || changed.ConfigOptions["fallback"] != "default" || changed.ConfigOptions["shared"] != "device" {
		t.Fatal("live edit, key removal or fallback failed", changed.ConfigOptions)
	}
	if err := db.Model(&definition).UpdateColumn("config_options", `{"shared":"new-named","new-key":"new"}`).Error; err != nil {
		t.Fatal(err)
	}
	restarted, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot(t, restarted, peer, changed.Revision).Changed {
		t.Fatal("restart or JSON order changed normalized revision")
	}
	var afterPeers []model.Peer
	if err := db.Find(&afterPeers).Error; err != nil || !reflect.DeepEqual(beforePeers, afterPeers) {
		t.Fatal("definition edit or heartbeat fanned out peer updates", err)
	}
	if err := db.Model(&peer).UpdateColumn("strategy_definition_id", nil).Error; err != nil {
		t.Fatal(err)
	}
	detached := snapshot(t, restarted, peer, changed.Revision)
	if !detached.Changed || detached.ConfigOptions["new-key"] != "" || detached.ConfigOptions["shared"] != "device" || detached.ConfigOptions["fallback"] != "default" {
		t.Fatal("detach lost history/fallback", detached.ConfigOptions)
	}
	if _, exists := detached.ConfigOptions["unmanaged-local"]; exists {
		t.Fatal("detach touched an unmanaged local key")
	}
}

func TestNamedPolicyBadDocumentsAndReadFailureNeverPublishOrAdvanceHistory(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"":"bad"}`, `{"key":null}`, `{"key":3}`, `{"key":"a","key":"b"}`, `broken`, `query-error`, `missing-definition`, `zero-reference`} {
		t.Run(raw, func(t *testing.T) {
			db, store, peer, definition := namedFixture(t)
			snapshot(t, store, peer, 0)
			var before model.PeerStrategyState
			if err := db.First(&before, "peer_row_id = ?", peer.RowId).Error; err != nil {
				t.Fatal(err)
			}
			fired := false
			switch raw {
			case "query-error":
				if err := db.Callback().Query().Before("gorm:query").Register("named:read-failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "strategy_definitions" {
						fired = true
						tx.AddError(errors.New("private named SQL fault"))
					}
				}); err != nil {
					t.Fatal(err)
				}
			case "missing-definition", "zero-reference":
				// Corrupt-state reader defense only: native FK rejection is covered
				// by migration/real-engine tests, not bypassed in application code.
				if err := db.Exec("PRAGMA foreign_keys=OFF").Error; err != nil {
					t.Fatal(err)
				}
				id := uint(0)
				if raw == "missing-definition" {
					id = definition.Id + 100
				}
				if err := db.Model(&peer).UpdateColumn("strategy_definition_id", id).Error; err != nil {
					t.Fatal(err)
				}
			default:
				if err := db.Model(&definition).UpdateColumn("config_options", raw).Error; err != nil {
					t.Fatal(err)
				}
			}
			got, err := store.Heartbeat(context.Background(), peer.Id, peer.Uuid, 0)
			if !errors.Is(err, ErrUnavailable) || !reflect.DeepEqual(got, Result{}) {
				t.Fatal("bad named policy published partial result", err)
			}
			if raw == "query-error" && !fired {
				t.Fatal("named query fault not reached")
			}
			var after model.PeerStrategyState
			if err := db.First(&after, "peer_row_id = ?", peer.RowId).Error; err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("bad named policy changed durable managed keys", err)
			}
		})
	}
}

func TestNamedPolicyCommitFailurePreservesHistoryAndRetries(t *testing.T) {
	db, store, peer, definition := namedFixture(t)
	first := snapshot(t, store, peer, 0)
	var before model.PeerStrategyState
	if err := db.First(&before, "peer_row_id = ?", peer.RowId).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&definition).UpdateColumn("config_options", `{"new-managed":"value"}`).Error; err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE TABLE named_commit_guard (peer_id INTEGER REFERENCES peers(row_id) DEFERRABLE INITIALLY DEFERRED)",
		"CREATE TRIGGER named_fail_commit AFTER UPDATE ON peer_strategy_states BEGIN INSERT INTO named_commit_guard(peer_id) VALUES(-1); END",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if got, err := store.Heartbeat(context.Background(), peer.Id, peer.Uuid, first.Revision); !errors.Is(err, ErrUnavailable) || !reflect.DeepEqual(got, Result{}) {
		t.Fatal("named failed COMMIT published payload", err)
	}
	var after model.PeerStrategyState
	if err := db.First(&after, "peer_row_id = ?", peer.RowId).Error; err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("named failed COMMIT advanced history", err)
	}
	if err := db.Exec("DROP TRIGGER named_fail_commit").Error; err != nil {
		t.Fatal(err)
	}
	retry := snapshot(t, store, peer, first.Revision)
	if !retry.Changed || retry.ConfigOptions["new-managed"] != "value" || retry.ConfigOptions["named-only"] != "" {
		t.Fatal("named COMMIT failure was not retryable", retry.ConfigOptions)
	}
}
