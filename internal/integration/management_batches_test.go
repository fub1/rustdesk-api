package integration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/bookadmin"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionadmin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestRealManagementBatchesTargetsRawDuplicatesAndFirstPatch(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		admin, owner, other := realCollectionsSetup(t, f)
		s := realBookStore(t, f.DB)
		ctx := context.Background()
		book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "batch-book")
		first := realBookEntry(t, f.DB, owner.User.ID, 0, "batch-device", `["before"]`)
		realBookMust(t, f.DB.Model(&model.AddressBook{}).Where("row_id=?", first.RowId).Updates(map[string]any{"username": "keep-user", "force_always_relay": true, "same_server": true, "online": true}).Error)
		duplicate := realBookEntry(t, f.DB, owner.User.ID, 0, "batch-device", `["duplicate"]`)
		named := realBookEntry(t, f.DB, owner.User.ID, book.ID, "batch-device", `["named"]`)
		peer := model.Peer{Id: "batch-device", Uuid: "batch-uuid", UserId: owner.User.ID, Alias: "keep-peer", Note: "keep-note"}
		transactionCreate(t, f.DB, &peer)
		beforeDuplicate, beforeNamed, beforePeer := realBookRawEntry(t, f.DB, duplicate.RowId), realBookRawEntry(t, f.DB, named.RowId), realEntryPeerRaw(t, f.DB, peer.RowId)
		realBookMust(t, s.BatchEntries(ctx, admin, bookadmin.BatchEntryInput{UserIDs: []uint{owner.User.ID, owner.User.ID}, Entry: bookadmin.EntryInput{ID: first.Id, UserID: other.User.ID, CollectionID: book.ID, Alias: "patch", Tags: []string{"discarded"}}}))
		got := realEntryRead(t, f.DB, first.RowId)
		if got.CollectionId != 0 || got.Alias != "patch" || got.Username != "keep-user" || got.Password != "private fixture value" || !got.ForceAlwaysRelay || !got.SameServer || !got.Online || string(got.Tags) != "[]" || got.Note != "independent note" {
			t.Fatal("raw duplicate target switch or nonzero first-row patch differs")
		}
		if !reflect.DeepEqual(beforeDuplicate, realBookRawEntry(t, f.DB, duplicate.RowId)) || !reflect.DeepEqual(beforeNamed, realBookRawEntry(t, f.DB, named.RowId)) || !reflect.DeepEqual(beforePeer, realEntryPeerRaw(t, f.DB, peer.RowId)) {
			t.Fatal("batch merged historical duplicate, cleaned another book or wrote peer")
		}
		for _, ids := range [][]uint{{}, {0}, {owner.User.ID, 0}, {owner.User.ID, 999999}} {
			before := realBookSnapshot(t, f.DB)
			if err := s.BatchEntries(ctx, admin, bookadmin.BatchEntryInput{UserIDs: ids, Entry: bookadmin.EntryInput{ID: "reject"}}); !errors.Is(err, bookadmin.ErrInput) {
				t.Fatal("invalid target subset was silently accepted", err)
			}
			realBookNoChange(t, f.Reopen(t), before)
		}
		before := realBookSnapshot(t, f.DB)
		if err := s.BatchEntries(ctx, admin, bookadmin.BatchEntryInput{UserIDs: []uint{other.User.ID}, Entry: bookadmin.EntryInput{ID: "wrong-parent", CollectionID: book.ID}}); !errors.Is(err, bookadmin.ErrInput) {
			t.Fatal("wrong-owner named target accepted", err)
		}
		realBookNoChange(t, f.DB, before)
		realBookMust(t, f.DB.Model(&model.User{}).Where("id=?", other.User.ID).UpdateColumn("status", model.COMMON_STATUS_DISABLED).Error)
		realBookMust(t, s.BatchEntries(ctx, admin, bookadmin.BatchEntryInput{UserIDs: []uint{other.User.ID, owner.User.ID}, Entry: bookadmin.EntryInput{ID: "both-targets", CollectionID: book.ID, Tags: []string{"ignored"}, Username: "mapped"}}))
		var made []model.AddressBook
		realBookMust(t, f.DB.Where("id=?", "both-targets").Order("user_id").Find(&made).Error)
		if len(made) != 2 {
			t.Fatal("batch skipped disabled existing target")
		}
		for _, row := range made {
			if row.CollectionId != 0 || string(row.Tags) != "[]" || row.Username != "mapped" {
				t.Fatal("multiple target compatibility mapping differs")
			}
		}
		before = realBookSnapshot(t, f.DB)
		realBookMust(t, f.DB.Delete(&model.UserToken{}, admin.Session.ID).Error)
		if err := s.BatchEntries(ctx, admin, bookadmin.BatchEntryInput{UserIDs: []uint{owner.User.ID}, Entry: bookadmin.EntryInput{ID: "stale"}}); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatal("stale batch session accepted", err)
		}
		realBookNoChange(t, f.DB, before)
	})
}

