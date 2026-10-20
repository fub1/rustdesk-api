package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientassign"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientstrategy"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/internal/strategydefinitions"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

func namedInput(t *testing.T, peer model.Peer, fields map[string]string) clientassign.Input {
	t.Helper()
	fields["id"], fields["uuid"] = peer.Id, peer.Uuid
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var result clientassign.Input
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal("approved CLI input rejected", err)
	}
	return result
}

func namedRelation(t *testing.T, db *gorm.DB, id uint) sql.NullInt64 {
	t.Helper()
	var result sql.NullInt64
	if err := db.Raw("SELECT strategy_definition_id FROM peers WHERE row_id = ?", id).Row().Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func namedSetup(t *testing.T, f *fixture) (auth.Principal, model.User, model.Peer) {
	t.Helper()
	upgrade(t, f.DB, 281)
	actor := definitionAdmin(t, f.DB, "named-assignment")
	owner := model.User{Username: "named-owner", Status: model.COMMON_STATUS_ENABLE}
	transactionCreate(t, f.DB, &owner)
	peer := model.Peer{Id: "named-device", Uuid: "named-uuid", UserId: owner.Id, Hostname: "before-host", Username: "before-user", Note: "before-note"}
	transactionCreate(t, f.DB, &peer)
	return actor, owner, peer
}

func namedDefinition(t *testing.T, f *fixture, actor auth.Principal, name string, options map[string]string) strategydefinitions.Definition {
	t.Helper()
	result, err := definitionStore(t, f.DB).Create(context.Background(), actor, strategydefinitions.Input{Name: name, ConfigOptions: options})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Reconstruct only the old missing-column boundary, even when the shared fixture
// invokes the current migration schema while recording its historical version.
// Constraint names are discovered from metadata, not guessed from GORM fields.
func removeNamedRelationForUpgrade(t *testing.T, f *fixture) {
	t.Helper()
	if !f.DB.Migrator().HasColumn(&model.Peer{}, "strategy_definition_id") {
		return
	}
	if f.Engine == "mysql" {
		var names []string
		if err := f.DB.Raw("SELECT DISTINCT CONSTRAINT_NAME FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='peers' AND COLUMN_NAME='strategy_definition_id' AND REFERENCED_TABLE_NAME IS NOT NULL").Scan(&names).Error; err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.ContainsAny(name, "`\x00") {
				t.Fatal("unexpected fixture FK identifier")
			}
			transactionExec(t, f.DB, "ALTER TABLE peers DROP FOREIGN KEY `"+name+"`")
		}
		transactionExec(t, f.DB, "ALTER TABLE peers DROP COLUMN strategy_definition_id")
	} else {
		transactionExec(t, f.DB, "ALTER TABLE peers DROP COLUMN strategy_definition_id CASCADE")
	}
}

func assertNamedRelationSchema(t *testing.T, f *fixture) {
	t.Helper()
	columns, err := f.DB.Migrator().ColumnTypes(&model.Peer{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, column := range columns {
		if column.Name() == "strategy_definition_id" {
			found = true
			if nullable, known := column.Nullable(); !known || !nullable {
				t.Fatal("relation must remain nullable")
			}
		}
	}
	if !found {
		t.Fatal("missing relation column")
	}
	var count int64
	if f.Engine == "mysql" {
		if err := f.DB.Raw("SELECT count(*) FROM information_schema.statistics WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='peers' AND COLUMN_NAME='strategy_definition_id' AND SEQ_IN_INDEX=1").Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
	} else {
		if err := f.DB.Raw("SELECT count(*) FROM pg_index i JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=i.indkey[0] WHERE i.indrelid='peers'::regclass AND i.indisvalid AND a.attname='strategy_definition_id'").Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
	}
	if count == 0 {
		t.Fatal("relation has no usable leading-column index")
	}
}

func TestNamedStrategyRealUpgrade281NullableRestrictAndRetry(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		upgrade(t, f.DB, 280)
		removeNamedRelationForUpgrade(t, f)
		old := model.Peer{Id: "old-unassigned", Uuid: "old-uuid"}
		// The current struct may expose the new field; this actual old-shape INSERT does not.
		if err := f.DB.Exec("INSERT INTO peers (id,uuid,note) VALUES (?,?,?)", old.Id, old.Uuid, "old-note").Error; err != nil {
			t.Fatal(err)
		}
		if err := f.DB.Where("id = ?", old.Id).First(&old).Error; err != nil {
			t.Fatal(err)
		}
		state := model.PeerStrategyState{PeerRowID: old.RowId, ManagedKeys: `["old-key"]`}
		transactionCreate(t, f.DB, &state)
		policy := model.PeerStrategy{PeerId: old.Id, ConfigOptions: custom_types.AutoJson(`{"old-key":"old-value"}`)}
		transactionCreate(t, f.DB, &policy)
		transactionExec(t, f.DB, "ALTER TABLE versions ADD CONSTRAINT reject_named_281 CHECK (version <> 281)")
		if err := database.Update(f.DB, 281, nil); err == nil || !strings.Contains(err.Error(), "record database version 281") {
			t.Fatalf("final version failure was not reached: %v", err)
		}
		var version model.Version
		if err := f.Reopen(t).Last(&version).Error; err != nil || version.Version != 280 {
			t.Fatal("failed 281 advanced version", err)
		}
		if has := f.DB.Migrator().HasColumn(&model.Peer{}, "strategy_definition_id"); has != (f.Engine == "mysql") {
			t.Fatalf("DDL rollback/partial retry boundary changed: has=%t engine=%s", has, f.Engine)
		}
		drop := "ALTER TABLE versions DROP CONSTRAINT reject_named_281"
		if f.Engine == "mysql" {
			drop = "ALTER TABLE versions DROP CHECK reject_named_281"
		}
		transactionExec(t, f.DB, drop)
		upgrade(t, f.DB, 281)
		assertNamedRelationSchema(t, f)
		if namedRelation(t, f.DB, old.RowId).Valid {
			t.Fatal("upgrade inferred an assignment for old peer")
		}
		var retained model.PeerStrategyState
		if err := f.Reopen(t).First(&retained, state.PeerRowID).Error; err != nil || retained.ManagedKeys != state.ManagedKeys {
			t.Fatal("migration erased/rebuilt durable managed-key history", err)
		}
		if transactionCount(t, f.DB, &model.PeerStrategy{}) != 1 || transactionCount(t, f.DB, &model.StrategyDefinition{}) != 0 {
			t.Fatal("migration copied/inferred definitions or per-device policy")
		}
		actor := definitionAdmin(t, f.DB, "fk")
		definition := namedDefinition(t, f, actor, "fk-target", map[string]string{})
		if err := f.DB.Model(&model.Peer{}).Where("row_id = ?", old.RowId).Update("strategy_definition_id", definition.ID+99999).Error; err == nil {
			t.Fatal("nonexistent parent accepted by real FK")
		}
		if err := f.DB.Model(&model.Peer{}).Where("row_id = ?", old.RowId).Update("strategy_definition_id", definition.ID).Error; err != nil {
			t.Fatal(err)
		}
		other := model.Peer{Id: "another-device", Uuid: "another-uuid"}
		transactionCreate(t, f.DB, &other)
		if err := f.DB.Model(&model.Peer{}).Where("row_id = ?", other.RowId).Update("strategy_definition_id", definition.ID).Error; err != nil {
			t.Fatal("one definition must support multiple devices", err)
		}
		if err := f.DB.Delete(&model.StrategyDefinition{}, definition.ID).Error; err == nil {
			t.Fatal("native ON DELETE RESTRICT did not protect referenced definition")
		}
		if err := f.DB.Model(&model.Peer{}).Where("row_id IN ?", []uint{old.RowId, other.RowId}).Update("strategy_definition_id", nil).Error; err != nil {
			t.Fatal(err)
		}
		if err := f.DB.Delete(&model.StrategyDefinition{}, definition.ID).Error; err != nil {
			t.Fatal("detached definition cannot be deleted", err)
		}
	})
}

func TestNamedStrategyRealCLIByteExactOmissionAndMixedRollback(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		actor, owner, peer := namedSetup(t, f)
		store, err := clientassign.New(f.DB, clientassign.Options{})
		if err != nil {
			t.Fatal(err)
		}
		definitions := []strategydefinitions.Definition{}
		for _, name := range []string{"Name", "name", "Name ", "Náme", "Name\x00"} {
			definitions = append(definitions, namedDefinition(t, f, actor, name, map[string]string{"selected": name}))
		}
		policy := model.PeerStrategy{PeerId: peer.Id, ConfigOptions: custom_types.AutoJson(`{"keep":"device"}`)}
		transactionCreate(t, f.DB, &policy)
		for _, definition := range definitions {
			if err := store.Assign(context.Background(), actor, namedInput(t, peer, map[string]string{"strategy_name": definition.Name})); err != nil {
				t.Fatal(err)
			}
			if got := namedRelation(t, f.DB, peer.RowId); !got.Valid || uint(got.Int64) != definition.ID {
				t.Fatalf("byte name selected wrong definition: %q %+v", definition.Name, got)
			}
		}
		if err := definitionStore(t, f.DB).Delete(context.Background(), actor, definitions[len(definitions)-1].ID); !errors.Is(err, strategydefinitions.ErrReferenced) {
			t.Fatal("referenced definition CRUD deletion was not rejected", err)
		}
		if err := store.Assign(context.Background(), actor, namedInput(t, peer, map[string]string{"note": "omitted-preserves"})); err != nil {
			t.Fatal(err)
		}
		if got := namedRelation(t, f.DB, peer.RowId); !got.Valid || uint(got.Int64) != definitions[len(definitions)-1].ID {
			t.Fatal("omitted selector cleared assignment")
		}
		book := model.AddressBookCollection{UserId: owner.Id, Name: "mixed-book"}
		transactionCreate(t, f.DB, &book)
		entry := model.AddressBook{Id: peer.Id, UserId: owner.Id, CollectionId: book.Id, Alias: "before-book", Tags: custom_types.AutoJson(`[]`)}
		transactionCreate(t, f.DB, &entry)
		beforePeer, beforeBooks := assignmentRead(t, f.DB, peer.RowId), assignmentBooks(t, f.DB)
		for _, name := range []string{"NAME", "Namé", "Name  ", "Name\x00x", "missing"} {
			request := namedInput(t, peer, map[string]string{"strategy_name": name, "user_name": owner.Username, "note": "must-rollback", "address_book_name": book.Name, "address_book_alias": "must-rollback", "address_book_tag": "must-rollback"})
			if err := store.Assign(context.Background(), actor, request); !errors.Is(err, clientassign.ErrInput) {
				t.Fatalf("near/missing name accepted %q: %v", name, err)
			}
			if !reflect.DeepEqual(beforePeer, assignmentRead(t, f.DB, peer.RowId)) || !reflect.DeepEqual(beforeBooks, assignmentBooks(t, f.DB)) || transactionCount(t, f.DB, &model.Tag{}) != 0 {
				t.Fatal("invalid mixed action retained partial peer/book/tag writes")
			}
		}
		if err := store.Assign(context.Background(), actor, namedInput(t, peer, map[string]string{"strategy_name": ""})); err != nil {
			t.Fatal(err)
		}
		if namedRelation(t, f.DB, peer.RowId).Valid {
			t.Fatal("empty string did not detach to SQL NULL")
		}
		if transactionCount(t, f.DB, &model.StrategyDefinition{}) != int64(len(definitions)) || transactionCount(t, f.DB, &model.PeerStrategy{}) != 1 {
			t.Fatal("CLI copied definition/device policy instead of only linking peer")
		}
		var retained model.PeerStrategy
		if err := f.DB.First(&retained, policy.Id).Error; err != nil || string(retained.ConfigOptions) != string(policy.ConfigOptions) {
			t.Fatal("CLI changed single-device policy", err)
		}
	})
}

