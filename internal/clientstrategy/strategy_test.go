package clientstrategy

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func fixture(t *testing.T) (*gorm.DB, *Store, model.Peer) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "strategy.db") + "?_foreign_keys=on&_txlock=immediate&_busy_timeout=5000"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&model.Peer{}, &model.PeerStrategy{}, &model.PeerStrategyState{}, &model.AddressBookCollection{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().CreateConstraint(&model.PeerStrategyState{}, "Peer"); err != nil {
		t.Fatal(err)
	}
	peer := model.Peer{Id: "123456", Uuid: "device-uuid", Alias: "bound", PresetModifiedAt: 1 << 60}
	if err := db.Create(&peer).Error; err != nil {
		t.Fatal(err)
	}
	store, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	return db, store, peer
}

func policy(t *testing.T, db *gorm.DB, peer, raw string, modified int64) {
	t.Helper()
	if err := db.Exec("INSERT INTO peer_strategies (peer_id, config_options, modified_at) VALUES (?, ?, ?) ON CONFLICT(peer_id) DO UPDATE SET config_options=excluded.config_options, modified_at=excluded.modified_at", peer, raw, modified).Error; err != nil {
		t.Fatal(err)
	}
}

func snapshot(t *testing.T, s *Store, peer model.Peer, known int64) Result {
	t.Helper()
	got, err := s.Heartbeat(context.Background(), peer.Id, peer.Uuid, known)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision <= 0 {
		t.Fatal("revision must be a nonzero positive i64")
	}
	return got
}

func TestDeleteFallbackHistoryAndStablePayloadRevision(t *testing.T) {
	db, store, peer := fixture(t)
	policy(t, db, "", `{"enable-keyboard":"N","only-default":"default"}`, 10)
	policy(t, db, peer.Id, `{"enable-keyboard":"Y","only-device":"secret-value"}`, 1<<61)
	first := snapshot(t, store, peer, 0)
	if first.ConfigOptions["enable-keyboard"] != "Y" || first.ConfigOptions["only-default"] != "default" {
		t.Fatal(first.ConfigOptions)
	}
	if snapshot(t, store, peer, first.Revision).Changed {
		t.Fatal("identical payload was resent")
	}
	policy(t, db, peer.Id, `{"only-device":"secret-value","enable-keyboard":"Y"}`, 1)
	if got := snapshot(t, store, peer, first.Revision); got.Changed {
		t.Fatal("JSON entry order or old timestamp changed revision")
	}
	// A default-only change cannot be masked by the device/preset timestamps.
	policy(t, db, "", `{"enable-keyboard":"N","only-default":"updated"}`, 10)
	changed := snapshot(t, store, peer, first.Revision)
	if !changed.Changed || changed.ConfigOptions["only-default"] != "updated" {
		t.Fatal("default update hidden")
	}
	if err := db.Where("peer_id = ?", peer.Id).Delete(&model.PeerStrategy{}).Error; err != nil {
		t.Fatal(err)
	}
	fallback := snapshot(t, store, peer, changed.Revision)
	if !fallback.Changed || fallback.ConfigOptions["enable-keyboard"] != "N" || fallback.ConfigOptions["only-device"] != "" {
		t.Fatal(fallback.ConfigOptions)
	}
	if err := db.Where("peer_id = ''").Delete(&model.PeerStrategy{}).Error; err != nil {
		t.Fatal(err)
	}
	// Construct a fresh owner over the same DB: no in-memory key registry.
	restarted, _ := New(db)
	cleared := snapshot(t, restarted, peer, fallback.Revision)
	for _, key := range []string{"enable-keyboard", "only-device", "only-default"} {
		if v, exists := cleared.ConfigOptions[key]; !exists || v != "" {
			t.Fatalf("removed key %s not explicitly cleared", key)
		}
	}
	if _, exists := cleared.ConfigOptions["personal-only"]; exists {
		t.Fatal("unmanaged local key was touched")
	}
	var state model.PeerStrategyState
	if err := db.First(&state, "peer_row_id = ?", peer.RowId).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(state.ManagedKeys, "secret-value") {
		t.Fatal("history stored a policy value")
	}
	var names []string
	if json.Unmarshal([]byte(state.ManagedKeys), &names) != nil || len(names) != 4 {
		t.Fatal(state.ManagedKeys)
	}
}

