package bookadmin_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/bookadmin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

func entryRead(t *testing.T, f *fixture, id uint) model.AddressBook {
	t.Helper()
	var row model.AddressBook
	must(t, f.db.First(&row, id).Error)
	return row
}
func entryPeer(t *testing.T, f *fixture, device string) model.Peer {
	t.Helper()
	row := model.Peer{Id: device, Uuid: "private-uuid-" + device, UserId: f.other.User.ID,
		AbId: f.foreign.Id, Alias: "primary alias", Username: "registered username", Hostname: "registered host",
		GroupId: 19, Note: "private device note", Os: "private os", PresetAbId: 31, PresetAbAlias: "preset", Version: "1.5.0"}
	must(t, f.db.Create(&row).Error)
	return row
}
func peerRead(t *testing.T, f *fixture, id uint) model.Peer {
	t.Helper()
	var peer model.Peer
	must(t, f.db.First(&peer, id).Error)
	return peer
}
func entryFull(id uint) bookadmin.EntryInput {
	return bookadmin.EntryInput{RowID: id, Username: "new username", Password: "raw secret", Hostname: "new host", Alias: "new alias",
		Platform: "Linux", Hash: "unchanged encoding", RDPPort: "3390", RDPUsername: "rdp", LoginName: "login",
		Tags: []string{"中文标签", "keep", "keep"}, ForceAlwaysRelay: true, Online: true, SameServer: true}
}
func peerExceptAliasFields(peer model.Peer) model.Peer {
	peer.AbId, peer.Alias, peer.Username, peer.Hostname = 0, "", "", ""
	peer.TimeModel = model.TimeModel{}
	return peer
}

func TestEntryCreateOwnerScopeDuplicateAndNoPeerWrite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	peer := entryPeer(t, f, "device")
	beforePeer := peerRead(t, f, peer.RowId)
	input := entryFull(0)
	input.ID, input.CollectionID = peer.Id, f.book.Id
	must(t, f.store.CreateEntry(ctx, f.owner, bookadmin.My, input))
	var created model.AddressBook
	must(t, f.db.Where("id = ? AND user_id = ? AND collection_id = ?", peer.Id, f.owner.User.ID, f.book.Id).First(&created).Error)
	if created.Note != "" || created.UserId != f.owner.User.ID || created.Id != peer.Id || created.Password != input.Password || created.Hash != input.Hash || created.Tags.String() != `["中文标签","keep","keep"]` {
		t.Fatalf("create did not preserve DTO scope/fields: %#v", created)
	}
	if time.Time(created.CreatedAt).IsZero() || time.Time(created.UpdatedAt).IsZero() {
		t.Fatal("create omitted existing automatic timestamps")
	}
	if !reflect.DeepEqual(beforePeer, peerRead(t, f, peer.RowId)) {
		t.Fatal("My Create changed registered peer")
	}
	before := snapshot(t, f)
	expect(t, f.store.CreateEntry(ctx, f.owner, bookadmin.My, input), bookadmin.ErrInput)
	noChange(t, f, before)
	input.UserID = f.admin.User.ID // historical owner1 exception is gone
	expect(t, f.store.CreateEntry(ctx, f.owner, bookadmin.My, input), bookadmin.ErrInput)
	noChange(t, f, before)
	input.UserID = 0
	input.CollectionID = f.foreign.Id
	expect(t, f.store.CreateEntry(ctx, f.owner, bookadmin.My, input), bookadmin.ErrInput)
	noChange(t, f, before)
	input.UserID = f.owner.User.ID
	expect(t, f.store.CreateEntry(ctx, f.admin, bookadmin.Admin, input), bookadmin.ErrInput)
	noChange(t, f, before)
	input.UserID = 0 // Admin derives actual parent owner
	must(t, f.store.CreateEntry(ctx, f.admin, bookadmin.Admin, input))
	if !reflect.DeepEqual(beforePeer, peerRead(t, f, peer.RowId)) {
		t.Fatal("Admin Create changed registered peer")
	}
	// Same device in another book or with a different byte spelling is distinct.
	input.CollectionID, input.UserID = 0, f.owner.User.ID
	must(t, f.store.CreateEntry(ctx, f.admin, bookadmin.Admin, input))
	input.ID = "Device"
	must(t, f.store.CreateEntry(ctx, f.admin, bookadmin.Admin, input))
	zeroOwner := model.AddressBookCollection{Name: "historical zero owner"}
	must(t, f.db.Create(&zeroOwner).Error)
	input.UserID, input.CollectionID = 0, zeroOwner.Id
	before = snapshot(t, f)
	expect(t, f.store.CreateEntry(ctx, f.admin, bookadmin.Admin, input), bookadmin.ErrInput)
	noChange(t, f, before)
}

