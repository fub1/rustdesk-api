package bookadmin_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/bookadmin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

func batchSource(t *testing.T, f *fixture, id string, owner uint, os string) model.Peer {
	t.Helper()
	peer := entryPeer(t, f, id)
	must(t, f.db.Model(&model.Peer{}).Where("row_id = ?", peer.RowId).Updates(map[string]any{
		"user_id": owner, "os": os, "alias": "do not map alias", "note": "do not map note",
	}).Error)
	return peerRead(t, f, peer.RowId)
}
func batchEntry(t *testing.T, f *fixture, uid, cid uint, device string) model.AddressBook {
	t.Helper()
	var row model.AddressBook
	must(t, f.db.Where("user_id = ? AND collection_id = ? AND id = ?", uid, cid, device).Order("row_id ASC").First(&row).Error)
	return row
}

func TestBatchEntriesRawMultiTargetSwitchAndNoLegacySideWrites(t *testing.T) {
	f := newFixture(t)
	peer := batchSource(t, f, "device", f.other.User.ID, "Windows")
	oldOwnerOne := entry(t, f, f.admin.User.ID, f.book.Id, "device", `["keep"]`)
	beforeOne := entryRead(t, f, oldOwnerOne.RowId)
	beforePeer := peerRead(t, f, peer.RowId)
	input := bookadmin.BatchEntryInput{UserIDs: []uint{f.owner.User.ID, f.owner.User.ID}, Entry: entryFull(0)}
	input.Entry.ID = peer.Id
	input.Entry.UserID, input.Entry.CollectionID = 99999, f.foreign.Id // ignored owner; raw len forces cid0
	must(t, f.store.BatchEntries(context.Background(), f.admin, input))
	row := batchEntry(t, f, f.owner.User.ID, 0, peer.Id)
	if row.Tags.String() != `[]` || row.Note != "" || row.CollectionId != 0 || row.UserId != f.owner.User.ID {
		t.Fatal("raw duplicate target length did not retain stock personal/empty-tags switch")
	}
	var count int64
	must(t, f.db.Model(&model.AddressBook{}).Where("user_id = ? AND id = ?", f.owner.User.ID, peer.Id).Count(&count).Error)
	if count != 1 {
		t.Fatal("duplicate target IDs produced duplicate physical writes")
	}
	if !reflect.DeepEqual(beforeOne, entryRead(t, f, oldOwnerOne.RowId)) || !reflect.DeepEqual(beforePeer, peerRead(t, f, peer.RowId)) {
		t.Fatal("batch invoked old CleanUp or changed a registered peer")
	}
	before := snapshot(t, f)
	expect(t, f.store.BatchEntries(context.Background(), f.owner, input), auth.ErrForbidden)
	noChange(t, f, before)
}

func TestBatchEntriesOnlyFirstHistoricalRowNonzeroPatch(t *testing.T) {
	f := newFixture(t)
	a := entry(t, f, f.owner.User.ID, f.book.Id, "device", `["old"]`)
	b := entry(t, f, f.owner.User.ID, f.book.Id, "device", `["duplicate"]`)
	must(t, f.db.Model(&model.AddressBook{}).Where("row_id = ?", a.RowId).Updates(map[string]any{
		"username": "keep username", "hostname": "keep host", "hash": "keep hash", "rdp_port": "3390",
		"rdp_username": "keep rdp", "login_name": "keep login", "platform": "keep platform",
		"force_always_relay": true, "online": true, "same_server": true,
	}).Error)
	before := entryRead(t, f, a.RowId)
	beforeDuplicate := entryRead(t, f, b.RowId)
	input := bookadmin.BatchEntryInput{UserIDs: []uint{f.owner.User.ID}, Entry: bookadmin.EntryInput{ID: a.Id, CollectionID: f.book.Id, Alias: "changed alias"}}
	must(t, f.store.BatchEntries(context.Background(), f.admin, input))
	got := entryRead(t, f, a.RowId)
	want := before
	want.Alias, want.Tags = input.Entry.Alias, custom_types.AutoJson(`[]`)
	want.TimeModel, got.TimeModel = model.TimeModel{}, model.TimeModel{}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("batch nonzero patch cleared omitted values or changed Note/identity/scope")
	}
	if !reflect.DeepEqual(beforeDuplicate, entryRead(t, f, b.RowId)) {
		t.Fatal("batch guessed a merge of historical duplicates")
	}
	input.Entry = entryFull(0)
	input.Entry.ID, input.Entry.CollectionID = a.Id, f.book.Id
	must(t, f.store.BatchEntries(context.Background(), f.admin, input))
	got = entryRead(t, f, a.RowId)
	if got.Password != input.Entry.Password || got.Hash != input.Entry.Hash || got.Note != a.Note || got.Tags.String() != `["中文标签","keep","keep"]` {
		t.Fatal("batch omitted a nonzero DTO field or normalized tags")
	}
}

