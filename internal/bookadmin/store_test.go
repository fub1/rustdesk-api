package bookadmin_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/bookadmin"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type fixture struct {
	db                  *gorm.DB
	store               *bookadmin.Store
	admin, owner, other auth.Principal
	book, foreign       model.AddressBookCollection
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func user(t *testing.T, db *gorm.DB, name string, admin bool) auth.Principal {
	t.Helper()
	row := model.User{Username: name, Status: model.COMMON_STATUS_ENABLE, IsAdmin: &admin}
	must(t, db.Create(&row).Error)
	end := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	token := model.UserToken{UserId: row.Id, Token: "private-" + name, ExpiredAt: end.Unix()}
	must(t, db.Create(&token).Error)
	return auth.Principal{User: auth.User{ID: row.Id, Enabled: true, IsAdmin: admin}, Session: auth.Session{ID: token.Id, UserID: row.Id, Token: token.Token, ExpiresAt: end}}
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "bookadmin.db")+"?_foreign_keys=on&_txlock=immediate&_busy_timeout=5000"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true, Logger: logger.Default.LogMode(logger.Silent)})
	must(t, err)
	pool, err := db.DB()
	must(t, err)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	must(t, database.Update(db, 281, nil))
	s, err := bookadmin.New(db, bookadmin.Options{})
	must(t, err)
	f := &fixture{db: db, store: s, admin: user(t, db, "admin", true), owner: user(t, db, "owner", false), other: user(t, db, "other", false)}
	f.book = model.AddressBookCollection{UserId: f.owner.User.ID, Name: "book"}
	f.foreign = model.AddressBookCollection{UserId: f.other.User.ID, Name: "foreign"}
	must(t, db.Create(&f.book).Error)
	must(t, db.Create(&f.foreign).Error)
	return f
}
func tag(t *testing.T, f *fixture, uid, cid uint, name string, color uint) model.Tag {
	t.Helper()
	r := model.Tag{UserId: uid, CollectionId: cid, Name: name, Color: color}
	must(t, f.db.Create(&r).Error)
	return r
}
func entry(t *testing.T, f *fixture, uid, cid uint, id, tags string) model.AddressBook {
	t.Helper()
	r := model.AddressBook{UserId: uid, CollectionId: cid, Id: id, Alias: "personal alias", Note: "preserve note", Password: "preserve secret", Tags: custom_types.AutoJson(tags)}
	must(t, f.db.Create(&r).Error)
	return r
}
func readTag(t *testing.T, f *fixture, id uint) model.Tag {
	t.Helper()
	var r model.Tag
	must(t, f.db.First(&r, id).Error)
	return r
}
func readEntry(t *testing.T, f *fixture, id uint) map[string]any {
	t.Helper()
	var r map[string]any
	must(t, f.db.Table("address_books").Where("row_id = ?", id).Take(&r).Error)
	return r
}
func snapshot(t *testing.T, f *fixture) map[string][]map[string]any {
	t.Helper()
	result := map[string][]map[string]any{}
	for _, table := range []string{"users", "user_tokens", "address_book_collections", "address_book_collection_rules", "tags", "address_books", "peers"} {
		var rows []map[string]any
		key := "id"
		if table == "address_books" || table == "peers" {
			key = "row_id"
		}
		must(t, f.db.Table(table).Order(key).Find(&rows).Error)
		result[table] = rows
	}
	return result
}
func noChange(t *testing.T, f *fixture, before map[string][]map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(before, snapshot(t, f)) {
		t.Fatal("rejected write changed persisted state")
	}
}
func expect(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v; want %v", err, want)
	}
}