func TestEntryUpdateExplicitFieldsPreservesIdentityNoteAndDuplicates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := entry(t, f, f.owner.User.ID, f.book.Id, "device", `["old"]`)
	b := entry(t, f, f.owner.User.ID, f.book.Id, "device", `["duplicate"]`)
	peer := entryPeer(t, f, a.Id)
	beforeDuplicate := entryRead(t, f, b.RowId)
	beforePeer := peerRead(t, f, peer.RowId)
	input := entryFull(a.RowId)
	must(t, f.store.UpdateEntry(ctx, f.owner, bookadmin.My, input))
	got := entryRead(t, f, a.RowId)
	if got.UserId != a.UserId || got.CollectionId != a.CollectionId || got.Id != a.Id || got.Note != a.Note || got.Password != input.Password || got.Hash != input.Hash || !got.ForceAlwaysRelay || !got.Online || !got.SameServer {
		t.Fatal("update changed protected columns or omitted explicit DTO values")
	}
	if !reflect.DeepEqual(beforeDuplicate, entryRead(t, f, b.RowId)) || !reflect.DeepEqual(beforePeer, peerRead(t, f, peer.RowId)) {
		t.Fatal("My Update changed a duplicate or registered peer")
	}
	// Zero/empty identity preserves the fixed scope; empty DTO fields clear.
	must(t, f.store.UpdateEntry(ctx, f.owner, bookadmin.My, bookadmin.EntryInput{RowID: a.RowId}))
	got = entryRead(t, f, a.RowId)
	if got.UserId != a.UserId || got.CollectionId != a.CollectionId || got.Id != a.Id || got.Note != a.Note || got.Tags.String() != `[]` || got.Alias != "" || got.Password != "" || got.Hash != "" || got.RdpPort != "" || got.RdpUsername != "" || got.LoginName != "" || got.ForceAlwaysRelay || got.Online || got.SameServer {
		t.Fatal("explicit zero DTO update did not clear fields while preserving identity/note")
	}
	for _, change := range []bookadmin.EntryInput{
		{RowID: a.RowId, UserID: f.other.User.ID}, {RowID: a.RowId, CollectionID: f.foreign.Id}, {RowID: a.RowId, ID: "different-device"},
	} {
		before := snapshot(t, f)
		expect(t, f.store.UpdateEntry(ctx, f.admin, bookadmin.Admin, change), bookadmin.ErrInput)
		noChange(t, f, before)
	}
	before := snapshot(t, f)
	expect(t, f.store.UpdateEntry(ctx, f.other, bookadmin.My, bookadmin.EntryInput{RowID: a.RowId}), auth.ErrForbidden)
	noChange(t, f, before)
	input = bookadmin.EntryInput{RowID: a.RowId, UserID: a.UserId, CollectionID: a.CollectionId, ID: a.Id}
	must(t, f.store.UpdateEntry(ctx, f.owner, bookadmin.My, input))
}

func TestEntryAdminUpdatePeerNarrowAndMissingPeerAllowed(t *testing.T) {
	f := newFixture(t)
	a := entry(t, f, f.owner.User.ID, f.book.Id, "device", `[]`)
	other := entry(t, f, f.other.User.ID, f.foreign.Id, "device", `[]`)
	peer := entryPeer(t, f, a.Id)
	beforeOther := entryRead(t, f, other.RowId)
	beforePeer := peerRead(t, f, peer.RowId)
	input := entryFull(a.RowId)
	must(t, f.store.UpdateEntry(context.Background(), f.admin, bookadmin.Admin, input))
	got := peerRead(t, f, peer.RowId)
	if got.AbId != a.CollectionId || got.Alias != input.Alias || got.Username != input.Username || got.Hostname != input.Hostname || !reflect.DeepEqual(peerExceptAliasFields(beforePeer), peerExceptAliasFields(got)) {
		t.Fatal("Admin Update did not narrow its registered-peer side write")
	}
	if !reflect.DeepEqual(beforeOther, entryRead(t, f, other.RowId)) {
		t.Fatal("Admin Update changed another book")
	}
	unregistered := entry(t, f, f.owner.User.ID, f.book.Id, "unregistered", `[]`)
	must(t, f.store.UpdateEntry(context.Background(), f.admin, bookadmin.Admin, entryFull(unregistered.RowId)))
	// Empty values also propagate to the four authorized peer columns.
	must(t, f.store.UpdateEntry(context.Background(), f.admin, bookadmin.Admin, bookadmin.EntryInput{RowID: a.RowId}))
	got = peerRead(t, f, peer.RowId)
	if got.AbId != a.CollectionId || got.Alias != "" || got.Username != "" || got.Hostname != "" {
		t.Fatal("Admin Update omitted explicit empty peer values")
	}
}