func TestBatchEntriesRejectsAnyInvalidTargetAtomically(t *testing.T) {
	for _, name := range []string{"empty", "zero", "missing", "row-id", "empty-device", "parent-owner"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			input := bookadmin.BatchEntryInput{UserIDs: []uint{f.owner.User.ID}, Entry: bookadmin.EntryInput{ID: "device", Alias: "change"}}
			switch name {
			case "empty":
				input.UserIDs = nil
			case "zero":
				input.UserIDs = []uint{f.owner.User.ID, 0}
			case "missing":
				input.UserIDs = []uint{f.owner.User.ID, 99999}
			case "row-id":
				input.Entry.RowID = 1
			case "empty-device":
				input.Entry.ID = ""
			case "parent-owner":
				input.Entry.CollectionID = f.foreign.Id
			}
			before := snapshot(t, f)
			expect(t, f.store.BatchEntries(context.Background(), f.admin, input), bookadmin.ErrInput)
			noChange(t, f, before)
		})
	}
}

func TestEntriesFromPeersPureMappingMissingOnlyAndNoPeerWrites(t *testing.T) {
	f := newFixture(t)
	a := batchSource(t, f, "first", f.owner.User.ID, "Android Linux")
	b := batchSource(t, f, "second", f.owner.User.ID, "Mac Sonoma")
	old := entry(t, f, f.owner.User.ID, f.book.Id, a.Id, `["old"]`)
	oldDuplicate := entry(t, f, f.owner.User.ID, f.book.Id, a.Id, `["duplicate"]`)
	other := entry(t, f, f.owner.User.ID, 0, b.Id, `["personal"]`)
	beforeA, beforeB := peerRead(t, f, a.RowId), peerRead(t, f, b.RowId)
	beforeOld, beforeDuplicate, beforeOther := entryRead(t, f, old.RowId), entryRead(t, f, oldDuplicate.RowId), entryRead(t, f, other.RowId)
	input := bookadmin.FromPeersInput{PeerIDs: []uint{b.RowId, a.RowId}, CollectionID: f.book.Id, Tags: []string{"中文", "same", "same"}}
	must(t, f.store.EntriesFromPeers(context.Background(), f.owner, bookadmin.My, input))
	got := batchEntry(t, f, f.owner.User.ID, f.book.Id, b.Id)
	if got.Id != b.Id || got.Username != b.Username || got.Hostname != b.Hostname || got.Platform != "Mac OS" || got.UserId != f.owner.User.ID || got.CollectionId != f.book.Id || got.Tags.String() != `["中文","same","same"]` || got.Alias != "" || got.Note != "" || got.Password != "" || got.Hash != "" {
		t.Fatal("FromPeers did not retain pure FromPeer mapping")
	}
	if !reflect.DeepEqual(beforeA, peerRead(t, f, a.RowId)) || !reflect.DeepEqual(beforeB, peerRead(t, f, b.RowId)) || !reflect.DeepEqual(beforeOld, entryRead(t, f, old.RowId)) || !reflect.DeepEqual(beforeDuplicate, entryRead(t, f, oldDuplicate.RowId)) || !reflect.DeepEqual(beforeOther, entryRead(t, f, other.RowId)) {
		t.Fatal("FromPeers changed existing entries, other books, or peers")
	}
	// Admin may use a foreign source, and derive the destination owner from cid.
	input = bookadmin.FromPeersInput{PeerIDs: []uint{a.RowId}, CollectionID: f.foreign.Id}
	must(t, f.store.EntriesFromPeers(context.Background(), f.admin, bookadmin.Admin, input))
	got = batchEntry(t, f, f.other.User.ID, f.foreign.Id, a.Id)
	if got.Platform != "Android" || got.Tags.String() != `[]` || got.Note != "" || got.Alias != "" {
		t.Fatal("platform precedence or nil-tags/new note mapping changed")
	}
}

