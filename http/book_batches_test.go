package http

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/bookadmin"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

type batchHTTPCase struct {
	path, body string
	actor      model.User
	peers      []model.Peer
	rows       []model.AddressBook
}

func batchHTTPState(t *testing.T, f definitionFixture) []any {
	t.Helper()
	var peers []model.Peer
	if err := f.db.Order("row_id").Find(&peers).Error; err != nil {
		t.Fatal(err)
	}
	return append(collectionSnapshot(t, f), peers)
}

func seedBatchHTTPCase(t *testing.T, f definitionFixture, book model.AddressBookCollection, name string, atomic bool) batchHTTPCase {
	t.Helper()
	c := batchHTTPCase{actor: f.owner}
	for _, id := range []string{"source-first", "source-second"} {
		peer := model.Peer{Id: id, Uuid: "private-" + id, UserId: f.owner.Id, Hostname: "source-host", Username: "source-user", Os: "Linux",
			Alias: "do not map alias", Note: "do not map note", AbId: book.Id, GroupId: 33, PresetAbAlias: "preserve preset"}
		compatCreate(t, &peer)
		c.peers = append(c.peers, peer)
	}
	var original model.AddressBook
	if err := f.db.Where("collection_id=?", book.Id).First(&original).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&original).UpdateColumn("note", "preserve entry note").Error; err != nil {
		t.Fatal(err)
	}
	second := model.AddressBook{UserId: f.owner.Id, Id: "another-entry", Alias: "preserve alias", Note: "preserve second note", Tags: custom_types.AutoJson(`["old"]`)}
	compatCreate(t, &second)
	c.rows = []model.AddressBook{original, second}
	switch name {
	case "stock":
		c.path, c.actor = "/api/admin/address_book/batchCreate", f.admin
		owners, device := fmt.Sprintf("[%d]", f.owner.Id), "stock-new"
		if atomic {
			owners, device = fmt.Sprintf("[%d,%d]", f.owner.Id, f.hidden.Id), "stock-atomic"
		}
		c.body = fmt.Sprintf(`{"id":%q,"user_ids":%s,"user_id":%d,"collection_id":%d,"alias":"batch alias","password":"raw-private-password","hash":"input hash","username":"dto user","hostname":"dto host","platform":"Linux","forceAlwaysRelay":true,"rdpPort":"3390","rdpUsername":"rdp-user","online":true,"loginName":"login-user","sameServer":true,"tags":["new","new"],"note":"ignored"}`, device, owners, f.hidden.Id, book.Id)
	case "admin-peers", "my-peers":
		c.path = "/api/admin/my/address_book/batchCreateFromPeers"
		if name == "admin-peers" {
			c.path, c.actor = "/api/admin/address_book/batchCreateFromPeers", f.admin
		}
		c.body = fmt.Sprintf(`{"peer_ids":[%d,%d],"user_id":%d,"collection_id":%d,"tags":["new","new"],"alias":"ignored","note":"ignored"}`, c.peers[1].RowId, c.peers[0].RowId, book.UserId, book.Id)
	case "tags":
		c.path = "/api/admin/my/address_book/batchUpdateTags"
		c.body = fmt.Sprintf(`{"row_ids":[%d,%d],"tags":["new","new"],"user_id":%d,"alias":"ignored","note":"ignored"}`, second.RowId, original.RowId, f.hidden.Id)
	default:
		t.Fatal("unknown case", name)
	}
	return c
}

func assertBatchHTTPControl(t *testing.T, f definitionFixture, book model.AddressBookCollection, c batchHTTPCase, name string, atomic bool) {
	t.Helper()
	for _, peer := range c.peers {
		var got model.Peer
		if err := f.db.First(&got, peer.RowId).Error; err != nil || got.AbId != peer.AbId || got.Alias != peer.Alias || got.Note != peer.Note || got.GroupId != peer.GroupId || got.Hostname != peer.Hostname || got.Username != peer.Username || got.PresetAbAlias != peer.PresetAbAlias {
			t.Fatal("batch changed registered-peer management fields", err)
		}
	}
	if name == "tags" {
		for i, row := range c.rows {
			var got model.AddressBook
			if err := f.db.First(&got, row.RowId).Error; err != nil || got.Tags.String() != `["new","new"]` || got.Alias != row.Alias || (i == 0 && got.Note != "preserve entry note") || (i == 1 && got.Note != row.Note) {
				t.Fatal("valid tags batch missed its whole target set or changed other fields", err)
			}
		}
		return
	}
	if name == "stock" {
		device, cid, owners := "stock-new", book.Id, []uint{f.owner.Id}
		if atomic {
			device, cid, owners = "stock-atomic", 0, []uint{f.owner.Id, f.hidden.Id}
		}
		for _, owner := range owners {
			var got model.AddressBook
			err := f.db.Where("id=? AND user_id=? AND collection_id=?", device, owner, cid).First(&got).Error
			wantTags := `["new","new"]`
			if atomic {
				wantTags = `[]`
			}
			if err != nil || got.Alias != "batch alias" || got.Password != "raw-private-password" || got.Hash != "input hash" || got.Tags.String() != wantTags || got.Note != "" || got.Username != "dto user" || got.Hostname != "dto host" || got.Platform != "Linux" || !got.ForceAlwaysRelay || got.RdpPort != "3390" || got.RdpUsername != "rdp-user" || !got.Online || got.LoginName != "login-user" || !got.SameServer {
				t.Fatal("flattened stock DTO did not reach the correct owner/columns", err)
			}
		}
		return
	}
	for _, source := range c.peers {
		var got model.AddressBook
		if err := f.db.Where("id=? AND user_id=? AND collection_id=?", source.Id, book.UserId, book.Id).First(&got).Error; err != nil || got.Username != source.Username || got.Hostname != source.Hostname || got.Platform != "Linux" || got.Tags.String() != `["new","new"]` || got.Alias != "" || got.Note != "" {
			t.Fatal("source peer batch failed pure-mapping control", err)
		}
	}
}

