package integration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/bookadmin"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionadmin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

type realEntryFixture struct {
	Admin, Owner, PeerOwner auth.Principal
	Book                    collectionadmin.Collection
	Entry                   model.AddressBook
	Peer                    model.Peer
}

func realEntrySetup(t *testing.T, f *fixture) realEntryFixture {
	t.Helper()
	admin, owner, other := realCollectionsSetup(t, f)
	book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, "entry-book")
	entry := realBookEntry(t, f.DB, owner.User.ID, book.ID, "device", "[\"old\"]")
	peer := model.Peer{Id: "device", Uuid: "original-uuid", UserId: other.User.ID, Alias: "old-alias", Hostname: "old-host", Username: "old-user", Note: "peer note", Cpu: "preserve-cpu", PresetAbId: 999, PresetAbAlias: "preserve-preset", PresetModifiedAt: 42}
	transactionCreate(t, f.DB, &peer)
	return realEntryFixture{admin, owner, other, book, entry, peer}
}
func realEntryRead(t *testing.T, db *gorm.DB, id uint) model.AddressBook {
	t.Helper()
	var row model.AddressBook
	realBookMust(t, db.First(&row, id).Error)
	return row
}
func realEntryPeerRaw(t *testing.T, db *gorm.DB, id uint) map[string]any {
	t.Helper()
	var row map[string]any
	realBookMust(t, db.Table("peers").Where("row_id=?", id).Take(&row).Error)
	return row
}
func realEntryNarrowPeer(t *testing.T, before, after map[string]any, columns ...string) {
	t.Helper()
	copy := map[string]any{}
	for k, v := range before {
		copy[k] = v
	}
	for _, key := range append(columns, "updated_at") {
		copy[key] = after[key]
	}
	if !reflect.DeepEqual(copy, after) {
		t.Fatal("entry side write changed unapproved peer columns")
	}
}
func realEntryInput(id uint) bookadmin.EntryInput {
	return bookadmin.EntryInput{RowID: id, Alias: "new-alias", Username: "new-user", Hostname: "new-host", Tags: []string{"old", "keep"}, Platform: "new-platform", Password: "new-private-fixture", Hash: "new-hash", RDPPort: "3390", RDPUsername: "new-rdp", LoginName: "new-login", ForceAlwaysRelay: true, Online: true, SameServer: true}
}