func TestEntriesFromPeersRejectsInvalidSetOrScopeAtomically(t *testing.T) {
	for _, name := range []string{"empty", "zero", "duplicate", "missing", "foreign-source", "empty-device", "wrong-parent", "personal-no-owner"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a := batchSource(t, f, "first", f.owner.User.ID, "windows")
			b := batchSource(t, f, "second", f.owner.User.ID, "linux")
			input := bookadmin.FromPeersInput{PeerIDs: []uint{a.RowId, b.RowId}, CollectionID: f.book.Id}
			actor, mode, want := f.owner, bookadmin.My, error(bookadmin.ErrInput)
			switch name {
			case "empty":
				input.PeerIDs = nil
			case "zero":
				input.PeerIDs = []uint{a.RowId, 0}
			case "duplicate":
				input.PeerIDs = []uint{a.RowId, a.RowId}
			case "missing":
				input.PeerIDs = []uint{a.RowId, 99999}
				want = bookadmin.ErrNotFound
			case "foreign-source":
				must(t, f.db.Model(&model.Peer{}).Where("row_id = ?", b.RowId).Update("user_id", f.other.User.ID).Error)
				want = auth.ErrForbidden
			case "empty-device":
				must(t, f.db.Model(&model.Peer{}).Where("row_id = ?", b.RowId).Update("id", "").Error)
			case "wrong-parent":
				input.CollectionID = f.foreign.Id
			case "personal-no-owner":
				input.CollectionID, actor, mode = 0, f.admin, bookadmin.Admin
			}
			before := snapshot(t, f)
			expect(t, f.store.EntriesFromPeers(context.Background(), actor, mode, input), want)
			noChange(t, f, before)
		})
	}
}

func TestEntriesFromPeersIdentityDriftFailsAndLockedBasicFieldsMap(t *testing.T) {
	for _, field := range []string{"id", "uuid", "user_id", "username"} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t)
			peer := batchSource(t, f, "device", f.owner.User.ID, "linux")
			before := snapshot(t, f)
			fired := false
			must(t, f.db.Callback().Query().Before("gorm:query").Register("batch:drift", func(tx *gorm.DB) {
				_, isPeer := tx.Statement.Dest.(*model.Peer)
				if !fired && isPeer && len(tx.Statement.Selects) == 0 {
					fired = true
					var value any = "changed"
					if field == "user_id" {
						value = f.other.User.ID
					}
					must(t, tx.Exec("UPDATE peers SET "+field+" = ? WHERE row_id = ?", value, peer.RowId).Error)
				}
			}))
			err := f.store.EntriesFromPeers(context.Background(), f.owner, bookadmin.My, bookadmin.FromPeersInput{PeerIDs: []uint{peer.RowId}, CollectionID: f.book.Id})
			if !fired {
				t.Fatal("source locking read was not reached")
			}
			if field == "username" {
				must(t, err)
				if batchEntry(t, f, f.owner.User.ID, f.book.Id, peer.Id).Username != "changed" {
					t.Fatal("basic fields did not come from the locked source")
				}
			} else {
				expect(t, err, bookadmin.ErrInput)
				noChange(t, f, before)
			}
		})
	}
}