func TestManagementBatchesHTTPPostAuthRevokedAndRestoredControls(t *testing.T) {
	for _, name := range []string{"stock", "admin-peers", "my-peers", "tags"} {
		t.Run(name, func(t *testing.T) {
			f, book := bookManagementFixture(t)
			c := seedBatchHTTPCase(t, f, book, name, false)
			before, fired := batchHTTPState(t, f), false
			reader := &collectionChangingReader{Reader: strings.NewReader(c.body), change: func() error {
				fired = true
				return f.db.Exec("DELETE FROM user_tokens WHERE token=?", f.tokens[c.actor.Id]).Error
			}}
			r := httptest.NewRequest("POST", c.path, reader)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("api-token", f.tokens[c.actor.Id])
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			definitionResult(t, w, 200, 403)
			if !fired || !reflect.DeepEqual(before, batchHTTPState(t, f)) {
				t.Fatal("post-auth revoked batch retained writes")
			}
			compatCreate(t, &model.UserToken{UserId: c.actor.Id, Token: f.tokens[c.actor.Id], ExpiredAt: time.Now().Add(time.Hour).Unix()})
			definitionResult(t, f.request(context.Background(), "POST", c.path, c.body, f.tokens[c.actor.Id]), 200, 0)
			assertBatchHTTPControl(t, f, book, c, name, false)
		})
	}
}

type batchHTTPFailedCommit struct {
	*sql.Tx
	reached *bool
}

func (tx *batchHTTPFailedCommit) Commit() error {
	*tx.reached = true
	_ = tx.Tx.Rollback()
	return errors.New("private batch commit error")
}

type batchHTTPCommitPool struct {
	gorm.ConnPool
	db      *sql.DB
	reached *bool
}

func (pool batchHTTPCommitPool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := pool.db.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &batchHTTPFailedCommit{Tx: tx, reached: pool.reached}, nil
}