func TestRealManagementEntriesOwnerScopeAndPhysicalRows(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		x := realEntrySetup(t, f)
		s := realBookStore(t, f.DB)
		ctx := context.Background()
		realBookMust(t, f.DB.Model(&model.AddressBook{}).Where("row_id=?", x.Entry.RowId).Updates(map[string]any{"username": "old-user", "hostname": "old-host", "platform": "old-platform", "hash": "old-hash", "rdp_port": "3389", "rdp_username": "old-rdp", "login_name": "old-login", "force_always_relay": true, "online": true, "same_server": true}).Error)
		foreign := realCollectionCreate(t, f.DB, x.PeerOwner, collectionadmin.My, 0, "foreign")
		transactionCreate(t, f.DB, &model.AddressBookCollectionRule{UserId: x.Owner.User.ID, CollectionId: x.Book.ID, Type: 1, Rule: 3, ToId: x.PeerOwner.User.ID})
		before := realBookSnapshot(t, f.DB)
		for _, op := range []func() error{
			func() error {
				return s.CreateEntry(ctx, x.PeerOwner, bookadmin.My, bookadmin.EntryInput{CollectionID: x.Book.ID, ID: "forbidden"})
			},
			func() error { return s.UpdateEntry(ctx, x.PeerOwner, bookadmin.My, realEntryInput(x.Entry.RowId)) },
			func() error {
				return s.DeleteEntry(ctx, x.PeerOwner, bookadmin.My, bookadmin.EntryInput{RowID: x.Entry.RowId})
			},
		} {
			err := op()
			if !errors.Is(err, auth.ErrForbidden) && !errors.Is(err, bookadmin.ErrInput) {
				t.Fatal("shared fullcontrol acquired entry management", err)
			}
			realBookNoChange(t, f.Reopen(t), before)
		}
		for _, identity := range []bookadmin.EntryInput{{RowID: x.Entry.RowId, ID: "other-device"}, {RowID: x.Entry.RowId, UserID: x.PeerOwner.User.ID}, {RowID: x.Entry.RowId, CollectionID: foreign.ID}} {
			for _, op := range []func() error{func() error { return s.UpdateEntry(ctx, x.Admin, bookadmin.Admin, identity) }, func() error { return s.DeleteEntry(ctx, x.Admin, bookadmin.Admin, identity) }} {
				if err := op(); !errors.Is(err, bookadmin.ErrInput) {
					t.Fatal("fixed identity/scope changed", err)
				}
				realBookNoChange(t, f.DB, before)
			}
		}
		// Exact create duplicate detection does not conflate trailing/case variants.
		if err := s.CreateEntry(ctx, x.Admin, bookadmin.Admin, bookadmin.EntryInput{CollectionID: x.Book.ID, ID: x.Entry.Id}); !errors.Is(err, bookadmin.ErrInput) {
			t.Fatal("same-scope duplicate device accepted", err)
		}
		if err := s.CreateEntry(ctx, x.Admin, bookadmin.Admin, bookadmin.EntryInput{CollectionID: x.Book.ID, UserID: x.PeerOwner.User.ID, ID: "contradiction"}); !errors.Is(err, bookadmin.ErrInput) {
			t.Fatal("create contradictory parent owner accepted", err)
		}
		peerBefore := realEntryPeerRaw(t, f.DB, x.Peer.RowId)
		realBookMust(t, s.CreateEntry(ctx, x.Admin, bookadmin.Admin, bookadmin.EntryInput{CollectionID: x.Book.ID, ID: "device ", Tags: []string{"a", "a"}}))
		var made model.AddressBook
		realBookMust(t, f.DB.Where("user_id=? AND collection_id=?", x.Owner.User.ID, x.Book.ID).Order("row_id DESC").First(&made).Error)
		if made.UserId != x.Owner.User.ID || made.Id != "device " || made.Note != "" || string(made.Tags) != `["a","a"]` || time.Time(made.CreatedAt).IsZero() {
			t.Fatal("create lost explicit parent owner/zero fields/timestamps")
		}
		if !reflect.DeepEqual(peerBefore, realEntryPeerRaw(t, f.DB, x.Peer.RowId)) {
			t.Fatal("entry create wrote registered peer")
		}
		realBookMust(t, s.CreateEntry(ctx, x.Owner, bookadmin.My, bookadmin.EntryInput{ID: "personal-my-created", Tags: []string{"same", "same"}}))
		var personal model.AddressBook
		realBookMust(t, f.DB.Where("id=?", "personal-my-created").First(&personal).Error)
		if personal.UserId != x.Owner.User.ID || personal.CollectionId != 0 || string(personal.Tags) != `["same","same"]` {
			t.Fatal("my create did not derive authenticated personal owner")
		}
		if !reflect.DeepEqual(peerBefore, realEntryPeerRaw(t, f.DB, x.Peer.RowId)) {
			t.Fatal("my create wrote registered peer")
		}
		beforePersonalReject := realBookSnapshot(t, f.DB)
		if err := s.CreateEntry(ctx, x.Admin, bookadmin.Admin, bookadmin.EntryInput{ID: "missing-personal-owner"}); !errors.Is(err, bookadmin.ErrInput) {
			t.Fatal("admin personal create guessed owner", err)
		}
		realBookNoChange(t, f.DB, beforePersonalReject)
		duplicate := realBookEntry(t, f.DB, x.Owner.User.ID, x.Book.ID, x.Entry.Id, `["duplicate"]`)
		duplicateBefore := realBookRawEntry(t, f.DB, duplicate.RowId)
		original := realEntryRead(t, f.DB, x.Entry.RowId)
		realBookMust(t, s.UpdateEntry(ctx, x.Owner, bookadmin.My, bookadmin.EntryInput{RowID: x.Entry.RowId}))
		updated := realEntryRead(t, f.DB, x.Entry.RowId)
		if updated.Note != original.Note || updated.Id != original.Id || updated.UserId != original.UserId || updated.CollectionId != original.CollectionId || updated.CreatedAt != original.CreatedAt || updated.Username != "" || updated.Password != "" || updated.Hostname != "" || updated.Alias != "" || updated.Platform != "" || updated.Hash != "" || updated.RdpPort != "" || updated.RdpUsername != "" || updated.LoginName != "" || string(updated.Tags) != "[]" || updated.ForceAlwaysRelay || updated.Online || updated.SameServer {
			t.Fatal("my replacement wrote hidden fields or failed explicit clearing")
		}
		if !reflect.DeepEqual(duplicateBefore, realBookRawEntry(t, f.DB, duplicate.RowId)) || !reflect.DeepEqual(peerBefore, realEntryPeerRaw(t, f.DB, x.Peer.RowId)) {
			t.Fatal("physical-row update merged duplicate or my wrote peer")
		}
		realBookMust(t, s.DeleteEntry(ctx, x.Owner, bookadmin.My, bookadmin.EntryInput{RowID: x.Entry.RowId, ID: x.Entry.Id, UserID: x.Owner.User.ID, CollectionID: x.Book.ID}))
		if !reflect.DeepEqual(duplicateBefore, realBookRawEntry(t, f.DB, duplicate.RowId)) || !reflect.DeepEqual(peerBefore, realEntryPeerRaw(t, f.DB, x.Peer.RowId)) {
			t.Fatal("physical-row/my delete changed duplicate or peer")
		}
	})
}

