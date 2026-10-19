package http

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSciterWriteInitialAndResetWithoutColors(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(fmt.Sprintf("populated=%t", populated), func(t *testing.T) {
			f := newCompatFixture(t)
			book := model.AddressBookCollection{UserId: f.actor.Id, Name: "shared-protected"}
			compatCreate(t, &book)
			protected := []model.AddressBook{compatRow("shared", f.actor, book.Id), compatRow("foreign", f.hidden, 0)}
			compatCreate(t, &protected)
			labels := []model.Tag{{UserId: f.actor.Id, CollectionId: book.Id, Name: "shared", Color: 7}, {UserId: f.hidden.Id, Name: "foreign", Color: 8}}
			compatCreate(t, &labels)
			if populated {
				row := compatRow("personal", f.actor, 0)
				compatCreate(t, &row)
				label := model.Tag{UserId: f.actor.Id, Name: "old", Color: 9}
				compatCreate(t, &label)
			}
			w := f.call("POST", "/api/ab", compatDocument(t, map[string]any{"tags": []string{}, "peers": []any{}}), f.actor)
			if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "null" {
				t.Fatalf("Sciter initial/reset rejected: %d %s", w.Code, w.Body.String())
			}
			for _, table := range []any{&model.AddressBook{}, &model.Tag{}} {
				var count int64
				if err := global.DB.Model(table).Where("user_id = ? AND collection_id = 0", f.actor.Id).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("Sciter reset retained personal facts: %d %v", count, err)
				}
			}
			for _, row := range protected {
				var got model.AddressBook
				if err := global.DB.First(&got, row.RowId).Error; err != nil || got.UserId != row.UserId || got.CollectionId != row.CollectionId {
					t.Fatal("Sciter reset escaped personal scope", err)
				}
			}
			for _, label := range labels {
				var got model.Tag
				if err := global.DB.First(&got, label.Id).Error; err != nil || got.Name != label.Name || got.Color != label.Color {
					t.Fatal("Sciter reset changed another scope's color", err)
				}
			}
		})
	}
}

func TestSciterWriteOmittedColorsPreservesFactsAndExtensions(t *testing.T) {
	f := newCompatFixture(t)
	row := compatRow("retained", f.actor, 0)
	row.Tags = custom_types.AutoJson(`["keep"]`)
	row.Alias, row.Note, row.Password, row.Hash = "before", "note", "password", "hash"
	row.RdpPort, row.RdpUsername, row.LoginName = "3389", "rdp-user", "login-user"
	row.ForceAlwaysRelay, row.SameServer = true, true
	compatCreate(t, &row)
	labels := []model.Tag{{UserId: f.actor.Id, Name: "keep", Color: 17}, {UserId: f.actor.Id, Name: "drop", Color: 18}}
	compatCreate(t, &labels)
	var baseline model.AddressBook
	if err := global.DB.First(&baseline, row.RowId).Error; err != nil {
		t.Fatal(err)
	}
	document := map[string]any{"tags": []string{"keep", "new", "keep"}, "peers": []any{map[string]any{"id": row.Id, "alias": "after", "user_id": f.hidden.Id, "collection_id": 99}}}
	for i, colors := range []any{nil, "{}", `{"keep":0,"new":9,"drop":999}`} {
		if colors != nil {
			document["tag_colors"] = colors
		}
		w := f.call("POST", "/api/ab", compatDocument(t, document), f.actor)
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "null" {
			t.Fatalf("metadata variant %d: %d %s", i, w.Code, w.Body.String())
		}
		var got model.AddressBook
		if err := global.DB.First(&got, row.RowId).Error; err != nil {
			t.Fatal(err)
		}
		if got.Alias != "after" {
			t.Fatal("present alias was not updated")
		}
		got.Alias, got.TimeModel = baseline.Alias, baseline.TimeModel
		if !reflect.DeepEqual(got, baseline) {
			t.Fatalf("omitted peer fields or ownership changed: %+v", got)
		}
		wantColors := map[string]uint{"keep": 17, "new": 0}
		if i == 2 {
			wantColors = map[string]uint{"keep": 0, "new": 9}
		}
		for _, request := range []struct{ method, path string }{{"GET", "/api/ab"}, {"POST", "/api/ab/get"}} {
			read := f.call(request.method, request.path, "{}", f.actor)
			var envelope struct{ Data string }
			var snapshot struct {
				Tags   []string
				Colors string `json:"tag_colors"`
				Peers  []clientcompat.Peer
			}
			if read.Code != 200 || json.Unmarshal(read.Body.Bytes(), &envelope) != nil || json.Unmarshal([]byte(envelope.Data), &snapshot) != nil {
				t.Fatal("legacy readback failed", read.Body.String())
			}
			var gotColors map[string]uint
			if json.Unmarshal([]byte(snapshot.Colors), &gotColors) != nil || !reflect.DeepEqual(gotColors, wantColors) || !reflect.DeepEqual(snapshot.Tags, []string{"keep", "new"}) || len(snapshot.Peers) != 1 || snapshot.Peers[0].Note != row.Note || snapshot.Peers[0].RdpPort != row.RdpPort {
				t.Fatalf("tag facts/colors/extensions did not round trip: %+v %+v", snapshot, gotColors)
			}
		}
	}
}

