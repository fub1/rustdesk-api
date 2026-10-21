package clientcompat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type accessFixture struct {
	db    *gorm.DB
	store *clientcompat.Store
}

func accessMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func newAccessFixture(t *testing.T) accessFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "access.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	accessMust(t, err)
	pool, err := db.DB()
	accessMust(t, err)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	accessMust(t, database.Update(db, 281, nil))
	store, err := clientcompat.New(db)
	accessMust(t, err)
	return accessFixture{db, store}
}
func (f accessFixture) user(t *testing.T, id, group uint, enabled, admin bool) {
	t.Helper()
	status := model.COMMON_STATUS_DISABLED
	if enabled {
		status = model.COMMON_STATUS_ENABLE
	}
	accessMust(t, f.db.Create(&model.User{IdModel: model.IdModel{Id: id}, Username: fmt.Sprintf("u%03d", id), GroupId: group, Status: status, IsAdmin: &admin, Password: "secret-password", Remark: "secret-remark"}).Error)
}
func (f accessFixture) peer(t *testing.T, id string, owner, group uint) {
	t.Helper()
	accessMust(t, f.db.Create(&model.Peer{Id: id, UserId: owner, GroupId: group, Uuid: "private-uuid", Hostname: "host", Username: "local", Os: "Linux", Note: "peer-note", LastOnlineTime: 123, LastOnlineIp: "private-ip"}).Error)
}
func accessNames[T any](rows []T, name func(T) string) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, name(r))
	}
	return out
}
func accessEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestDeviceAccessScopeApprovedOrdinaryAdminAndProjection(t *testing.T) {
	f := newAccessFixture(t)
	accessMust(t, f.db.Create(&[]model.Group{{IdModel: model.IdModel{Id: 1}, Name: "share", Type: model.GroupTypeShare}, {IdModel: model.IdModel{Id: 2}, Name: "private", Type: model.GroupTypeDefault}, {IdModel: model.IdModel{Id: 3}, Name: "other", Type: model.GroupTypeShare}}).Error)
	for _, u := range []struct {
		id, g          uint
		enabled, admin bool
	}{{1, 3, true, false}, {10, 1, true, false}, {11, 1, true, false}, {12, 1, false, false}, {13, 2, true, false}, {20, 3, true, true}} {
		f.user(t, u.id, u.g, u.enabled, u.admin)
	}
	// Even a malformed persisted user id=0 cannot supply ownership to owner0 peers.
	accessMust(t, f.db.Exec("INSERT INTO users(id,username,group_id,status) VALUES(0,'dirty-zero',1,1)").Error)
	accessMust(t, f.db.Create(&[]model.DeviceGroup{{IdModel: model.IdModel{Id: 1}, Name: "invisible"}, {IdModel: model.IdModel{Id: 2}, Name: "same"}, {IdModel: model.IdModel{Id: 3}, Name: "same"}, {IdModel: model.IdModel{Id: 4}, Name: "unused"}}).Error)
	for _, p := range []struct {
		id       string
		owner, g uint
	}{{"hidden", 1, 1}, {"actor", 10, 2}, {"sibling", 11, 3}, {"duplicate-group", 11, 2}, {"missing-group", 11, 999}, {"no-group", 11, 0}, {"disabled", 12, 1}, {"private", 13, 1}, {"admin", 20, 1}, {"owner-zero", 0, 2}, {"owner-missing", 999, 2}} {
		f.peer(t, p.id, p.owner, p.g)
	}
	accessMust(t, f.db.Model(&model.Peer{}).Where("id = ?", "actor").Update("last_online_time", 0).Error)
	// Address-book full-control grants remain independent from MyGroup visibility.
	collection := model.AddressBookCollection{UserId: 1, Name: "granted"}
	accessMust(t, f.db.Create(&collection).Error)
	accessMust(t, f.db.Create(&[]model.AddressBookCollectionRule{{UserId: 1, CollectionId: collection.Id, Type: 1, ToId: 10, Rule: 3}, {UserId: 1, CollectionId: collection.Id, Type: 2, ToId: 1, Rule: 3}}).Error)
	ctx := context.Background()
	page := clientcompat.Page{Current: 1, Size: 100}
	for _, enabled := range []bool{false, true} {
		total, rows, err := f.store.Users(ctx, clientcompat.Actor{ID: 10}, page, enabled)
		accessMust(t, err)
		accessEqual(t, total, int64(2))
		accessEqual(t, accessNames(rows, func(u clientcompat.User) string { return u.Name }), []string{"u010", "u011"})
		total, peers, err := f.store.GroupPeers(ctx, clientcompat.Actor{ID: 10}, page, enabled)
		accessMust(t, err)
		accessEqual(t, total, int64(5))
		accessEqual(t, accessNames(peers, func(p clientcompat.GroupPeer) string { return p.ID }), []string{"actor", "sibling", "duplicate-group", "missing-group", "no-group"})
		accessEqual(t, peers[3].DeviceGroupName, "")
		accessEqual(t, peers[4].DeviceGroupName, "")
		for _, p := range peers {
			accessEqual(t, p.Status, 0)
			accessEqual(t, p.Note, "peer-note")
		}
	}
	total, groups, err := f.store.DeviceGroups(ctx, clientcompat.Actor{ID: 10}, page)
	accessMust(t, err)
	accessEqual(t, total, int64(2))
	accessEqual(t, groups, []clientcompat.DeviceGroup{{Name: "same"}, {Name: "same"}})
	// Exact physical definitions are paginated once, even if multiple visible peers reference one.
	total, groups, err = f.store.DeviceGroups(ctx, clientcompat.Actor{ID: 10}, clientcompat.Page{Current: 2, Size: 1})
	accessMust(t, err)
	accessEqual(t, total, int64(2))
	accessEqual(t, len(groups), 1)
	for _, enabled := range []bool{false, true} {
		wantUsers, wantPeers := int64(6), int64(9)
		if enabled {
			wantUsers--
			wantPeers--
		}
		n, _, err := f.store.Users(ctx, clientcompat.Actor{ID: 20}, page, enabled)
		accessMust(t, err)
		accessEqual(t, n, wantUsers)
		n, _, err = f.store.GroupPeers(ctx, clientcompat.Actor{ID: 20}, page, enabled)
		accessMust(t, err)
		accessEqual(t, n, wantPeers)
	}
	total, groups, err = f.store.DeviceGroups(ctx, clientcompat.Actor{ID: 20}, page)
	accessMust(t, err)
	accessEqual(t, total, int64(4))
	accessEqual(t, len(groups), 4)
	// Projection remains the established native DTO; private model fields never enter it.
	_, users, err := f.store.Users(ctx, clientcompat.Actor{ID: 10}, page, false)
	accessMust(t, err)
	_, peers, err := f.store.GroupPeers(ctx, clientcompat.Actor{ID: 10}, page, false)
	accessMust(t, err)
	for _, tc := range []struct {
		v    any
		keys []string
	}{{users[0], []string{"avatar", "display_name", "email", "info", "is_admin", "name", "note", "status"}}, {peers[0], []string{"device_group_name", "id", "info", "note", "status", "user", "user_name"}}} {
		raw, err := json.Marshal(tc.v)
		accessMust(t, err)
		var values map[string]any
		accessMust(t, json.Unmarshal(raw, &values))
		keys := []string{}
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		accessEqual(t, keys, tc.keys)
	}
	accessEqual(t, users[0].DisplayName, "")
	accessEqual(t, users[0].Avatar, "")
	accessEqual(t, users[0].Note, "")
}

