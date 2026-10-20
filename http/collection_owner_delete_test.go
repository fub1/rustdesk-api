package http

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/model"
)

func TestCollectionOwnerDeletionHTTPRejectsReferencesAndRollsBack(t *testing.T) {
	f, book := collectionFixture(t)
	peer := model.Peer{Id: "owner-delete-ref", Uuid: "owner-delete-uuid", UserId: f.hidden.Id, AbId: book.Id, Alias: "preserve"}
	compatCreate(t, &peer)
	body := fmt.Sprintf(`{"id":%d}`, f.owner.Id)
	before := collectionSnapshot(t, f)
	definitionResult(t, f.call("POST", "/api/admin/user/delete", body), 200, 101)
	if !reflect.DeepEqual(before, collectionSnapshot(t, f)) {
		t.Fatal("referenced owner deletion changed collection scopes")
	}
	var owner model.User
	if err := f.db.First(&owner, f.owner.Id).Error; err != nil {
		t.Fatal("referenced owner was deleted", err)
	}
	if err := f.db.Model(&peer).Update("ab_id", 0).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec("CREATE TRIGGER reject_collection_owner_delete BEFORE DELETE ON users BEGIN SELECT RAISE(ABORT,'private user deletion SQL'); END").Error; err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/api/admin/user/delete", body)
	definitionResult(t, w, 200, 101)
	if strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "SQL") || !reflect.DeepEqual(before, collectionSnapshot(t, f)) {
		t.Fatal("owner deletion SQL failure leaked cause or retained partial cleanup", w.Body.String())
	}
	if err := f.db.Exec("DROP TRIGGER reject_collection_owner_delete").Error; err != nil {
		t.Fatal(err)
	}
	definitionResult(t, f.call("POST", "/api/admin/user/delete", body), 200, 0)
	for _, row := range []any{&model.AddressBook{}, &model.Tag{}, &model.AddressBookCollectionRule{}} {
		var count int64
		if err := f.db.Model(row).Where("collection_id=?", book.Id).Count(&count).Error; err != nil || count != 0 {
			t.Fatal("owner deletion left collection child", count, err)
		}
	}
	var users int64
	if err := f.db.Model(&model.User{}).Where("id=?", f.owner.Id).Count(&users).Error; err != nil || users != 0 {
		t.Fatal("unreferenced owner deletion failed", users, err)
	}
}