func realBatchPeer(t *testing.T, db *gorm.DB, owner uint, id, os string) model.Peer {
	t.Helper()
	row := model.Peer{UserId: owner, Id: id, Uuid: "uuid-" + id, Os: os, Username: "source-user", Hostname: "source-host", Alias: "must-not-copy-alias", Note: "must-not-copy-note", PresetAbId: 99}
	transactionCreate(t, db, &row)
	return row
}
func TestRealManagementBatchesFromPeersCompleteAdmissionMappingAndDrift(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		admin, owner, other := realCollectionsSetup(t, f)
		s := realBookStore(t, f.DB)
		ctx := context.Background()
		book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "from-peers")
		a, b := realBatchPeer(t, f.DB, owner.User.ID, "source-a", "Windows 11"), realBatchPeer(t, f.DB, owner.User.ID, "source-b", "macOS")
		foreign := realBatchPeer(t, f.DB, other.User.ID, "foreign-source", "Linux")
		empty := realBatchPeer(t, f.DB, owner.User.ID, "", "Android")
		existing := realBookEntry(t, f.DB, owner.User.ID, book.ID, b.Id, `["existing"]`)
		beforeExisting := realBookRawEntry(t, f.DB, existing.RowId)
		for _, ids := range [][]uint{{a.RowId, foreign.RowId}, {a.RowId, empty.RowId}, {a.RowId, 999999}, {a.RowId, a.RowId}, {0}, {}} {
			before := realBookSnapshot(t, f.DB)
			err := s.EntriesFromPeers(ctx, owner, bookadmin.My, bookadmin.FromPeersInput{PeerIDs: ids, CollectionID: book.ID})
			if !errors.Is(err, auth.ErrForbidden) && !errors.Is(err, bookadmin.ErrInput) && !errors.Is(err, bookadmin.ErrNotFound) {
				t.Fatal("invalid source set partially admitted", err)
			}
			realBookNoChange(t, f.Reopen(t), before)
		}
		peerBefore := realBookSnapshot(t, f.DB).Collections.Peers
		realBookMust(t, s.EntriesFromPeers(ctx, owner, bookadmin.My, bookadmin.FromPeersInput{PeerIDs: []uint{b.RowId, a.RowId}, CollectionID: book.ID, Tags: []string{"x", "x"}}))
		var got model.AddressBook
		realBookMust(t, f.DB.Where("user_id=? AND collection_id=? AND id=?", owner.User.ID, book.ID, a.Id).First(&got).Error)
		if got.Username != a.Username || got.Hostname != a.Hostname || got.Platform != "Windows" || got.Alias != "" || got.Note != "" || got.Password != "" || string(got.Tags) != `["x","x"]` {
			t.Fatal("FromPeers copied management state or changed pure mapping")
		}
		if !reflect.DeepEqual(beforeExisting, realBookRawEntry(t, f.DB, existing.RowId)) || !reflect.DeepEqual(peerBefore, realBookSnapshot(t, f.DB).Collections.Peers) {
			t.Fatal("FromPeers patched existing entry or peer")
		}
		zero := realBatchPeer(t, f.DB, 0, "zero-owner-source", "macOS")
		realBookMust(t, s.EntriesFromPeers(ctx, admin, bookadmin.Admin, bookadmin.FromPeersInput{PeerIDs: []uint{zero.RowId}, CollectionID: book.ID}))
		got = model.AddressBook{}
		realBookMust(t, f.DB.Where("user_id=? AND collection_id=? AND id=?", owner.User.ID, book.ID, zero.Id).First(&got).Error)
		if got.Platform != "Mac OS" || got.Alias != "" || got.Note != "" {
			t.Fatal("admin owner0 source mapping differs")
		}
		before := realBookSnapshot(t, f.DB)
		if err := s.EntriesFromPeers(ctx, admin, bookadmin.Admin, bookadmin.FromPeersInput{PeerIDs: []uint{foreign.RowId}}); !errors.Is(err, bookadmin.ErrInput) {
			t.Fatal("personal Admin destination guessed owner", err)
		}
		realBookNoChange(t, f.DB, before)
		// A real principal with primary-key user 1 must not acquire foreign My scope.
		end := time.Now().Add(time.Hour).Truncate(time.Second)
		token := model.UserToken{UserId: 1, Token: "batch-owner-one", ExpiredAt: end.Unix()}
		transactionCreate(t, f.DB, &token)
		one := auth.Principal{User: auth.User{ID: 1, Enabled: true, IsAdmin: true}, Session: auth.Session{ID: token.Id, UserID: 1, Token: token.Token, ExpiresAt: end}}
		if err := s.EntriesFromPeers(ctx, one, bookadmin.My, bookadmin.FromPeersInput{PeerIDs: []uint{a.RowId}, CollectionID: book.ID}); !errors.Is(err, bookadmin.ErrInput) && !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("owner1 My scope exception survived", err)
		}
		realBookNoChange(t, f.DB, before)
		for _, column := range []string{"id", "uuid", "user_id"} {
			t.Run("drift_"+column, func(t *testing.T) {
				source := realBatchPeer(t, f.DB, owner.User.ID, "drift-"+column, "Linux")
				db, control := f.Reopen(t), f.Reopen(t)
				var fired atomic.Bool
				var snapshot realBookState
				realBookMust(t, db.Callback().Query().After("gorm:query").Register("batch:source-drift", func(tx *gorm.DB) {
					row, typed := tx.Statement.Dest.(*model.Peer)
					_, locked := tx.Statement.Clauses["FOR"]
					if tx.Statement.Table != "peers" || !typed || row.RowId != source.RowId || locked || tx.Error != nil || !fired.CompareAndSwap(false, true) {
						return
					}
					value := any("drifted-" + column)
					if column == "user_id" {
						value = other.User.ID
					}
					realBookMust(t, control.Model(&model.Peer{}).Where("row_id=?", source.RowId).UpdateColumn(column, value).Error)
					snapshot = realBookSnapshot(t, control)
				}))
				err := realBookStore(t, db).EntriesFromPeers(ctx, owner, bookadmin.My, bookadmin.FromPeersInput{PeerIDs: []uint{source.RowId}, CollectionID: book.ID})
				if !fired.Load() || !errors.Is(err, bookadmin.ErrInput) {
					t.Fatal("source drift was accepted", err)
				}
				realBookNoChange(t, f.Reopen(t), snapshot)
			})
		}
	})
}