func TestEntryBatchesAdminPersonalTargetAndUserSessionPeerOrder(t *testing.T) {
	f := newFixture(t)
	a := batchSource(t, f, "first", f.owner.User.ID, "linux")
	b := batchSource(t, f, "second", 0, "unknown OS")
	x := entry(t, f, f.other.User.ID, 0, a.Id, `["preserve"]`)
	y := entry(t, f, f.other.User.ID, 0, b.Id, `["preserve"]`)
	var events []string
	must(t, f.db.Callback().Query().After("gorm:query").Register("batch:order", func(tx *gorm.DB) {
		if tx.Error != nil || len(tx.Statement.Selects) != 0 {
			return
		}
		switch row := tx.Statement.Dest.(type) {
		case *model.User:
			events = append(events, fmt.Sprintf("user-%d", row.Id))
		case *model.UserToken:
			events = append(events, "session")
		case *model.Peer:
			events = append(events, fmt.Sprintf("peer-%d", row.RowId))
		case *model.AddressBook:
			events = append(events, fmt.Sprintf("entry-%d", row.RowId))
		}
	}))
	must(t, f.store.EntriesFromPeers(context.Background(), f.admin, bookadmin.Admin, bookadmin.FromPeersInput{PeerIDs: []uint{b.RowId, a.RowId}, UserID: f.other.User.ID}))
	want := []string{
		fmt.Sprintf("user-%d", f.admin.User.ID), fmt.Sprintf("user-%d", f.owner.User.ID), fmt.Sprintf("user-%d", f.other.User.ID), "session",
		fmt.Sprintf("peer-%d", a.RowId), fmt.Sprintf("peer-%d", b.RowId), fmt.Sprintf("entry-%d", x.RowId), fmt.Sprintf("entry-%d", y.RowId),
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("unexpected user/session/peer/entry order: %v", events)
	}
	must(t, f.db.Callback().Query().Remove("batch:order"))
	before := snapshot(t, f)
	expect(t, f.store.EntriesFromPeers(context.Background(), f.owner, bookadmin.My, bookadmin.FromPeersInput{PeerIDs: []uint{a.RowId}, UserID: f.other.User.ID}), bookadmin.ErrInput)
	noChange(t, f, before)
	// An existing disabled target remains a valid owner; this slice adds no
	// owner-status policy to the legacy administrator batch.
	must(t, f.db.Model(&model.User{}).Where("id = ?", f.other.User.ID).Update("status", model.COMMON_STATUS_DISABLED).Error)
	must(t, f.store.BatchEntries(context.Background(), f.admin, bookadmin.BatchEntryInput{UserIDs: []uint{f.other.User.ID}, Entry: bookadmin.EntryInput{ID: "new-personal"}}))
}

func TestReplaceEntryTagsAllTargetsNarrowAndCanonical(t *testing.T) {
	f := newFixture(t)
	a := entry(t, f, f.owner.User.ID, f.book.Id, "first", `["old"]`)
	b := entry(t, f, f.owner.User.ID, 0, "second", `["old"]`)
	peer := batchSource(t, f, a.Id, f.owner.User.ID, "linux")
	beforePeer := peerRead(t, f, peer.RowId)
	beforeA, beforeB := entryRead(t, f, a.RowId), entryRead(t, f, b.RowId)
	updates := 0
	must(t, f.db.Callback().Update().After("gorm:update").Register("batch:updates", func(tx *gorm.DB) {
		if tx.Error == nil && tx.Statement.Table == "address_books" {
			updates++
		}
	}))
	must(t, f.store.ReplaceEntryTags(context.Background(), f.owner, bookadmin.EntryTagsInput{RowIDs: []uint{b.RowId, a.RowId, b.RowId}, Tags: []string{"中文", "same", "same"}}))
	if updates != 2 {
		t.Fatal("duplicate requested IDs were not canonicalized")
	}
	for _, old := range []model.AddressBook{beforeA, beforeB} {
		got := entryRead(t, f, old.RowId)
		want := old
		want.Tags, want.TimeModel, got.TimeModel = custom_types.AutoJson(`["中文","same","same"]`), model.TimeModel{}, model.TimeModel{}
		if !reflect.DeepEqual(want, got) {
			t.Fatal("tags replace changed another entry column")
		}
	}
	if !reflect.DeepEqual(beforePeer, peerRead(t, f, peer.RowId)) {
		t.Fatal("tags replace changed peer")
	}
	var count int64
	must(t, f.db.Model(&model.Tag{}).Count(&count).Error)
	if count != 0 {
		t.Fatal("tags replace created a tag definition")
	}
	must(t, f.store.ReplaceEntryTags(context.Background(), f.owner, bookadmin.EntryTagsInput{RowIDs: []uint{a.RowId, b.RowId}}))
	if entryRead(t, f, a.RowId).Tags.String() != `[]` || entryRead(t, f, b.RowId).Tags.String() != `[]` {
		t.Fatal("nil tags did not clear to an empty list")
	}
}

