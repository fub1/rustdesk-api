package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

type compatFixture struct {
	handler                     stdhttp.Handler
	actor, owner, hidden, admin model.User
	tokens                      map[uint]string
}

func newCompatFixture(t *testing.T) *compatFixture {
	t.Helper()
	sessions := installLegacyFixture(t)
	global.DB.Config.DisableForeignKeyConstraintWhenMigrating = true
	if err := global.DB.AutoMigrate(&model.User{}, &model.UserToken{}, &model.Group{}, &model.DeviceGroup{}, &model.Peer{}, &model.AddressBook{}, &model.AddressBookCollection{}, &model.AddressBookCollectionRule{}, &model.Tag{}); err != nil {
		t.Fatal(err)
	}
	// Use the runtime schema for current-model peer writes.
	if err := database.Update(global.DB, 281, nil); err != nil {
		t.Fatal(err)
	}
	pool, err := global.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	groups := []model.Group{{IdModel: model.IdModel{Id: 1}, Name: "shared", Type: model.GroupTypeShare}, {IdModel: model.IdModel{Id: 2}, Name: "private", Type: model.GroupTypeDefault}, {IdModel: model.IdModel{Id: 3}, Name: "other", Type: model.GroupTypeShare}}
	if err := global.DB.Create(&groups).Error; err != nil {
		t.Fatal(err)
	}
	no, yes := false, true
	users := []model.User{{Username: "actor", GroupId: 1, IsAdmin: &no, Status: model.COMMON_STATUS_ENABLE}, {Username: "owner", GroupId: 1, IsAdmin: &no, Status: model.COMMON_STATUS_ENABLE}, {Username: "hidden", GroupId: 3, IsAdmin: &no, Status: model.COMMON_STATUS_ENABLE}, {Username: "administrator", GroupId: 1, IsAdmin: &yes, Status: model.COMMON_STATUS_ENABLE}}
	if err := global.DB.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	f := &compatFixture{actor: users[0], owner: users[1], hidden: users[2], admin: users[3], tokens: map[uint]string{}}
	global.Jwt.TokenExpireDuration = time.Hour
	for _, u := range users {
		token := global.Jwt.GenerateToken(u.Id)
		if err := global.DB.Create(&model.UserToken{UserId: u.Id, Token: token, ExpiredAt: time.Now().Add(time.Hour).Unix()}).Error; err != nil {
			t.Fatal(err)
		}
		f.tokens[u.Id] = token
	}
	g, err := NewHandler(sessions)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = g
	return f
}
func (f *compatFixture) call(method, path, body string, user model.User) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.tokens[user.Id])
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func compatGUID(user model.User, cid uint) string {
	return fmt.Sprintf("%d-%d-%d", user.GroupId, user.Id, cid)
}
func compatRow(id string, user model.User, cid uint) model.AddressBook {
	return model.AddressBook{Id: id, UserId: user.Id, CollectionId: cid, Tags: custom_types.AutoJson(`[]`)}
}
func compatCreate(t *testing.T, rows any) {
	t.Helper()
	if err := global.DB.CreateInBatches(rows, 40).Error; err != nil {
		t.Fatal(err)
	}
}
func compatDocument(t *testing.T, value any) string {
	t.Helper()
	inner, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal(map[string]string{"data": string(inner)})
	if err != nil {
		t.Fatal(err)
	}
	return string(outer)
}
func compatReadPages(t *testing.T, f *compatFixture, method, path, field string, user model.User, want int) []string {
	t.Helper()
	seen := map[string]bool{}
	all := []string{}
	for current := 1; current <= want/100+2; current++ {
		separator := "?"
		if strings.Contains(path, "?") {
			separator = "&"
		}
		w := f.call(method, fmt.Sprintf("%s%scurrent=%d&pageSize=100", path, separator, current), "", user)
		var response struct {
			Total int
			Data  []map[string]json.RawMessage
		}
		if json.Unmarshal(w.Body.Bytes(), &response) != nil || w.Code != 200 || response.Total != want || response.Data == nil {
			t.Fatalf("page %d %s: %d %s", current, path, w.Code, w.Body.String())
		}
		expected := want - (current-1)*100
		if expected > 100 {
			expected = 100
		}
		if expected < 0 {
			expected = 0
		}
		if len(response.Data) != expected {
			t.Fatalf("page %d has %d rows want %d", current, len(response.Data), expected)
		}
		for _, row := range response.Data {
			var key string
			if json.Unmarshal(row[field], &key) != nil || key == "" || seen[key] {
				t.Fatalf("missing/duplicate row key %q on page %d", key, current)
			}
			seen[key] = true
			all = append(all, key)
		}
		if expected == 0 {
			break
		}
	}
	if len(all) != want {
		t.Fatalf("read %d of %d", len(all), want)
	}
	return all
}