func TestRealManagementBatchesReplaceTagsAll1001AndCanonicalLocks(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, owner, other := realCollectionsSetup(t, f)
		ctx := context.Background()
		book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "large-replace")
		rows := make([]model.AddressBook, 1001)
		for i := range rows {
			rows[i] = model.AddressBook{UserId: owner.User.ID, CollectionId: book.ID, Id: fmt.Sprintf("bulk-%04d", i), Tags: custom_types.AutoJson(`["before"]`), Note: "preserve-note", Alias: "preserve-alias"}
		}
		realBookMust(t, f.DB.CreateInBatches(&rows, 100).Error)
		foreign := realBookEntry(t, f.DB, other.User.ID, 0, "foreign", `["foreign"]`)
		personal := realBookEntry(t, f.DB, owner.User.ID, 0, "personal", `["personal"]`)
		beforeForeign, beforePersonal := realBookRawEntry(t, f.DB, foreign.RowId), realBookRawEntry(t, f.DB, personal.RowId)
		realBookTag(t, f.DB, owner.User.ID, book.ID, "definition", 9)
		db := f.Reopen(t)
		var phases []int
		var lockedRows []uint
		realBookMust(t, db.Callback().Query().After("gorm:query").Register("batch:canonical-lock-order", func(tx *gorm.DB) {
			if _, ok := tx.Statement.Clauses["FOR"]; !ok || tx.Error != nil {
				return
			}
			phase := -1
			switch tx.Statement.Table {
			case "users":
				phase = 0
			case "user_tokens":
				phase = 1
			case "peers":
				phase = 2
			case "address_book_collections":
				phase = 3
			case "address_books":
				phase = 4
				if row, ok := tx.Statement.Dest.(*model.AddressBook); ok {
					lockedRows = append(lockedRows, row.RowId)
				}
			}
			if phase >= 0 {
				phases = append(phases, phase)
			}
		}))
		ids := make([]uint, 0, 1002)
		for i := len(rows) - 1; i >= 0; i-- {
			ids = append(ids, rows[i].RowId)
		}
		ids = append(ids, rows[0].RowId)
		realBookMust(t, realBookStore(t, db).ReplaceEntryTags(ctx, owner, bookadmin.EntryTagsInput{RowIDs: ids, Tags: []string{"replaced", "replaced"}}))
		if !sort.IntsAreSorted(phases) || len(lockedRows) != 1001 || !sort.SliceIsSorted(lockedRows, func(i, j int) bool { return lockedRows[i] < lockedRows[j] }) {
			t.Fatal("requested targets were not canonically locked once after users/session/parents")
		}
		var after []model.AddressBook
		realBookMust(t, f.DB.Where("user_id=? AND collection_id=?", owner.User.ID, book.ID).Order("row_id").Find(&after).Error)
		if len(after) != 1001 {
			t.Fatal("large replace lost rows")
		}
		for i, row := range after {
			if string(row.Tags) != `["replaced","replaced"]` || row.RowId != rows[i].RowId || row.Note != "preserve-note" || row.Alias != "preserve-alias" {
				t.Fatal("1001-row request was truncated or changed metadata")
			}
		}
		if !reflect.DeepEqual(beforeForeign, realBookRawEntry(t, f.DB, foreign.RowId)) || !reflect.DeepEqual(beforePersonal, realBookRawEntry(t, f.DB, personal.RowId)) {
			t.Fatal("large replace crossed scope")
		}
		s := realBookStore(t, f.DB)
		for _, targets := range [][]uint{{rows[0].RowId, foreign.RowId}, {rows[0].RowId, 999999}, {rows[0].RowId, 0}, {}} {
			before := realBookSnapshot(t, f.DB)
			err := s.ReplaceEntryTags(ctx, owner, bookadmin.EntryTagsInput{RowIDs: targets})
			if !errors.Is(err, auth.ErrForbidden) && !errors.Is(err, bookadmin.ErrInput) && !errors.Is(err, bookadmin.ErrNotFound) {
				t.Fatal("invalid target batch was filtered into success", err)
			}
			realBookNoChange(t, f.Reopen(t), before)
		}
		before := realBookSnapshot(t, f.DB)
		realBookMust(t, f.DB.Delete(&model.UserToken{}, owner.Session.ID).Error)
		if err := s.ReplaceEntryTags(ctx, owner, bookadmin.EntryTagsInput{RowIDs: []uint{rows[0].RowId}}); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatal("replace ignored stored session revocation", err)
		}
		realBookNoChange(t, f.DB, before)
	})
}