func TestTagRenameDeleteDuplicateDefinitionsAndNarrowColor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := tag(t, f, f.owner.User.ID, f.book.Id, "old", 1)
	b := tag(t, f, f.owner.User.ID, f.book.Id, "old", 2)
	sentinel := tag(t, f, f.owner.User.ID, 0, "old", 3)
	other := tag(t, f, f.other.User.ID, f.foreign.Id, "old", 4)
	x := entry(t, f, f.owner.User.ID, f.book.Id, "device", `["new","old","keep","old","new"]`)
	untouched := entry(t, f, f.owner.User.ID, f.book.Id, "other", `["new","new","keep"]`)
	personal := entry(t, f, f.owner.User.ID, 0, "device", `["old"]`)
	foreign := entry(t, f, f.other.User.ID, f.foreign.Id, "device", `["old"]`)
	beforeX := readEntry(t, f, x.RowId)
	beforeUntouched := readEntry(t, f, untouched.RowId)
	beforePersonal := readEntry(t, f, personal.RowId)
	beforeForeign := readEntry(t, f, foreign.RowId)
	must(t, f.store.UpdateTag(ctx, f.owner, bookadmin.My, bookadmin.TagInput{ID: a.Id, Name: "new", Color: 9}))
	if readTag(t, f, a.Id).Name != "new" || readTag(t, f, b.Id).Name != "new" || readTag(t, f, a.Id).Color != 9 || readTag(t, f, b.Id).Color != 2 {
		t.Fatal("duplicate rename or narrow color differs")
	}
	after := readEntry(t, f, x.RowId)
	if after["tags"] != `["new","keep"]` {
		t.Fatal("linked tags not rewritten", after["tags"])
	}
	beforeX["tags"] = after["tags"]
	if !reflect.DeepEqual(beforeX, after) {
		t.Fatal("rename wrote entry metadata or time")
	}
	if !reflect.DeepEqual(beforeUntouched, readEntry(t, f, untouched.RowId)) || !reflect.DeepEqual(beforePersonal, readEntry(t, f, personal.RowId)) || !reflect.DeepEqual(beforeForeign, readEntry(t, f, foreign.RowId)) {
		t.Fatal("rename crossed scope or touched nonparticipant")
	}
	must(t, f.store.UpdateTag(ctx, f.admin, bookadmin.Admin, bookadmin.TagInput{ID: a.Id, Name: "new", Color: 0}))
	if readTag(t, f, b.Id).Color != 2 {
		t.Fatal("color affected duplicate id")
	}
	must(t, f.store.DeleteTag(ctx, f.owner, bookadmin.My, bookadmin.TagInput{ID: b.Id}))
	var count int64
	must(t, f.db.Model(&model.Tag{}).Where("id IN ?", []uint{a.Id, b.Id}).Count(&count).Error)
	if count != 0 {
		t.Fatal("same-name definitions survived delete")
	}
	if readEntry(t, f, x.RowId)["tags"] != `["keep"]` || readTag(t, f, sentinel.Id).Name != "old" || readTag(t, f, other.Id).Name != "old" {
		t.Fatal("delete crossed scope")
	}
}

func TestChildOwnerScopeAndActualParentBoundary(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	owned := tag(t, f, f.owner.User.ID, f.book.Id, "old", 1)
	foreign := tag(t, f, f.other.User.ID, f.foreign.Id, "foreign", 1)
	must(t, f.db.Create(&model.AddressBookCollectionRule{UserId: f.other.User.ID, CollectionId: f.foreign.Id, Type: 1, ToId: f.owner.User.ID, Rule: 3}).Error)
	for _, tc := range []struct {
		name string
		fn   func() error
		want error
	}{
		{"shared fullcontrol cannot update", func() error {
			return f.store.UpdateTag(ctx, f.owner, bookadmin.My, bookadmin.TagInput{ID: foreign.Id, Name: "forbidden"})
		}, auth.ErrForbidden},
		{"shared fullcontrol cannot delete", func() error { return f.store.DeleteTag(ctx, f.owner, bookadmin.My, bookadmin.TagInput{ID: foreign.Id}) }, auth.ErrForbidden},
		{"owner immutable", func() error {
			return f.store.UpdateTag(ctx, f.admin, bookadmin.Admin, bookadmin.TagInput{ID: owned.Id, UserID: f.other.User.ID, Name: "forbidden"})
		}, bookadmin.ErrInput},
		{"book immutable", func() error {
			return f.store.UpdateTag(ctx, f.owner, bookadmin.My, bookadmin.TagInput{ID: owned.Id, CollectionID: f.foreign.Id, Name: "forbidden"})
		}, bookadmin.ErrInput},
		{"parent actual owner mismatch", func() error {
			return f.store.CreateTag(ctx, f.admin, bookadmin.Admin, bookadmin.TagInput{UserID: f.owner.User.ID, CollectionID: f.foreign.Id, Name: "forbidden"})
		}, bookadmin.ErrInput},
		{"missing parent", func() error {
			return f.store.CreateTag(ctx, f.admin, bookadmin.Admin, bookadmin.TagInput{UserID: f.owner.User.ID, CollectionID: 9999, Name: "forbidden"})
		}, bookadmin.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) { before := snapshot(t, f); expect(t, tc.fn(), tc.want); noChange(t, f, before) })
	}
	must(t, f.store.CreateTag(ctx, f.admin, bookadmin.Admin, bookadmin.TagInput{CollectionID: f.foreign.Id, Name: "actual-owner"}))
	var got model.Tag
	must(t, f.db.Where("name = ?", "actual-owner").First(&got).Error)
	if got.UserId != f.other.User.ID {
		t.Fatal("admin did not derive parent owner")
	}
	must(t, f.store.UpdateTag(ctx, f.owner, bookadmin.My, bookadmin.TagInput{ID: owned.Id, UserID: f.owner.User.ID, CollectionID: f.book.Id, Name: "valid"}))
	must(t, f.db.Model(&model.User{}).Where("id = ?", f.other.User.ID).Update("status", 0).Error)
	must(t, f.store.CreateTag(ctx, f.admin, bookadmin.Admin, bookadmin.TagInput{CollectionID: f.foreign.Id, Name: "disabled-owner-allowed"}))
}

