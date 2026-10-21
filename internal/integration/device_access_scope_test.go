package integration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

type dev04Fixture struct {
	actor, sameA, sameB, disabled, foreign model.User
	shared, other, private                 model.Group
	dgA, dgB, dgC, collision, unused       model.DeviceGroup
}

func dev04Setup(t *testing.T, f *fixture) dev04Fixture {
	t.Helper()
	if err := database.Update(f.DB, 281, nil); err != nil {
		t.Fatal(err)
	}
	d := dev04Fixture{
		shared:    model.Group{IdModel: model.IdModel{Id: 80}, Name: "scope-share", Type: model.GroupTypeShare},
		other:     model.Group{IdModel: model.IdModel{Id: 81}, Name: "scope-other", Type: model.GroupTypeShare},
		private:   model.Group{IdModel: model.IdModel{Id: 82}, Name: "scope-default", Type: model.GroupTypeDefault},
		dgA:       model.DeviceGroup{IdModel: model.IdModel{Id: 91}, Name: "scope-visible-one"},
		dgB:       model.DeviceGroup{IdModel: model.IdModel{Id: 92}, Name: "scope-duplicate-name"},
		dgC:       model.DeviceGroup{IdModel: model.IdModel{Id: 93}, Name: "scope-duplicate-name"},
		collision: model.DeviceGroup{IdModel: model.IdModel{Id: 80}, Name: "same-number-is-not-a-grant"},
		unused:    model.DeviceGroup{IdModel: model.IdModel{Id: 94}, Name: "scope-unused"},
	}
	for _, row := range []any{&d.shared, &d.other, &d.private, &d.dgA, &d.dgB, &d.dgC, &d.collision, &d.unused} {
		transactionCreate(t, f.DB, row)
	}
	d.actor = model.User{Username: "scope-actor", GroupId: d.shared.Id, Status: model.COMMON_STATUS_ENABLE}
	d.sameA = model.User{Username: "scope-same-a", GroupId: d.shared.Id, Status: model.COMMON_STATUS_ENABLE}
	d.sameB = model.User{Username: "scope-same-b", GroupId: d.shared.Id, Status: model.COMMON_STATUS_ENABLE}
	d.disabled = model.User{Username: "scope-disabled", GroupId: d.shared.Id, Status: 2}
	d.foreign = model.User{Username: "scope-foreign", GroupId: d.other.Id, Status: model.COMMON_STATUS_ENABLE}
	for _, u := range []*model.User{&d.actor, &d.sameA, &d.sameB, &d.disabled, &d.foreign} {
		transactionCreate(t, f.DB, u)
	}
	for _, p := range []model.Peer{
		{Id: "scope-own-online", UserId: d.actor.Id, GroupId: d.dgA.Id, LastOnlineTime: 9999999999, Note: "own-note"},
		{Id: "scope-own-offline", UserId: d.actor.Id, GroupId: d.dgB.Id, LastOnlineTime: 1},
		{Id: "scope-shared-a", UserId: d.sameA.Id, GroupId: d.dgA.Id},
		{Id: "scope-shared-no-group", UserId: d.sameA.Id, GroupId: 0},
		{Id: "scope-shared-missing-group", UserId: d.sameB.Id, GroupId: 99991},
		{Id: "scope-shared-c", UserId: d.sameB.Id, GroupId: d.dgC.Id},
		{Id: "scope-disabled-peer", UserId: d.disabled.Id, GroupId: d.dgA.Id},
		{Id: "scope-foreign-collision", UserId: d.foreign.Id, GroupId: d.collision.Id},
		{Id: "scope-owner-zero", UserId: 0, GroupId: d.dgA.Id},
		{Id: "scope-owner-missing", UserId: 99999, GroupId: d.dgA.Id},
	} {
		p := p
		transactionCreate(t, f.DB, &p)
	}
	book := model.AddressBookCollection{UserId: d.foreign.Id, Name: "scope-foreign-shared-book"}
	transactionCreate(t, f.DB, &book)
	for _, rule := range []model.AddressBookCollectionRule{
		{UserId: d.foreign.Id, CollectionId: book.Id, Type: model.ShareAddressBookRuleTypePersonal, ToId: d.actor.Id, Rule: 3},
		{UserId: d.foreign.Id, CollectionId: book.Id, Type: model.ShareAddressBookRuleTypePersonal, ToId: d.actor.Id, Rule: 1},
		{UserId: d.foreign.Id, CollectionId: book.Id, Type: model.ShareAddressBookRuleTypeGroup, ToId: d.shared.Id, Rule: 3},
	} {
		rule := rule
		transactionCreate(t, f.DB, &rule)
	}
	transactionCreate(t, f.DB, &model.AddressBook{Id: "scope-foreign-collision", UserId: d.foreign.Id, CollectionId: book.Id, Note: "book-note", Tags: custom_types.AutoJson(`[]`)})
	return d
}