// Faults are attached to the second and final child mutation of a two-item
// operation. PostgreSQL defers a genuine constraint error to COMMIT. MySQL kills
// the actual connection after the final child SQL succeeded. Neither case uses
// GORM AddError to pretend that COMMIT was reached.
func realBatchCommitFault(t *testing.T, f *fixture, insert bool) func() {
	t.Helper()
	var reached atomic.Bool
	if f.Engine == "postgres" {
		transactionExec(t, f.DB, "CREATE TABLE batch_commit_parent(id bigint PRIMARY KEY)")
		transactionExec(t, f.DB, "CREATE TABLE batch_commit_guard(parent_id bigint REFERENCES batch_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)")
		transactionExec(t, f.DB, "CREATE FUNCTION batch_commit_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO batch_commit_guard(parent_id) VALUES(999999); RETURN NEW; END $$")
		event := "UPDATE"
		if insert {
			event = "INSERT"
		}
		transactionExec(t, f.DB, "CREATE TRIGGER batch_commit_fault AFTER "+event+" ON address_books FOR EACH ROW EXECUTE FUNCTION batch_commit_fault()")
	}
	control := f.Reopen(t)
	var count atomic.Int32
	var killed atomic.Bool
	hook := func(tx *gorm.DB) {
		if tx.Statement.Table != "address_books" || tx.Error != nil || tx.RowsAffected != 1 {
			return
		}
		if count.Add(1) != 2 {
			return
		}
		reached.Store(true)
		if f.Engine != "mysql" {
			return
		}
		var id uint64
		if err := tx.Statement.ConnPool.QueryRowContext(tx.Statement.Context, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
			tx.AddError(err)
			return
		}
		if err := control.Exec(fmt.Sprintf("KILL CONNECTION %d", id)).Error; err != nil {
			tx.AddError(err)
			return
		}
		killed.Store(true)
	}
	if insert {
		realBookMust(t, f.DB.Callback().Create().After("gorm:create").Register("batch:native-final-child", hook))
	} else {
		realBookMust(t, f.DB.Callback().Update().After("gorm:update").Register("batch:native-final-child", hook))
	}
	return func() {
		if !reached.Load() || count.Load() != 2 || f.Engine == "mysql" && !killed.Load() {
			t.Error("second and final child write was not reached before native COMMIT failure")
		}
		if insert {
			realBookMust(t, f.DB.Callback().Create().Remove("batch:native-final-child"))
		} else {
			realBookMust(t, f.DB.Callback().Update().Remove("batch:native-final-child"))
		}
		if f.Engine == "postgres" {
			transactionExec(t, f.DB, "DROP TRIGGER batch_commit_fault ON address_books")
			transactionExec(t, f.DB, "DROP FUNCTION batch_commit_fault()")
			transactionExec(t, f.DB, "DROP TABLE batch_commit_guard")
			transactionExec(t, f.DB, "DROP TABLE batch_commit_parent")
		}
	}
}
func TestRealManagementBatchesLaterItemSQLNativeCommitCancellation(t *testing.T) {
	for _, kind := range []string{"batch_insert", "from_peers_insert", "replace_update"} {
		t.Run(kind, func(t *testing.T) {
			forEngines(t, func(t *testing.T, f *fixture) {
				admin, owner, other := realCollectionsSetup(t, f)
				s := realBookStore(t, f.DB)
				book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "fault-book")
				a, b := realBatchPeer(t, f.DB, owner.User.ID, "fault-a", "Linux"), realBatchPeer(t, f.DB, owner.User.ID, "fault-b", "Windows")
				r1, r2 := realBookEntry(t, f.DB, owner.User.ID, book.ID, "replace-a", `["before"]`), realBookEntry(t, f.DB, owner.User.ID, book.ID, "replace-b", `["before"]`)
				change := func(ctx context.Context) error {
					switch kind {
					case "batch_insert":
						return s.BatchEntries(ctx, admin, bookadmin.BatchEntryInput{UserIDs: []uint{other.User.ID, owner.User.ID}, Entry: bookadmin.EntryInput{ID: "batch-fault"}})
					case "from_peers_insert":
						return s.EntriesFromPeers(ctx, owner, bookadmin.My, bookadmin.FromPeersInput{PeerIDs: []uint{b.RowId, a.RowId}, CollectionID: book.ID, Tags: []string{"inserted"}})
					default:
						return s.ReplaceEntryTags(ctx, owner, bookadmin.EntryTagsInput{RowIDs: []uint{r2.RowId, r1.RowId}, Tags: []string{"updated"}})
					}
				}
				insert := kind != "replace_update"
				before := realBookSnapshot(t, f.DB)
				// The SQL predicate singles out the later sorted item, after the first write.
				predicate := fmt.Sprintf("NEW.row_id=%d", r2.RowId)
				if kind == "batch_insert" {
					predicate = fmt.Sprintf("NEW.user_id=%d AND NEW.id='batch-fault'", other.User.ID)
				} else if kind == "from_peers_insert" {
					predicate = "NEW.id='fault-b'"
				}
				event := "UPDATE"
				if insert {
					event = "INSERT"
				}
				if f.Engine == "mysql" {
					transactionExec(t, f.DB, "CREATE TRIGGER batch_reject_later BEFORE "+event+" ON address_books FOR EACH ROW BEGIN IF "+predicate+" THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='private fixture cause'; END IF; END")
				} else {
					transactionExec(t, f.DB, "CREATE FUNCTION batch_reject_later() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF "+predicate+" THEN RAISE EXCEPTION 'private fixture cause'; END IF; RETURN NEW; END $$")
					transactionExec(t, f.DB, "CREATE TRIGGER batch_reject_later BEFORE "+event+" ON address_books FOR EACH ROW EXECUTE FUNCTION batch_reject_later()")
				}
				var completed atomic.Int32
				sqlHook := func(tx *gorm.DB) {
					if tx.Statement.Table == "address_books" && tx.Error == nil && tx.RowsAffected == 1 {
						completed.Add(1)
					}
				}
				if insert {
					realBookMust(t, f.DB.Callback().Create().After("gorm:create").Register("batch:earlier-sql-complete", sqlHook))
				} else {
					realBookMust(t, f.DB.Callback().Update().After("gorm:update").Register("batch:earlier-sql-complete", sqlHook))
				}
				err := change(context.Background())
				if insert {
					realBookMust(t, f.DB.Callback().Create().Remove("batch:earlier-sql-complete"))
				} else {
					realBookMust(t, f.DB.Callback().Update().Remove("batch:earlier-sql-complete"))
				}
				if !errors.Is(err, bookadmin.ErrUnavailable) || completed.Load() != 1 {
					t.Fatal("later-item native failure did not follow one successful child write", err)
				}
				realBookNoChange(t, f.Reopen(t), before)
				if f.Engine == "mysql" {
					transactionExec(t, f.DB, "DROP TRIGGER batch_reject_later")
				} else {
					transactionExec(t, f.DB, "DROP TRIGGER batch_reject_later ON address_books")
					transactionExec(t, f.DB, "DROP FUNCTION batch_reject_later()")
				}
				remove := realBatchCommitFault(t, f, insert)
				err = change(context.Background())
				remove()
				if !errors.Is(err, bookadmin.ErrUnavailable) {
					t.Fatal("actual final-child COMMIT failure acknowledged", err)
				}
				realBookNoChange(t, f.Reopen(t), before)
				ctx, cancel := context.WithCancel(context.Background())
				var wrote atomic.Bool
				cancelHook := func(tx *gorm.DB) {
					if tx.Statement.Table == "address_books" && tx.Error == nil && tx.RowsAffected == 1 && wrote.CompareAndSwap(false, true) {
						cancel()
					}
				}
				if insert {
					realBookMust(t, f.DB.Callback().Create().After("gorm:create").Register("batch:cancel-first-child", cancelHook))
				} else {
					realBookMust(t, f.DB.Callback().Update().After("gorm:update").Register("batch:cancel-first-child", cancelHook))
				}
				err = change(ctx)
				cancel()
				if insert {
					realBookMust(t, f.DB.Callback().Create().Remove("batch:cancel-first-child"))
				} else {
					realBookMust(t, f.DB.Callback().Update().Remove("batch:cancel-first-child"))
				}
				if !wrote.Load() || !errors.Is(err, context.Canceled) {
					t.Fatal("batch cancellation never followed actual first write", err)
				}
				realBookNoChange(t, f.Reopen(t), before)
				realBookMust(t, change(context.Background()))
				var n int64
				switch kind {
				case "batch_insert":
					realBookMust(t, f.DB.Model(&model.AddressBook{}).Where("id=?", "batch-fault").Count(&n).Error)
				case "from_peers_insert":
					realBookMust(t, f.DB.Model(&model.AddressBook{}).Where("collection_id=? AND id IN ?", book.ID, []string{a.Id, b.Id}).Count(&n).Error)
				default:
					for _, id := range []uint{r1.RowId, r2.RowId} {
						if string(realEntryRead(t, f.DB, id).Tags) != `["updated"]` {
							t.Fatal("retry omitted requested target")
						}
					}
					n = 2
				}
				if n != 2 {
					t.Fatal("retry did not commit both requested children")
				}
				if !reflect.DeepEqual(before.Collections.Peers, realBookSnapshot(t, f.DB).Collections.Peers) {
					t.Fatal("batch operation modified source or registered peer")
				}
			})
		})
	}
}