func TestEntryDeletePrimaryMatchOnlyAndMyNeverChangesPeer(t *testing.T) {
	for _, name := range []string{"matching", "other-book", "personal", "my"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			cid := f.book.Id
			if name == "personal" {
				cid = 0
			}
			a := entry(t, f, f.owner.User.ID, cid, "device", `[]`)
			duplicate := entry(t, f, f.owner.User.ID, cid, "device", `[]`)
			peer := entryPeer(t, f, a.Id)
			if name != "other-book" {
				must(t, f.db.Model(&model.Peer{}).Where("row_id = ?", peer.RowId).Update("ab_id", cid).Error)
			}
			beforePeer := peerRead(t, f, peer.RowId)
			mode, actor := bookadmin.Admin, f.admin
			if name == "my" {
				mode, actor = bookadmin.My, f.owner
			}
			must(t, f.store.DeleteEntry(context.Background(), actor, mode, bookadmin.EntryInput{RowID: a.RowId}))
			var count int64
			must(t, f.db.Model(&model.AddressBook{}).Where("row_id = ?", a.RowId).Count(&count).Error)
			if count != 0 {
				t.Fatal("entry was not deleted")
			}
			_ = entryRead(t, f, duplicate.RowId)
			got := peerRead(t, f, peer.RowId)
			if name == "matching" {
				want := beforePeer
				want.AbId, want.Alias, want.TimeModel = 0, "", model.TimeModel{}
				got.TimeModel = model.TimeModel{}
				if !reflect.DeepEqual(want, got) {
					t.Fatal("Admin Delete changed columns beyond matching primary/alias")
				}
			} else if !reflect.DeepEqual(beforePeer, got) {
				t.Fatal("nonmatching/personal/My Delete changed peer")
			}
		})
	}
}

func TestEntryPeerAmbiguityAndCandidateDriftFailClosed(t *testing.T) {
	for _, change := range []string{"ambiguous", "peer-id", "peer-uuid", "peer-owner", "entry-device", "entry-owner", "entry-cid", "peer-appeared"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			a := entry(t, f, f.owner.User.ID, f.book.Id, "device", `[]`)
			peer := entryPeer(t, f, a.Id)
			if change == "ambiguous" {
				must(t, f.db.Exec("DROP INDEX idx_peers_id").Error)
				other := model.Peer{Id: a.Id, Uuid: "different-uuid"}
				must(t, f.db.Create(&other).Error)
			}
			if change == "peer-appeared" {
				must(t, f.db.Delete(&peer).Error)
			}
			before := snapshot(t, f)
			fired := false
			if change != "ambiguous" {
				must(t, f.db.Callback().Query().Before("gorm:query").Register("entry:drift", func(tx *gorm.DB) {
					if fired || tx.Error != nil {
						return
					}
					_, isPeer := tx.Statement.Dest.(*model.Peer)
					_, isEntry := tx.Statement.Dest.(*model.AddressBook)
					if (change == "peer-appeared" && isEntry && len(tx.Statement.Selects) == 0) || (change != "peer-appeared" && isPeer && len(tx.Statement.Selects) == 0) {
						fired = true
						switch change {
						case "peer-id":
							must(t, tx.Exec("UPDATE peers SET id = ? WHERE row_id = ?", "different-device", peer.RowId).Error)
						case "peer-uuid":
							must(t, tx.Exec("UPDATE peers SET uuid = ? WHERE row_id = ?", "different-uuid", peer.RowId).Error)
						case "peer-owner":
							must(t, tx.Exec("UPDATE peers SET user_id = ? WHERE row_id = ?", f.owner.User.ID, peer.RowId).Error)
						case "entry-device":
							must(t, tx.Exec("UPDATE address_books SET id = ? WHERE row_id = ?", "different-device", a.RowId).Error)
						case "entry-owner":
							must(t, tx.Exec("UPDATE address_books SET user_id = ? WHERE row_id = ?", f.other.User.ID, a.RowId).Error)
						case "entry-cid":
							must(t, tx.Exec("UPDATE address_books SET collection_id = ? WHERE row_id = ?", f.foreign.Id, a.RowId).Error)
						case "peer-appeared":
							newPeer := model.Peer{Id: a.Id, Uuid: "new-uuid"}
							must(t, tx.Session(&gorm.Session{NewDB: true}).Create(&newPeer).Error)
						}
					}
				}))
			}
			expect(t, f.store.UpdateEntry(context.Background(), f.admin, bookadmin.Admin, entryFull(a.RowId)), bookadmin.ErrInput)
			if change != "ambiguous" && !fired {
				t.Fatal("drift control was not reached")
			}
			noChange(t, f, before)
		})
	}
}