func TestDeviceAccessScopeGroupZeroPrivateDanglingAndCurrentState(t *testing.T) {
	f := newAccessFixture(t)
	f.user(t, 10, 0, true, false)
	f.user(t, 11, 0, true, false)
	// Dirty id=0 group must never turn unassigned users into a shared directory.
	accessMust(t, f.db.Exec("INSERT INTO groups(id,name,type) VALUES(0,'dirty-zero',2),(2,'private',1),(3,'share',2)").Error)
	accessMust(t, f.db.Exec("INSERT INTO device_groups(id,name) VALUES(0,'dirty-zero')").Error)
	f.peer(t, "mine", 10, 0)
	f.peer(t, "other", 11, 0)
	page := clientcompat.Page{Current: 1, Size: 100}
	ctx := context.Background()
	for _, gid := range []uint{0, 2, 999} {
		accessMust(t, f.db.Model(&model.User{}).Where("id IN ?", []uint{10, 11}).Update("group_id", gid).Error)
		n, users, err := f.store.Users(ctx, clientcompat.Actor{ID: 10}, page, false)
		accessMust(t, err)
		accessEqual(t, n, int64(1))
		accessEqual(t, users[0].Name, "u010")
		n, peers, err := f.store.GroupPeers(ctx, clientcompat.Actor{ID: 10}, page, false)
		accessMust(t, err)
		accessEqual(t, n, int64(1))
		accessEqual(t, peers[0].DeviceGroupName, "")
		n, _, err = f.store.DeviceGroups(ctx, clientcompat.Actor{ID: 10}, page)
		accessMust(t, err)
		accessEqual(t, n, int64(0))
	}
	accessMust(t, f.db.Model(&model.User{}).Where("id IN ?", []uint{10, 11}).Update("group_id", 3).Error)
	n, _, err := f.store.Users(ctx, clientcompat.Actor{ID: 10}, page, false)
	accessMust(t, err)
	accessEqual(t, n, int64(2))
	accessMust(t, f.db.Model(&model.User{}).Where("id = ?", 11).Update("status", model.COMMON_STATUS_DISABLED).Error)
	n, _, err = f.store.Users(ctx, clientcompat.Actor{ID: 10}, page, false)
	accessMust(t, err)
	accessEqual(t, n, int64(1))
	accessMust(t, f.db.Model(&model.User{}).Where("id = ?", 10).Update("status", model.COMMON_STATUS_DISABLED).Error)
	_, _, err = f.store.DeviceGroups(ctx, clientcompat.Actor{ID: 10}, page)
	if !errors.Is(err, clientcompat.ErrForbidden) {
		t.Fatal(err)
	}
}