func TestRealManagementBatchesCollectionDeleteBothOrders(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		upgrade(t, f.DB, 281)
		for _, kind := range []string{"batch", "from_peers", "replace"} {
			for _, batchFirst := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/batch_first_%v", kind, batchFirst), func(t *testing.T) {
					suffix := fmt.Sprintf("%s-%v", kind, batchFirst)
					admin := definitionAdmin(t, f.DB, "batch-delete-admin-"+suffix)
					owner := definitionAdmin(t, f.DB, "batch-delete-owner-"+suffix)
					realBookMust(t, f.DB.Model(&model.User{}).Where("id=?", owner.User.ID).UpdateColumn("is_admin", false).Error)
					owner.User.IsAdmin = false
					book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "delete-"+suffix)
					entry := realBookEntry(t, f.DB, owner.User.ID, book.ID, "entry-"+suffix, `["old"]`)
					peer := realBatchPeer(t, f.DB, owner.User.ID, "peer-"+suffix, "Linux")
					batchDB, deleteDB := f.Reopen(t), f.Reopen(t)
					leading, following := batchDB, deleteDB
					if !batchFirst {
						leading, following = deleteDB, batchDB
					}
					locked, attempted, release := realBookOwnerBarrier(t, leading, following, owner.User.ID)
					defer release()
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					s, deleter := realBookStore(t, batchDB), realCollectionsStore(t, deleteDB)
					batchDone, deleteDone := make(chan error, 1), make(chan error, 1)
					batch := func() {
						var err error
						switch kind {
						case "batch":
							err = s.BatchEntries(ctx, admin, bookadmin.BatchEntryInput{UserIDs: []uint{owner.User.ID}, Entry: bookadmin.EntryInput{ID: "new-" + suffix, CollectionID: book.ID}})
						case "from_peers":
							err = s.EntriesFromPeers(ctx, owner, bookadmin.My, bookadmin.FromPeersInput{PeerIDs: []uint{peer.RowId}, CollectionID: book.ID})
						default:
							err = s.ReplaceEntryTags(ctx, owner, bookadmin.EntryTagsInput{RowIDs: []uint{entry.RowId}, Tags: []string{"new"}})
						}
						batchDone <- err
					}
					del := func() {
						deleteDone <- deleter.Delete(ctx, owner, collectionadmin.My, collectionadmin.Input{ID: book.ID})
					}
					if batchFirst {
						go batch()
					} else {
						go del()
					}
					realBookWait(t, ctx, locked, "leader did not hold common owner")
					if batchFirst {
						go del()
					} else {
						go batch()
					}
					realBookWait(t, ctx, attempted, "follower did not attempt owner")
					realBookNativeWait(t, f, ctx, "users")
					release()
					batchErr, deleteErr := realBookResult(t, ctx, batchDone), realBookResult(t, ctx, deleteDone)
					realBookMust(t, deleteErr)
					if batchFirst {
						realBookMust(t, batchErr)
					} else if !errors.Is(batchErr, bookadmin.ErrNotFound) {
						t.Fatal("deleted parent admitted batch", batchErr)
					}
					var n int64
					for _, entity := range []any{&model.AddressBook{}, &model.Tag{}, &model.AddressBookCollectionRule{}} {
						realBookMust(t, f.DB.Model(entity).Where("collection_id=?", book.ID).Count(&n).Error)
						if n != 0 {
							t.Fatal("batch/delete left child orphan")
						}
					}
					if assignmentRead(t, f.DB, peer.RowId).AbId != 0 {
						t.Fatal("batch invented registered-peer primary reference")
					}
				})
			}
		}
	})
}