func TestReplaceEntryTagsRejectsAnyMissingForeignOrInvalidRow(t *testing.T) {
	for _, name := range []string{"empty", "zero", "missing", "foreign", "scope-drift"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a := entry(t, f, f.owner.User.ID, f.book.Id, "first", `["old"]`)
			b := entry(t, f, f.owner.User.ID, 0, "second", `["old"]`)
			ids := []uint{a.RowId, b.RowId}
			want := error(bookadmin.ErrInput)
			fired := false
			switch name {
			case "empty":
				ids = nil
			case "zero":
				ids = []uint{a.RowId, 0}
			case "missing":
				ids = []uint{a.RowId, 99999}
				want = bookadmin.ErrNotFound
			case "foreign":
				must(t, f.db.Model(&model.AddressBook{}).Where("row_id = ?", b.RowId).Update("user_id", f.other.User.ID).Error)
				want = auth.ErrForbidden
			case "scope-drift":
				must(t, f.db.Callback().Query().Before("gorm:query").Register("batch:entrydrift", func(tx *gorm.DB) {
					_, isEntry := tx.Statement.Dest.(*model.AddressBook)
					if !fired && isEntry && len(tx.Statement.Selects) == 0 {
						fired = true
						must(t, tx.Exec("UPDATE address_books SET collection_id = 0 WHERE row_id = ?", a.RowId).Error)
					}
				}))
			}
			before := snapshot(t, f)
			expect(t, f.store.ReplaceEntryTags(context.Background(), f.owner, bookadmin.EntryTagsInput{RowIDs: ids, Tags: []string{"new"}}), want)
			if name == "scope-drift" && !fired {
				t.Fatal("scope drift control was not reached")
			}
			noChange(t, f, before)
		})
	}
}

func TestReplaceEntryTagsHasNoLegacy999RowCap(t *testing.T) {
	f := newFixture(t)
	rows := make([]model.AddressBook, 1001)
	for i := range rows {
		rows[i] = model.AddressBook{UserId: f.owner.User.ID, Id: fmt.Sprintf("device-%04d", i), Tags: custom_types.AutoJson(`["old"]`)}
	}
	must(t, f.db.CreateInBatches(&rows, 100).Error)
	ids := make([]uint, len(rows))
	for i := range rows {
		ids[len(rows)-i-1] = rows[i].RowId
	}
	must(t, f.store.ReplaceEntryTags(context.Background(), f.owner, bookadmin.EntryTagsInput{RowIDs: ids, Tags: []string{"new"}}))
	var count int64
	must(t, f.db.Model(&model.AddressBook{}).Where("tags = ?", `["new"]`).Count(&count).Error)
	if count != int64(len(rows)) {
		t.Fatalf("only %d of %d entries updated", count, len(rows))
	}
}