func TestSciterWriteRejectsInvalidMetadataAndPartialSnapshots(t *testing.T) {
	f := newCompatFixture(t)
	row := compatRow("protected", f.actor, 0)
	compatCreate(t, &row)
	label := model.Tag{UserId: f.actor.Id, Name: "keep", Color: 17}
	compatCreate(t, &label)
	invalid := []string{"null", "{}", `{"tags":[]}`, `{"peers":[]}`, `{"tags":null,"peers":[]}`, `{"tags":[],"peers":null}`, `{"tags":{},"peers":[]}`, `{"tags":[],"peers":{}}`}
	for _, colors := range []any{nil, "", " ", "null", "[]", "broken-json", `{"keep":null}`, `{"keep":"1"}`, `{"keep":true}`, `{"keep":-1}`, `{"keep":1.5}`, `{"keep":18446744073709551616}`, map[string]any{}, 7} {
		data, err := json.Marshal(map[string]any{"tags": []string{}, "peers": []any{}, "tag_colors": colors})
		if err != nil {
			t.Fatal(err)
		}
		invalid = append(invalid, string(data))
	}
	for _, data := range invalid {
		outer, err := json.Marshal(map[string]string{"data": data})
		if err != nil {
			t.Fatal(err)
		}
		w := f.call("POST", "/api/ab", string(outer), f.actor)
		if w.Code != 400 {
			t.Fatalf("invalid snapshot accepted: %s -> %d %s", data, w.Code, w.Body.String())
		}
		var got model.AddressBook
		var tag model.Tag
		if err := global.DB.First(&got, row.RowId).Error; err != nil {
			t.Fatal("invalid metadata erased peer", err)
		}
		if err := global.DB.First(&tag, label.Id).Error; err != nil || tag.Color != label.Color || tag.Name != label.Name {
			t.Fatal("invalid metadata changed tag", err)
		}
	}
}

type sciterFailCommitPool struct {
	gorm.ConnPool
	db      *sql.DB
	reached *bool
}

func (p sciterFailCommitPool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.db.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &sciterFailCommitTx{Tx: tx, reached: p.reached}, nil
}

type sciterFailCommitTx struct {
	*sql.Tx
	reached *bool
}

func (tx sciterFailCommitTx) Commit() error {
	*tx.reached = true
	_ = tx.Tx.Rollback()
	return errors.New("private-sciter-commit-fault")
}

func TestSciterWriteOmittedColorsTransactionFailuresRollback(t *testing.T) {
	for _, fault := range []string{"previous-tags-read", "second-tag-create", "commit-omitted", "commit-explicit"} {
		t.Run(fault, func(t *testing.T) {
			f := newCompatFixture(t)
			row := compatRow("retained", f.actor, 0)
			row.Alias, row.Note = "before", "keep-note"
			compatCreate(t, &row)
			label := model.Tag{UserId: f.actor.Id, Name: "old", Color: 17}
			compatCreate(t, &label)
			body := map[string]any{"tags": []string{"first", "second"}, "peers": []any{map[string]any{"id": row.Id, "alias": "after"}}}
			if fault == "commit-explicit" {
				body["tag_colors"] = `{"first":1}`
			}
			fired, count := false, 0
			remove := func() {}
			switch fault {
			case "previous-tags-read":
				if err := global.DB.Callback().Query().Before("gorm:query").Register("sciter-write-fault", func(tx *gorm.DB) {
					if tx.Statement.Table == "tags" {
						fired = true
						tx.AddError(errors.New("private-sciter-query-fault"))
					}
				}); err != nil {
					t.Fatal(err)
				}
				remove = func() { _ = global.DB.Callback().Query().Remove("sciter-write-fault") }
			case "second-tag-create":
				if err := global.DB.Callback().Create().Before("gorm:create").Register("sciter-write-fault", func(tx *gorm.DB) {
					if tx.Statement.Table == "tags" {
						count++
						if count == 2 {
							fired = true
							tx.AddError(errors.New("private-sciter-create-fault"))
						}
					}
				}); err != nil {
					t.Fatal(err)
				}
				remove = func() { _ = global.DB.Callback().Create().Remove("sciter-write-fault") }
			default:
				pool, err := global.DB.DB()
				if err != nil {
					t.Fatal(err)
				}
				faultDB, err := gorm.Open(sqlite.Dialector{Conn: sciterFailCommitPool{ConnPool: pool, db: pool, reached: &fired}}, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
				if err != nil {
					t.Fatal(err)
				}
				original := service.DB
				service.DB = faultDB
				remove = func() { service.DB = original }
			}
			defer remove()
			w := f.call("POST", "/api/ab", compatDocument(t, body), f.actor)
			remove()
			if w.Code != 503 || strings.Contains(w.Body.String(), "private-sciter") {
				t.Fatalf("failed transaction acknowledged/leaked details: %d %s", w.Code, w.Body.String())
			}
			if !fired {
				t.Fatal("injected fault/COMMIT was never reached")
			}
			var got model.AddressBook
			if err := global.DB.First(&got, row.RowId).Error; err != nil || got.Alias != row.Alias || got.Note != row.Note {
				t.Fatalf("failed transaction changed peer: %+v %v", got, err)
			}
			var tags []model.Tag
			if err := global.DB.Order("id").Find(&tags).Error; err != nil || len(tags) != 1 || tags[0].Name != label.Name || tags[0].Color != label.Color {
				t.Fatalf("failed transaction changed tags/colors: %+v %v", tags, err)
			}
		})
	}
}
