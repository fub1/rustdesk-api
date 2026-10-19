package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

func TestClientCompatTagReferencesRenameDelete(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%t", shared), func(t *testing.T) {
			f := newCompatFixture(t)
			owner, cid := f.actor, uint(0)
			if shared {
				owner = f.owner
				book := model.AddressBookCollection{UserId: owner.Id, Name: "shared-tags"}
				compatCreate(t, &book)
				cid = book.Id
				grants := []model.AddressBookCollectionRule{
					{UserId: owner.Id, CollectionId: cid, Type: 1, ToId: f.actor.Id, Rule: 1},
					{UserId: owner.Id, CollectionId: cid, Type: 2, ToId: f.actor.GroupId, Rule: 3},
				}
				compatCreate(t, &grants)
			}
			labels := []model.Tag{{UserId: owner.Id, CollectionId: cid, Name: "old", Color: 0xff123456}, {UserId: owner.Id, CollectionId: cid, Name: "unused", Color: 17}}
			compatCreate(t, &labels)
			rows := make([]model.AddressBook, 1007)
			for i := range rows {
				rows[i] = compatRow(fmt.Sprintf("tag-peer-%04d", i), owner, cid)
				rows[i].Tags = custom_types.AutoJson(`["before","old","new","after","old","after"]`)
				rows[i].Alias, rows[i].Note = "alias", "note"
				rows[i].Password, rows[i].Hash = "password", "hash"
				rows[i].ForceAlwaysRelay, rows[i].SameServer = true, true
				rows[i].RdpPort, rows[i].RdpUsername, rows[i].LoginName = "3389", "rdp-user", "login-user"
			}
			rows[1006].Id = rows[0].Id // Physical duplicate IDs must also be maintained.
			compatCreate(t, &rows)
			otherBook := model.AddressBookCollection{UserId: owner.Id, Name: "unrelated"}
			compatCreate(t, &otherBook)
			untouched := []model.AddressBook{compatRow("other-owner", f.hidden, 0), compatRow("other-book", owner, otherBook.Id)}
			if shared {
				untouched = append(untouched, compatRow("owner-personal", owner, 0))
			}
			for i := range untouched {
				untouched[i].Tags = custom_types.AutoJson(`["old"]`)
			}
			compatCreate(t, &untouched)
			guid := compatGUID(owner, cid)
			w := f.call("PUT", "/api/ab/tag/rename/"+guid, `{"old":"old","new":"new"}`, f.actor)
			if w.Code != 200 || w.Body.Len() != 0 {
				t.Fatalf("rename: %d %s", w.Code, w.Body.String())
			}
			// The client pulls peers and filters their string arrays after rename.
			matched := 0
			for page := 1; page <= 2; page++ {
				read := f.call("POST", fmt.Sprintf("/api/ab/peers?ab=%s&current=%d&pageSize=1000", guid, page), "", f.actor)
				var body struct{ Data []struct{ Tags []string } }
				if read.Code != 200 || json.Unmarshal(read.Body.Bytes(), &body) != nil {
					t.Fatal("peer pull failed", read.Body.String())
				}
				for _, peer := range body.Data {
					if !reflect.DeepEqual(peer.Tags, []string{"before", "new", "after", "after"}) {
						t.Fatalf("rename did not maintain peer membership/order: %v", peer.Tags)
					}
					matched++
				}
			}
			if matched != len(rows) {
				t.Fatalf("new tag filter matched %d of %d physical rows", matched, len(rows))
			}
			var label model.Tag
			if err := global.DB.First(&label, labels[0].Id).Error; err != nil || label.Name != "new" || label.Color != labels[0].Color {
				t.Fatalf("rename changed color or lost tag: %+v %v", label, err)
			}
			w = f.call("DELETE", "/api/ab/tag/"+guid, `["new","unused","new"]`, f.actor)
			if w.Code != 200 || w.Body.Len() != 0 {
				t.Fatalf("delete: %d %s", w.Code, w.Body.String())
			}
			var stored []model.AddressBook
			if err := global.DB.Where("user_id = ? AND collection_id = ?", owner.Id, cid).Order("row_id").Find(&stored).Error; err != nil {
				t.Fatal(err)
			}
			if len(stored) != len(rows) {
				t.Fatal("reference maintenance changed peer count")
			}
			for i, row := range stored {
				if string(row.Tags) != `["before","after","after"]` {
					t.Fatalf("deleted tag reference survived: %s", row.Tags)
				}
				row.Tags = rows[i].Tags
				if !time.Time(row.CreatedAt).Equal(time.Time(rows[i].CreatedAt)) || !time.Time(row.UpdatedAt).Equal(time.Time(rows[i].UpdatedAt)) {
					t.Fatal("reference maintenance changed peer timestamps")
				}
				row.TimeModel = rows[i].TimeModel
				if !reflect.DeepEqual(row, rows[i]) {
					t.Fatalf("reference maintenance changed another peer field: %+v", row)
				}
			}
			for _, row := range untouched {
				var got model.AddressBook
				if err := global.DB.First(&got, row.RowId).Error; err != nil || string(got.Tags) != `["old"]` {
					t.Fatal("reference maintenance escaped owner/collection", err)
				}
			}
		})
	}
}