func TestTagStoredCorruptionAndTargetConflictRollBackEverything(t *testing.T) {
	for _, bad := range []string{`null`, `{}`, `[null]`, `[1]`, `["old",`, ``} {
		t.Run(fmt.Sprintf("corrupt-%q", bad), func(t *testing.T) {
			f := newFixture(t)
			old := tag(t, f, f.owner.User.ID, f.book.Id, "old", 1)
			entry(t, f, f.owner.User.ID, f.book.Id, "early", `["old"]`)
			broken := entry(t, f, f.owner.User.ID, f.book.Id, "late", `[]`)
			must(t, f.db.Model(&model.AddressBook{}).Where("row_id = ?", broken.RowId).UpdateColumn("tags", bad).Error)
			for _, remove := range []bool{false, true} {
				before := snapshot(t, f)
				var err error
				if remove {
					err = f.store.DeleteTag(context.Background(), f.owner, bookadmin.My, bookadmin.TagInput{ID: old.Id})
				} else {
					err = f.store.UpdateTag(context.Background(), f.owner, bookadmin.My, bookadmin.TagInput{ID: old.Id, Name: "new", Color: 9})
				}
				expect(t, err, bookadmin.ErrUnavailable)
				noChange(t, f, before)
			}
		})
	}
	f := newFixture(t)
	old := tag(t, f, f.owner.User.ID, f.book.Id, "old", 1)
	tag(t, f, f.owner.User.ID, f.book.Id, "new", 2)
	entry(t, f, f.owner.User.ID, f.book.Id, "device", `["old"]`)
	before := snapshot(t, f)
	expect(t, f.store.UpdateTag(context.Background(), f.owner, bookadmin.My, bookadmin.TagInput{ID: old.Id, Name: "new", Color: 9}), bookadmin.ErrInput)
	noChange(t, f, before)
}