func TestClientCompatAddressBookPaginationAndWireTypes(t *testing.T) {
	f := newCompatFixture(t)
	rows := make([]model.AddressBook, 1007)
	for i := range rows {
		rows[i] = compatRow(fmt.Sprintf("peer-%04d", i), f.actor, 0)
		rows[i].Alias = fmt.Sprintf("row-%04d", i)
		rows[i].ForceAlwaysRelay = i%2 == 0
		rows[i].SameServer = i%3 == 0
	}
	// A pre-existing duplicate business ID must not cause later physical rows
	// to be skipped. No new uniqueness policy is imposed by this patch.
	rows[1006].Id = rows[0].Id
	compatCreate(t, &rows)
	hidden := compatRow("hidden", f.hidden, 0)
	compatCreate(t, &hidden)
	collection := model.AddressBookCollection{UserId: f.actor.Id, Name: "another"}
	compatCreate(t, &collection)
	other := compatRow("other-collection", f.actor, collection.Id)
	compatCreate(t, &other)
	labels := make([]model.Tag, 1003)
	for i := range labels {
		labels[i] = model.Tag{UserId: f.actor.Id, Name: fmt.Sprintf("tag-%04d", i)}
	}
	compatCreate(t, &labels)
	guid := compatGUID(f.actor, 0)
	keys := compatReadPages(t, f, "POST", "/api/ab/peers?ab="+guid, "alias", f.actor, 1007)
	for i, key := range keys {
		if key != fmt.Sprintf("row-%04d", i) {
			t.Fatal("row_id ordering is unstable")
		}
	}
	legacyPage := f.call("POST", "/api/ab/peers?ab="+guid+"&page=2&pageSize=100", "", f.actor)
	var legacyPageBody struct {
		Total int
		Data  []struct{ Alias string }
	}
	if json.Unmarshal(legacyPage.Body.Bytes(), &legacyPageBody) != nil || legacyPageBody.Total != 1007 || len(legacyPageBody.Data) != 100 || legacyPageBody.Data[0].Alias != "row-0100" {
		t.Fatal("legacy page alias repeated first page", legacyPage.Body.String())
	}
	first := f.call("POST", "/api/ab/peers?ab="+guid+"&current=1&pageSize=1", "", f.actor)
	var page struct{ Data []map[string]any }
	if json.Unmarshal(first.Body.Bytes(), &page) != nil || page.Data[0]["forceAlwaysRelay"] != "true" || page.Data[0]["same_server"] != true {
		t.Fatalf("client field types: %s", first.Body.String())
	}
	for _, field := range []string{"sameServer", "row_id", "user_id", "collection_id", "created_at"} {
		if _, ok := page.Data[0][field]; ok {
			t.Fatal("persistence field leaked into client DTO", field)
		}
	}
	legacy := f.call("GET", "/api/ab", "", f.actor)
	var outer struct{ Data string }
	if json.Unmarshal(legacy.Body.Bytes(), &outer) != nil || legacy.Code != 200 {
		t.Fatal(legacy.Body.String())
	}
	var document struct {
		Peers     []json.RawMessage
		Tags      []string
		TagColors string `json:"tag_colors"`
	}
	if json.Unmarshal([]byte(outer.Data), &document) != nil || len(document.Peers) != 1007 || len(document.Tags) != 1003 {
		t.Fatal("legacy whole book truncated at 1000", legacy.Body.String())
	}
	tags := f.call("POST", "/api/ab/tags/"+guid, "", f.actor)
	var tagList []map[string]any
	if json.Unmarshal(tags.Body.Bytes(), &tagList) != nil || len(tagList) != 1003 {
		t.Fatal("tags truncated or wrapped")
	}
	for _, query := range []string{"current=0", "current=-1", "current=2&page=1", "current=1&current=2", "pageSize=0", "pageSize=1001", "current=9223372036854775807&pageSize=100"} {
		w := f.call("POST", "/api/ab/peers?ab="+guid+"&"+query, "", f.actor)
		if w.Code != 400 {
			t.Fatalf("invalid pagination accepted: %s %s", query, w.Body.String())
		}
	}
}