func TestRealManagementBatchesAndNativeTagRenameBothOrders(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		upgrade(t, f.DB, 281)
		for _, batchFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("batch_first_%v", batchFirst), func(t *testing.T) {
				owner := definitionAdmin(t, f.DB, fmt.Sprintf("batch-native-owner-%v", batchFirst))
				realBookMust(t, f.DB.Model(&model.User{}).Where("id=?", owner.User.ID).Updates(map[string]any{"group_id": 1, "is_admin": false}).Error)
				owner.User.IsAdmin = false
				book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, fmt.Sprintf("batch-native-%v", batchFirst))
				tag := realBookTag(t, f.DB, owner.User.ID, book.ID, "old", 9)
				a := realBookEntry(t, f.DB, owner.User.ID, book.ID, "a", `["old"]`)
				b := realBookEntry(t, f.DB, owner.User.ID, book.ID, "b", `["old"]`)
				batchDB, nativeDB := f.Reopen(t), f.Reopen(t)
				leading, following := batchDB, nativeDB
				if !batchFirst {
					leading, following = nativeDB, batchDB
				}
				locked, attempted, release := realBookOwnerBarrier(t, leading, following, owner.User.ID)
				defer release()
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				s := realBookStore(t, batchDB)
				native, err := clientcompat.New(nativeDB)
				realBookMust(t, err)
				batchDone, nativeDone := make(chan error, 1), make(chan error, 1)
				wanted := "old"
				if !batchFirst {
					wanted = "new"
				}
				batch := func() {
					batchDone <- s.ReplaceEntryTags(ctx, owner, bookadmin.EntryTagsInput{RowIDs: []uint{b.RowId, a.RowId}, Tags: []string{wanted, "keep"}})
				}
				rename := func() {
					nativeDone <- native.RenameTag(ctx, clientcompat.Actor{ID: owner.User.ID}, fmt.Sprintf("1-%d-%d", owner.User.ID, book.ID), "old", "new")
				}
				if batchFirst {
					go batch()
				} else {
					go rename()
				}
				realBookWait(t, ctx, locked, "leader did not hold shared owner")
				if batchFirst {
					go rename()
				} else {
					go batch()
				}
				realBookWait(t, ctx, attempted, "follower did not attempt owner")
				realBookNativeWait(t, f, ctx, "users")
				release()
				realBookMust(t, realBookResult(t, ctx, batchDone))
				realBookMust(t, realBookResult(t, ctx, nativeDone))
				var got model.Tag
				realBookMust(t, f.DB.First(&got, tag.Id).Error)
				if got.Name != "new" || got.Color != 9 {
					t.Fatal("batch lost tag definition facts")
				}
				for _, id := range []uint{a.RowId, b.RowId} {
					row := realEntryRead(t, f.DB, id)
					if string(row.Tags) != `["new","keep"]` || row.Note != "independent note" {
						t.Fatal("batch/native rename did not follow serial replacement semantics")
					}
				}
			})
		}
	})
}