func TestRulesImmutableScopeTargetChecksAndNarrowWrites(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := bookadmin.RuleInput{CollectionID: f.book.Id, ToID: f.other.User.ID, Type: 1, Rule: 2}
	must(t, f.store.CreateRule(ctx, f.owner, bookadmin.My, in))
	var row model.AddressBookCollectionRule
	must(t, f.db.Where("collection_id = ?", f.book.Id).First(&row).Error)
	in.ID = row.Id
	in.Rule = 3
	must(t, f.store.UpdateRule(ctx, f.owner, bookadmin.My, in))
	var updated model.AddressBookCollectionRule
	must(t, f.db.First(&updated, row.Id).Error)
	if updated.UserId != row.UserId || updated.CollectionId != row.CollectionId || updated.CreatedAt != row.CreatedAt || updated.Rule != 3 {
		t.Fatal("rule fields escaped whitelist")
	}
	for _, tc := range []struct {
		name  string
		input bookadmin.RuleInput
		want  error
	}{
		{"owner", bookadmin.RuleInput{ID: row.Id, UserID: f.other.User.ID, ToID: f.other.User.ID, Type: 1, Rule: 1}, bookadmin.ErrInput},
		{"collection", bookadmin.RuleInput{ID: row.Id, CollectionID: f.foreign.Id, ToID: f.other.User.ID, Type: 1, Rule: 1}, bookadmin.ErrInput},
		{"self", bookadmin.RuleInput{ID: row.Id, ToID: f.owner.User.ID, Type: 1, Rule: 1}, bookadmin.ErrInput},
		{"missing user", bookadmin.RuleInput{ID: row.Id, ToID: 9999, Type: 1, Rule: 1}, bookadmin.ErrInput},
		{"missing group", bookadmin.RuleInput{ID: row.Id, ToID: 9999, Type: 2, Rule: 1}, bookadmin.ErrInput},
		{"invalid rule", bookadmin.RuleInput{ID: row.Id, ToID: f.other.User.ID, Type: 1, Rule: 4}, bookadmin.ErrInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshot(t, f)
			expect(t, f.store.UpdateRule(ctx, f.admin, bookadmin.Admin, tc.input), tc.want)
			noChange(t, f, before)
		})
	}
	before := snapshot(t, f)
	in.ID = 0
	expect(t, f.store.CreateRule(ctx, f.owner, bookadmin.My, in), bookadmin.ErrInput)
	noChange(t, f, before)
	group := model.Group{Name: "target"}
	must(t, f.db.Create(&group).Error)
	must(t, f.store.UpdateRule(ctx, f.admin, bookadmin.Admin, bookadmin.RuleInput{ID: row.Id, ToID: group.Id, Type: 2, Rule: 1}))
	must(t, f.store.DeleteRule(ctx, f.owner, bookadmin.My, bookadmin.RuleInput{ID: row.Id}))
	var count int64
	must(t, f.db.Model(&model.AddressBookCollectionRule{}).Where("id = ?", row.Id).Count(&count).Error)
	if count != 0 {
		t.Fatal("rule not removed")
	}
}