type dev04ReadState struct {
	Users        []model.User
	Peers        []model.Peer
	Groups       []model.Group
	DeviceGroups []model.DeviceGroup
	Books        []model.AddressBook
	Collections  []model.AddressBookCollection
	Rules        []model.AddressBookCollectionRule
	Tokens       []model.UserToken
}

func dev04State(t *testing.T, db *gorm.DB) dev04ReadState {
	t.Helper()
	var s dev04ReadState
	for _, item := range []struct {
		dest  any
		order string
	}{
		{&s.Users, "id"}, {&s.Peers, "row_id"}, {&s.Groups, "id"}, {&s.DeviceGroups, "id"},
		{&s.Books, "row_id"}, {&s.Collections, "id"}, {&s.Rules, "id"}, {&s.Tokens, "id"},
	} {
		if err := db.Order(item.order).Find(item.dest).Error; err != nil {
			t.Fatal(err)
		}
	}
	return s
}

type dev04PageRead func(clientcompat.Page) (int64, []string, error)

func dev04Readers(s *clientcompat.Store, actor clientcompat.Actor, enabled bool, ctx context.Context) map[string]dev04PageRead {
	return map[string]dev04PageRead{
		"users": func(p clientcompat.Page) (int64, []string, error) {
			n, rows, e := s.Users(ctx, actor, p, enabled)
			out := []string{}
			for _, r := range rows {
				out = append(out, r.Name)
			}
			return n, out, e
		},
		"peers": func(p clientcompat.Page) (int64, []string, error) {
			n, rows, e := s.GroupPeers(ctx, actor, p, enabled)
			out := []string{}
			for _, r := range rows {
				out = append(out, r.ID)
			}
			return n, out, e
		},
		"device_groups": func(p clientcompat.Page) (int64, []string, error) {
			n, rows, e := s.DeviceGroups(ctx, actor, p)
			out := []string{}
			for _, r := range rows {
				out = append(out, r.Name)
			}
			return n, out, e
		},
	}
}

func dev04Pages(t *testing.T, read dev04PageRead, want []string) {
	t.Helper()
	got := []string{}
	// Size two crosses a page boundary for both visible users and device groups.
	for current := 1; current <= len(want)/2+2; current++ {
		n, rows, err := read(clientcompat.Page{Current: current, Size: 2})
		if err != nil || n != int64(len(want)) {
			t.Fatalf("page %d total=%d want=%d err=%v", current, n, len(want), err)
		}
		remaining := len(want) - (current-1)*2
		if remaining < 0 {
			remaining = 0
		}
		if remaining > 2 {
			remaining = 2
		}
		if len(rows) != remaining {
			t.Fatalf("page %d length=%d want=%d", current, len(rows), remaining)
		}
		got = append(got, rows...)
	}
	// Compare multiplicities, not a set: duplicate peer joins and collapsed
	// same-name device-group definitions must both be detected.
	expected := append([]string{}, want...)
	sort.Strings(expected)
	sort.Strings(got)
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("visible rows=%v want=%v", got, expected)
	}
}