func TestClientCompatProfilesPaginationMaximumRuleAndScope(t *testing.T) {
	f := newCompatFixture(t)
	collections := make([]model.AddressBookCollection, 1021)
	for i := range collections {
		owner := f.owner.Id
		if i < 120 {
			owner = f.actor.Id
		}
		collections[i] = model.AddressBookCollection{UserId: owner, Name: fmt.Sprintf("profile-%04d", i)}
	}
	compatCreate(t, &collections)
	rules := []model.AddressBookCollectionRule{}
	for _, collection := range collections[120:] {
		rules = append(rules, model.AddressBookCollectionRule{UserId: f.owner.Id, CollectionId: collection.Id, Type: 2, ToId: f.actor.GroupId, Rule: 1})
	}
	// Duplicated personal/group grants count a profile once, using the maximum.
	for _, rule := range []int{2, 3} {
		rules = append(rules, model.AddressBookCollectionRule{UserId: f.owner.Id, CollectionId: collections[120].Id, Type: 1, ToId: f.actor.Id, Rule: rule})
	}
	compatCreate(t, &rules)
	unshared := model.AddressBookCollection{UserId: f.hidden.Id, Name: "inaccessible"}
	compatCreate(t, &unshared)
	compatReadPages(t, f, "POST", "/api/ab/shared/profiles", "guid", f.actor, 1021)
	full := f.call("POST", "/api/ab/shared/profiles?current=2&pageSize=100", "", f.actor)
	var response struct {
		Data []struct {
			GUID string `json:"guid"`
			Rule int
		}
	}
	if json.Unmarshal(full.Body.Bytes(), &response) != nil {
		t.Fatal(full.Body.String())
	}
	found := false
	for _, row := range response.Data {
		if row.GUID == compatGUID(f.owner, collections[120].Id) {
			found = true
			if row.Rule != 3 {
				t.Fatal("maximum rule not selected")
			}
		}
	}
	if !found {
		t.Fatal("shared profile missing")
	}
	writeRow := compatRow("write", f.owner, collections[120].Id)
	readRow := compatRow("read", f.owner, collections[121].Id)
	compatCreate(t, &writeRow)
	compatCreate(t, &readRow)
	for _, tc := range []struct {
		row  model.AddressBook
		want int
	}{{writeRow, 200}, {readRow, 400}} {
		w := f.call("PUT", "/api/ab/peer/update/"+compatGUID(f.owner, tc.row.CollectionId), fmt.Sprintf(`{"id":%q,"alias":"changed"}`, tc.row.Id), f.actor)
		if w.Code != tc.want {
			t.Fatalf("rule scope mismatch %d: %s", tc.want, w.Body.String())
		}
	}
	for _, raw := range []string{compatGUID(f.hidden, unshared.Id), compatGUID(f.hidden, 0), fmt.Sprintf("%d-%d-%d", f.owner.GroupId, f.owner.Id, unshared.Id), "-1-1-0"} {
		w := f.call("POST", "/api/ab/peers?ab="+url.QueryEscape(raw), "", f.actor)
		if w.Code == 200 {
			t.Fatal("inaccessible/forged guid accepted", raw)
		}
	}
}