func TestUnassignedDeviceDoesNotClaimLocalPresetKeys(t *testing.T) {
	db, store, peer := fixture(t)
	if err := db.Model(&peer).Update("alias", "").Error; err != nil {
		t.Fatal(err)
	}
	first := snapshot(t, store, peer, 0)
	for _, key := range []string{"preset-address-book-alias", "preset-address-book-name", "preset-device-username", "preset-device-name"} {
		if _, exists := first.ConfigOptions[key]; exists {
			t.Fatalf("unassigned device cleared local key %s", key)
		}
	}
	var history model.PeerStrategyState
	if err := db.First(&history, "peer_row_id = ?", peer.RowId).Error; err != nil {
		t.Fatal(err)
	}
	if history.ManagedKeys != "[]" {
		t.Fatalf("unassigned local presets became permanently managed: %s", history.ManagedKeys)
	}
	if snapshot(t, store, peer, first.Revision).Changed {
		t.Fatal("empty effective payload has unstable revision")
	}
	// Alias alone assigns only the alias, not the other three preset keys.
	if err := db.Model(&peer).Update("alias", "assigned-alias").Error; err != nil {
		t.Fatal(err)
	}
	assigned := snapshot(t, store, peer, first.Revision)
	if assigned.ConfigOptions["preset-address-book-alias"] != "assigned-alias" || len(assigned.ConfigOptions) != 1 {
		t.Fatal(assigned.ConfigOptions)
	}
	if err := db.Model(&peer).Update("alias", "").Error; err != nil {
		t.Fatal(err)
	}
	removed := snapshot(t, store, peer, assigned.Revision)
	if value, exists := removed.ConfigOptions["preset-address-book-alias"]; !exists || value != "" || len(removed.ConfigOptions) != 1 {
		t.Fatal("alias removal did not clear exactly its historical key", removed.ConfigOptions)
	}
}

func TestPresetAssignmentOverridesPolicyAndRemovalPreservesValidFallback(t *testing.T) {
	db, store, peer := fixture(t)
	if err := db.Model(&peer).Update("alias", "").Error; err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"preset-address-book-alias": "policy-alias", "preset-address-book-name": "policy-book", "preset-device-username": "policy-user", "preset-device-name": "policy-device"}
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	policy(t, db, model.UnboundDefaultPeerId, string(encoded), 1)
	policy(t, db, "", string(encoded), 1)
	first := snapshot(t, store, peer, 0)
	for key, expected := range values {
		if first.ConfigOptions[key] != expected {
			t.Fatalf("unassigned metadata overwrote valid policy key %s", key)
		}
	}
	collection := model.AddressBookCollection{Name: "assigned-book"}
	if err := db.Create(&collection).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&peer).Updates(map[string]any{"alias": "assigned-alias", "ab_id": collection.Id, "username": "assigned-user", "hostname": "assigned-device"}).Error; err != nil {
		t.Fatal(err)
	}
	assigned := snapshot(t, store, peer, first.Revision)
	for key, expected := range map[string]string{"preset-address-book-alias": "assigned-alias", "preset-address-book-name": "assigned-book", "preset-device-username": "assigned-user", "preset-device-name": "assigned-device"} {
		if assigned.ConfigOptions[key] != expected {
			t.Fatalf("real assignment failed to override %s", key)
		}
	}
	if err := db.Model(&peer).Updates(map[string]any{"alias": "", "ab_id": 0}).Error; err != nil {
		t.Fatal(err)
	}
	fallback := snapshot(t, store, peer, assigned.Revision)
	for key, expected := range values {
		if fallback.ConfigOptions[key] != expected {
			t.Fatalf("removal cleared currently valid fallback %s", key)
		}
	}
	if err := db.Where("peer_id IN ?", []string{"", model.UnboundDefaultPeerId}).Delete(&model.PeerStrategy{}).Error; err != nil {
		t.Fatal(err)
	}
	restarted, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	removed := snapshot(t, restarted, peer, fallback.Revision)
	if len(removed.ConfigOptions) != len(values) {
		t.Fatal("cleared nonhistorical keys", removed.ConfigOptions)
	}
	for key := range values {
		if value, exists := removed.ConfigOptions[key]; !exists || value != "" {
			t.Fatalf("historical preset %s not explicitly cleared", key)
		}
	}
}

func TestDefaultScopePresetsAndExplicitEmptyOverride(t *testing.T) {
	db, store, peer := fixture(t)
	policy(t, db, "", `{"bound-key":"yes","overridden":"yes"}`, 5)
	policy(t, db, model.UnboundDefaultPeerId, `{"unbound-key":"yes"}`, 5)
	policy(t, db, peer.Id, `{"overridden":""}`, 5)
	first := snapshot(t, store, peer, 0)
	if first.ConfigOptions["overridden"] != "" {
		t.Fatal("explicit empty must override default")
	}
	if err := db.Model(&peer).Update("alias", "").Error; err != nil {
		t.Fatal(err)
	}
	unbound := snapshot(t, store, peer, first.Revision)
	if !unbound.Changed || unbound.ConfigOptions["bound-key"] != "" || unbound.ConfigOptions["unbound-key"] != "yes" {
		t.Fatal(unbound.ConfigOptions)
	}
	collection := model.AddressBookCollection{Name: "book-before"}
	if err := db.Create(&collection).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&peer).Updates(map[string]any{"ab_id": collection.Id, "hostname": "pc", "username": "user"}).Error; err != nil {
		t.Fatal(err)
	}
	preset := snapshot(t, store, peer, unbound.Revision)
	if preset.ConfigOptions["preset-address-book-name"] != "book-before" || preset.ConfigOptions["preset-device-name"] != "pc" {
		t.Fatal(preset.ConfigOptions)
	}
	if err := db.Model(&collection).Update("name", "book-after").Error; err != nil {
		t.Fatal(err)
	}
	rename := snapshot(t, store, peer, preset.Revision)
	if !rename.Changed || rename.ConfigOptions["preset-address-book-name"] != "book-after" {
		t.Fatal("collection rename was hidden")
	}
	if err := db.Model(&peer).Update("ab_id", 0).Error; err != nil {
		t.Fatal(err)
	}
	removed := snapshot(t, store, peer, rename.Revision)
	for _, key := range []string{"preset-address-book-name", "preset-device-name", "preset-device-username"} {
		if removed.ConfigOptions[key] != "" {
			t.Fatal(removed.ConfigOptions)
		}
	}
}