func TestRealManagementBatchesReplaceParentDriftAndWaitingSession(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, owner, other := realCollectionsSetup(t, f)
		book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "drift-parent")
		a, b := realBookEntry(t, f.DB, owner.User.ID, book.ID, "drift-a", `["before"]`), realBookEntry(t, f.DB, owner.User.ID, book.ID, "drift-b", `["before"]`)
		db, control := f.Reopen(t), f.Reopen(t)
		var fired atomic.Bool
		var expected realBookState
		realBookMust(t, db.Callback().Query().After("gorm:query").Register("batch:parent-candidate-drift", func(tx *gorm.DB) {
			row, typed := tx.Statement.Dest.(*model.AddressBook)
			_, locking := tx.Statement.Clauses["FOR"]
			if tx.Statement.Table != "address_books" || !typed || row.RowId != a.RowId || locking || tx.Error != nil || !fired.CompareAndSwap(false, true) {
				return
			}
			realBookMust(t, control.Model(&model.AddressBookCollection{}).Where("id=?", book.ID).UpdateColumn("user_id", other.User.ID).Error)
			expected = realBookSnapshot(t, control)
		}))
		err := realBookStore(t, db).ReplaceEntryTags(context.Background(), owner, bookadmin.EntryTagsInput{RowIDs: []uint{b.RowId, a.RowId}, Tags: []string{"forbidden"}})
		if !fired.Load() || !errors.Is(err, bookadmin.ErrInput) {
			t.Fatal("planned parent owner drift was ignored", err)
		}
		realBookNoChange(t, f.Reopen(t), expected)
		realBookMust(t, control.Model(&model.AddressBookCollection{}).Where("id=?", book.ID).UpdateColumn("user_id", owner.User.ID).Error)
		hold := f.Reopen(t).Begin()
		if hold.Error != nil {
			t.Fatal(hold.Error)
		}
		defer hold.Rollback()
		var lockedUser model.User
		realBookMust(t, hold.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lockedUser, owner.User.ID).Error)
		waiting := f.Reopen(t)
		attempted := make(chan struct{})
		var signaled atomic.Bool
		realBookMust(t, waiting.Callback().Query().Before("gorm:query").Register("batch:wait-current-session", func(tx *gorm.DB) {
			if realCollectionsAttemptsOwner(tx, owner.User.ID) && signaled.CompareAndSwap(false, true) {
				close(attempted)
			}
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		done := make(chan error, 1)
		s := realBookStore(t, waiting)
		before := realBookSnapshot(t, f.DB)
		go func() {
			done <- s.ReplaceEntryTags(ctx, owner, bookadmin.EntryTagsInput{RowIDs: []uint{b.RowId, a.RowId}, Tags: []string{"stale"}})
		}()
		realBookWait(t, ctx, attempted, "batch did not attempt current owner")
		realBookNativeWait(t, f, ctx, "users")
		realBookMust(t, control.Delete(&model.UserToken{}, owner.Session.ID).Error)
		realBookMust(t, hold.Commit().Error)
		if err := realBookResult(t, ctx, done); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatal("batch missed session revocation during owner wait", err)
		}
		realBookNoChange(t, f.Reopen(t), before)
	})
}