func TestClientCompatTagReferenceFailuresRollback(t *testing.T) {
	for _, action := range []string{"rename", "delete"} {
		for _, fault := range []string{"corrupt", "reference-query", "reference-write", "tag-write", "cas", "cancel"} {
			t.Run(action+"/"+fault, func(t *testing.T) {
				f := newCompatFixture(t)
				labels := []model.Tag{{UserId: f.actor.Id, Name: "old", Color: 7}, {UserId: f.actor.Id, Name: "spare", Color: 8}}
				compatCreate(t, &labels)
				if err := global.DB.Order("id").Find(&labels).Error; err != nil {
					t.Fatal(err)
				}
				rows := []model.AddressBook{compatRow("first", f.actor, 0), compatRow("second", f.actor, 0)}
				for i := range rows {
					rows[i].Tags = custom_types.AutoJson(`["old","spare","keep"]`)
				}
				compatCreate(t, &rows)
				if fault == "corrupt" {
					if err := global.DB.Model(&model.AddressBook{}).Where("row_id = ?", rows[1].RowId).UpdateColumn("tags", `{"bad":true}`).Error; err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				fired, writes := false, 0
				fail := func(tx *gorm.DB) { fired = true; tx.AddError(errors.New("private-reference-fault")) }
				remove := func() {}
				switch fault {
				case "reference-query":
					if err := global.DB.Callback().Query().Before("gorm:query").Register("tags:injected", func(tx *gorm.DB) {
						if tx.Statement.Table == "address_books" {
							fail(tx)
						}
					}); err != nil {
						t.Fatal(err)
					}
					remove = func() { _ = global.DB.Callback().Query().Remove("tags:injected") }
				case "reference-write", "cas", "cancel":
					if err := global.DB.Callback().Update().Before("gorm:update").Register("tags:injected", func(tx *gorm.DB) {
						if tx.Statement.Table != "address_books" {
							return
						}
						writes++
						if fault == "reference-write" && writes == 2 {
							fail(tx)
						}
						if fault == "cas" && writes == 1 {
							fired = true
							// Deliberately change the raw value before the guarded write.
							if err := tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE address_books SET tags = ? WHERE row_id = ?", `["OLD","spare","keep"]`, rows[0].RowId).Error; err != nil {
								tx.AddError(err)
							}
						}
						if fault == "cancel" && writes == 2 {
							fired = true
							cancel()
						}
					}); err != nil {
						t.Fatal(err)
					}
					remove = func() { _ = global.DB.Callback().Update().Remove("tags:injected") }
				case "tag-write":
					if action == "rename" {
						if err := global.DB.Callback().Update().Before("gorm:update").Register("tags:injected", func(tx *gorm.DB) {
							if tx.Statement.Table == "tags" {
								fail(tx)
							}
						}); err != nil {
							t.Fatal(err)
						}
						remove = func() { _ = global.DB.Callback().Update().Remove("tags:injected") }
					} else {
						if err := global.DB.Callback().Delete().Before("gorm:delete").Register("tags:injected", func(tx *gorm.DB) {
							if tx.Statement.Table == "tags" {
								writes++
								if writes == 2 {
									fail(tx)
								}
							}
						}); err != nil {
							t.Fatal(err)
						}
						remove = func() { _ = global.DB.Callback().Delete().Remove("tags:injected") }
					}
				}
				defer remove()
				method, path, body := "PUT", "/api/ab/tag/rename/", `{"old":"old","new":"new"}`
				if action == "delete" {
					method, path, body = "DELETE", "/api/ab/tag/", `["old","spare"]`
				}
				r := httptest.NewRequest(method, path+compatGUID(f.actor, 0), strings.NewReader(body)).WithContext(ctx)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+f.tokens[f.actor.Id])
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				remove()
				wantStatus := 503
				if fault == "cancel" {
					wantStatus = 408
				}
				if w.Code != wantStatus || strings.Contains(w.Body.String(), "private-reference-fault") {
					t.Fatalf("failed tag transaction acknowledged/leaked fault: %d %s", w.Code, w.Body.String())
				}
				if fault != "corrupt" && !fired {
					t.Fatal("injected failure was never reached")
				}
				var got []model.Tag
				if err := global.DB.Order("id").Find(&got).Error; err != nil || !reflect.DeepEqual(got, labels) {
					t.Fatalf("failed transaction changed tags/colors: %+v %v", got, err)
				}
				for i, row := range rows {
					var raw string
					if err := global.DB.Raw("SELECT tags FROM address_books WHERE row_id = ?", row.RowId).Scan(&raw).Error; err != nil {
						t.Fatal(err)
					}
					want := string(row.Tags)
					if fault == "corrupt" && i == 1 {
						want = `{"bad":true}`
					}
					if raw != want {
						t.Fatalf("failed transaction changed reference %d: %s", i, raw)
					}
				}
			})
		}
	}
}

