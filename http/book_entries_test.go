package http

import (
	"context"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
)

func managementEntryState(t *testing.T, f definitionFixture) []any {
	t.Helper()
	var peers []model.Peer
	if err := f.db.Order("row_id").Find(&peers).Error; err != nil {
		t.Fatal(err)
	}
	return append(collectionSnapshot(t, f), peers)
}

func TestManagementEntriesHTTPRevocationAndValidControls(t *testing.T) {
	for _, scope := range []string{"admin", "my"} {
		for _, action := range []string{"create", "update", "delete"} {
			t.Run(scope+"/"+action, func(t *testing.T) {
				f, book := bookManagementFixture(t)
				actor, prefix := f.admin, "/api/admin/address_book/"
				if scope == "my" {
					actor, prefix = f.owner, "/api/admin/my/address_book/"
				}
				var original model.AddressBook
				if err := f.db.Where("collection_id=?", book.Id).First(&original).Error; err != nil {
					t.Fatal(err)
				}
				peer := model.Peer{Id: original.Id, Uuid: "entry-peer", UserId: f.owner.Id, Alias: "before", Note: "peer-note"}
				compatCreate(t, &peer)
				rowID, device := original.RowId, original.Id
				if action == "create" {
					rowID, device = 0, "new-device"
				}
				body := fmt.Sprintf(`{"row_id":%d,"id":%q,"user_id":%d,"collection_id":%d,"alias":"changed","tags":[],"note":"ignored"}`, rowID, device, book.UserId, book.Id)
				before := managementEntryState(t, f)
				fired := false
				reader := &collectionChangingReader{Reader: strings.NewReader(body), change: func() error {
					fired = true
					return f.db.Exec("DELETE FROM user_tokens WHERE token=?", f.tokens[actor.Id]).Error
				}}
				r := httptest.NewRequest("POST", prefix+action, reader)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("api-token", f.tokens[actor.Id])
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				definitionResult(t, w, 200, 403)
				if !fired || !reflect.DeepEqual(before, managementEntryState(t, f)) {
					t.Fatal("revoked entry request retained AB or peer writes")
				}
				compatCreate(t, &model.UserToken{UserId: actor.Id, Token: f.tokens[actor.Id], ExpiredAt: time.Now().Add(time.Hour).Unix()})
				definitionResult(t, f.request(context.Background(), "POST", prefix+action, body, f.tokens[actor.Id]), 200, 0)
				if action == "delete" {
					var n int64
					if err := f.db.Model(&model.AddressBook{}).Where("row_id=?", rowID).Count(&n).Error; err != nil || n != 0 {
						t.Fatal("valid delete failed", n, err)
					}
				} else {
					var got model.AddressBook
					if err := f.db.Where("user_id=? AND collection_id=? AND id=?", book.UserId, book.Id, device).First(&got).Error; err != nil || got.Alias != "changed" || got.Note != "" || time.Time(got.CreatedAt).IsZero() {
						t.Fatal("valid create/update did not reach intended row", got, err)
					}
				}
			})
		}
	}
}