func TestCorruptDocumentsFailClosedAndNeverExposePayload(t *testing.T) {
	for _, raw := range []string{`[]`, `null`, `broken-secret`, `{"credential":null}`, `{"credential":3}`} {
		t.Run("policy_"+raw, func(t *testing.T) {
			db, store, peer := fixture(t)
			policy(t, db, peer.Id, raw, 1)
			got, err := store.Heartbeat(context.Background(), peer.Id, peer.Uuid, 0)
			if !errors.Is(err, ErrUnavailable) || !reflect.DeepEqual(got, Result{}) {
				t.Fatalf("partial/corrupt success: %#v %v", got, err)
			}
			var count int64
			db.Model(&model.PeerStrategyState{}).Count(&count)
			if count != 0 {
				t.Fatal("failed document wrote managed history")
			}
		})
	}
	for _, raw := range []string{`null`, `{"key":"value"}`, `[1]`, `[""]`, `broken-secret`} {
		t.Run("history_"+raw, func(t *testing.T) {
			db, store, peer := fixture(t)
			if err := db.Create(&model.PeerStrategyState{PeerRowID: peer.RowId, ManagedKeys: raw}).Error; err != nil {
				t.Fatal(err)
			}
			if got, err := store.Heartbeat(context.Background(), peer.Id, peer.Uuid, 0); !errors.Is(err, ErrUnavailable) || got.ConfigOptions != nil {
				t.Fatal("corrupt history accepted")
			}
		})
	}
}

func TestCommitFailureDoesNotPublishOrLoseHistory(t *testing.T) {
	db, store, peer := fixture(t)
	policy(t, db, peer.Id, `{"managed":"value"}`, 1)
	// A deferred FK fails at COMMIT, after state Create itself has succeeded.
	for _, statement := range []string{
		"CREATE TABLE strategy_commit_probe (peer_id INTEGER REFERENCES peers(row_id) DEFERRABLE INITIALLY DEFERRED)",
		"CREATE TRIGGER strategy_fail_commit AFTER INSERT ON peer_strategy_states BEGIN INSERT INTO strategy_commit_probe(peer_id) VALUES(-1); END",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if got, err := store.Heartbeat(context.Background(), peer.Id, peer.Uuid, 0); !errors.Is(err, ErrUnavailable) || got.ConfigOptions != nil {
		t.Fatal("failed commit published payload")
	}
	var count int64
	db.Model(&model.PeerStrategyState{}).Count(&count)
	if count != 0 {
		t.Fatal("failed commit kept state")
	}
	if err := db.Exec("DROP TRIGGER strategy_fail_commit").Error; err != nil {
		t.Fatal(err)
	}
	if snapshot(t, store, peer, 0).ConfigOptions["managed"] != "value" {
		t.Fatal("commit failure was not retryable")
	}
}

func TestDeviceBoundaryCancellationAndCascade(t *testing.T) {
	db, store, peer := fixture(t)
	if _, err := New(nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := store.Heartbeat(context.Background(), peer.Id, "other-uuid", 0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Heartbeat(ctx, peer.Id, peer.Uuid, 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if err := db.Callback().Query().Before("gorm:query").Register("wait_context", func(tx *gorm.DB) { <-tx.Statement.Context.Done(); tx.AddError(tx.Statement.Context.Err()) }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Heartbeat(ctx, peer.Id, peer.Uuid, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	db.Callback().Query().Remove("wait_context")
	snapshot(t, store, peer, 0)
	if err := db.Delete(&peer).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&model.PeerStrategyState{}).Count(&count)
	if count != 0 {
		t.Fatal("deleted peer left history")
	}
	if err := db.Create(&model.PeerStrategyState{PeerRowID: peer.RowId, ManagedKeys: `[]`}).Error; err == nil {
		t.Fatal("orphan history accepted")
	}
	pool, _ := db.DB()
	_ = pool.Close()
	if _, err := store.Heartbeat(context.Background(), peer.Id, peer.Uuid, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestConcurrentHeartbeatUsesOneDurableHistory(t *testing.T) {
	db, store, peer := fixture(t)
	policy(t, db, peer.Id, `{"managed":"value"}`, 1)
	var wg sync.WaitGroup
	errorsCh := make(chan error, 12)
	for i := 0; i < cap(errorsCh); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Heartbeat(context.Background(), peer.Id, peer.Uuid, 0)
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	db.Model(&model.PeerStrategyState{}).Count(&count)
	if count != 1 {
		t.Fatal("concurrent first snapshots created multiple state rows")
	}
}