func namedHeartbeat(t *testing.T, db *gorm.DB, peer model.Peer, known int64) clientstrategy.Result {
	t.Helper()
	store, err := clientstrategy.New(db)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Heartbeat(context.Background(), peer.Id, peer.Uuid, known)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNamedStrategyRealHeartbeatPrecedenceRevisionsAndDurableClears(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		actor, owner, peer := namedSetup(t, f)
		definition := namedDefinition(t, f, actor, "live\x00", map[string]string{"shared": "named", "fallback": "named", "named-only": "first", "removed": "old", "preset-address-book-alias": "named-alias"})
		transactionCreate(t, f.DB, &model.PeerStrategy{PeerId: model.UnboundDefaultPeerId, ConfigOptions: custom_types.AutoJson(`{"shared":"default","fallback":"default","default-only":"d"}`)})
		device := model.PeerStrategy{PeerId: peer.Id, ConfigOptions: custom_types.AutoJson(`{"shared":"device","device-only":"v","preset-address-book-alias":"device-alias"}`)}
		transactionCreate(t, f.DB, &device)
		assign, err := clientassign.New(f.DB, clientassign.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := assign.Assign(context.Background(), actor, namedInput(t, peer, map[string]string{"strategy_name": definition.Name})); err != nil {
			t.Fatal(err)
		}
		first := namedHeartbeat(t, f.DB, peer, 0)
		for key, want := range map[string]string{"shared": "device", "fallback": "named", "default-only": "d", "named-only": "first", "device-only": "v", "preset-address-book-alias": "device-alias"} {
			if first.ConfigOptions[key] != want {
				t.Fatalf("initial precedence %s=%q want=%q", key, first.ConfigOptions[key], want)
			}
		}
		if first.Revision == 0 || !first.Changed {
			t.Fatal("initial named payload missing revision")
		}
		var historyBefore model.PeerStrategyState
		if err := f.DB.First(&historyBefore, "peer_row_id = ?", peer.RowId).Error; err != nil {
			t.Fatal(err)
		}
		if err := f.DB.Model(&model.StrategyDefinition{}).Where("id = ?", definition.ID).Update("config_options", `{"broken":null}`).Error; err != nil {
			t.Fatal(err)
		}
		hb, err := clientstrategy.New(f.Reopen(t))
		if err != nil {
			t.Fatal(err)
		}
		broken, err := hb.Heartbeat(context.Background(), peer.Id, peer.Uuid, first.Revision)
		if !errors.Is(err, clientstrategy.ErrUnavailable) || broken.Revision != 0 || broken.ConfigOptions != nil {
			t.Fatal("damaged named JSON leaked a successful payload", err)
		}
		var historyAfter model.PeerStrategyState
		if err := f.DB.First(&historyAfter, "peer_row_id = ?", peer.RowId).Error; err != nil || !reflect.DeepEqual(historyBefore, historyAfter) {
			t.Fatal("bad named JSON changed durable key history", err)
		}
		_, err = definitionStore(t, f.DB).Update(context.Background(), actor, strategydefinitions.Input{ID: definition.ID, Name: definition.Name, ConfigOptions: map[string]string{"shared": "named-updated", "named-only": "second", "new": "new-value", "preset-address-book-alias": "named-alias"}})
		if err != nil {
			t.Fatal(err)
		}
		second := namedHeartbeat(t, f.Reopen(t), peer, first.Revision)
		if !second.Changed || second.Revision == first.Revision || second.ConfigOptions["fallback"] != "default" || second.ConfigOptions["removed"] != "" || second.ConfigOptions["named-only"] != "second" || second.ConfigOptions["shared"] != "device" {
			t.Fatal("definition update did not fall back/clear/refresh revision")
		}
		if _, present := second.ConfigOptions["removed"]; !present {
			t.Fatal("removed named key must be an explicit clear, not an omitted field")
		}
		// A real preset still overrides named/device values. This deliberately
		// preserves the old bound/unbound default-selection rule.
		book := model.AddressBookCollection{UserId: owner.Id, Name: "preset-book"}
		transactionCreate(t, f.DB, &book)
		if err := f.DB.Model(&model.Peer{}).Where("row_id = ?", peer.RowId).Updates(map[string]any{"ab_id": book.Id, "alias": "real-preset", "hostname": "preset-host", "username": "preset-user"}).Error; err != nil {
			t.Fatal(err)
		}
		preset := namedHeartbeat(t, f.DB, peer, second.Revision)
		for key, want := range map[string]string{"preset-address-book-alias": "real-preset", "preset-address-book-name": book.Name, "preset-device-name": "preset-host", "preset-device-username": "preset-user"} {
			if preset.ConfigOptions[key] != want {
				t.Fatalf("preset precedence %s=%q", key, preset.ConfigOptions[key])
			}
		}
		if !preset.Changed || preset.Revision == second.Revision {
			t.Fatal("preset change missing revision")
		}
		if err := assign.Assign(context.Background(), actor, namedInput(t, peer, map[string]string{"strategy_name": ""})); err != nil {
			t.Fatal(err)
		}
		clear := namedHeartbeat(t, f.Reopen(t), peer, preset.Revision)
		if !clear.Changed || clear.ConfigOptions["named-only"] != "" || clear.ConfigOptions["new"] != "" || clear.ConfigOptions["shared"] != "device" || clear.ConfigOptions["preset-address-book-alias"] != "real-preset" {
			t.Fatal("detach did not clear history/preserve higher-priority policy")
		}
		for _, key := range []string{"named-only", "new", "removed"} {
			if value, present := clear.ConfigOptions[key]; !present || value != "" {
				t.Fatalf("detached named key %q lacks its explicit empty clear", key)
			}
		}
		var state model.PeerStrategyState
		if err := f.DB.First(&state, "peer_row_id = ?", peer.RowId).Error; err != nil {
			t.Fatal(err)
		}
		var managed []string
		if json.Unmarshal([]byte(state.ManagedKeys), &managed) != nil {
			t.Fatal("invalid durable managed-key list")
		}
		set := map[string]bool{}
		for _, name := range managed {
			set[name] = true
		}
		if !set["named-only"] || !set["new"] || !set["removed"] {
			t.Fatal("detach forgot previously managed named keys")
		}
		stable := namedHeartbeat(t, f.Reopen(t), peer, clear.Revision)
		if stable.Changed || stable.Revision != clear.Revision || !reflect.DeepEqual(stable.ConfigOptions, clear.ConfigOptions) {
			t.Fatal("restart lost stable clear/fallback state")
		}
	})
}