func TestReplaceEntryTagsLocksAllParentsThenSortedRowsBeforeWrites(t *testing.T) {
	f := newFixture(t)
	parent := model.AddressBookCollection{UserId: f.owner.User.ID, Name: "second owned"}
	must(t, f.db.Create(&parent).Error)
	a := entry(t, f, f.owner.User.ID, parent.Id, "first", `[]`)
	b := entry(t, f, f.owner.User.ID, f.book.Id, "second", `[]`)
	c := entry(t, f, f.owner.User.ID, 0, "third", `[]`)
	var events []string
	rowsLocked := 0
	must(t, f.db.Callback().Query().After("gorm:query").Register("batch:tagorder", func(tx *gorm.DB) {
		if tx.Error != nil || len(tx.Statement.Selects) != 0 {
			return
		}
		switch row := tx.Statement.Dest.(type) {
		case *model.AddressBookCollection:
			events = append(events, fmt.Sprintf("parent-%d", row.Id))
		case *model.AddressBook:
			rowsLocked++
			events = append(events, fmt.Sprintf("entry-%d", row.RowId))
		}
	}))
	must(t, f.db.Callback().Update().Before("gorm:update").Register("batch:tagwrite", func(tx *gorm.DB) {
		if tx.Statement.Table == "address_books" && rowsLocked != 3 {
			t.Error("an entry write occurred before every target row lock")
		}
	}))
	must(t, f.store.ReplaceEntryTags(context.Background(), f.owner, bookadmin.EntryTagsInput{RowIDs: []uint{c.RowId, b.RowId, a.RowId}, Tags: []string{"replace"}}))
	want := []string{fmt.Sprintf("parent-%d", f.book.Id), fmt.Sprintf("parent-%d", parent.Id), fmt.Sprintf("entry-%d", a.RowId), fmt.Sprintf("entry-%d", b.RowId), fmt.Sprintf("entry-%d", c.RowId)}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("unexpected parent/row order: %v", events)
	}
}