func TestEntrySQLAndActualCommitFailuresRollbackRetry(t *testing.T) {
	for _, name := range []string{"create", "update", "delete", "update-peer", "delete-peer", "update-commit", "delete-commit", "create-commit"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a := entry(t, f, f.owner.User.ID, f.book.Id, "device", `[]`)
			peer := entryPeer(t, f, a.Id)
			must(t, f.db.Model(&model.Peer{}).Where("row_id = ?", peer.RowId).Update("ab_id", f.book.Id).Error)
			beforePeer := peerRead(t, f, peer.RowId)
			before := snapshot(t, f)
			store := f.store
			reached := false
			trigger := ""
			switch name {
			case "create":
				trigger = "CREATE TRIGGER fail_entry BEFORE INSERT ON address_books BEGIN SELECT RAISE(ABORT,'private SQL failure'); END"
			case "update":
				trigger = "CREATE TRIGGER fail_entry BEFORE UPDATE ON address_books BEGIN SELECT RAISE(ABORT,'private SQL failure'); END"
			case "delete":
				trigger = "CREATE TRIGGER fail_entry BEFORE DELETE ON address_books BEGIN SELECT RAISE(ABORT,'private SQL failure'); END"
			case "update-peer", "delete-peer":
				trigger = "CREATE TRIGGER fail_entry BEFORE UPDATE ON peers BEGIN SELECT RAISE(ABORT,'private SQL failure'); END"
			default:
				owned := f.db.Session(&gorm.Session{NewDB: true, Context: context.Background()})
				pool, err := f.db.DB()
				must(t, err)
				owned.Statement.ConnPool = commitPool{ConnPool: f.db.Statement.ConnPool, db: pool, reached: &reached}
				store, err = bookadmin.New(owned, bookadmin.Options{})
				must(t, err)
			}
			if trigger != "" {
				must(t, f.db.Exec(trigger).Error)
			}
			operation := func(s *bookadmin.Store) error {
				switch name {
				case "create", "create-commit":
					return s.CreateEntry(context.Background(), f.admin, bookadmin.Admin, bookadmin.EntryInput{UserID: f.owner.User.ID, CollectionID: f.book.Id, ID: "new-device"})
				case "delete", "delete-peer", "delete-commit":
					return s.DeleteEntry(context.Background(), f.admin, bookadmin.Admin, bookadmin.EntryInput{RowID: a.RowId})
				default:
					return s.UpdateEntry(context.Background(), f.admin, bookadmin.Admin, entryFull(a.RowId))
				}
			}
			expect(t, operation(store), bookadmin.ErrUnavailable)
			if trigger == "" && !reached {
				t.Fatal("did not reach the actual Commit boundary")
			}
			noChange(t, f, before)
			if trigger != "" {
				must(t, f.db.Exec("DROP TRIGGER fail_entry").Error)
			}
			must(t, operation(f.store))
			switch name {
			case "create", "create-commit":
				var created model.AddressBook
				must(t, f.db.Where("id = ? AND user_id = ? AND collection_id = ?", "new-device", f.owner.User.ID, f.book.Id).First(&created).Error)
				if created.Id != "new-device" || created.UserId != f.owner.User.ID || created.CollectionId != f.book.Id || created.Note != "" || created.Tags.String() != `[]` || created.Alias != "" || created.Username != "" || created.Hostname != "" || created.Password != "" || created.Hash != "" || created.Platform != "" || created.RdpPort != "" || created.RdpUsername != "" || created.LoginName != "" || created.ForceAlwaysRelay || created.Online || created.SameServer {
					t.Fatal("create retry did not persist the expected scope and explicit field values")
				}
				if time.Time(created.CreatedAt).IsZero() || time.Time(created.UpdatedAt).IsZero() {
					t.Fatal("create retry omitted managed timestamps")
				}
				var count int64
				must(t, f.db.Model(&model.AddressBook{}).Where("id = ? AND user_id = ? AND collection_id = ?", "new-device", f.owner.User.ID, f.book.Id).Count(&count).Error)
				if count != 1 || !reflect.DeepEqual(beforePeer, peerRead(t, f, peer.RowId)) {
					t.Fatal("create retry duplicated its entry or changed the registered peer")
				}
			case "delete", "delete-peer", "delete-commit":
				var count int64
				must(t, f.db.Model(&model.AddressBook{}).Where("row_id = ?", a.RowId).Count(&count).Error)
				want, got := beforePeer, peerRead(t, f, peer.RowId)
				want.AbId, want.Alias = 0, ""
				// Keep the existing ORM updated_at behavior; only two peer
				// business columns may differ after a matching primary delete.
				want.TimeModel, got.TimeModel = model.TimeModel{}, model.TimeModel{}
				if count != 0 || !reflect.DeepEqual(want, got) {
					t.Fatal("delete retry retained the entry or changed peer columns beyond primary/alias")
				}
			default:
				input := entryFull(a.RowId)
				got := entryRead(t, f, a.RowId)
				if got.Id != a.Id || got.UserId != a.UserId || got.CollectionId != a.CollectionId || got.Note != a.Note || !time.Time(got.CreatedAt).Equal(time.Time(a.CreatedAt)) || got.Username != input.Username || got.Hostname != input.Hostname || got.Alias != input.Alias || got.Password != input.Password || got.Platform != input.Platform || got.Hash != input.Hash || got.RdpPort != input.RDPPort || got.RdpUsername != input.RDPUsername || got.LoginName != input.LoginName || got.Tags.String() != `["中文标签","keep","keep"]` || !got.ForceAlwaysRelay || !got.Online || !got.SameServer {
					t.Fatal("update retry did not persist the expected fields while preserving identity and note")
				}
				afterPeer := peerRead(t, f, peer.RowId)
				if afterPeer.AbId != a.CollectionId || afterPeer.Alias != input.Alias || afterPeer.Username != input.Username || afterPeer.Hostname != input.Hostname || !reflect.DeepEqual(peerExceptAliasFields(beforePeer), peerExceptAliasFields(afterPeer)) {
					t.Fatal("update retry did not persist the four authorized peer business fields")
				}
			}
		})
	}
}