func TestDeviceAccessScopeRealAuthorizationBeforePagination(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		d := dev04Setup(t, f)
		before := dev04State(t, f.DB)
		s, err := clientcompat.New(f.Reopen(t))
		if err != nil {
			t.Fatal(err)
		}
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("ordinary_enabled_only_%t", enabled), func(t *testing.T) {
				want := map[string][]string{
					"users":         {d.actor.Username, d.sameA.Username, d.sameB.Username},
					"peers":         {"scope-own-online", "scope-own-offline", "scope-shared-a", "scope-shared-no-group", "scope-shared-missing-group", "scope-shared-c"},
					"device_groups": {d.dgA.Name, d.dgB.Name, d.dgC.Name},
				}
				for name, read := range dev04Readers(s, clientcompat.Actor{ID: d.actor.Id}, enabled, context.Background()) {
					t.Run(name, func(t *testing.T) { dev04Pages(t, read, want[name]) })
				}
			})
		}
		_, peers, err := s.GroupPeers(context.Background(), clientcompat.Actor{ID: d.actor.Id}, clientcompat.Page{Current: 1, Size: 100}, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range peers {
			if strings.HasSuffix(p.ID, "no-group") || strings.HasSuffix(p.ID, "missing-group") {
				if p.DeviceGroupName != "" {
					t.Fatal("missing group invented a name")
				}
			}
			if p.ID == "scope-own-online" && p.Note != "own-note" {
				t.Fatal("native peer note lost")
			}
		}
		if after := dev04State(t, f.Reopen(t)); !reflect.DeepEqual(before, after) {
			t.Fatal("directory reads changed persisted state")
		}
	})
}

func TestDeviceAccessScopeRealCurrentRoleGroupAndDisabledState(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		d := dev04Setup(t, f)
		s, err := clientcompat.New(f.Reopen(t))
		if err != nil {
			t.Fatal(err)
		}
		who := clientcompat.Actor{ID: d.actor.Id}
		for _, gid := range []uint{0, 99998, d.private.Id} {
			if err := f.DB.Model(&model.User{}).Where("id = ?", d.actor.Id).Update("group_id", gid).Error; err != nil {
				t.Fatal(err)
			}
			for name, read := range dev04Readers(s, who, false, context.Background()) {
				want := map[string][]string{"users": {d.actor.Username}, "peers": {"scope-own-online", "scope-own-offline"}, "device_groups": {d.dgA.Name, d.dgB.Name}}
				t.Run(fmt.Sprintf("group_%d_%s", gid, name), func(t *testing.T) { dev04Pages(t, read, want[name]) })
			}
		}
		// Reuse the exact Store and Actor across changes; never trust cached role.
		if err := f.DB.Model(&model.User{}).Where("id = ?", d.actor.Id).Updates(map[string]any{"is_admin": true, "group_id": d.private.Id}).Error; err != nil {
			t.Fatal(err)
		}
		for _, enabled := range []bool{false, true} {
			state := dev04State(t, f.Reopen(t))
			want := map[string][]string{"users": {}, "peers": {}, "device_groups": {}}
			owners := map[uint]model.User{}
			for _, u := range state.Users {
				owners[u.Id] = u
				if !enabled || u.Status == model.COMMON_STATUS_ENABLE {
					want["users"] = append(want["users"], u.Username)
				}
			}
			for _, p := range state.Peers {
				if u, ok := owners[p.UserId]; ok && p.UserId != 0 && (!enabled || u.Status == model.COMMON_STATUS_ENABLE) {
					want["peers"] = append(want["peers"], p.Id)
				}
			}
			for _, g := range state.DeviceGroups {
				want["device_groups"] = append(want["device_groups"], g.Name)
			}
			for name, read := range dev04Readers(s, who, enabled, context.Background()) {
				t.Run(fmt.Sprintf("admin_%t_%s", enabled, name), func(t *testing.T) { dev04Pages(t, read, want[name]) })
			}
		}
		if err := f.DB.Model(&model.User{}).Where("id = ?", d.actor.Id).Update("is_admin", false).Error; err != nil {
			t.Fatal(err)
		}
		dev04Pages(t, dev04Readers(s, who, false, context.Background())["users"], []string{d.actor.Username})
		if err := f.DB.Model(&model.User{}).Where("id = ?", d.actor.Id).Update("status", 2).Error; err != nil {
			t.Fatal(err)
		}
		before := dev04State(t, f.Reopen(t))
		for name, read := range dev04Readers(s, who, false, context.Background()) {
			_, rows, err := read(clientcompat.Page{Current: 1, Size: 100})
			if !errors.Is(err, clientcompat.ErrForbidden) || len(rows) != 0 {
				t.Fatalf("%s reused disabled actor: rows=%v err=%v", name, rows, err)
			}
		}
		if after := dev04State(t, f.Reopen(t)); !reflect.DeepEqual(before, after) {
			t.Fatal("disabled actor rejection wrote state")
		}
	})
}