func TestRealManagementEntriesAdminNarrowPeerAndDeleteScope(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		x := realEntrySetup(t, f)
		ctx := context.Background()
		s := realBookStore(t, f.DB)
		duplicate := realBookEntry(t, f.DB, x.Owner.User.ID, x.Book.ID, x.Entry.Id, `["duplicate"]`)
		duplicateBefore := realBookRawEntry(t, f.DB, duplicate.RowId)
		before := realEntryPeerRaw(t, f.DB, x.Peer.RowId)
		input := realEntryInput(x.Entry.RowId)
		realBookMust(t, s.UpdateEntry(ctx, x.Admin, bookadmin.Admin, input))
		peer := assignmentRead(t, f.DB, x.Peer.RowId)
		after := realEntryPeerRaw(t, f.DB, x.Peer.RowId)
		if peer.AbId != x.Book.ID || peer.Alias != input.Alias || peer.Username != input.Username || peer.Hostname != input.Hostname {
			t.Fatal("admin update did not replace four peer fields")
		}
		realEntryNarrowPeer(t, before, after, "ab_id", "alias", "username", "hostname")
		if realEntryRead(t, f.DB, x.Entry.RowId).Note != "independent note" || !reflect.DeepEqual(duplicateBefore, realBookRawEntry(t, f.DB, duplicate.RowId)) {
			t.Fatal("admin update changed note or historical duplicate")
		}
		realBookMust(t, s.DeleteEntry(ctx, x.Admin, bookadmin.Admin, bookadmin.EntryInput{RowID: x.Entry.RowId}))
		cleared := assignmentRead(t, f.DB, x.Peer.RowId)
		if cleared.AbId != 0 || cleared.Alias != "" || cleared.Username != input.Username || cleared.Hostname != input.Hostname {
			t.Fatal("admin delete did not clear exactly matching primary fields")
		}
		realEntryNarrowPeer(t, after, realEntryPeerRaw(t, f.DB, x.Peer.RowId), "ab_id", "alias")
		if !reflect.DeepEqual(duplicateBefore, realBookRawEntry(t, f.DB, duplicate.RowId)) {
			t.Fatal("admin delete removed another duplicate")
		}
		for _, kind := range []string{"other_primary", "personal", "my"} {
			t.Run(kind, func(t *testing.T) {
				cid := x.Book.ID
				if kind == "personal" {
					cid = 0
				}
				row := realBookEntry(t, f.DB, x.Owner.User.ID, cid, x.Entry.Id, `[]`)
				realBookMust(t, f.DB.Model(&model.Peer{}).Where("row_id=?", x.Peer.RowId).Updates(map[string]any{"ab_id": x.Book.ID + 100, "alias": "preserve", "username": "preserve-user"}).Error)
				before := realEntryPeerRaw(t, f.DB, x.Peer.RowId)
				mode, actor := bookadmin.Admin, x.Admin
				if kind == "my" {
					mode, actor = bookadmin.My, x.Owner
				}
				realBookMust(t, s.DeleteEntry(ctx, actor, mode, bookadmin.EntryInput{RowID: row.RowId}))
				if !reflect.DeepEqual(before, realEntryPeerRaw(t, f.DB, x.Peer.RowId)) {
					t.Fatal("nonmatching/personal/my delete changed peer")
				}
			})
		}
		missing := realBookEntry(t, f.DB, x.Owner.User.ID, x.Book.ID, "no-registered-peer", `[]`)
		realBookMust(t, s.UpdateEntry(ctx, x.Admin, bookadmin.Admin, realEntryInput(missing.RowId)))
	})
}