func TestEntryBatchesSQLAndActualCommitRollbackRetry(t *testing.T) {
	for _, method := range []string{"stock", "from-peers", "tags"} {
		for _, fault := range []string{"sql", "commit"} {
			t.Run(method+"-"+fault, func(t *testing.T) {
				f := newFixture(t)
				a := entry(t, f, f.owner.User.ID, 0, "first", `["old"]`)
				b := entry(t, f, f.owner.User.ID, f.book.Id, "second", `["old"]`)
				duplicate := entry(t, f, f.owner.User.ID, 0, a.Id, `["duplicate"]`)
				outside := entry(t, f, f.other.User.ID, f.foreign.Id, a.Id, `["outside"]`)
				pa := batchSource(t, f, "source-a", f.owner.User.ID, "linux")
				pb := batchSource(t, f, "source-b", f.owner.User.ID, "windows")
				beforeA, beforeB := entryRead(t, f, a.RowId), entryRead(t, f, b.RowId)
				beforeDuplicate, beforeOutside := entryRead(t, f, duplicate.RowId), entryRead(t, f, outside.RowId)
				beforePA, beforePB := peerRead(t, f, pa.RowId), peerRead(t, f, pb.RowId)
				before := snapshot(t, f)
				store, reached := f.store, false
				originalPool := f.db.Statement.ConnPool
				if fault == "commit" {
					owned := f.db.Session(&gorm.Session{NewDB: true, Context: context.Background()})
					if owned.Statement == f.db.Statement {
						t.Fatal("commit injection reused the original statement")
					}
					pool, err := f.db.DB()
					must(t, err)
					owned.Statement.ConnPool = commitPool{ConnPool: f.db.Statement.ConnPool, db: pool, reached: &reached}
					store, err = bookadmin.New(owned, bookadmin.Options{})
					must(t, err)
				} else {
					trigger := ""
					switch method {
					case "stock":
						trigger = fmt.Sprintf("CREATE TRIGGER fail_batch BEFORE INSERT ON address_books WHEN NEW.user_id = %d BEGIN SELECT RAISE(ABORT,'private later failure'); END", f.other.User.ID)
					case "from-peers":
						trigger = "CREATE TRIGGER fail_batch BEFORE INSERT ON address_books WHEN NEW.id = 'source-b' BEGIN SELECT RAISE(ABORT,'private later failure'); END"
					case "tags":
						trigger = fmt.Sprintf("CREATE TRIGGER fail_batch BEFORE UPDATE ON address_books WHEN NEW.row_id = %d BEGIN SELECT RAISE(ABORT,'private later failure'); END", b.RowId)
					}
					must(t, f.db.Exec(trigger).Error)
				}
				operation := func(s *bookadmin.Store) error {
					switch method {
					case "stock":
						return s.BatchEntries(context.Background(), f.admin, bookadmin.BatchEntryInput{UserIDs: []uint{f.other.User.ID, f.owner.User.ID}, Entry: bookadmin.EntryInput{ID: a.Id, Alias: "updated before later failure"}})
					case "from-peers":
						return s.EntriesFromPeers(context.Background(), f.owner, bookadmin.My, bookadmin.FromPeersInput{PeerIDs: []uint{pb.RowId, pa.RowId}, CollectionID: f.book.Id})
					default:
						return s.ReplaceEntryTags(context.Background(), f.owner, bookadmin.EntryTagsInput{RowIDs: []uint{b.RowId, a.RowId}, Tags: []string{"new"}})
					}
				}
				expect(t, operation(store), bookadmin.ErrUnavailable)
				if fault == "commit" && !reached {
					t.Fatal("actual Commit was not reached")
				}
				noChange(t, f, before)
				if f.db.Statement.ConnPool != originalPool {
					t.Fatal("commit injection replaced the fixture connection pool")
				}
				if fault == "sql" {
					must(t, f.db.Exec("DROP TRIGGER fail_batch").Error)
				}
				must(t, operation(f.store))
				switch method {
				case "stock":
					want, got := beforeA, entryRead(t, f, a.RowId)
					want.Alias, want.Tags = "updated before later failure", custom_types.AutoJson(`[]`)
					if !time.Time(got.CreatedAt).Equal(time.Time(beforeA.CreatedAt)) {
						t.Fatal("stock retry changed the entry creation time")
					}
					want.TimeModel, got.TimeModel = model.TimeModel{}, model.TimeModel{}
					if !reflect.DeepEqual(want, got) || !reflect.DeepEqual(beforeB, entryRead(t, f, b.RowId)) {
						t.Fatal("stock retry did not apply only the first-row nonzero patch and explicit tags")
					}
					created := batchEntry(t, f, f.other.User.ID, 0, a.Id)
					if created.Id != a.Id || created.UserId != f.other.User.ID || created.CollectionId != 0 || created.Alias != "updated before later failure" || created.Tags.String() != `[]` || created.Note != "" || created.Password != "" || created.Hash != "" || created.Username != "" || created.Hostname != "" || created.Platform != "" || created.ForceAlwaysRelay || created.Online || created.SameServer || time.Time(created.CreatedAt).IsZero() || time.Time(created.UpdatedAt).IsZero() {
						t.Fatal("stock retry did not create the second target with the expected fields")
					}
				case "from-peers":
					for i, peer := range []model.Peer{pa, pb} {
						created := batchEntry(t, f, f.owner.User.ID, f.book.Id, peer.Id)
						platform := []string{"Linux", "Windows"}[i]
						if created.Id != peer.Id || created.UserId != f.owner.User.ID || created.CollectionId != f.book.Id || created.Username != peer.Username || created.Hostname != peer.Hostname || created.Platform != platform || created.Tags.String() != `[]` || created.Alias != "" || created.Note != "" || created.Password != "" || created.Hash != "" || time.Time(created.CreatedAt).IsZero() || time.Time(created.UpdatedAt).IsZero() {
							t.Fatal("from-peers retry did not persist the pure source mapping")
						}
					}
					if !reflect.DeepEqual(beforeA, entryRead(t, f, a.RowId)) || !reflect.DeepEqual(beforeB, entryRead(t, f, b.RowId)) {
						t.Fatal("from-peers retry changed existing unrelated entries")
					}
				case "tags":
					for _, old := range []model.AddressBook{beforeA, beforeB} {
						want, got := old, entryRead(t, f, old.RowId)
						want.Tags = custom_types.AutoJson(`["new"]`)
						if !time.Time(got.CreatedAt).Equal(time.Time(old.CreatedAt)) {
							t.Fatal("tag retry changed the entry creation time")
						}
						want.TimeModel, got.TimeModel = model.TimeModel{}, model.TimeModel{}
						if !reflect.DeepEqual(want, got) {
							t.Fatal("tag retry changed columns beyond tags and normal update time")
						}
					}
				}
				if !reflect.DeepEqual(beforeDuplicate, entryRead(t, f, duplicate.RowId)) || !reflect.DeepEqual(beforeOutside, entryRead(t, f, outside.RowId)) || !reflect.DeepEqual(beforePA, peerRead(t, f, pa.RowId)) || !reflect.DeepEqual(beforePB, peerRead(t, f, pb.RowId)) {
					t.Fatal("batch retry changed a historical duplicate, another scope, or registered peer configuration")
				}
				var total int64
				must(t, f.db.Model(&model.AddressBook{}).Count(&total).Error)
				wantTotal := int64(4)
				if method == "stock" {
					wantTotal++
				} else if method == "from-peers" {
					wantTotal += 2
				}
				if total != wantTotal {
					t.Fatal("batch retry wrote an unexpected number of entries", total, wantTotal)
				}
			})
		}
	}
}