func TestEveryChildMutationRechecksCurrentActorAndSession(t *testing.T) {
	for _, kind := range []string{"tag-create", "tag-update", "tag-delete", "rule-create", "rule-update", "rule-delete"} {
		for _, state := range []string{"revoked", "expired-db", "expired-jwt", "disabled", "demoted", "duplicate-token"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				f := newFixture(t)
				r := tag(t, f, f.owner.User.ID, f.book.Id, "old", 1)
				rule := model.AddressBookCollectionRule{UserId: f.owner.User.ID, CollectionId: f.book.Id, ToId: f.other.User.ID, Type: 1, Rule: 1}
				must(t, f.db.Create(&rule).Error)
				actor := f.admin
				want := auth.ErrUnauthenticated
				switch state {
				case "revoked":
					must(t, f.db.Delete(&model.UserToken{}, actor.Session.ID).Error)
				case "expired-db":
					must(t, f.db.Model(&model.UserToken{}).Where("id = ?", actor.Session.ID).Update("expired_at", time.Now().Add(-time.Second).Unix()).Error)
				case "expired-jwt":
					actor.Session.ExpiresAt = time.Now().Add(-time.Second)
				case "disabled":
					must(t, f.db.Model(&model.User{}).Where("id = ?", actor.User.ID).Update("status", 0).Error)
				case "demoted":
					must(t, f.db.Model(&model.User{}).Where("id = ?", actor.User.ID).Update("is_admin", false).Error)
					want = auth.ErrForbidden
				case "duplicate-token":
					must(t, f.db.Create(&model.UserToken{UserId: f.owner.User.ID, Token: actor.Session.Token, ExpiredAt: actor.Session.ExpiresAt.Unix()}).Error)
				}
				before := snapshot(t, f)
				var err error
				switch kind {
				case "tag-create":
					err = f.store.CreateTag(context.Background(), actor, bookadmin.Admin, bookadmin.TagInput{CollectionID: f.book.Id, Name: "new"})
				case "tag-update":
					err = f.store.UpdateTag(context.Background(), actor, bookadmin.Admin, bookadmin.TagInput{ID: r.Id, Name: "new"})
				case "tag-delete":
					err = f.store.DeleteTag(context.Background(), actor, bookadmin.Admin, bookadmin.TagInput{ID: r.Id})
				case "rule-create":
					err = f.store.CreateRule(context.Background(), actor, bookadmin.Admin, bookadmin.RuleInput{CollectionID: f.foreign.Id, ToID: f.owner.User.ID, Type: 1, Rule: 1})
				case "rule-update":
					err = f.store.UpdateRule(context.Background(), actor, bookadmin.Admin, bookadmin.RuleInput{ID: rule.Id, ToID: f.other.User.ID, Type: 1, Rule: 2})
				case "rule-delete":
					err = f.store.DeleteRule(context.Background(), actor, bookadmin.Admin, bookadmin.RuleInput{ID: rule.Id})
				}
				expect(t, err, want)
				noChange(t, f, before)
				// A valid current owner performs the same mutation category afterward.
				switch kind {
				case "tag-create":
					err = f.store.CreateTag(context.Background(), f.owner, bookadmin.My, bookadmin.TagInput{CollectionID: f.book.Id, Name: "new"})
				case "tag-update":
					err = f.store.UpdateTag(context.Background(), f.owner, bookadmin.My, bookadmin.TagInput{ID: r.Id, Name: "new"})
				case "tag-delete":
					err = f.store.DeleteTag(context.Background(), f.owner, bookadmin.My, bookadmin.TagInput{ID: r.Id})
				case "rule-create":
					err = f.store.CreateRule(context.Background(), f.other, bookadmin.My, bookadmin.RuleInput{CollectionID: f.foreign.Id, ToID: f.owner.User.ID, Type: 1, Rule: 1})
				case "rule-update":
					err = f.store.UpdateRule(context.Background(), f.owner, bookadmin.My, bookadmin.RuleInput{ID: rule.Id, ToID: f.other.User.ID, Type: 1, Rule: 2})
				case "rule-delete":
					err = f.store.DeleteRule(context.Background(), f.owner, bookadmin.My, bookadmin.RuleInput{ID: rule.Id})
				}
				must(t, err)
			})
		}
	}
}

type failedCommit struct {
	*sql.Tx
	reached *bool
}

func (c *failedCommit) Commit() error {
	*c.reached = true
	_ = c.Tx.Rollback()
	return errors.New("private injected commit failure")
}

type commitPool struct {
	gorm.ConnPool
	db      *sql.DB
	reached *bool
}