// The entry write is followed by exactly one peer UPDATE, the transaction's
// final SQL statement. An injected native fault therefore cannot be mistaken
// for a pre-write failure or an artificial GORM Commit error.
func realEntryFailCommit(t *testing.T, f *fixture) func() {
	t.Helper()
	if f.Engine == "postgres" {
		transactionExec(t, f.DB, "CREATE TABLE entry_commit_parent(id bigint PRIMARY KEY)")
		transactionExec(t, f.DB, "CREATE TABLE entry_commit_guard(parent_id bigint REFERENCES entry_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)")
		transactionExec(t, f.DB, "CREATE FUNCTION entry_commit_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO entry_commit_guard(parent_id) VALUES(999999); RETURN NEW; END $$")
		transactionExec(t, f.DB, "CREATE TRIGGER entry_commit_fault AFTER UPDATE ON peers FOR EACH ROW EXECUTE FUNCTION entry_commit_fault()")
		var reached atomic.Bool
		realBookMust(t, f.DB.Callback().Update().After("gorm:update").Register("entry:deferred-peer-reached", func(tx *gorm.DB) {
			if tx.Statement.Table == "peers" && tx.Error == nil && tx.RowsAffected == 1 {
				reached.Store(true)
			}
		}))
		return func() {
			if !reached.Load() {
				t.Error("final peer UPDATE never succeeded before COMMIT failure")
			}
			realBookMust(t, f.DB.Callback().Update().Remove("entry:deferred-peer-reached"))
			transactionExec(t, f.DB, "DROP TRIGGER entry_commit_fault ON peers")
			transactionExec(t, f.DB, "DROP FUNCTION entry_commit_fault()")
			transactionExec(t, f.DB, "DROP TABLE entry_commit_guard")
			transactionExec(t, f.DB, "DROP TABLE entry_commit_parent")
		}
	}
	control := f.Reopen(t)
	var fired, killed atomic.Bool
	realBookMust(t, f.DB.Callback().Update().After("gorm:update").Register("entry:kill-final-peer", func(tx *gorm.DB) {
		if tx.Statement.Table != "peers" || tx.Error != nil || tx.RowsAffected != 1 || !fired.CompareAndSwap(false, true) {
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
	}))
	return func() {
		if !fired.Load() || !killed.Load() {
			t.Error("final peer UPDATE never reached native connection kill")
		}
		realBookMust(t, f.DB.Callback().Update().Remove("entry:kill-final-peer"))
	}
}
func TestRealManagementEntriesNativePeerCommitSQLCancellationRollback(t *testing.T) {
	for _, operation := range []string{"update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			forEngines(t, func(t *testing.T, f *fixture) {
				x := realEntrySetup(t, f)
				s := realBookStore(t, f.DB)
				input := realEntryInput(x.Entry.RowId)
				if operation == "delete" {
					realBookMust(t, f.DB.Model(&model.Peer{}).Where("row_id=?", x.Peer.RowId).UpdateColumn("ab_id", x.Book.ID).Error)
				}
				before := realBookSnapshot(t, f.DB)
				change := func(ctx context.Context) error {
					if operation == "delete" {
						return s.DeleteEntry(ctx, x.Admin, bookadmin.Admin, bookadmin.EntryInput{RowID: x.Entry.RowId})
					}
					return s.UpdateEntry(ctx, x.Admin, bookadmin.Admin, input)
				}
				if f.Engine == "mysql" {
					transactionExec(t, f.DB, "CREATE TRIGGER entry_reject_peer BEFORE UPDATE ON peers FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='private fixture cause'")
				} else {
					transactionExec(t, f.DB, "CREATE FUNCTION entry_reject_peer() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'private fixture cause'; END $$")
					transactionExec(t, f.DB, "CREATE TRIGGER entry_reject_peer BEFORE UPDATE ON peers FOR EACH ROW EXECUTE FUNCTION entry_reject_peer()")
				}
				if err := change(context.Background()); !errors.Is(err, bookadmin.ErrUnavailable) {
					t.Fatal("final peer SQL failure acknowledged", err)
				}
				realBookNoChange(t, f.Reopen(t), before)
				if f.Engine == "mysql" {
					transactionExec(t, f.DB, "DROP TRIGGER entry_reject_peer")
				} else {
					transactionExec(t, f.DB, "DROP TRIGGER entry_reject_peer ON peers")
					transactionExec(t, f.DB, "DROP FUNCTION entry_reject_peer()")
				}
				remove := realEntryFailCommit(t, f)
				err := change(context.Background())
				remove()
				if !errors.Is(err, bookadmin.ErrUnavailable) {
					t.Fatal("native final peer COMMIT failure acknowledged", err)
				}
				realBookNoChange(t, f.Reopen(t), before)
				ctx, cancel := context.WithCancel(context.Background())
				var wrote atomic.Bool
				afterAB := func(tx *gorm.DB) {
					if tx.Statement.Table == "address_books" && tx.Error == nil && tx.RowsAffected == 1 {
						wrote.Store(true)
						cancel()
					}
				}
				if operation == "delete" {
					realBookMust(t, f.DB.Callback().Delete().After("gorm:delete").Register("entry:cancel-after-ab", afterAB))
				} else {
					realBookMust(t, f.DB.Callback().Update().After("gorm:update").Register("entry:cancel-after-ab", afterAB))
				}
				err = change(ctx)
				cancel()
				if operation == "delete" {
					realBookMust(t, f.DB.Callback().Delete().Remove("entry:cancel-after-ab"))
				} else {
					realBookMust(t, f.DB.Callback().Update().Remove("entry:cancel-after-ab"))
				}
				if !wrote.Load() || !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation did not happen after successful AB write", err)
				}
				realBookNoChange(t, f.Reopen(t), before)
				realBookMust(t, change(context.Background()))
				peer := assignmentRead(t, f.DB, x.Peer.RowId)
				if operation == "delete" {
					var n int64
					realBookMust(t, f.DB.Model(&model.AddressBook{}).Where("row_id=?", x.Entry.RowId).Count(&n).Error)
					if n != 0 || peer.AbId != 0 || peer.Alias != "" || peer.Note != "peer note" {
						t.Fatal("delete retry was not atomic")
					}
				} else {
					got := realEntryRead(t, f.DB, x.Entry.RowId)
					if got.Alias != input.Alias || peer.Alias != input.Alias || peer.AbId != x.Book.ID || got.Note != "independent note" || peer.Note != "peer note" {
						t.Fatal("update retry did not atomically apply only approved fields")
					}
				}
			})
		})
	}
}