func TestDeviceAccessScopeRealDependencyErrorAndCancellationAreReadOnly(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		d := dev04Setup(t, f)
		who := clientcompat.Actor{ID: d.actor.Id}
		before := dev04State(t, f.Reopen(t))
		// Missing real native table produces a genuine server-side SELECT error,
		// independently of callback-based cancellation tests below.
		quote := func(name string) string {
			if f.Engine == "mysql" {
				return "`" + name + "`"
			}
			return `"` + name + `"`
		}
		for _, table := range []string{"groups", "peers", "device_groups"} {
			transactionExec(t, f.DB, "ALTER TABLE "+quote(table)+" RENAME TO "+quote("dev04_hidden_"+table))
			db := f.Reopen(t)
			s, err := clientcompat.New(db)
			if err != nil {
				t.Fatal(err)
			}
			var reads = dev04Readers(s, who, false, context.Background())
			var names []string
			switch table {
			case "groups":
				names = []string{"users", "peers", "device_groups"}
			case "peers":
				names = []string{"peers", "device_groups"}
			case "device_groups":
				names = []string{"peers", "device_groups"}
			}
			for _, name := range names {
				_, rows, e := reads[name](clientcompat.Page{Current: 1, Size: 2})
				if !errors.Is(e, clientcompat.ErrUnavailable) || len(rows) != 0 {
					t.Errorf("native missing %s via %s: rows=%v err=%v", table, name, rows, e)
				}
			}
			transactionExec(t, f.DB, "ALTER TABLE "+quote("dev04_hidden_"+table)+" RENAME TO "+quote(table))
		}
		for _, name := range []string{"users", "peers", "device_groups"} {
			t.Run(name, func(t *testing.T) {
				db := f.Reopen(t)
				var writes atomic.Int32
				for _, register := range []func(string, func(*gorm.DB)) error{
					func(n string, cb func(*gorm.DB)) error {
						return db.Callback().Create().Before("gorm:create").Register(n, cb)
					},
					func(n string, cb func(*gorm.DB)) error {
						return db.Callback().Update().Before("gorm:update").Register(n, cb)
					},
					func(n string, cb func(*gorm.DB)) error {
						return db.Callback().Delete().Before("gorm:delete").Register(n, cb)
					},
				} {
					if err := register("dev04:forbid-write", func(*gorm.DB) { writes.Add(1) }); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var once sync.Once
				var actualActorSelect atomic.Bool
				if err := db.Callback().Query().After("gorm:query").Register("dev04:cancel-after-real-select", func(tx *gorm.DB) {
					if tx.Error == nil && tx.Statement.Table == "users" && tx.RowsAffected > 0 {
						once.Do(func() { actualActorSelect.Store(true); cancel() })
					}
				}); err != nil {
					t.Fatal(err)
				}
				s, err := clientcompat.New(db)
				if err != nil {
					t.Fatal(err)
				}
				_, rows, err := dev04Readers(s, who, false, ctx)[name](clientcompat.Page{Current: 1, Size: 2})
				if !actualActorSelect.Load() || !errors.Is(err, context.Canceled) || len(rows) != 0 {
					t.Fatalf("canceled after actor SELECT: actual=%t rows=%v err=%v", actualActorSelect.Load(), rows, err)
				}
				if writes.Load() != 0 {
					t.Fatal("read path attempted a write")
				}
				pre, cancelPre := context.WithCancel(context.Background())
				cancelPre()
				_, rows, err = dev04Readers(s, who, false, pre)[name](clientcompat.Page{Current: 1, Size: 2})
				if !errors.Is(err, context.Canceled) || len(rows) != 0 {
					t.Fatal("pre-canceled read succeeded")
				}
			})
		}
		if after := dev04State(t, f.Reopen(t)); !reflect.DeepEqual(before, after) {
			t.Fatal("query errors/cancellation changed directory or auth facts")
		}
	})
}