func (c commitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := c.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &failedCommit{Tx: tx, reached: c.reached}, nil
}
func TestChildSQLAndActualCommitFailureRollBack(t *testing.T) {
	for _, failure := range []string{"entry-update", "definition-update", "definition-delete", "commit"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			old := tag(t, f, f.owner.User.ID, f.book.Id, "old", 1)
			linked := entry(t, f, f.owner.User.ID, f.book.Id, "device", `["old"]`)
			entryBefore := readEntry(t, f, linked.RowId)
			before := snapshot(t, f)
			store := f.store
			originalPool := f.db.Statement.ConnPool
			reached := false
			switch failure {
			case "entry-update":
				must(t, f.db.Exec(`CREATE TRIGGER fail_entry BEFORE UPDATE ON address_books BEGIN SELECT RAISE(ABORT,'private failure'); END`).Error)
			case "definition-update":
				must(t, f.db.Exec(`CREATE TRIGGER fail_tag BEFORE UPDATE ON tags BEGIN SELECT RAISE(ABORT,'private failure'); END`).Error)
			case "definition-delete":
				must(t, f.db.Exec(`CREATE TRIGGER fail_tag BEFORE DELETE ON tags BEGIN SELECT RAISE(ABORT,'private failure'); END`).Error)
			case "commit":
				owned := f.db.Session(&gorm.Session{NewDB: true, Context: context.Background()})
				if owned.Statement == f.db.Statement {
					t.Fatal("commit injection reused the fixture statement")
				}
				pool, err := f.db.DB()
				must(t, err)
				owned.Statement.ConnPool = commitPool{ConnPool: f.db.Statement.ConnPool, db: pool, reached: &reached}
				store, err = bookadmin.New(owned, bookadmin.Options{})
				must(t, err)
			}
			var err error
			if failure == "definition-delete" {
				err = store.DeleteTag(context.Background(), f.owner, bookadmin.My, bookadmin.TagInput{ID: old.Id})
			} else {
				err = store.UpdateTag(context.Background(), f.owner, bookadmin.My, bookadmin.TagInput{ID: old.Id, Name: "new", Color: 9})
			}
			expect(t, err, bookadmin.ErrUnavailable)
			if failure == "commit" && !reached {
				t.Fatal("actual final commit was not reached")
			}
			noChange(t, f, before)
			if f.db.Statement.ConnPool != originalPool {
				t.Fatal("commit injection replaced the original fixture connection pool")
			}
			switch failure {
			case "entry-update":
				must(t, f.db.Exec("DROP TRIGGER fail_entry").Error)
			case "definition-update", "definition-delete":
				must(t, f.db.Exec("DROP TRIGGER fail_tag").Error)
			}
			// Retry through the original store after removing the SQL fault or
			// discarding the isolated commit wrapper, then read its real effects.
			if failure == "definition-delete" {
				must(t, f.store.DeleteTag(context.Background(), f.owner, bookadmin.My, bookadmin.TagInput{ID: old.Id}))
				var remaining int64
				must(t, f.db.Model(&model.Tag{}).Where("id = ?", old.Id).Count(&remaining).Error)
				if remaining != 0 {
					t.Fatal("delete retry retained the definition")
				}
				entryBefore["tags"] = `[]`
			} else {
				must(t, f.store.UpdateTag(context.Background(), f.owner, bookadmin.My, bookadmin.TagInput{ID: old.Id, Name: "new", Color: 9}))
				row := readTag(t, f, old.Id)
				if row.Name != "new" || row.Color != 9 {
					t.Fatal("update retry did not persist the definition")
				}
				entryBefore["tags"] = `["new"]`
			}
			if !reflect.DeepEqual(entryBefore, readEntry(t, f, linked.RowId)) {
				t.Fatal("retry failed to rewrite linked tags or changed other entry fields")
			}
		})
	}
}

func TestHistoricalZeroOwnerScopesAndPersonalRuleCleanup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	parent := model.AddressBookCollection{UserId: 0, Name: "historical"}
	must(t, f.db.Create(&parent).Error)
	for _, cid := range []uint{0, parent.Id} {
		old := tag(t, f, 0, cid, "old", 1)
		dup := tag(t, f, 0, cid, "old", 2)
		a := entry(t, f, 0, cid, fmt.Sprintf("old-%d", cid), `["old"]`)
		before := snapshot(t, f)
		expect(t, f.store.UpdateTag(ctx, f.owner, bookadmin.My, bookadmin.TagInput{ID: old.Id, Name: "new"}), auth.ErrForbidden)
		noChange(t, f, before)
		must(t, f.store.UpdateTag(ctx, f.admin, bookadmin.Admin, bookadmin.TagInput{ID: dup.Id, Name: "new", Color: 8}))
		if readTag(t, f, old.Id).Name != "new" || readEntry(t, f, a.RowId)["tags"] != `["new"]` {
			t.Fatal("historical admin maintenance failed")
		}
		before = snapshot(t, f)
		expect(t, f.store.CreateTag(ctx, f.admin, bookadmin.Admin, bookadmin.TagInput{CollectionID: cid, Name: "not-new-owner-zero"}), bookadmin.ErrInput)
		noChange(t, f, before)
		must(t, f.store.DeleteTag(ctx, f.admin, bookadmin.Admin, bookadmin.TagInput{ID: old.Id}))
	}
	bad := model.AddressBookCollectionRule{UserId: f.owner.User.ID, CollectionId: 0, ToId: f.other.User.ID, Type: 1, Rule: 1}
	must(t, f.db.Create(&bad).Error)
	before := snapshot(t, f)
	expect(t, f.store.UpdateRule(ctx, f.owner, bookadmin.My, bookadmin.RuleInput{ID: bad.Id, ToID: f.other.User.ID, Type: 1, Rule: 2}), bookadmin.ErrInput)
	noChange(t, f, before)
	must(t, f.store.DeleteRule(ctx, f.owner, bookadmin.My, bookadmin.RuleInput{ID: bad.Id}))
	// Existing create compatibility permits duplicates; no new uniqueness rule.
	for i := 0; i < 2; i++ {
		must(t, f.store.CreateTag(ctx, f.owner, bookadmin.My, bookadmin.TagInput{CollectionID: f.book.Id, Name: "duplicate"}))
	}
	var duplicates int64
	must(t, f.db.Model(&model.Tag{}).Where("user_id = ? AND collection_id = ? AND name = ?", f.owner.User.ID, f.book.Id, "duplicate").Count(&duplicates).Error)
	if duplicates != 2 {
		t.Fatal("legacy duplicate creation did not persist both rows")
	}
}