func TestRealManagementEntriesCollectionDeleteBothOrders(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		upgrade(t, f.DB, 281)
		for _, updateFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("update_first_%v", updateFirst), func(t *testing.T) {
				admin := definitionAdmin(t, f.DB, fmt.Sprintf("entry-delete-admin-%v", updateFirst))
				owner := definitionAdmin(t, f.DB, fmt.Sprintf("entry-delete-owner-%v", updateFirst))
				owner.User.IsAdmin = false
				realBookMust(t, f.DB.Model(&model.User{}).Where("id=?", owner.User.ID).UpdateColumn("is_admin", false).Error)
				book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, fmt.Sprintf("delete-order-%v", updateFirst))
				entry := realBookEntry(t, f.DB, owner.User.ID, book.ID, fmt.Sprintf("delete-device-%v", updateFirst), `["old"]`)
				peer := model.Peer{Id: entry.Id, Uuid: entry.Id, UserId: owner.User.ID, Note: "preserve"}
				transactionCreate(t, f.DB, &peer)
				updateDB, deleteDB := f.Reopen(t), f.Reopen(t)
				leading, following := updateDB, deleteDB
				if !updateFirst {
					leading, following = deleteDB, updateDB
				}
				locked, attempted, release := realBookOwnerBarrier(t, leading, following, owner.User.ID)
				defer release()
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				updater, deleter := realBookStore(t, updateDB), realCollectionsStore(t, deleteDB)
				updated, deleted := make(chan error, 1), make(chan error, 1)
				update := func() { updated <- updater.UpdateEntry(ctx, admin, bookadmin.Admin, realEntryInput(entry.RowId)) }
				del := func() { deleted <- deleter.Delete(ctx, owner, collectionadmin.My, collectionadmin.Input{ID: book.ID}) }
				if updateFirst {
					go update()
				} else {
					go del()
				}
				realBookWait(t, ctx, locked, "leader did not hold owner")
				if updateFirst {
					go del()
				} else {
					go update()
				}
				realBookWait(t, ctx, attempted, "follower did not attempt same owner")
				realBookNativeWait(t, f, ctx, "users")
				release()
				updateErr, deleteErr := realBookResult(t, ctx, updated), realBookResult(t, ctx, deleted)
				if updateFirst {
					realBookMust(t, updateErr)
					if !errors.Is(deleteErr, collectionadmin.ErrReferenced) {
						t.Fatal("committed entry peer reference did not prevent collection delete", deleteErr)
					}
				} else {
					realBookMust(t, deleteErr)
					if !errors.Is(updateErr, bookadmin.ErrNotFound) {
						t.Fatal("deleted parent allowed entry update", updateErr)
					}
				}
				after := assignmentRead(t, f.DB, peer.RowId)
				want := uint(0)
				if updateFirst {
					want = book.ID
				}
				if after.AbId != want || after.Note != "preserve" {
					t.Fatal("collection race left partial peer state")
				}
				if !updateFirst {
					var n int64
					realBookMust(t, f.DB.Model(&model.AddressBook{}).Where("collection_id=?", book.ID).Count(&n).Error)
					if n != 0 {
						t.Fatal("deleted parent retained entry")
					}
				}
			})
		}
	})
}