func TestEntryBatchesQueryFailureCancellationAndStaleSession(t *testing.T) {
	for _, method := range []string{"stock", "from-peers", "tags"} {
		for _, fault := range []string{"query", "cancel", "stale"} {
			t.Run(method+"-"+fault, func(t *testing.T) {
				f := newFixture(t)
				a := entry(t, f, f.owner.User.ID, 0, "first", `["old"]`)
				peer := batchSource(t, f, "source", f.owner.User.ID, "linux")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				actor := f.owner
				if method == "stock" {
					actor = f.admin
				}
				fired, armed := false, false
				want := error(bookadmin.ErrUnavailable)
				switch fault {
				case "query":
					must(t, f.db.Callback().Query().Before("gorm:query").Register("batch:query", func(tx *gorm.DB) {
						if armed && !fired && tx.Statement.Table == "address_books" {
							fired = true
							tx.AddError(errors.New("private lookup failure"))
						}
					}))
				case "cancel":
					want = context.Canceled
					cancelWrite := func(tx *gorm.DB) {
						if !fired && tx.Error == nil && tx.Statement.Table == "address_books" {
							fired = true
							cancel()
						}
					}
					must(t, f.db.Callback().Update().After("gorm:update").Register("batch:cancelupdate", cancelWrite))
					must(t, f.db.Callback().Create().After("gorm:create").Register("batch:cancelcreate", cancelWrite))
				case "stale":
					want = auth.ErrUnauthenticated
					must(t, f.db.Delete(&model.UserToken{}, actor.Session.ID).Error)
				}
				before := snapshot(t, f)
				armed = true
				var err error
				switch method {
				case "stock":
					err = f.store.BatchEntries(ctx, actor, bookadmin.BatchEntryInput{UserIDs: []uint{f.owner.User.ID}, Entry: bookadmin.EntryInput{ID: a.Id, Alias: "change"}})
				case "from-peers":
					err = f.store.EntriesFromPeers(ctx, actor, bookadmin.My, bookadmin.FromPeersInput{PeerIDs: []uint{peer.RowId}, CollectionID: f.book.Id})
				default:
					err = f.store.ReplaceEntryTags(ctx, actor, bookadmin.EntryTagsInput{RowIDs: []uint{a.RowId}, Tags: []string{"new"}})
				}
				expect(t, err, want)
				if fault != "stale" && !fired {
					t.Fatal("fault boundary was not reached")
				}
				noChange(t, f, before)
			})
		}
	}
}