func installBatchHTTPStore(t *testing.T, f definitionFixture, db *gorm.DB) {
	t.Helper()
	sessions, err := app.NewSessions(f.db, time.Hour, global.Jwt.Key)
	if err != nil {
		t.Fatal(err)
	}
	books, err := bookadmin.New(db, bookadmin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.New(sessions, httpapi.Options{Translate: LegacyTranslator(), Books: books})
	if err != nil {
		t.Fatal(err)
	}
	f.handler, err = NewHandler(handler)
	if err != nil {
		t.Fatal(err)
	}
}

func TestManagementBatchesHTTPAtomicSQLAndActualCommitFailures(t *testing.T) {
	for _, name := range []string{"stock", "admin-peers", "my-peers", "tags"} {
		for _, fault := range []string{"sql", "commit"} {
			t.Run(name+"/"+fault, func(t *testing.T) {
				f, book := bookManagementFixture(t)
				c := seedBatchHTTPCase(t, f, book, name, true)
				before, reached := batchHTTPState(t, f), false
				if fault == "sql" {
					trigger := ""
					switch name {
					case "stock":
						owners := []uint{f.owner.Id, f.hidden.Id}
						sort.Slice(owners, func(i, j int) bool { return owners[i] < owners[j] })
						trigger = fmt.Sprintf("CREATE TRIGGER reject_batch_http BEFORE INSERT ON address_books WHEN NEW.user_id=%d BEGIN SELECT RAISE(ABORT,'private SQL batch error'); END", owners[1])
					case "tags":
						trigger = fmt.Sprintf("CREATE TRIGGER reject_batch_http BEFORE UPDATE ON address_books WHEN NEW.row_id=%d BEGIN SELECT RAISE(ABORT,'private SQL batch error'); END", c.rows[1].RowId)
					default:
						trigger = "CREATE TRIGGER reject_batch_http BEFORE INSERT ON address_books WHEN NEW.id='source-second' BEGIN SELECT RAISE(ABORT,'private SQL batch error'); END"
					}
					if err := f.db.Exec(trigger).Error; err != nil {
						t.Fatal(err)
					}
				} else {
					owned := f.db.Session(&gorm.Session{NewDB: true, Context: context.Background()})
					pool, err := f.db.DB()
					if err != nil {
						t.Fatal(err)
					}
					owned.Statement.ConnPool = batchHTTPCommitPool{ConnPool: f.db.Statement.ConnPool, db: pool, reached: &reached}
					installBatchHTTPStore(t, f, owned)
				}
				w := f.request(context.Background(), "POST", c.path, c.body, f.tokens[c.actor.Id])
				definitionResult(t, w, 503, 503)
				if (fault == "commit" && !reached) || strings.Contains(w.Body.String(), "private") || !reflect.DeepEqual(before, batchHTTPState(t, f)) {
					t.Fatal("batch failure did not reach Commit, leaked, or retained partial writes")
				}
				if fault == "sql" {
					if err := f.db.Exec("DROP TRIGGER reject_batch_http").Error; err != nil {
						t.Fatal(err)
					}
				} else {
					installBatchHTTPStore(t, f, f.db)
				}
				definitionResult(t, f.request(context.Background(), "POST", c.path, c.body, f.tokens[c.actor.Id]), 200, 0)
				assertBatchHTTPControl(t, f, book, c, name, true)
			})
		}
	}
}

func TestManagementBatchesHTTPWholeSetAdmissionAndLegacyFields(t *testing.T) {
	f, book := bookManagementFixture(t)
	c := seedBatchHTTPCase(t, f, book, "tags", false)
	foreign := model.AddressBook{UserId: f.hidden.Id, Id: "foreign-entry", Note: "preserve foreign", Tags: custom_types.AutoJson(`[]`)}
	compatCreate(t, &foreign)
	before := batchHTTPState(t, f)
	for _, body := range []string{
		fmt.Sprintf(`{"row_ids":[%d,%d],"tags":[]}`, c.rows[0].RowId, foreign.RowId),
		fmt.Sprintf(`{"row_ids":[%d,99999],"tags":[]}`, c.rows[0].RowId),
	} {
		w := f.request(context.Background(), "POST", c.path, body, f.tokens[f.owner.Id])
		if w.Code != 200 || strings.Contains(w.Body.String(), `"code":0`) || !reflect.DeepEqual(before, batchHTTPState(t, f)) {
			t.Fatal("tag batch accepted partial/foreign target set", w.Body.String())
		}
	}
	// Duplicate UserIDs retain the raw multiple-user switch, including clearing
	// tags and overriding the supplied collection before canonicalizing owners.
	body := fmt.Sprintf(`{"id":"duplicate-target","user_ids":[%d,%d],"user_id":%d,"collection_id":%d,"tags":["ignored"],"alias":"alias"}`, f.owner.Id, f.owner.Id, f.hidden.Id, book.Id)
	definitionResult(t, f.call("POST", "/api/admin/address_book/batchCreate", body), 200, 0)
	var got model.AddressBook
	if err := f.db.Where("id=? AND user_id=?", "duplicate-target", f.owner.Id).First(&got).Error; err != nil || got.CollectionId != 0 || got.Tags.String() != `[]` {
		t.Fatal("legacy raw multi-user JSON behavior was lost", err)
	}
	var count int64
	if err := f.db.Model(&model.AddressBook{}).Where("id=?", got.Id).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("canonical target wrote more than one physical row", count, err)
	}
}

func TestManagementBatchesHTTPReplacesAll1001Rows(t *testing.T) {
	f, book := bookManagementFixture(t)
	rows := make([]model.AddressBook, 1001)
	for i := range rows {
		rows[i] = model.AddressBook{UserId: f.owner.Id, CollectionId: book.Id, Id: fmt.Sprintf("many-%04d", i), Alias: "preserve alias", Note: "preserve note", Tags: custom_types.AutoJson(`["old"]`)}
	}
	if err := f.db.CreateInBatches(&rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	ids := make([]uint, len(rows))
	for i := range rows {
		ids[len(rows)-i-1] = rows[i].RowId
	}
	body, err := json.Marshal(map[string]any{"row_ids": ids, "tags": []string{"all"}, "note": "ignored", "alias": "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	definitionResult(t, f.request(context.Background(), "POST", "/api/admin/my/address_book/batchUpdateTags", string(body), f.tokens[f.owner.Id]), 200, 0)
	var count int64
	if err := f.db.Model(&model.AddressBook{}).Where("collection_id=? AND tags=? AND alias=? AND note=?", book.Id, `["all"]`, "preserve alias", "preserve note").Count(&count).Error; err != nil || count != 1001 {
		t.Fatal("HTTP batch truncated or changed protected fields", count, err)
	}
}