func TestManagementEntriesHTTPAtomicPeerSideEffectsAndDeleteScope(t *testing.T) {
	f, book := bookManagementFixture(t)
	var row model.AddressBook
	if err := f.db.Where("collection_id=?", book.Id).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&row).UpdateColumn("note", "book-note").Error; err != nil {
		t.Fatal(err)
	}
	other := model.AddressBookCollection{UserId: f.owner.Id, Name: "other"}
	compatCreate(t, &other)
	otherEntry := model.AddressBook{UserId: f.owner.Id, CollectionId: other.Id, Id: row.Id, Alias: "other-entry", Note: "other-note", Tags: custom_types.AutoJson("[]")}
	compatCreate(t, &otherEntry)
	peer := model.Peer{Id: row.Id, Uuid: "atomic-peer", UserId: f.hidden.Id, AbId: other.Id, Alias: "old", Hostname: "old-host", Username: "old-user", Note: "peer-note", GroupId: 17}
	compatCreate(t, &peer)
	body := fmt.Sprintf(`{"row_id":%d,"id":%q,"alias":"fresh","hostname":"new-host","username":"new-user","password":"raw-password","hash":"hash","tags":["x"],"note":"ignored","created_at":"ignored"}`, row.RowId, row.Id)
	before := managementEntryState(t, f)
	if err := f.db.Exec("CREATE TRIGGER reject_management_peer BEFORE UPDATE ON peers BEGIN SELECT RAISE(ABORT,'private peer SQL'); END").Error; err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/api/admin/address_book/update", body)
	definitionResult(t, w, 503, 503)
	if strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "SQL") || !reflect.DeepEqual(before, managementEntryState(t, f)) {
		t.Fatal("peer SQL failure leaked or retained entry write", w.Body.String())
	}
	if err := f.db.Exec("DROP TRIGGER reject_management_peer").Error; err != nil {
		t.Fatal(err)
	}
	definitionResult(t, f.call("POST", "/api/admin/address_book/update", body), 200, 0)
	var actual model.Peer
	var entry model.AddressBook
	if err := f.db.First(&actual, peer.RowId).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.First(&entry, row.RowId).Error; err != nil {
		t.Fatal(err)
	}
	if actual.AbId != book.Id || actual.Alias != "fresh" || actual.Hostname != "new-host" || actual.Username != "new-user" || actual.Note != peer.Note || actual.GroupId != peer.GroupId || entry.Note != "book-note" || entry.UserId != book.UserId || entry.CollectionId != book.Id || entry.Id != row.Id || entry.Password != "raw-password" {
		t.Fatal("admin update did not commit narrow peer fields and protected entry", actual, entry)
	}
	retained := actual
	definitionResult(t, f.call("POST", "/api/admin/address_book/delete", fmt.Sprintf(`{"row_id":%d}`, otherEntry.RowId)), 200, 0)
	if err := f.db.First(&actual, peer.RowId).Error; err != nil || !reflect.DeepEqual(actual, retained) {
		t.Fatal("deleting another book entry cleared primary configuration", err)
	}
	definitionResult(t, f.call("POST", "/api/admin/address_book/delete", fmt.Sprintf(`{"row_id":%d}`, row.RowId)), 200, 0)
	if err := f.db.First(&actual, peer.RowId).Error; err != nil || actual.AbId != 0 || actual.Alias != "" || actual.Hostname != retained.Hostname || actual.Username != retained.Username || actual.Note != retained.Note || actual.GroupId != retained.GroupId {
		t.Fatal("matching primary delete cleared unrelated peer values", err, actual)
	}
}

func TestManagementEntriesHTTPMyScopeAndProtectedIdentity(t *testing.T) {
	f, book := bookManagementFixture(t)
	peer := model.Peer{Id: "my-new-device", Uuid: "my-new-uuid", UserId: f.hidden.Id, Alias: "unchanged", AbId: book.Id, Note: "peer-note"}
	compatCreate(t, &peer)
	// Compare persisted values on both sides, including driver time locations.
	if err := f.db.First(&peer, peer.RowId).Error; err != nil {
		t.Fatal(err)
	}
	before := managementEntryState(t, f)
	for _, body := range []string{fmt.Sprintf(`{"id":"forbidden","user_id":1}`), fmt.Sprintf(`{"id":"forbidden","collection_id":%d}`, book.Id)} {
		actor := f.owner
		if strings.Contains(body, "collection_id") {
			actor = f.actor
		}
		w := f.request(context.Background(), "POST", "/api/admin/my/address_book/create", body, f.tokens[actor.Id])
		if w.Code != 200 || strings.Contains(w.Body.String(), `"code":0`) {
			t.Fatal("my foreign-owner write accepted", w.Body.String())
		}
	}
	if !reflect.DeepEqual(before, managementEntryState(t, f)) {
		t.Fatal("foreign scope write changed state")
	}
	body := fmt.Sprintf(`{"id":%q,"collection_id":%d,"alias":"book-alias","tags":[]}`, peer.Id, book.Id)
	definitionResult(t, f.request(context.Background(), "POST", "/api/admin/my/address_book/create", body, f.tokens[f.owner.Id]), 200, 0)
	var row model.AddressBook
	if err := f.db.Where("id=? AND collection_id=?", peer.Id, book.Id).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{fmt.Sprintf(`{"row_id":%d,"id":"another-device"}`, row.RowId), fmt.Sprintf(`{"row_id":%d,"user_id":%d}`, row.RowId, f.hidden.Id)} {
		saved := managementEntryState(t, f)
		definitionResult(t, f.call("POST", "/api/admin/address_book/update", bad), 200, 101)
		if !reflect.DeepEqual(saved, managementEntryState(t, f)) {
			t.Fatal("ordinary edit rewrote protected identity/scope")
		}
	}
	definitionResult(t, f.request(context.Background(), "POST", "/api/admin/my/address_book/update", fmt.Sprintf(`{"row_id":%d,"alias":"updated"}`, row.RowId), f.tokens[f.owner.Id]), 200, 0)
	definitionResult(t, f.request(context.Background(), "POST", "/api/admin/my/address_book/delete", fmt.Sprintf(`{"row_id":%d}`, row.RowId), f.tokens[f.owner.Id]), 200, 0)
	var actual model.Peer
	if err := f.db.First(&actual, peer.RowId).Error; err != nil || !reflect.DeepEqual(actual, peer) {
		t.Fatal("my create/update/delete changed peer configuration", err)
	}
}