func TestEntryLockOrderAndFreshAuthentication(t *testing.T) {
	f := newFixture(t)
	a := entry(t, f, f.owner.User.ID, f.book.Id, "device", `[]`)
	_ = entryPeer(t, f, a.Id)
	var events []string
	must(t, f.db.Callback().Query().After("gorm:query").Register("entry:order", func(tx *gorm.DB) {
		if tx.Error != nil || len(tx.Statement.Selects) != 0 {
			return
		}
		switch tx.Statement.Dest.(type) {
		case *model.User:
			events = append(events, "user")
		case *model.UserToken:
			events = append(events, "session")
		case *model.Peer:
			events = append(events, "peer")
		case *model.AddressBookCollection:
			events = append(events, "parent")
		case *model.AddressBook:
			events = append(events, "entry")
		}
	}))
	must(t, f.store.UpdateEntry(context.Background(), f.admin, bookadmin.Admin, entryFull(a.RowId)))
	if !reflect.DeepEqual(events, []string{"user", "user", "user", "session", "peer", "parent", "entry"}) {
		t.Fatalf("unexpected lock order %v", events)
	}
	must(t, f.db.Callback().Query().Remove("entry:order"))
	for _, name := range []string{"disabled", "demoted", "revoked", "ambiguous-token", "expired"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a := entry(t, f, f.owner.User.ID, f.book.Id, "device", `[]`)
			actor := f.admin
			want := auth.ErrUnauthenticated
			switch name {
			case "disabled":
				must(t, f.db.Model(&model.User{}).Where("id = ?", actor.User.ID).Update("status", model.COMMON_STATUS_DISABLED).Error)
			case "demoted":
				must(t, f.db.Model(&model.User{}).Where("id = ?", actor.User.ID).Update("is_admin", false).Error)
				want = auth.ErrForbidden
			case "revoked":
				must(t, f.db.Delete(&model.UserToken{}, actor.Session.ID).Error)
			case "ambiguous-token":
				must(t, f.db.Create(&model.UserToken{UserId: f.other.User.ID, Token: actor.Session.Token, ExpiredAt: actor.Session.ExpiresAt.Unix()}).Error)
			case "expired":
				must(t, f.db.Model(&model.UserToken{}).Where("id = ?", actor.Session.ID).Update("expired_at", time.Now().Add(-time.Hour).Unix()).Error)
			}
			before := snapshot(t, f)
			expect(t, f.store.UpdateEntry(context.Background(), actor, bookadmin.Admin, entryFull(a.RowId)), want)
			expect(t, f.store.DeleteEntry(context.Background(), actor, bookadmin.Admin, bookadmin.EntryInput{RowID: a.RowId}), want)
			noChange(t, f, before)
		})
	}
}