func TestRealManagementEntriesForeignPeerOwnerUserDeleteBothOrders(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		upgrade(t, f.DB, 281)
		for _, updateFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("update_first_%v", updateFirst), func(t *testing.T) {
				admin := definitionAdmin(t, f.DB, fmt.Sprintf("cross-admin-%v", updateFirst))
				owner := definitionAdmin(t, f.DB, fmt.Sprintf("cross-owner-%v", updateFirst))
				peerOwner := definitionAdmin(t, f.DB, fmt.Sprintf("cross-peer-owner-%v", updateFirst))
				for _, u := range []auth.Principal{owner, peerOwner} {
					realBookMust(t, f.DB.Model(&model.User{}).Where("id=?", u.User.ID).UpdateColumn("is_admin", false).Error)
				}
				owner.User.IsAdmin = false
				peerOwner.User.IsAdmin = false
				book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, fmt.Sprintf("cross-book-%v", updateFirst))
				entry := realBookEntry(t, f.DB, owner.User.ID, book.ID, fmt.Sprintf("cross-peer-%v", updateFirst), `["old"]`)
				peer := model.Peer{Id: entry.Id, Uuid: entry.Id, UserId: peerOwner.User.ID, Note: "cross-owner-note"}
				transactionCreate(t, f.DB, &peer)
				before := realBookRawEntry(t, f.DB, entry.RowId)
				updateDB, deleteDB := f.Reopen(t), f.Reopen(t)
				leading, following := updateDB, deleteDB
				if !updateFirst {
					leading, following = deleteDB, updateDB
				}
				// User deletion now locks the administrator set before its target.
				// The earliest common row is this administrator, so observe that
				// actual wait rather than waiting for a later peer-owner lock.
				locked, attempted, release := realBookOwnerBarrier(t, leading, following, admin.User.ID)
				defer release()
				var peerOwnerAttempt atomic.Bool
				realBookMust(t, updateDB.Callback().Query().Before("gorm:query").Register("entry:peer-owner-before-peer", func(tx *gorm.DB) {
					if realCollectionsAttemptsOwner(tx, peerOwner.User.ID) {
						peerOwnerAttempt.Store(true)
					}
					if realBookIsLockedTable(tx, "peers") && !peerOwnerAttempt.Load() {
						tx.AddError(errors.New("entry peer locked before its planned user owner"))
					}
				}))
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				updater := realBookStore(t, updateDB)
				users := legacyLifecycleUsers(t, deleteDB)
				updated, deleted := make(chan error, 1), make(chan error, 1)
				update := func() { updated <- updater.UpdateEntry(ctx, admin, bookadmin.Admin, realEntryInput(entry.RowId)) }
				del := func() {
					deleted <- users.DeleteContext(ctx, &model.User{IdModel: model.IdModel{Id: peerOwner.User.ID}})
				}
				if updateFirst {
					go update()
				} else {
					go del()
				}
				realBookWait(t, ctx, locked, "leader did not lock the shared administrator")
				if updateFirst {
					go del()
				} else {
					go update()
				}
				realBookWait(t, ctx, attempted, "follower did not attempt the shared administrator lock")
				realBookNativeWait(t, f, ctx, "users")
				release()
				updateErr, deleteErr := realBookResult(t, ctx, updated), realBookResult(t, ctx, deleted)
				if !peerOwnerAttempt.Load() {
					t.Fatal("entry update omitted the planned distinct peer-owner user lock")
				}
				realBookMust(t, deleteErr)
				if updateFirst {
					realBookMust(t, updateErr)
				} else if !errors.Is(updateErr, bookadmin.ErrInput) {
					t.Fatal("removed peer owner did not invalidate candidate", updateErr)
				}
				got := assignmentRead(t, f.DB, peer.RowId)
				want := uint(0)
				if updateFirst {
					want = book.ID
				}
				if got.UserId != 0 || got.AbId != want || got.Note != "cross-owner-note" {
					t.Fatal("user deletion and entry side write disagreed")
				}
				if !updateFirst && !reflect.DeepEqual(before, realBookRawEntry(t, f.DB, entry.RowId)) {
					t.Fatal("rejected entry update changed AB")
				}
			})
		}
	})
}