func TestNamedStrategyRealMixedAttachCommitAndHeartbeatCommitRetry(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		actor, owner, peer := namedSetup(t, f)
		definition := namedDefinition(t, f, actor, "commit-target", map[string]string{"named-managed": "value"})
		book := model.AddressBookCollection{UserId: owner.Id, Name: "commit-book"}
		transactionCreate(t, f.DB, &book)
		entries := []model.AddressBook{{Id: peer.Id, UserId: owner.Id, CollectionId: book.Id, Alias: "first", Tags: custom_types.AutoJson(`[]`)}, {Id: peer.Id, UserId: owner.Id, CollectionId: book.Id, Alias: "second", Tags: custom_types.AutoJson(`[]`)}}
		transactionCreate(t, f.DB, &entries)
		assign, err := clientassign.New(f.DB, clientassign.Options{})
		if err != nil {
			t.Fatal(err)
		}
		request := namedInput(t, peer, map[string]string{"strategy_name": definition.Name, "note": "after", "address_book_name": book.Name, "address_book_alias": "after", "address_book_tag": "new"})
		beforePeer, beforeBooks := assignmentRead(t, f.DB, peer.RowId), assignmentBooks(t, f.DB)
		// The final business SQL is the second duplicate AB UPDATE. Definition
		// lookup and peer mutation occur earlier, with no SQL after this boundary.
		bookUpdates := 0
		if err := f.DB.Callback().Update().After("gorm:update").Register("named:observe-final-book-update", func(tx *gorm.DB) {
			if tx.Statement.Table == "address_books" && tx.Error == nil {
				bookUpdates++
			}
		}); err != nil {
			t.Fatal(err)
		}
		remove := failAssignmentBookCommit(t, f)
		err = assign.Assign(context.Background(), actor, request)
		remove()
		if err := f.DB.Callback().Update().Remove("named:observe-final-book-update"); err != nil {
			t.Fatal(err)
		}
		if bookUpdates != 2 {
			t.Fatal("mixed transaction did not reach the successful final duplicate AB UPDATE")
		}
		if !errors.Is(err, clientassign.ErrUnavailable) {
			t.Fatal("failed mixed relation COMMIT acknowledged", err)
		}
		fresh := f.Reopen(t)
		if namedRelation(t, fresh, peer.RowId).Valid || !reflect.DeepEqual(beforePeer, assignmentRead(t, fresh, peer.RowId)) || !reflect.DeepEqual(beforeBooks, assignmentBooks(t, fresh)) || transactionCount(t, fresh, &model.Tag{}) != 0 {
			t.Fatal("failed COMMIT retained partial relation/peer/book/tag")
		}
		assign, err = clientassign.New(fresh, clientassign.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := assign.Assign(context.Background(), actor, request); err != nil {
			t.Fatal("mixed relation retry", err)
		}
		if relation := namedRelation(t, fresh, peer.RowId); !relation.Valid || uint(relation.Int64) != definition.ID {
			t.Fatal("relation retry not persisted")
		}
		// Reuse the verified fresh-state final INSERT COMMIT boundary; heartbeat
		// does not return its computed payload until this history write commits.
		hb, err := clientstrategy.New(f.DB)
		if err != nil {
			t.Fatal(err)
		}
		historyInserted := false
		if err := f.DB.Callback().Create().After("gorm:create").Register("named:observe-final-history-insert", func(tx *gorm.DB) {
			if tx.Statement.Table == "peer_strategy_states" && tx.Error == nil {
				historyInserted = true
			}
		}); err != nil {
			t.Fatal(err)
		}
		remove = failTransactionCommit(t, f, "peer_strategy_states")
		result, err := hb.Heartbeat(context.Background(), peer.Id, peer.Uuid, 0)
		remove()
		if err := f.DB.Callback().Create().Remove("named:observe-final-history-insert"); err != nil {
			t.Fatal(err)
		}
		if !historyInserted {
			t.Fatal("heartbeat did not reach the successful final history INSERT before COMMIT")
		}
		if !errors.Is(err, clientstrategy.ErrUnavailable) || result.Revision != 0 || result.ConfigOptions != nil {
			t.Fatal("failed heartbeat COMMIT leaked strategy payload", err)
		}
		if transactionCount(t, f.Reopen(t), &model.PeerStrategyState{}) != 0 {
			t.Fatal("failed heartbeat COMMIT retained history")
		}
		retried := namedHeartbeat(t, f.Reopen(t), peer, 0)
		if retried.ConfigOptions["named-managed"] != "value" || retried.Revision == 0 || !retried.Changed || transactionCount(t, f.DB, &model.PeerStrategyState{}) != 1 {
			t.Fatal("heartbeat retry did not persist named key history/payload")
		}
	})
}