func TestEntryCancellationAndQueryFaultsFailClosed(t *testing.T) {
	for _, name := range []string{"candidate", "peers", "locked-entry", "peer-recheck", "cancel-after-entry-write"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a := entry(t, f, f.owner.User.ID, f.book.Id, "device", `[]`)
			_ = entryPeer(t, f, a.Id)
			before := snapshot(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fired, peerReads := false, 0
			if name == "cancel-after-entry-write" {
				must(t, f.db.Callback().Update().After("gorm:update").Register("entry:cancel", func(tx *gorm.DB) {
					if tx.Statement.Table == "address_books" && tx.Error == nil {
						fired = true
						cancel()
					}
				}))
			} else {
				must(t, f.db.Callback().Query().Before("gorm:query").Register("entry:fault", func(tx *gorm.DB) {
					_, isEntry := tx.Statement.Dest.(*model.AddressBook)
					_, isPeers := tx.Statement.Dest.(*[]model.Peer)
					if isPeers {
						peerReads++
					}
					if !fired && ((name == "candidate" && isEntry && len(tx.Statement.Selects) != 0) || (name == "locked-entry" && isEntry && len(tx.Statement.Selects) == 0) || (name == "peers" && isPeers && peerReads == 1) || (name == "peer-recheck" && isPeers && peerReads == 2)) {
						fired = true
						tx.AddError(errors.New("private query failure"))
					}
				}))
			}
			want := bookadmin.ErrUnavailable
			if name == "cancel-after-entry-write" {
				want = context.Canceled
			}
			expect(t, f.store.UpdateEntry(ctx, f.admin, bookadmin.Admin, entryFull(a.RowId)), want)
			if !fired {
				t.Fatal("fault boundary was not reached")
			}
			noChange(t, f, before)
		})
	}
}

// Ensure tests exercise the DTO's raw tags rather than a model scan that may
// normalize malformed stored legacy JSON. Management update replaces tags.
func TestEntryUpdateReplacesMalformedStoredTags(t *testing.T) {
	f := newFixture(t)
	a := entry(t, f, f.owner.User.ID, f.book.Id, "device", `[]`)
	must(t, f.db.Exec("UPDATE address_books SET tags = ? WHERE row_id = ?", "not-json", a.RowId).Error)
	must(t, f.store.UpdateEntry(context.Background(), f.owner, bookadmin.My, bookadmin.EntryInput{RowID: a.RowId, Tags: []string{"中文"}}))
	got := entryRead(t, f, a.RowId)
	if !reflect.DeepEqual(got.Tags, custom_types.AutoJson(`["中文"]`)) {
		t.Fatal("DTO tags were not persisted exactly")
	}
}
