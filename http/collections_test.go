package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionadmin"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	"github.com/lejianwen/rustdesk-api/v2/model"
)

func collectionFixture(t *testing.T) (definitionFixture, model.AddressBookCollection) {
	t.Helper()
	f := newDefinitionFixture(t)
	sessions, err := app.NewSessions(f.db, time.Hour, global.Jwt.Key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := collectionadmin.New(f.db, collectionadmin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h, err := httpapi.New(sessions, httpapi.Options{Translate: LegacyTranslator(), Collections: store})
	if err != nil {
		t.Fatal(err)
	}
	f.handler, err = NewHandler(h)
	if err != nil {
		t.Fatal(err)
	}
	book := model.AddressBookCollection{UserId: f.owner.Id, Name: "original"}
	compatCreate(t, &book)
	compatCreate(t, &model.AddressBook{Id: "device", UserId: f.owner.Id, CollectionId: book.Id, Alias: "original-entry"})
	compatCreate(t, &model.AddressBookCollectionRule{UserId: f.owner.Id, CollectionId: book.Id, ToId: f.actor.Id, Type: 1, Rule: 3})
	compatCreate(t, &model.Tag{UserId: f.owner.Id, CollectionId: book.Id, Name: "original-tag"})
	return f, book
}

func collectionSnapshot(t *testing.T, f definitionFixture) []any {
	t.Helper()
	var books []model.AddressBookCollection
	var entries []model.AddressBook
	var rules []model.AddressBookCollectionRule
	var tags []model.Tag
	for _, rows := range []any{&books, &entries, &rules, &tags} {
		if err := f.db.Order("1").Find(rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	return []any{books, entries, rules, tags}
}

type collectionChangingReader struct {
	io.Reader
	change func() error
}

func (r *collectionChangingReader) Read(p []byte) (int, error) {
	if r.change != nil {
		change := r.change
		r.change = nil
		if err := change(); err != nil {
			return 0, err
		}
	}
	return r.Reader.Read(p)
}

func TestCollectionsHTTPPostAuthenticationRevocation(t *testing.T) {
	for _, scope := range []string{"admin", "my"} {
		for _, operation := range []string{"create", "update", "delete"} {
			for _, change := range []string{"revoked", "disabled", "demoted", "expired"} {
				if scope == "my" && change == "demoted" {
					continue
				}
				t.Run(scope+"/"+operation+"/"+change, func(t *testing.T) {
					f, book := collectionFixture(t)
					user, prefix := f.admin, "/api/admin/address_book_collection/"
					if scope == "my" {
						user, prefix = f.owner, "/api/admin/my/address_book_collection/"
					}
					before := collectionSnapshot(t, f)
					body := fmt.Sprintf(`{"id":%d,"name":"changed"}`, book.Id)
					if operation == "create" {
						body = fmt.Sprintf(`{"name":"created","user_id":%d}`, f.owner.Id)
					}
					fired := false
					reader := &collectionChangingReader{Reader: strings.NewReader(body), change: func() error {
						fired = true
						switch change {
						case "revoked":
							return f.db.Exec("DELETE FROM user_tokens WHERE token=?", f.tokens[user.Id]).Error
						case "disabled":
							return f.db.Exec("UPDATE users SET status=? WHERE id=?", model.COMMON_STATUS_DISABLED, user.Id).Error
						case "demoted":
							return f.db.Exec("UPDATE users SET is_admin=? WHERE id=?", false, user.Id).Error
						default:
							return f.db.Exec("UPDATE user_tokens SET expired_at=? WHERE token=?", time.Now().Add(-time.Minute).Unix(), f.tokens[user.Id]).Error
						}
					}}
					r := httptest.NewRequest("POST", prefix+operation, reader)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("api-token", f.tokens[user.Id])
					w := httptest.NewRecorder()
					f.handler.ServeHTTP(w, r)
					definitionResult(t, w, 200, 403)
					if !fired || !reflect.DeepEqual(before, collectionSnapshot(t, f)) {
						t.Fatal("post-authentication change retained collection writes")
					}
				})
			}
		}
	}
}

func TestCollectionsHTTPImmutableOwnerAndLegacyPayload(t *testing.T) {
	f, book := collectionFixture(t)
	for _, scope := range []string{"admin", "my"} {
		user, prefix := f.admin, "/api/admin/address_book_collection/"
		if scope == "my" {
			user, prefix = f.owner, "/api/admin/my/address_book_collection/"
		}
		before := collectionSnapshot(t, f)
		definitionResult(t, f.request(context.Background(), "POST", prefix+"update", fmt.Sprintf(`{"id":%d,"name":"moved","user_id":%d}`, book.Id, f.hidden.Id), f.tokens[user.Id]), 200, 101)
		if !reflect.DeepEqual(before, collectionSnapshot(t, f)) {
			t.Fatal("rejected owner transfer changed child scopes")
		}
		for _, owner := range []uint{0, f.owner.Id} {
			body := fmt.Sprintf(`{"id":%d,"name":"renamed","user_id":%d,"created_at":"2000-01-01","updated_at":null,"unknown":"legacy"}`, book.Id, owner)
			definitionResult(t, f.request(context.Background(), "POST", prefix+"update", body, f.tokens[user.Id]), 200, 0)
		}
	}
	// A full-control grantee remains unable to rename/delete collection metadata.
	for _, operation := range []string{"update", "delete"} {
		definitionResult(t, f.request(context.Background(), "POST", "/api/admin/my/address_book_collection/"+operation, fmt.Sprintf(`{"id":%d,"name":"forbidden"}`, book.Id), f.tokens[f.actor.Id]), 200, 403)
	}
	var got model.AddressBookCollection
	if err := f.db.First(&got, book.Id).Error; err != nil || got.UserId != book.UserId || !time.Time(got.CreatedAt).Equal(time.Time(book.CreatedAt)) {
		t.Fatal("protected metadata changed", err)
	}
	definitionResult(t, f.request(context.Background(), "POST", "/api/admin/my/address_book_collection/create", `{"id":0,"name":"own"}`, f.tokens[f.actor.Id]), 200, 0)
	definitionResult(t, f.call("POST", "/api/admin/address_book_collection/create", fmt.Sprintf(`{"id":%d,"name":"bad","user_id":%d}`, book.Id, f.owner.Id)), 200, 101)
}

func TestCollectionsHTTPReferencedDeleteAndScopedCleanup(t *testing.T) {
	f, book := collectionFixture(t)
	peer := model.Peer{Id: "primary-peer", Uuid: "primary-uuid", UserId: f.owner.Id, AbId: book.Id, Alias: "primary-alias"}
	compatCreate(t, &peer)
	heartbeat := func() *httptest.ResponseRecorder {
		return f.request(context.Background(), "POST", "/api/heartbeat", `{"id":"primary-peer","uuid":"primary-uuid","ver":1005001,"modified_at":0}`, "")
	}
	if w := heartbeat(); w.Code != 200 {
		t.Fatal("primary-book heartbeat control failed", w.Code, w.Body.String())
	}
	before := collectionSnapshot(t, f)
	definitionResult(t, f.call("POST", "/api/admin/address_book_collection/delete", fmt.Sprintf(`{"id":%d}`, book.Id)), 200, 101)
	if !reflect.DeepEqual(before, collectionSnapshot(t, f)) {
		t.Fatal("referenced delete partially removed collection")
	}
	if w := heartbeat(); w.Code != 200 {
		t.Fatal("rejected deletion broke heartbeat", w.Code, w.Body.String())
	}
	var retained model.Peer
	if err := f.db.First(&retained, peer.RowId).Error; err != nil || retained.AbId != book.Id || retained.Alias != peer.Alias {
		t.Fatal("reference rejection detached peer or alias", err)
	}
	if err := f.db.Model(&peer).Update("ab_id", 0).Error; err != nil {
		t.Fatal(err)
	}
	// Historical mismatched owner rows still belong to the collection by its ID.
	compatCreate(t, &model.Tag{UserId: f.hidden.Id, CollectionId: book.Id, Name: "mismatched-owner"})
	compatCreate(t, &model.Tag{UserId: f.owner.Id, Name: "personal-keep"})
	definitionResult(t, f.call("POST", "/api/admin/address_book_collection/delete", fmt.Sprintf(`{"id":%d}`, book.Id)), 200, 0)
	for _, row := range []any{&model.AddressBook{}, &model.Tag{}, &model.AddressBookCollectionRule{}} {
		var n int64
		if err := f.db.Model(row).Where("collection_id=?", book.Id).Count(&n).Error; err != nil || n != 0 {
			t.Fatal("collection child survived", n, err)
		}
	}
	var keep int64
	if err := f.db.Model(&model.Tag{}).Where("collection_id=0").Count(&keep).Error; err != nil || keep != 1 {
		t.Fatal("personal scope was cleared", keep, err)
	}
}

func TestCollectionsHTTPInputPaginationAndSafeFailure(t *testing.T) {
	f, book := collectionFixture(t)
	before := collectionSnapshot(t, f)
	for _, body := range []string{`null`, `[]`, `{}`, `{"id":-1,"name":"bad"}`, `{"id":1,"name":1}`, `{"id":1,"name":"bad"} {}`, strings.Repeat(" ", 1<<20) + `{}`} {
		definitionResult(t, f.call("POST", "/api/admin/address_book_collection/update", body), 200, 101)
	}
	for _, query := range []string{"page=-1", "page_size=1001", "page=1&page=2", "page_size=", "page=9223372036854775807&page_size=1000"} {
		definitionResult(t, f.call("GET", "/api/admin/address_book_collection/list?"+query, ""), 200, 101)
	}
	if !reflect.DeepEqual(before, collectionSnapshot(t, f)) {
		t.Fatal("invalid input changed rows")
	}
	raw := definitionResult(t, f.call("GET", "/api/admin/address_book_collection/list?page=0&page_size=0&user_id="+fmt.Sprint(f.owner.Id), ""), 200, 0)
	var page struct {
		List []struct {
			ID uint `json:"id"`
		}
		Total    int64
		Page     int
		PageSize int `json:"page_size"`
	}
	if json.Unmarshal(raw, &page) != nil || page.Total != 1 || len(page.List) != 1 || page.List[0].ID != book.Id || page.Page != 1 || page.PageSize != 10 {
		t.Fatal("legacy page envelope changed", string(raw))
	}
	if err := f.db.Exec("CREATE TRIGGER reject_collection_delete BEFORE DELETE ON tags BEGIN SELECT RAISE(ABORT,'private secret SQL'); END").Error; err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/api/admin/address_book_collection/delete", fmt.Sprintf(`{"id":%d}`, book.Id))
	definitionResult(t, w, 503, 503)
	if strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "SQL") || !reflect.DeepEqual(before, collectionSnapshot(t, f)) {
		t.Fatal("failed delete leaked SQL or retained partial writes")
	}
}