func TestClientCompatGroupListsPaginateAfterAuthorization(t *testing.T) {
	f := newCompatFixture(t)
	no := false
	users := make([]model.User, 1005)
	for i := range users {
		users[i] = model.User{Username: fmt.Sprintf("group-user-%04d", i), GroupId: 1, IsAdmin: &no, Status: model.COMMON_STATUS_ENABLE}
	}
	compatCreate(t, &users)
	disabled := model.User{Username: "disabled", GroupId: 1, IsAdmin: &no, Status: model.COMMON_STATUS_DISABLED}
	compatCreate(t, &disabled)
	groups := make([]model.DeviceGroup, 1003)
	for i := range groups {
		groups[i] = model.DeviceGroup{Name: fmt.Sprintf("device-group-%04d", i)}
	}
	compatCreate(t, &groups)
	peers := make([]model.Peer, 1041)
	for i := range peers {
		peers[i] = model.Peer{Id: fmt.Sprintf("accessible-%04d", i), UserId: f.owner.Id, GroupId: groups[1002].Id, Hostname: "host", Username: "os-user", Os: "Windows"}
	}
	compatCreate(t, &peers)
	outside := []model.Peer{{Id: "hidden-peer", UserId: f.hidden.Id}, {Id: "disabled-peer", UserId: disabled.Id}}
	compatCreate(t, &outside)
	compatReadPages(t, f, "GET", "/api/users?accessible=&status=1", "name", f.actor, 1008)
	compatReadPages(t, f, "GET", "/api/peers?accessible=&status=1", "id", f.actor, 1041)
	compatReadPages(t, f, "GET", "/api/device-group/accessible", "name", f.admin, 1003)
	if w := f.call("GET", "/api/device-group/accessible?current=1&pageSize=100", "", f.actor); w.Code == 200 {
		t.Fatal("ordinary actor gained device-group administration")
	}
	first := f.call("GET", "/api/peers?current=1&pageSize=1&status=1", "", f.actor)
	var page struct {
		Data []struct {
			DeviceGroupName string `json:"device_group_name"`
			Info            struct {
				DeviceName string `json:"device_name"`
			}
		}
	}
	if json.Unmarshal(first.Body.Bytes(), &page) != nil || page.Data[0].DeviceGroupName != groups[1002].Name || page.Data[0].Info.DeviceName != "host" {
		t.Fatal("host/group mapping truncated beyond 999", first.Body.String())
	}
	if err := global.DB.Model(&model.User{}).Where("id = ?", f.actor.Id).Update("group_id", 2).Error; err != nil {
		t.Fatal(err)
	}
	compatReadPages(t, f, "GET", "/api/users?status=1", "name", f.actor, 1)
	compatReadPages(t, f, "GET", "/api/peers?status=1", "id", f.actor, 0)
}