func TestRealManagementEntriesTagRenameAndPeerBeforeParentOrdering(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		upgrade(t, f.DB, 281)
		for _, entryFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("entry_first_%v", entryFirst), func(t *testing.T) {
				admin := definitionAdmin(t, f.DB, fmt.Sprintf("tag-entry-admin-%v", entryFirst))
				owner := definitionAdmin(t, f.DB, fmt.Sprintf("tag-entry-owner-%v", entryFirst))
				realBookMust(t, f.DB.Model(&model.User{}).Where("id=?", owner.User.ID).UpdateColumn("is_admin", false).Error)
				owner.User.IsAdmin = false
				book := realCollectionCreate(t, f.DB, owner, collectionadmin.My, 0, fmt.Sprintf("tag-entry-%v", entryFirst))
				tag := realBookTag(t, f.DB, owner.User.ID, book.ID, "old", 1)
				entry := realBookEntry(t, f.DB, owner.User.ID, book.ID, fmt.Sprintf("tag-entry-device-%v", entryFirst), `["old"]`)
				peer := model.Peer{Id: entry.Id, Uuid: entry.Id, UserId: owner.User.ID, Note: "preserve"}
				transactionCreate(t, f.DB, &peer)
				entryDB, tagDB := f.Reopen(t), f.Reopen(t)
				leading, following := entryDB, tagDB
				if !entryFirst {
					leading, following = tagDB, entryDB
				}
				locked, attempted, release := realBookOwnerBarrier(t, leading, following, owner.User.ID)
				defer release()
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				var parent, latePeer atomic.Bool
				realBookMust(t, entryDB.Callback().Query().Before("gorm:query").Register("entry:assert-peer-before-parent", func(tx *gorm.DB) {
					if realBookIsLockedTable(tx, "address_book_collections") {
						parent.Store(true)
					}
					if realBookIsLockedTable(tx, "peers") && parent.Load() {
						latePeer.Store(true)
					}
				}))
				entries, tags := realBookStore(t, entryDB), realBookStore(t, tagDB)
				entryDone, tagDone := make(chan error, 1), make(chan error, 1)
				input := realEntryInput(entry.RowId)
				if !entryFirst {
					input.Tags = []string{"new", "keep"}
				}
				update := func() { entryDone <- entries.UpdateEntry(ctx, admin, bookadmin.Admin, input) }
				rename := func() {
					tagDone <- tags.UpdateTag(ctx, owner, bookadmin.My, bookadmin.TagInput{ID: tag.Id, Name: "new", Color: 9})
				}
				if entryFirst {
					go update()
				} else {
					go rename()
				}
				realBookWait(t, ctx, locked, "leader did not hold owner")
				if entryFirst {
					go rename()
				} else {
					go update()
				}
				realBookWait(t, ctx, attempted, "follower did not attempt owner")
				realBookNativeWait(t, f, ctx, "users")
				release()
				realBookMust(t, realBookResult(t, ctx, entryDone))
				realBookMust(t, realBookResult(t, ctx, tagDone))
				if latePeer.Load() {
					t.Fatal("entry acquired peer lock after collection lock")
				}
				got := realEntryRead(t, f.DB, entry.RowId)
				if string(got.Tags) != `["new","keep"]` || got.Note != "independent note" || assignmentRead(t, f.DB, peer.RowId).AbId != book.ID {
					t.Fatal("tag rename/entry replacement did not serialize coherently")
				}
			})
		}
	})
}

