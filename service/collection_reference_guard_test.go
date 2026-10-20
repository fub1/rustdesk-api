package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/collectionguard"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type userCollectionFixture struct {
	db                   *gorm.DB
	service              *UserService
	user, other          model.User
	books                []model.AddressBookCollection
	ownedPeer, otherPeer model.Peer
}

func userCollectionFixtureDB(t *testing.T) userCollectionFixture {
	t.Helper()
	db := issuanceDB(t)
	us := issuanceService(t, db, time.Hour)
	user := issuanceUser(t, db)
	other := model.User{Username: "other-user", Status: model.COMMON_STATUS_ENABLE}
	issuanceCreate(t, db, &other)
	books := []model.AddressBookCollection{{UserId: user.Id, Name: "owned-a"}, {UserId: user.Id, Name: "owned-b"}, {UserId: other.Id, Name: "other-book"}}
	for i := range books {
		issuanceCreate(t, db, &books[i])
	}
	for _, row := range []any{&model.OIDCIdentity{Issuer: "https://issuer.example.test", Subject: "subject", UserID: user.Id}, &model.UserThird{UserId: user.Id, Op: "legacy"}, &model.UserToken{UserId: user.Id, Token: "target-session-a"}, &model.UserToken{UserId: user.Id, Token: "target-session-b"}} {
		issuanceCreate(t, db, row)
	}
	for _, scope := range []struct{ owner, cid uint }{{user.Id, 0}, {other.Id, 0}, {other.Id, books[0].Id}, {user.Id, books[1].Id}, {user.Id, books[2].Id}, {other.Id, books[2].Id}} {
		for _, row := range []any{&model.Tag{UserId: scope.owner, CollectionId: scope.cid, Name: "tag"}, &model.AddressBook{Id: "device", UserId: scope.owner, CollectionId: scope.cid, Tags: custom_types.AutoJson(`[]`)}, &model.AddressBookCollectionRule{UserId: scope.owner, CollectionId: scope.cid, Rule: 3, Type: 1, ToId: other.Id}} {
			issuanceCreate(t, db, row)
		}
	}
	owned := model.Peer{Id: "owned-device", Uuid: "owned-uuid", UserId: user.Id, AbId: books[2].Id, Alias: "preserved"}
	issuanceCreate(t, db, &owned)
	foreign := model.Peer{Id: "other-device", Uuid: "other-uuid", UserId: other.Id, Alias: "foreign"}
	issuanceCreate(t, db, &foreign)
	return userCollectionFixture{db: db, service: us, user: user, other: other, books: books, ownedPeer: owned, otherPeer: foreign}
}
func userCollectionSnapshot(t *testing.T, db *gorm.DB) []any {
	t.Helper()
	var users []model.User
	var tokens []model.UserToken
	var identities []model.OIDCIdentity
	var thirds []model.UserThird
	var books []model.AddressBookCollection
	var tags []model.Tag
	var entries []model.AddressBook
	var rules []model.AddressBookCollectionRule
	var peers []model.Peer
	for _, rows := range []any{&users, &tokens, &identities, &thirds, &books, &tags, &entries, &rules, &peers} {
		if err := db.Find(rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	return []any{users, tokens, identities, thirds, books, tags, entries, rules, peers}
}

func TestUserCollectionDeleteRejectsAnyPeerOwnerReference(t *testing.T) {
	f := userCollectionFixtureDB(t)
	if err := f.db.Model(&model.Peer{}).Where("row_id=?", f.otherPeer.RowId).Update("ab_id", f.books[0].Id).Error; err != nil {
		t.Fatal(err)
	}
	before := userCollectionSnapshot(t, f.db)
	if err := f.service.DeleteContext(context.Background(), &f.user); !errors.Is(err, collectionguard.ErrReferenced) {
		t.Fatal("foreign peer owner reference bypassed user delete guard", err)
	}
	if !reflect.DeepEqual(before, userCollectionSnapshot(t, f.db)) {
		t.Fatal("referenced user delete partially changed identity/session/peer/child data")
	}
}

func TestUserCollectionDeleteCleansSplitOwnerAndPersonalTags(t *testing.T) {
	f := userCollectionFixtureDB(t)
	if err := f.service.DeleteContext(context.Background(), &f.user); err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{&model.Tag{}, &model.AddressBook{}, &model.AddressBookCollectionRule{}} {
		var n int64
		if err := f.db.Model(row).Where("collection_id IN ? OR user_id=?", []uint{f.books[0].Id, f.books[1].Id}, f.user.Id).Count(&n).Error; err != nil || n != 0 {
			t.Fatal("split-owner CID/personal user data remains", err)
		}
		if err := f.db.Model(row).Where("user_id=?", f.other.Id).Count(&n).Error; err != nil || n != 2 {
			t.Fatal("delete changed other user's personal/book rows", err)
		}
	}
	var peer model.Peer
	if err := f.db.First(&peer, f.ownedPeer.RowId).Error; err != nil || peer.UserId != 0 || peer.AbId != f.books[2].Id || peer.Alias != f.ownedPeer.Alias {
		t.Fatal("user deletion changed another owner's valid primary book/alias", err)
	}
	var remaining model.AddressBookCollection
	if err := f.db.First(&remaining, f.books[2].Id).Error; err != nil || remaining.UserId != f.other.Id {
		t.Fatal("other collection removed", err)
	}
}

func TestUserCollectionDeleteEveryStatementAndCommitRollbackRetry(t *testing.T) {
	for _, stage := range []string{"tags", "address_books", "address_book_collection_rules", "address_book_collections", "oidc_identities", "user_thirds", "user_tokens", "users", "peer-unlink", "commit"} {
		t.Run(stage, func(t *testing.T) {
			f := userCollectionFixtureDB(t)
			before := userCollectionSnapshot(t, f.db)
			reached := false
			if stage == "commit" {
				pool, err := f.db.DB()
				if err != nil {
					t.Fatal(err)
				}
				fault, err := gorm.Open(sqlite.Dialector{Conn: collectionCommitPool{ConnPool: pool, db: pool, reached: &reached}}, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
				if err != nil {
					t.Fatal(err)
				}
				f.service.db = fault
			} else {
				sql := fmt.Sprintf("CREATE TRIGGER fail_user_collection BEFORE DELETE ON %s BEGIN SELECT RAISE(ABORT,'user lifecycle statement fault'); END", stage)
				if stage == "peer-unlink" {
					sql = "CREATE TRIGGER fail_user_collection BEFORE UPDATE OF user_id ON peers BEGIN SELECT RAISE(ABORT,'user peer unlink fault'); END"
				}
				issuanceSQL(t, f.db, sql)
			}
			if err := f.service.DeleteContext(context.Background(), &f.user); !errors.Is(err, collectionguard.ErrUnavailable) || strings.Contains(err.Error(), "fault") {
				t.Fatal("failed deletion was not safely classified", err)
			}
			if stage == "commit" && !reached {
				t.Fatal("actual user COMMIT not reached")
			}
			if !reflect.DeepEqual(before, userCollectionSnapshot(t, f.db)) {
				t.Fatal("failed user delete retained partial changes")
			}
			if stage != "commit" {
				issuanceSQL(t, f.db, "DROP TRIGGER fail_user_collection")
			}
			f.service.db = f.db
			if err := f.service.DeleteContext(context.Background(), &f.user); err != nil {
				t.Fatal("retry", err)
			}
		})
	}
}

func TestUserCollectionDeleteLocksSessionsPeersBeforeCollections(t *testing.T) {
	f := userCollectionFixtureDB(t)
	events := []string{}
	ids := []uint{}
	if err := f.db.Callback().Query().After("gorm:query").Register("user:observe-lifecycle-locks", func(tx *gorm.DB) {
		switch tx.Statement.Table {
		case "users":
			events = append(events, "user")
		case "user_tokens":
			if _, ok := tx.Statement.Dest.(*[]model.UserToken); ok && strings.Contains(strings.Join(tx.Statement.Selects, ","), "user_id") {
				events = append(events, "sessions")
			}
		case "peers":
			if _, ok := tx.Statement.Dest.(*model.Peer); ok {
				events = append(events, "peers")
			}
		case "address_book_collections":
			if row, ok := tx.Statement.Dest.(*model.AddressBookCollection); ok {
				events = append(events, "collection")
				ids = append(ids, row.Id)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DeleteContext(context.Background(), &f.user); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"user", "sessions", "sessions", "peers", "collection", "collection"}) || !reflect.DeepEqual(ids, []uint{f.books[0].Id, f.books[1].Id}) {
		t.Fatalf("wrong lock order: events=%v ids=%v", events, ids)
	}
}

func TestUserCollectionDeleteQueryAndCancellationFailuresAreSafe(t *testing.T) {
	for _, stage := range []string{"identity", "third", "sessions", "peer", "first-count", "second-count", "cancel-after-session-delete"} {
		t.Run(stage, func(t *testing.T) {
			f := userCollectionFixtureDB(t)
			before := userCollectionSnapshot(t, f.db)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reached, counts := false, 0
			if err := f.db.Callback().Query().After("gorm:query").Register("user:query-guard-fault", func(tx *gorm.DB) {
				countQuery := tx.Statement.Table == "peers" && strings.Contains(strings.ToLower(tx.Statement.SQL.String()), "count(")
				if countQuery {
					counts++
				}
				fail := stage == "identity" && tx.Statement.Table == "oidc_identities" || stage == "third" && tx.Statement.Table == "user_thirds" || stage == "sessions" && tx.Statement.Table == "user_tokens" || stage == "peer" && tx.Statement.Table == "peers" && !countQuery || stage == "first-count" && countQuery && counts == 1 || stage == "second-count" && countQuery && counts == 2
				if fail {
					reached = true
					tx.AddError(errors.New("private lifecycle query fault"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Callback().Delete().After("gorm:delete").Register("user:cancel-after-sessions", func(tx *gorm.DB) {
				if stage == "cancel-after-session-delete" && tx.Statement.Table == "user_tokens" && tx.Error == nil {
					reached = true
					cancel()
				}
			}); err != nil {
				t.Fatal(err)
			}
			want := collectionguard.ErrUnavailable
			if stage == "cancel-after-session-delete" {
				want = context.Canceled
			}
			if err := f.service.DeleteContext(ctx, &f.user); !errors.Is(err, want) || !reached || strings.Contains(err.Error(), "private") {
				t.Fatalf("query/cancel boundary not safely rejected stage=%s err=%v reached=%t", stage, err, reached)
			}
			if err := f.db.Callback().Query().Remove("user:query-guard-fault"); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Callback().Delete().Remove("user:cancel-after-sessions"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, userCollectionSnapshot(t, f.db)) {
				t.Fatal("query/cancel failure retained user/child/identity changes")
			}
		})
	}
}

func TestUserCollectionDeleteKeepsLastAdministratorSentinel(t *testing.T) {
	f := userCollectionFixtureDB(t)
	if err := f.db.Model(&model.User{}).Where("id=?", f.user.Id).Update("is_admin", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.service.DeleteContext(context.Background(), &f.user); !errors.Is(err, errLastAdminDelete) || err.Error() != "The last admin user cannot be deleted" {
		t.Fatal("last-admin decision was hidden by SQL mapping", err)
	}
}

func TestPrimaryBookBindingAdmissionAndDeletionProtocol(t *testing.T) {
	for _, ownerMode := range []string{"normal", "disabled", "historical-owner0"} {
		t.Run(ownerMode, func(t *testing.T) {
			f := collectionFixtureDB(t)
			peer := model.Peer{Id: "bind-device", Uuid: "bind-uuid", Alias: "before"}
			issuanceCreate(t, f.db, &peer)
			if ownerMode == "disabled" {
				if err := f.db.Model(&f.owner).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
					t.Fatal(err)
				}
			}
			if ownerMode == "historical-owner0" {
				if err := f.db.Model(&f.book).Update("user_id", 0).Error; err != nil {
					t.Fatal(err)
				}
			}
			service := &PeerService{}
			if err := service.UpdateAlias(peer.RowId, f.book.Id, "after", "user", "host"); err != nil {
				t.Fatal("existing target rejected", err)
			}
			if err := (&AddressBookService{}).DeleteCollection(&f.book); !errors.Is(err, collectionguard.ErrReferenced) {
				t.Fatal("committed binding not counted", err)
			}
			if err := service.UpdateAlias(peer.RowId, 0, "personal-alias", "user", "host"); err != nil {
				t.Fatal("zero/personal semantics changed", err)
			}
			// Owner0 case uses a current parent snapshot for the legacy expectedOwner check.
			if err := f.db.First(&f.book, f.book.Id).Error; err != nil {
				t.Fatal(err)
			}
			if err := (&AddressBookService{}).DeleteCollection(&f.book); err != nil {
				t.Fatal("detached collection cannot delete", err)
			}
			if err := service.UpdateAlias(peer.RowId, f.book.Id, "must-not-write", "bad", "bad"); !errors.Is(err, collectionguard.ErrInput) {
				t.Fatal("deleted target accepted late binding", err)
			}
			var after model.Peer
			if err := f.db.First(&after, peer.RowId).Error; err != nil || after.AbId != 0 || after.Alias != "personal-alias" || after.Username != "user" {
				t.Fatal("failed late binding retained partial values", err)
			}
		})
	}
}

func TestPrimaryBookBindingOwnerDriftAndFailuresRollback(t *testing.T) {
	for _, stage := range []string{"missing-parent", "missing-peer", "missing-owner", "owner-drift", "owner-query", "peer-query", "parent-query", "write", "commit", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			f := collectionFixtureDB(t)
			peer := model.Peer{Id: "binding-fault", Uuid: "uuid", Alias: "before"}
			issuanceCreate(t, f.db, &peer)
			if err := f.db.First(&peer, peer.RowId).Error; err != nil {
				t.Fatal(err)
			}
			target := f.book.Id
			peerID := peer.RowId
			if stage == "missing-peer" {
				peerID += 999
			}
			reached := false
			bookQueries := 0
			if stage == "missing-parent" {
				target += 999
			}
			if stage == "missing-owner" {
				if err := f.db.Model(&f.book).Update("user_id", 999999).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := f.db.Callback().Query().After("gorm:query").Register("binding:query-fault", func(tx *gorm.DB) {
				if tx.Statement.Table == "address_book_collections" {
					bookQueries++
				}
				if stage == "owner-drift" && tx.Statement.Table == "address_book_collections" && bookQueries == 1 {
					reached = true
					if err := tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE address_book_collections SET user_id=? WHERE id=?", f.other.Id, f.book.Id).Error; err != nil {
						tx.AddError(err)
					}
				}
				if stage == "owner-query" && tx.Statement.Table == "users" || stage == "peer-query" && tx.Statement.Table == "peers" || stage == "parent-query" && tx.Statement.Table == "address_book_collections" && bookQueries == 2 {
					reached = true
					tx.AddError(errors.New("private binding query fault"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			if stage == "write" {
				issuanceSQL(t, f.db, "CREATE TRIGGER fail_primary_write BEFORE UPDATE OF ab_id ON peers BEGIN SELECT RAISE(ABORT,'primary write fault'); END")
			}
			if stage == "commit" {
				pool, err := f.db.DB()
				if err != nil {
					t.Fatal(err)
				}
				DB, err = gorm.Open(sqlite.Dialector{Conn: collectionCommitPool{ConnPool: pool, db: pool, reached: &reached}}, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
				if err != nil {
					t.Fatal(err)
				}
			}
			if stage == "cancel" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				DB = f.db.WithContext(ctx)
			}
			err := (&PeerService{}).UpdateAlias(peerID, target, "after", "user", "host")
			want := collectionguard.ErrUnavailable
			if stage == "missing-parent" || stage == "missing-peer" || stage == "missing-owner" || stage == "owner-drift" {
				want = collectionguard.ErrInput
			}
			if stage == "cancel" {
				want = context.Canceled
			}
			if !errors.Is(err, want) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "fault") {
				t.Fatal("failed binding was not safely classified", err)
			}
			if (stage == "commit" || stage == "owner-drift" || stage == "owner-query" || stage == "peer-query" || stage == "parent-query") && !reached {
				t.Fatal("fault point was not reached")
			}
			if stage == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancelled context lost", err)
			}
			if err := f.db.Callback().Query().Remove("binding:query-fault"); err != nil {
				t.Fatal(err)
			}
			var after model.Peer
			if err := f.db.First(&after, peer.RowId).Error; err != nil || !reflect.DeepEqual(peer, after) {
				t.Fatal("failed primary binding changed peer", err)
			}
		})
	}
}