func TestClientCompatPatchPreservesOmittedFieldsAndClearsExplicitValues(t *testing.T) {
	f := newCompatFixture(t)
	row := compatRow("patch-target", f.actor, 0)
	row.Alias = "alias"
	row.Note = "initial note"
	row.Username = "username"
	row.Hostname = "host"
	row.Platform = "Windows"
	row.Hash = "hash"
	row.Password = "password"
	row.Tags = custom_types.AutoJson(`["tag"]`)
	compatCreate(t, &row)
	path := "/api/ab/peer/update/" + compatGUID(f.actor, 0)
	w := f.call("PUT", path, `{"id":"patch-target","username":"new-user","hostname":"","platform":"Linux"}`, f.actor)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("patch ACK: %d %s", w.Code, w.Body.String())
	}
	var got model.AddressBook
	if err := global.DB.First(&got, row.RowId).Error; err != nil {
		t.Fatal(err)
	}
	if got.Username != "new-user" || got.Hostname != "" || got.Platform != "Linux" || got.Alias != "alias" || got.Hash != "hash" || got.Password != "password" || got.Note != "initial note" || string(got.Tags) != `["tag"]` {
		t.Fatal("patch changed omitted fields or ignored host fields")
	}
	w = f.call("PUT", path, `{"id":"patch-target","alias":"","hash":"","password":"","tags":[]}`, f.actor)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal(w.Body.String())
	}
	global.DB.First(&got, row.RowId)
	if got.Alias != "" || got.Hash != "" || got.Password != "" || string(got.Tags) != "[]" || got.Username != "new-user" {
		t.Fatal("explicit empty patch did not clear precisely")
	}
	for _, body := range []string{`{"id":42,"alias":"x"}`, `{"id":"patch-target","alias":null}`, `{"id":"patch-target","tags":null}`, `{"id":"patch-target","tags":[1]}`, `{"id":"patch-target","user_id":3}`, `{"id":"patch-target","alias":7}`, `{"id":"patch-target","note":null}`, `{"id":"patch-target","note":1}`, `{"id":"patch-target","alias":"x"} {}`} {
		if w := f.call("PUT", path, body, f.actor); w.Code != 400 {
			t.Fatalf("invalid patch accepted: %s => %s", body, w.Body.String())
		}
	}
	for _, note := range []string{"新的备注\nsecond line \"quoted\"", ""} {
		body, _ := json.Marshal(map[string]string{"id": "patch-target", "note": note})
		w := f.call("PUT", path, string(body), f.actor)
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatal("note patch ACK", w.Body.String())
		}
		global.DB.First(&got, row.RowId)
		if got.Note != note || got.Username != "new-user" {
			t.Fatal("note did not persist precisely")
		}
		read := f.call("POST", "/api/ab/peers?ab="+compatGUID(f.actor, 0), "", f.actor)
		var response struct{ Data []struct{ Note string } }
		if json.Unmarshal(read.Body.Bytes(), &response) != nil || len(response.Data) != 1 || response.Data[0].Note != note {
			t.Fatal("note failed HTTP readback", read.Body.String())
		}
	}
	var updateFault atomic.Bool
	updateFault.Store(true)
	if err := global.DB.Callback().Update().Before("gorm:update").Register("compat-update-fault", func(tx *gorm.DB) {
		if tx.Statement.Table == "address_books" && updateFault.Load() {
			tx.AddError(errors.New("private database failure marker"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	w = f.call("PUT", path, `{"id":"patch-target","alias":"bad"}`, f.actor)
	if w.Code != 503 || strings.Contains(w.Body.String(), "private database") {
		t.Fatal("write error lost or leaked", w.Body.String())
	}
	updateFault.Store(false)
	global.DB.First(&got, row.RowId)
	if got.Alias != "" {
		t.Fatal("failed update committed")
	}
}

func TestClientCompatLegacyReplaceIsAtomicAndPersonalOnly(t *testing.T) {
	f := newCompatFixture(t)
	collection := model.AddressBookCollection{UserId: f.actor.Id, Name: "shared"}
	compatCreate(t, &collection)
	personal := compatRow("personal", f.actor, 0)
	personal.Alias = "before"
	personal.Hash = "oldhash"
	personal.Note = "keep-existing-note"
	personal.ForceAlwaysRelay = true
	personal.SameServer = true
	personal.RdpPort = "3389"
	shared := compatRow("personal", f.actor, collection.Id)
	shared.Alias = "shared"
	foreign := compatRow("foreign", f.hidden, 0)
	compatCreate(t, &personal)
	compatCreate(t, &shared)
	compatCreate(t, &foreign)
	labels := []model.Tag{{UserId: f.actor.Id, Name: "before", CollectionId: 0}, {UserId: f.actor.Id, Name: "keep-shared", CollectionId: collection.Id}, {UserId: f.hidden.Id, Name: "keep-foreign"}}
	compatCreate(t, &labels)
	body := compatDocument(t, map[string]any{"peers": []any{map[string]any{"id": "personal", "alias": "after", "hash": "", "row_id": foreign.RowId, "user_id": f.hidden.Id, "collection_id": collection.Id}}, "tags": []string{"after"}, "tag_colors": `{"after":42}`})
	var fail atomic.Bool
	fail.Store(true)
	if err := global.DB.Callback().Create().Before("gorm:create").Register("compat-legacy-fault", func(tx *gorm.DB) {
		if tx.Statement.Table == "tags" && fail.Load() {
			tx.AddError(errors.New("private tag failure marker"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/api/ab", body, f.actor)
	if w.Code != 503 || strings.Contains(w.Body.String(), "private tag") {
		t.Fatal("legacy failure falsely acknowledged or leaked", w.Body.String())
	}
	var got model.AddressBook
	global.DB.First(&got, personal.RowId)
	if got.Alias != "before" || got.Hash != "oldhash" {
		t.Fatal("peers committed before tags failed")
	}
	var count int64
	global.DB.Model(&model.Tag{}).Where("user_id = ? AND name = ?", f.actor.Id, "before").Count(&count)
	if count != 1 {
		t.Fatal("tag deletion did not roll back")
	}
	fail.Store(false)
	w = f.call("POST", "/api/ab", body, f.actor)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "null" {
		t.Fatalf("legacy ACK changed: %d %s", w.Code, w.Body.String())
	}
	global.DB.First(&got, personal.RowId)
	if got.Alias != "after" || got.Hash != "" || got.UserId != f.actor.Id || got.CollectionId != 0 || got.Note != "keep-existing-note" || !got.ForceAlwaysRelay || !got.SameServer || got.RdpPort != "3389" {
		t.Fatal("replacement did not preserve ownership/row key or clear hash")
	}
	// Old clients omit note and other extensions. Only an explicit empty note
	// clears it; the omitted alias and relay settings remain untouched.
	clearNote := compatDocument(t, map[string]any{"peers": []any{map[string]any{"id": "personal", "note": ""}}, "tags": []string{"after"}, "tag_colors": `{"after":42}`})
	if result := f.call("POST", "/api/ab", clearNote, f.actor); result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	global.DB.First(&got, personal.RowId)
	if got.Note != "" || got.Alias != "after" || !got.ForceAlwaysRelay {
		t.Fatal("legacy presence semantics lost note/old fields")
	}
	var protected model.AddressBook
	global.DB.First(&protected, shared.RowId)
	if protected.Alias != "shared" {
		t.Fatal("legacy update modified shared collection")
	}
	protected = model.AddressBook{}
	global.DB.First(&protected, foreign.RowId)
	if protected.Id != "foreign" || protected.UserId != f.hidden.Id {
		t.Fatal("legacy update modified other owner")
	}
	for _, name := range []string{"keep-shared", "keep-foreign"} {
		global.DB.Model(&model.Tag{}).Where("name = ?", name).Count(&count)
		if count != 1 {
			t.Fatal("legacy replaced non-personal tags", name)
		}
	}
	w = f.call("POST", "/api/ab", compatDocument(t, map[string]any{"peers": []any{}, "tags": []string{}, "tag_colors": "{}"}), f.actor)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	global.DB.Model(&model.AddressBook{}).Where("user_id = ? AND collection_id = 0", f.actor.Id).Count(&count)
	if count != 0 {
		t.Fatal("empty whole document did not clear personal peers")
	}
	global.DB.Model(&model.AddressBook{}).Count(&count)
	if count != 2 {
		t.Fatal("empty whole document deleted shared/foreign peers")
	}
	for _, data := range []string{"null", "{}", `{"peers":null,"tags":[],"tag_colors":"{}"}`, `{"peers":[],"tags":[],"tag_colors":"null"}`} {
		body, _ := json.Marshal(map[string]string{"data": data})
		result := f.call("POST", "/api/ab", string(body), f.actor)
		if result.Code != 400 {
			t.Fatalf("malformed snapshot acknowledged: %s %s", data, result.Body.String())
		}
	}
}

func TestClientCompatQueryFailuresAndCanceledRequestsFailClosed(t *testing.T) {
	f := newCompatFixture(t)
	if err := global.DB.Callback().Query().Before("gorm:query").Register("compat-read-fault", func(tx *gorm.DB) {
		if tx.Statement.Table == "address_books" {
			tx.AddError(errors.New("private read failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/ab", "/api/ab/peers?ab=" + compatGUID(f.actor, 0)} {
		method := "GET"
		if strings.Contains(path, "/peers") {
			method = "POST"
		}
		w := f.call(method, path, "", f.actor)
		if w.Code != 503 || strings.Contains(w.Body.String(), "private read") {
			t.Fatalf("DB fault became successful empty list: %d %s", w.Code, w.Body.String())
		}
	}
	request := httptest.NewRequest("GET", "/api/users?current=1&pageSize=100", nil)
	request.Header.Set("Authorization", "Bearer "+f.tokens[f.actor.Id])
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request = request.WithContext(ctx)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, request)
	if w.Code != 408 {
		t.Fatalf("canceled request continued: %d %s", w.Code, w.Body.String())
	}
}

func TestClientCompatAddUsesWireFieldsAndCannotReadForeignHostMetadata(t *testing.T) {
	f := newCompatFixture(t)
	peers := []model.Peer{{Id: "owned-device", UserId: f.actor.Id, Username: "database-user", Hostname: "database-host", Os: "Windows 11"}, {Id: "foreign-device", UserId: f.hidden.Id, Username: "private-user", Hostname: "private-host", Os: "Linux"}}
	compatCreate(t, &peers)
	path := "/api/ab/peer/add/" + compatGUID(f.actor, 0)
	for _, body := range []string{`{"id":"owned-device","username":"chosen-user","alias":"chosen","forceAlwaysRelay":"true","same_server":true,"note":"机器备注","tags":[],"rdpPort":"3389","row_id":999,"user_id":3,"collection_id":99}`, `{"id":"foreign-device","note":"","tags":[],"forceAlwaysRelay":"false","same_server":null,"device_group_name":"untrusted"}`} {
		w := f.call("POST", path, body, f.actor)
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatalf("add ACK: %d %s", w.Code, w.Body.String())
		}
	}
	var owned, foreign model.AddressBook
	if err := global.DB.Where("id = ? AND user_id = ?", "owned-device", f.actor.Id).First(&owned).Error; err != nil {
		t.Fatal(err)
	}
	if owned.RowId == 999 || owned.CollectionId != 0 || owned.Username != "chosen-user" || owned.Hostname != "database-host" || owned.Platform != "Windows" || owned.Note != "机器备注" || !owned.ForceAlwaysRelay || !owned.SameServer {
		t.Fatal("add overwrote explicit fields or trusted body ownership")
	}
	if err := global.DB.Where("id = ? AND user_id = ?", "foreign-device", f.actor.Id).First(&foreign).Error; err != nil {
		t.Fatal(err)
	}
	if foreign.Username != "" || foreign.Hostname != "" || foreign.Platform != "" {
		t.Fatal("adding a guessed ID disclosed foreign host metadata")
	}
	w := f.call("POST", "/api/ab/peers?ab="+compatGUID(f.actor, 0), "", f.actor)
	var response struct {
		Data []struct {
			ID    string `json:"id"`
			Note  string `json:"note"`
			Relay string `json:"forceAlwaysRelay"`
			Same  bool   `json:"same_server"`
		}
	}
	if json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Data) != 2 || response.Data[0].Note != "机器备注" || response.Data[0].Relay != "true" || !response.Data[0].Same {
		t.Fatal("create/DTO readback mismatch", w.Body.String())
	}
}

func TestClientCompatAuthorizationDependencyFailureDoesNotExpandScope(t *testing.T) {
	f := newCompatFixture(t)
	if err := global.DB.Migrator().DropTable(&model.Group{}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/users?current=1&pageSize=100&status=1", "/api/peers?current=1&pageSize=100&status=1"} {
		result := f.call("GET", path, "", f.actor)
		if result.Code != 503 || strings.Contains(result.Body.String(), `"data"`) {
			t.Fatalf("authorization lookup fault fell back to data: %d %s", result.Code, result.Body.String())
		}
	}
}

func TestClientCompatLegacyTagsAreFactsAndColorsAreMetadata(t *testing.T) {
	f := newCompatFixture(t)
	collection := model.AddressBookCollection{UserId: f.actor.Id, Name: "protected-shared"}
	compatCreate(t, &collection)
	stored := []model.Tag{{UserId: f.actor.Id, Name: "keep", Color: 17}, {UserId: f.actor.Id, Name: "drop", Color: 18}, {UserId: f.actor.Id, CollectionId: collection.Id, Name: "shared", Color: 19}, {UserId: f.hidden.Id, Name: "foreign", Color: 20}}
	compatCreate(t, &stored)
	body := compatDocument(t, map[string]any{"peers": []any{}, "tags": []string{"keep", "new", "keep"}, "tag_colors": `{"drop":100,"stale-only":101}`})
	w := f.call("POST", "/api/ab", body, f.actor)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var rows []model.Tag
	if err := global.DB.Where("user_id = ? AND collection_id = 0", f.actor.Id).Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "keep" || rows[0].Color != 17 || rows[1].Name != "new" || rows[1].Color != 0 {
		t.Fatalf("tags lost/duplicated or colors resurrected removed tags: %+v", rows)
	}
	read := f.call("GET", "/api/ab", "", f.actor)
	var envelope struct{ Data string }
	if json.Unmarshal(read.Body.Bytes(), &envelope) != nil {
		t.Fatal(read.Body.String())
	}
	var document struct {
		Tags   []string
		Colors string `json:"tag_colors"`
	}
	if json.Unmarshal([]byte(envelope.Data), &document) != nil || len(document.Tags) != 2 || document.Tags[0] != "keep" || document.Tags[1] != "new" {
		t.Fatal("legacy tag facts did not round trip")
	}
	var colors map[string]uint
	if json.Unmarshal([]byte(document.Colors), &colors) != nil || len(colors) != 2 || colors["keep"] != 17 {
		t.Fatal("missing colors discarded prior metadata or leaked stale color keys")
	}
	body = compatDocument(t, map[string]any{"peers": []any{}, "tags": []string{}, "tag_colors": `{"keep":17,"stale-only":101}`})
	if result := f.call("POST", "/api/ab", body, f.actor); result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	if err := global.DB.Where("user_id = ? AND collection_id = 0", f.actor.Id).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatal("stale colors resurrected explicitly removed tags")
	}
	for _, id := range []uint{stored[2].Id, stored[3].Id} {
		var tag model.Tag
		if err := global.DB.First(&tag, id).Error; err != nil {
			t.Fatal("personal replacement erased shared/foreign tag", err)
		}
	}
}

func TestClientCompatCorruptStoredTagsFailBeforeSuccessfulSnapshot(t *testing.T) {
	f := newCompatFixture(t)
	row := compatRow("corrupt-tags", f.actor, 0)
	compatCreate(t, &row)
	for _, bad := range []string{"private-invalid-json-marker", "", `{"tag":true}`, `null`, `[null]`, `["keep",1]`} {
		if err := global.DB.Model(&model.AddressBook{}).Where("row_id = ?", row.RowId).UpdateColumn("tags", bad).Error; err != nil {
			t.Fatal(err)
		}
		for _, request := range []struct{ method, path string }{{"GET", "/api/ab"}, {"POST", "/api/ab/peers?ab=" + compatGUID(f.actor, 0)}} {
			w := f.call(request.method, request.path, "", f.actor)
			if w.Code != 503 || strings.Contains(w.Body.String(), `"data"`) || strings.Contains(w.Body.String(), "private-invalid-json") {
				t.Fatalf("corrupt tags became an empty successful snapshot: %q %d %s", bad, w.Code, w.Body.String())
			}
		}
		var raw string
		if err := global.DB.Raw("SELECT tags FROM address_books WHERE row_id = ?", row.RowId).Scan(&raw).Error; err != nil {
			t.Fatal(err)
		}
		if raw != bad {
			t.Fatal("reading corrupt tags altered stored facts")
		}
	}
	if err := global.DB.Model(&model.AddressBook{}).Where("row_id = ?", row.RowId).UpdateColumn("tags", `["kept"]`).Error; err != nil {
		t.Fatal(err)
	}
	read := f.call("POST", "/api/ab/peers?ab="+compatGUID(f.actor, 0), "", f.actor)
	var response struct{ Data []struct{ Tags []string } }
	if json.Unmarshal(read.Body.Bytes(), &response) != nil || read.Code != 200 || len(response.Data) != 1 || len(response.Data[0].Tags) != 1 || response.Data[0].Tags[0] != "kept" {
		t.Fatal("valid raw tags no longer parse", read.Body.String())
	}
}