func TestTagNamesCompareExactlyAndNullableStoredTagsFailClosed(t *testing.T) {
	t.Run("exact-names", func(t *testing.T) {
		f := newFixture(t)
		a := tag(t, f, f.owner.User.ID, f.book.Id, "A", 1)
		lower := tag(t, f, f.owner.User.ID, f.book.Id, "a", 2)
		space := tag(t, f, f.owner.User.ID, f.book.Id, "A ", 3)
		tag(t, f, f.owner.User.ID, f.book.Id, "b", 4)
		x := entry(t, f, f.owner.User.ID, f.book.Id, "device", `["A","a","A "]`)
		must(t, f.store.UpdateTag(context.Background(), f.owner, bookadmin.My, bookadmin.TagInput{ID: a.Id, Name: "B"}))
		if readTag(t, f, lower.Id).Name != "a" || readTag(t, f, space.Id).Name != "A " || readEntry(t, f, x.RowId)["tags"] != `["B","a","A "]` {
			t.Fatal("tag name folded case/space")
		}
	})
	t.Run("nullable-historical-schema", func(t *testing.T) {
		// A minimal historical nullable tags table exercises SQL NULL before
		// AutoJson can normalize it; this is not a migration acceptance test.
		db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "nullable.db")+"?_txlock=immediate"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true, Logger: logger.Default.LogMode(logger.Silent)})
		must(t, err)
		pool, err := db.DB()
		must(t, err)
		pool.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = pool.Close() })
		must(t, db.AutoMigrate(&model.User{}, &model.UserToken{}, &model.AddressBookCollection{}, &model.Tag{}))
		must(t, db.Exec("CREATE TABLE address_books (row_id INTEGER PRIMARY KEY,user_id INTEGER,collection_id INTEGER,tags TEXT)").Error)
		owner := user(t, db, "owner", false)
		parent := model.AddressBookCollection{UserId: owner.User.ID, Name: "book"}
		must(t, db.Create(&parent).Error)
		old := model.Tag{UserId: owner.User.ID, CollectionId: parent.Id, Name: "old"}
		must(t, db.Create(&old).Error)
		must(t, db.Exec("INSERT INTO address_books (row_id,user_id,collection_id,tags) VALUES (1,?,?,NULL)", owner.User.ID, parent.Id).Error)
		store, err := bookadmin.New(db, bookadmin.Options{})
		must(t, err)
		expect(t, store.UpdateTag(context.Background(), owner, bookadmin.My, bookadmin.TagInput{ID: old.Id, Name: "new"}), bookadmin.ErrUnavailable)
		var after model.Tag
		must(t, db.First(&after, old.Id).Error)
		if after.Name != "old" {
			t.Fatal("SQL NULL retained definition rename")
		}
		var raw sql.NullString
		must(t, db.Raw("SELECT tags FROM address_books WHERE row_id=1").Row().Scan(&raw))
		if raw.Valid {
			t.Fatal("SQL NULL was silently normalized")
		}
	})
}