func TestClientCompatTagUnreferencedRenameDeletePreserveOtherRawTags(t *testing.T) {
	f := newCompatFixture(t)
	labels := []model.Tag{{UserId: f.actor.Id, Name: "old", Color: 7}, {UserId: f.actor.Id, Name: "unused", Color: 8}}
	compatCreate(t, &labels)
	rows := []model.AddressBook{compatRow("unrelated-target-duplicates", f.actor, 0), compatRow("empty", f.actor, 0)}
	rows[0].Tags = custom_types.AutoJson(`[ "new", "new", "keep" ]`)
	compatCreate(t, &rows)
	guid := compatGUID(f.actor, 0)
	for _, action := range []struct{ method, path, body string }{
		{"PUT", "/api/ab/tag/rename/", `{"old":"old","new":"new"}`},
		{"DELETE", "/api/ab/tag/", `["unused"]`},
	} {
		w := f.call(action.method, action.path+guid, action.body, f.actor)
		if w.Code != 200 {
			t.Fatalf("zero-reference action: %d %s", w.Code, w.Body.String())
		}
		for _, row := range rows {
			var raw string
			if err := global.DB.Raw("SELECT tags FROM address_books WHERE row_id = ?", row.RowId).Scan(&raw).Error; err != nil {
				t.Fatal(err)
			}
			if raw != string(row.Tags) {
				t.Fatalf("unrelated raw tags changed: %q", raw)
			}
		}
	}
	if err := global.DB.Delete(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if w := f.call("DELETE", "/api/ab/tag/"+guid, `["new"]`, f.actor); w.Code != 200 {
		t.Fatal("empty book tag deletion failed", w.Body.String())
	}
}