func TestRealManagementEntriesPeerCandidateDriftRejects(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		x := realEntrySetup(t, f)
		for _, column := range []string{"id", "uuid", "user_id"} {
			t.Run(column, func(t *testing.T) {
				realBookMust(t, f.DB.Model(&model.Peer{}).Where("row_id=?", x.Peer.RowId).Updates(map[string]any{"id": x.Peer.Id, "uuid": x.Peer.Uuid, "user_id": x.PeerOwner.User.ID}).Error)
				db := f.Reopen(t)
				control := f.Reopen(t)
				var fired atomic.Bool
				var before realBookState
				realBookMust(t, db.Callback().Query().After("gorm:query").Register("entry:drift-peer-candidate", func(tx *gorm.DB) {
					_, locking := tx.Statement.Clauses["FOR"]
					_, typed := tx.Statement.Dest.(*[]model.Peer)
					if tx.Statement.Table != "peers" || locking || !typed || tx.Error != nil || !fired.CompareAndSwap(false, true) {
						return
					}
					value := any("drifted-" + column)
					if column == "user_id" {
						value = x.Owner.User.ID
					}
					realBookMust(t, control.Model(&model.Peer{}).Where("row_id=?", x.Peer.RowId).UpdateColumn(column, value).Error)
					before = realBookSnapshot(t, control)
				}))
				err := realBookStore(t, db).UpdateEntry(context.Background(), x.Admin, bookadmin.Admin, realEntryInput(x.Entry.RowId))
				if !fired.Load() || !errors.Is(err, bookadmin.ErrInput) {
					t.Fatal("changed peer candidate accepted", err)
				}
				realBookNoChange(t, f.Reopen(t), before)
			})
		}
	})
}

// No recipient/group lifecycle or batch write behavior is implied by this
// single-entry slice. Legacy UserService caller authorization remains legacy.