func TestCandidateScopeDriftAndExpiryAfterLockedReadsReject(t *testing.T) {
	t.Run("scope-drift", func(t *testing.T) {
		f := newFixture(t)
		old := tag(t, f, f.owner.User.ID, f.book.Id, "old", 1)
		fired := false
		must(t, f.db.Callback().Query().After("gorm:query").Register("bookadmin:drift", func(tx *gorm.DB) {
			if !fired && tx.Statement.Table == "tags" && len(tx.Statement.Selects) == 4 && tx.Error == nil {
				fired = true
				tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE tags SET user_id=?,collection_id=? WHERE id=?", f.other.User.ID, f.foreign.Id, old.Id).Error)
			}
		}))
		err := f.store.UpdateTag(context.Background(), f.owner, bookadmin.My, bookadmin.TagInput{ID: old.Id, Name: "must-not-write"})
		if err == nil || !fired {
			t.Fatal("scope drift injection not rejected", err, fired)
		}
		must(t, f.db.Callback().Query().Remove("bookadmin:drift"))
		if readTag(t, f, old.Id).Name != "old" {
			t.Fatal("drift accepted name write")
		}
	})
	t.Run("jwt-expiry-after-child-read", func(t *testing.T) {
		f := newFixture(t)
		old := tag(t, f, f.owner.User.ID, f.book.Id, "old", 1)
		entry(t, f, f.owner.User.ID, f.book.Id, "device", `["old"]`)
		now := time.Now().UTC()
		actor := f.owner
		actor.Session.ExpiresAt = now.Add(time.Second)
		fired := false
		store, err := bookadmin.New(f.db, bookadmin.Options{Now: func() time.Time { return now }})
		must(t, err)
		must(t, f.db.Callback().Query().After("gorm:query").Register("bookadmin:expiry", func(tx *gorm.DB) {
			_, typed := tx.Statement.Dest.(*[]model.Tag)
			if !fired && typed && tx.Statement.Table == "tags" && len(tx.Statement.Selects) == 0 && tx.Error == nil {
				fired = true
				now = actor.Session.ExpiresAt.Add(time.Second)
			}
		}))
		before := snapshot(t, f)
		expect(t, store.UpdateTag(context.Background(), actor, bookadmin.My, bookadmin.TagInput{ID: old.Id, Name: "new"}), auth.ErrUnauthenticated)
		if !fired {
			t.Fatal("expiry hook not reached")
		}
		noChange(t, f, before)
	})
}

func TestBookAdminCancellationAndIndependentInstance(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := snapshot(t, f)
	expect(t, f.store.CreateTag(ctx, f.owner, bookadmin.My, bookadmin.TagInput{CollectionID: f.book.Id, Name: "cancelled"}), context.Canceled)
	noChange(t, f, before)
	// Equal row IDs in an independent DB cannot redirect the owned Store.
	otherBefore := snapshot(t, other)
	must(t, f.store.CreateTag(context.Background(), f.owner, bookadmin.My, bookadmin.TagInput{CollectionID: f.book.Id, Name: "only-first"}))
	noChange(t, other, otherBefore)
}

func TestMissingChildrenKeepNotFoundAfterFreshAuthentication(t *testing.T) {
	f := newFixture(t)
	operations := []func(auth.Principal) error{
		func(p auth.Principal) error {
			return f.store.UpdateTag(context.Background(), p, bookadmin.My, bookadmin.TagInput{ID: 99999, Name: "new"})
		},
		func(p auth.Principal) error {
			return f.store.DeleteTag(context.Background(), p, bookadmin.My, bookadmin.TagInput{ID: 99999})
		},
		func(p auth.Principal) error {
			return f.store.UpdateRule(context.Background(), p, bookadmin.My, bookadmin.RuleInput{ID: 99999, ToID: f.other.User.ID, Type: 1, Rule: 1})
		},
		func(p auth.Principal) error {
			return f.store.DeleteRule(context.Background(), p, bookadmin.My, bookadmin.RuleInput{ID: 99999})
		},
	}
	for _, operation := range operations {
		before := snapshot(t, f)
		expect(t, operation(f.owner), bookadmin.ErrNotFound)
		noChange(t, f, before)
	}
	must(t, f.db.Delete(&model.UserToken{}, f.owner.Session.ID).Error)
	for _, operation := range operations {
		before := snapshot(t, f)
		expect(t, operation(f.owner), auth.ErrUnauthenticated)
		noChange(t, f, before)
	}
}