func TestNamedStrategyRealIndependentPoolsAssignmentDefinitionAndHeartbeatCompetition(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		actor, owner, peer := namedSetup(t, f)
		definitionActor := definitionAdmin(t, f.DB, "concurrent-definition")
		definition := namedDefinition(t, f, definitionActor, "race-target", map[string]string{"race": "before"})
		book := model.AddressBookCollection{UserId: owner.Id, Name: "race-book"}
		transactionCreate(t, f.DB, &book)
		entry := model.AddressBook{Id: peer.Id, UserId: owner.Id, CollectionId: book.Id, Alias: "before-note", Tags: custom_types.AutoJson(`[]`)}
		transactionCreate(t, f.DB, &entry)
		type outcome struct {
			action    string
			err       error
			retryable bool
			heartbeat clientstrategy.Result
		}
		start, results := make(chan struct{}), make(chan outcome, 5)
		var group sync.WaitGroup
		for _, action := range []string{"attach", "detach", "definition-update", "definition-delete", "heartbeat"} {
			db, observed := observeTransactions(t, f.Reopen(t))
			group.Add(1)
			go func(action string, db *gorm.DB, observed *transactionErrors) {
				defer group.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				result := outcome{action: action}
				switch action {
				case "attach", "detach":
					store, err := clientassign.New(db, clientassign.Options{})
					if err != nil {
						result.err = err
						break
					}
					name := definition.Name
					if action == "detach" {
						name = ""
					}
					// Construct from frozen fields here; do not call t.Fatal helpers in a worker.
					request := clientassign.Input{ID: peer.Id, UUID: peer.Uuid, StrategyName: assignmentValue(name), Note: assignmentValue(action), AddressBookName: assignmentValue(book.Name), AddressBookAlias: assignmentValue(action), AddressBookTag: assignmentValue(action)}
					result.err = store.Assign(ctx, actor, request)
				case "definition-update":
					store, err := strategydefinitions.New(db, strategydefinitions.Options{})
					if err != nil {
						result.err = err
						break
					}
					_, result.err = store.Update(ctx, definitionActor, strategydefinitions.Input{ID: definition.ID, Name: definition.Name, ConfigOptions: map[string]string{"race": "after"}})
				case "definition-delete":
					store, err := strategydefinitions.New(db, strategydefinitions.Options{})
					if err != nil {
						result.err = err
						break
					}
					result.err = store.Delete(ctx, definitionActor, definition.ID)
				case "heartbeat":
					store, err := clientstrategy.New(db)
					if err != nil {
						result.err = err
						break
					}
					result.heartbeat, result.err = store.Heartbeat(ctx, peer.Id, peer.Uuid, 0)
				}
				result.retryable = observed.retryable()
				results <- result
			}(action, db, observed)
		}
		close(start)
		group.Wait()
		close(results)
		committed := map[string]bool{}
		for result := range results {
			if result.err == nil {
				committed[result.action] = true
				continue
			}
			business := result.action == "attach" && errors.Is(result.err, clientassign.ErrInput) || (result.action == "definition-delete" && errors.Is(result.err, strategydefinitions.ErrReferenced)) || ((result.action == "definition-update" || result.action == "definition-delete") && errors.Is(result.err, strategydefinitions.ErrNotFound))
			storage := errors.Is(result.err, clientassign.ErrUnavailable) || errors.Is(result.err, strategydefinitions.ErrUnavailable) || errors.Is(result.err, clientstrategy.ErrUnavailable)
			if !business && !(storage && result.retryable) {
				t.Fatalf("unexpected/unclassified concurrency failure action=%s error=%v native-retryable=%t", result.action, result.err, result.retryable)
			}
			if result.action == "heartbeat" && (result.heartbeat.Revision != 0 || result.heartbeat.ConfigOptions != nil) {
				t.Fatal("failed competing heartbeat leaked payload")
			}
		}
		fresh := f.Reopen(t)
		var orphans int64
		if err := fresh.Raw("SELECT count(*) FROM peers p LEFT JOIN strategy_definitions d ON d.id=p.strategy_definition_id WHERE p.strategy_definition_id IS NOT NULL AND d.id IS NULL").Scan(&orphans).Error; err != nil || orphans != 0 {
			t.Fatal("competition left dangling relation", err)
		}
		finalPeer := assignmentRead(t, fresh, peer.RowId)
		var finalBook model.AddressBook
		if err := fresh.First(&finalBook, entry.RowId).Error; err != nil {
			t.Fatal(err)
		}
		if finalPeer.Note != finalBook.Alias {
			t.Fatal("partial CLI transaction separated peer and AB values")
		}
		if finalPeer.Note != "before-note" && !committed[finalPeer.Note] {
			t.Fatal("failed CLI action persisted its marker")
		}
		relation := namedRelation(t, fresh, peer.RowId)
		if finalPeer.Note == "attach" && (!relation.Valid || uint(relation.Int64) != definition.ID) || finalPeer.Note != "attach" && relation.Valid {
			t.Fatal("relation and peer/book markers came from different CLI transactions")
		}
		var tags []model.Tag
		if err := fresh.Where("collection_id = ?", book.Id).Find(&tags).Error; err != nil {
			t.Fatal(err)
		}
		for _, tag := range tags {
			if !committed[tag.Name] {
				t.Fatal("failed CLI action retained new tag")
			}
		}
		for _, action := range []string{"attach", "detach"} {
			if committed[action] {
				found := false
				for _, tag := range tags {
					if tag.Name == action {
						found = true
					}
				}
				if !found {
					t.Fatal("committed CLI action lost tag")
				}
			}
		}
		var memberships []string
		if err := json.Unmarshal([]byte(finalBook.Tags), &memberships); err != nil {
			t.Fatal("competition damaged AB tag JSON", err)
		}
		if len(memberships) != len(tags) {
			t.Fatal("AB tag membership and committed definitions disagree")
		}
		members := map[string]bool{}
		for _, tag := range memberships {
			if members[tag] || !committed[tag] {
				t.Fatal("competition duplicated/lost/partially committed AB tag")
			}
			members[tag] = true
		}
		// Converge from a clean independent pool only after all competitors end.
		// The racing calls may return native serialization/deadlock conflicts;
		// they are never reclassified as success or silently retried here.
		final := namedHeartbeat(t, fresh, peer, 0)
		if final.Revision == 0 || !final.Changed {
			t.Fatal("final committed policy snapshot invalid")
		}
		want := ""
		if relation.Valid {
			current, err := definitionStore(t, fresh).Get(context.Background(), definitionActor, uint(relation.Int64))
			if err != nil {
				t.Fatal("committed relation cannot read its parent", err)
			}
			want = current.ConfigOptions["race"]
		}
		if final.ConfigOptions["race"] != want {
			t.Fatal("final heartbeat does not reflect the committed relation and definition")
		}
	})
}