func TestDeviceAccessScopeAuthorizationBeforeTotalAndPagination(t *testing.T) {
	f := newAccessFixture(t)
	accessMust(t, f.db.Create(&model.Group{IdModel: model.IdModel{Id: 1}, Name: "share", Type: 2}).Error)
	f.user(t, 1, 2, true, false)
	f.user(t, 10, 1, true, false)
	accessMust(t, f.db.Create(&model.DeviceGroup{IdModel: model.IdModel{Id: 1}, Name: "hidden"}).Error)
	f.peer(t, "hidden", 1, 1)
	for i := 0; i < 205; i++ {
		id := uint(i + 100)
		f.user(t, id, 1, true, false)
		accessMust(t, f.db.Create(&model.DeviceGroup{IdModel: model.IdModel{Id: id}, Name: fmt.Sprintf("g%03d", i)}).Error)
		f.peer(t, fmt.Sprintf("p%03d", i), id, id)
	}
	ctx := context.Background()
	actor := clientcompat.Actor{ID: 10}
	for current, want := range []int{100, 100, 5, 0} {
		p := clientcompat.Page{Current: current + 1, Size: 100}
		n, peers, err := f.store.GroupPeers(ctx, actor, p, false)
		accessMust(t, err)
		accessEqual(t, n, int64(205))
		accessEqual(t, len(peers), want)
		n, groups, err := f.store.DeviceGroups(ctx, actor, p)
		accessMust(t, err)
		accessEqual(t, n, int64(205))
		accessEqual(t, len(groups), want)
		if want > 0 {
			accessEqual(t, peers[0].ID, fmt.Sprintf("p%03d", current*100))
			accessEqual(t, groups[0].Name, fmt.Sprintf("g%03d", current*100))
		}
	}
	n, users, err := f.store.Users(ctx, actor, clientcompat.Page{Current: 3, Size: 100}, false)
	accessMust(t, err)
	accessEqual(t, n, int64(206))
	accessEqual(t, len(users), 6)
	accessEqual(t, users[0].Name, "u299")
}

func TestDeviceAccessScopeInstancesInputAndStorageFailures(t *testing.T) {
	f := newAccessFixture(t)
	other := newAccessFixture(t)
	for _, fixture := range []accessFixture{f, other} {
		fixture.user(t, 10, 0, true, false)
	}
	f.peer(t, "only-first", 10, 0)
	p := clientcompat.Page{Current: 1, Size: 10}
	ctx := context.Background()
	n, _, err := other.store.GroupPeers(ctx, clientcompat.Actor{ID: 10}, p, false)
	accessMust(t, err)
	accessEqual(t, n, int64(0))
	n, _, err = f.store.GroupPeers(ctx, clientcompat.Actor{ID: 10}, p, false)
	accessMust(t, err)
	accessEqual(t, n, int64(1))
	calls := []func(context.Context, clientcompat.Page) error{
		func(c context.Context, p clientcompat.Page) error {
			_, _, e := f.store.Users(c, clientcompat.Actor{ID: 10}, p, false)
			return e
		},
		func(c context.Context, p clientcompat.Page) error {
			_, _, e := f.store.GroupPeers(c, clientcompat.Actor{ID: 10}, p, false)
			return e
		},
		func(c context.Context, p clientcompat.Page) error {
			_, _, e := f.store.DeviceGroups(c, clientcompat.Actor{ID: 10}, p)
			return e
		},
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, call := range calls {
		if e := call(canceled, p); !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
		if e := call(ctx, clientcompat.Page{}); !errors.Is(e, clientcompat.ErrInput) {
			t.Fatal(e)
		}
	}
	accessMust(t, f.db.Exec("DROP TABLE users").Error)
	for _, call := range calls {
		if e := call(ctx, p); !errors.Is(e, clientcompat.ErrUnavailable) {
			t.Fatal(e)
		}
	}
}
