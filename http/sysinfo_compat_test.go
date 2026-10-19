package http

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newSysInfoFixture(t *testing.T) *compatFixture {
	t.Helper()
	f := newCompatFixture(t)
	if err := global.DB.AutoMigrate(&model.LoginLog{}); err != nil {
		t.Fatal(err)
	}
	return f
}

func sysInfoCall(f *compatFixture, body string, ctx context.Context) *httptest.ResponseRecorder {
	request := httptest.NewRequest("POST", "/api/sysinfo", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if ctx != nil {
		request = request.WithContext(ctx)
	}
	result := httptest.NewRecorder()
	f.handler.ServeHTTP(result, request)
	return result
}

func TestSysInfoUsernameSnapshotAndExplicitEmptyFields(t *testing.T) {
	for _, username := range []string{"", `,"username":""`} {
		t.Run("username"+username, func(t *testing.T) {
			f := newSysInfoFixture(t)
			row := model.Peer{Id: "device", Uuid: "uuid", UserId: f.actor.Id, Cpu: "old-cpu", Hostname: "old-host", Memory: "old-memory", Os: "old-os", Username: "old-user", Version: "old-version"}
			compatCreate(t, &row)
			body := `{"id":"device","uuid":"uuid","cpu":"","hostname":"","memory":"","os":"","version":""` + username + `}`
			result := sysInfoCall(f, body, nil)
			if result.Code != 200 || result.Body.String() != "SYSINFO_UPDATED" {
				t.Fatalf("sysinfo ACK: %d %s", result.Code, result.Body.String())
			}
			var got model.Peer
			if err := global.DB.First(&got, row.RowId).Error; err != nil {
				t.Fatal(err)
			}
			if got.Username != "" || got.Hostname != "" || got.Cpu != "" || got.Memory != "" || got.Os != "" || got.Version != "" {
				t.Fatalf("reported empty snapshot retained stale values: %+v", got)
			}
			if got.Uuid != "uuid" || got.UserId != f.actor.Id {
				t.Fatal("snapshot changed identity or owner")
			}
		})
	}
}

func TestSysInfoPartialReportPreservesOtherFieldsAndManagementColumns(t *testing.T) {
	f := newSysInfoFixture(t)
	row := model.Peer{Id: "device", Uuid: "uuid", UserId: f.owner.Id, GroupId: 19, Alias: "admin-alias", AbId: 11, Cpu: "cpu", Hostname: "host", Memory: "memory", Os: "Windows", Username: "old", Version: "version", LastOnlineTime: 789, LastOnlineIp: "192.0.2.1", PresetAbId: 12, PresetAbAlias: "preset-alias", PresetDevUsrName: "preset-user", PresetDevName: "preset-host", PresetModifiedAt: 123}
	compatCreate(t, &row)
	result := sysInfoCall(f, `{"id":"device","username":"new","row_id":999,"user_id":3,"group_id":7,"alias":"injected","ab_id":99,"preset_ab_id":99,"preset_ab_alias":"injected","preset_dev_usr_name":"injected","preset_dev_name":"injected","preset_modified_at":999,"last_online_time":999,"last_online_ip":"injected","preset-address-book-tag":"ignored-unimplemented"}`, nil)
	if result.Code != 200 || result.Body.String() != "SYSINFO_UPDATED" {
		t.Fatalf("partial ACK: %d %s", result.Code, result.Body.String())
	}
	var got model.Peer
	if err := global.DB.First(&got, row.RowId).Error; err != nil {
		t.Fatal(err)
	}
	if got.Username != "new" || got.Id != row.Id || got.Uuid != row.Uuid || got.Cpu != row.Cpu || got.Hostname != row.Hostname || got.Memory != row.Memory || got.Os != row.Os || got.Version != row.Version {
		t.Fatalf("partial report modified omitted fields: %+v", got)
	}
	if got.UserId != row.UserId || got.GroupId != row.GroupId || got.Alias != row.Alias || got.AbId != row.AbId || got.PresetAbId != row.PresetAbId || got.PresetAbAlias != row.PresetAbAlias || got.PresetDevUsrName != row.PresetDevUsrName || got.PresetDevName != row.PresetDevName || got.PresetModifiedAt != row.PresetModifiedAt || got.LastOnlineTime != row.LastOnlineTime || got.LastOnlineIp != row.LastOnlineIp {
		t.Fatalf("sysinfo overwrote management columns: %+v", got)
	}
}

func TestSysInfoRejectsNullFieldsAndInvalidEnvelope(t *testing.T) {
	f := newSysInfoFixture(t)
	for _, field := range []string{"id", "uuid", "username", "hostname", "cpu", "memory", "os", "version", "preset-address-book-name", "preset-address-book-alias"} {
		body := `{"id":"device","` + field + `":null}`
		result := sysInfoCall(f, body, nil)
		if result.Code != 400 || result.Body.String() == "SYSINFO_UPDATED" {
			t.Errorf("null field accepted %s: %d %s", field, result.Code, result.Body.String())
		}
	}
	for _, body := range []string{`{}`, `null`, `{"id":""}`, `{"id":5}`, `{"id":"device","hostname":5}`, `{"id":"device"} {}`} {
		if result := sysInfoCall(f, body, nil); result.Code != 400 {
			t.Errorf("invalid sysinfo acknowledged %s: %d %s", body, result.Code, result.Body.String())
		}
	}
	var count int64
	if err := global.DB.Model(&model.Peer{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid inputs created %d peers", count)
	}
}

func TestSysInfoDatabaseErrorsAndCancellationFailClosed(t *testing.T) {
	for _, fault := range []string{"peer-read", "login-read", "create", "update"} {
		t.Run(fault, func(t *testing.T) {
			f := newSysInfoFixture(t)
			if fault == "update" {
				compatCreate(t, &model.Peer{Id: "device", Username: "old"})
			}
			callback := func(tx *gorm.DB) { tx.AddError(errors.New("private sysinfo failure")) }
			var err error
			switch fault {
			case "peer-read", "login-read":
				table := "peers"
				if fault == "login-read" {
					table = "login_logs"
				}
				err = global.DB.Callback().Query().Before("gorm:query").Register("sysinfo-failure", func(tx *gorm.DB) {
					if tx.Statement.Table == table {
						callback(tx)
					}
				})
			case "create":
				err = global.DB.Callback().Create().Before("gorm:create").Register("sysinfo-failure", callback)
			case "update":
				err = global.DB.Callback().Update().Before("gorm:update").Register("sysinfo-failure", callback)
			}
			if err != nil {
				t.Fatal(err)
			}
			result := sysInfoCall(f, `{"id":"device","uuid":"uuid","username":"new"}`, nil)
			if result.Code != 503 || strings.Contains(result.Body.String(), "private sysinfo") || strings.Contains(result.Body.String(), "SYSINFO_UPDATED") {
				t.Fatalf("DB failure acknowledged or leaked: %d %s", result.Code, result.Body.String())
			}
			if fault == "update" {
				var row model.Peer
				if err := global.DB.Where("id = ?", "device").First(&row).Error; err != nil {
					t.Fatal(err)
				}
				if row.Username != "old" {
					t.Fatal("failed update changed peer")
				}
			}
		})
	}
	f := newSysInfoFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := sysInfoCall(f, `{"id":"device","uuid":"uuid","username":"new"}`, ctx)
	if result.Code != 408 {
		t.Fatalf("canceled sysinfo continued: %d %s", result.Code, result.Body.String())
	}
	deadline, done := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer done()
	result = sysInfoCall(f, `{"id":"device","uuid":"uuid"}`, deadline)
	if result.Code != 504 {
		t.Fatalf("expired sysinfo continued: %d %s", result.Code, result.Body.String())
	}
}

func TestSysInfoNewPeerKeepsExistingLoginLogOwnerAssociation(t *testing.T) {
	f := newSysInfoFixture(t)
	compatCreate(t, &[]model.LoginLog{{UserId: f.actor.Id, DeviceId: "device", Uuid: "uuid"}, {UserId: f.owner.Id, DeviceId: "device", Uuid: "uuid"}, {UserId: f.hidden.Id, DeviceId: "other", Uuid: "uuid"}})
	result := sysInfoCall(f, `{"id":"device","uuid":"uuid","hostname":"host","username":"user","user_id":3}`, nil)
	if result.Code != 200 || result.Body.String() != "SYSINFO_UPDATED" {
		t.Fatalf("create ACK: %d %s", result.Code, result.Body.String())
	}
	var got model.Peer
	if err := global.DB.Where("id = ?", "device").First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.UserId != f.owner.Id || got.Hostname != "host" || got.Username != "user" {
		t.Fatalf("create ownership mapping changed: %+v", got)
	}
	// Later reports do not reassign an already registered peer to a new login.
	compatCreate(t, &model.LoginLog{UserId: f.hidden.Id, DeviceId: "device", Uuid: "uuid"})
	result = sysInfoCall(f, `{"id":"device","uuid":"uuid"}`, nil)
	if result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	if err := global.DB.First(&got, got.RowId).Error; err != nil {
		t.Fatal(err)
	}
	if got.UserId != f.owner.Id || got.Username != "" || got.Hostname != "host" {
		t.Fatalf("repeated report reassigned owner or lost snapshot semantics: %+v", got)
	}
}

func TestSysInfoLegacyPresetBehaviorIsIsolatedFromBasicFields(t *testing.T) {
	for _, scenario := range []string{"alias-only", "missing-book", "new-entry", "existing-entry"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSysInfoFixture(t)
			peer := model.Peer{Id: "device", UserId: f.owner.Id, GroupId: 5, Alias: "admin", Username: "old-user", Hostname: "old-host", PresetAbId: 99}
			compatCreate(t, &peer)
			book := model.AddressBookCollection{UserId: 1, Name: "target"}
			if scenario != "missing-book" {
				compatCreate(t, &book)
			}
			foreignBook := model.AddressBookCollection{UserId: f.owner.Id, Name: "target"}
			compatCreate(t, &foreignBook)
			old := compatRow("device", f.actor, 0)
			foreign := compatRow("device", f.owner, foreignBook.Id)
			compatCreate(t, &old)
			compatCreate(t, &foreign)
			var existing model.AddressBook
			if scenario == "existing-entry" {
				existing = compatRow("device", f.actor, book.Id)
				existing.Alias, existing.Username, existing.Hostname = "ab-alias", "ab-user", "ab-host"
				compatCreate(t, &existing)
			}
			body := `{"id":"device","hostname":"new-host","username":"new-user","os":"Linux","preset-address-book-alias":"new-alias"`
			if scenario != "alias-only" {
				body += `,"preset-address-book-name":"target"`
			}
			if scenario == "existing-entry" {
				body = `{"id":"device","hostname":"","preset-address-book-name":"target"`
			}
			result := sysInfoCall(f, body+`}`, nil)
			if result.Code != 200 || result.Body.String() != "SYSINFO_UPDATED" {
				t.Fatalf("legacy ACK: %d %s", result.Code, result.Body.String())
			}
			var got model.Peer
			if err := global.DB.First(&got, peer.RowId).Error; err != nil {
				t.Fatal(err)
			}
			if got.UserId != peer.UserId || got.GroupId != peer.GroupId || got.PresetAbId != peer.PresetAbId {
				t.Fatalf("preset changed unrelated management fields: %+v", got)
			}
			if scenario == "alias-only" || scenario == "missing-book" {
				if got.Alias != "new-alias" || got.AbId != 0 {
					t.Fatalf("legacy alias/no-target behavior changed: %+v", got)
				}
			} else {
				if got.AbId != book.Id {
					t.Fatal("legacy target was not linked")
				}
				var ab model.AddressBook
				if err := global.DB.Where("id = ? AND collection_id = ?", "device", book.Id).First(&ab).Error; err != nil {
					t.Fatal(err)
				}
				if scenario == "new-entry" {
					if ab.UserId != 1 || ab.Alias != "new-alias" || ab.Username != "new-user" || ab.Hostname != "new-host" || ab.Platform != "Linux" || got.Alias != "new-alias" {
						t.Fatalf("legacy create mapping: %+v peer %+v", ab, got)
					}
					var count int64
					if err := global.DB.Model(&model.AddressBook{}).Where("row_id = ?", old.RowId).Count(&count).Error; err != nil {
						t.Fatal(err)
					}
					if count != 0 {
						t.Fatal("legacy owner1 cleanup no longer ran")
					}
				} else {
					// Existing AB fields retain nonempty values; the legacy peer link
					// intentionally receives the empty report values, as before.
					if ab.Alias != "ab-alias" || ab.Username != "ab-user" || ab.Hostname != "ab-host" || got.Alias != "" || got.Username != "" || got.Hostname != "" {
						t.Fatalf("existing legacy empty-value behavior changed: %+v peer %+v", ab, got)
					}
				}
			}
			var protected model.AddressBook
			if err := global.DB.First(&protected, foreign.RowId).Error; err != nil || protected.UserId != foreign.UserId {
				t.Fatal("legacy cleanup escaped original owner1 range", err)
			}
		})
	}
}

func TestSysInfoEveryExecutedLegacyPresetFailureRollsBack(t *testing.T) {
	for _, stage := range []string{"collection-read", "ab-read", "ab-create", "ab-update", "cleanup", "alias", "link"} {
		t.Run(stage, func(t *testing.T) {
			f := newSysInfoFixture(t)
			peer := model.Peer{Id: "device", UserId: f.owner.Id, Alias: "before-alias", Username: "before-user", Hostname: "before-host"}
			compatCreate(t, &peer)
			book := model.AddressBookCollection{UserId: 1, Name: "target"}
			compatCreate(t, &book)
			old := compatRow("device", f.actor, 0)
			compatCreate(t, &old)
			if stage == "ab-update" {
				existing := compatRow("device", f.actor, book.Id)
				existing.Alias = "before-ab-alias"
				compatCreate(t, &existing)
			}
			reached := false
			fault := func(tx *gorm.DB) { reached = true; tx.AddError(errors.New("private preset failure")) }
			var err error
			switch stage {
			case "collection-read", "ab-read":
				table := "address_book_collections"
				if stage == "ab-read" {
					table = "address_books"
				}
				err = global.DB.Callback().Query().Before("gorm:query").Register("preset-failure", func(tx *gorm.DB) {
					if tx.Statement.Table == table {
						fault(tx)
					}
				})
			case "ab-create":
				err = global.DB.Callback().Create().Before("gorm:create").Register("preset-failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "address_books" {
						fault(tx)
					}
				})
			case "ab-update", "alias", "link":
				peerUpdates := 0
				err = global.DB.Callback().Update().Before("gorm:update").Register("preset-failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "peers" {
						peerUpdates++
					}
					if stage == "ab-update" && tx.Statement.Table == "address_books" || stage == "alias" && peerUpdates == 2 || stage == "link" && peerUpdates == 3 {
						fault(tx)
					}
				})
			case "cleanup":
				err = global.DB.Callback().Delete().Before("gorm:delete").Register("preset-failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "address_books" {
						fault(tx)
					}
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			result := sysInfoCall(f, `{"id":"device","username":"after-user","hostname":"after-host","preset-address-book-name":"target","preset-address-book-alias":"after-alias"}`, nil)
			if !reached {
				t.Fatal("test did not reach intended failure stage")
			}
			if result.Code != 503 || strings.Contains(result.Body.String(), "private preset") || strings.Contains(result.Body.String(), "SYSINFO_UPDATED") {
				t.Fatalf("preset failure acknowledged or leaked: %d %s", result.Code, result.Body.String())
			}
			if stage == "collection-read" || stage == "ab-read" {
				if err := global.DB.Callback().Query().Remove("preset-failure"); err != nil {
					t.Fatal(err)
				}
			}
			var got model.Peer
			if err := global.DB.First(&got, peer.RowId).Error; err != nil {
				t.Fatal(err)
			}
			if got.Username != peer.Username || got.Hostname != peer.Hostname || got.Alias != peer.Alias || got.AbId != 0 {
				t.Fatalf("failed preset retained partial peer changes: %+v", got)
			}
			var entries []model.AddressBook
			if err := global.DB.Where("id = ?", "device").Order("row_id ASC").Find(&entries).Error; err != nil {
				t.Fatal(err)
			}
			want := 1
			if stage == "ab-update" {
				want = 2
			}
			if len(entries) != want || entries[0].RowId != old.RowId || stage == "ab-update" && entries[1].Alias != "before-ab-alias" {
				t.Fatalf("preset failure retained AB creation/update/cleanup: %+v", entries)
			}
		})
	}
}

func TestSysInfoCommitFailureDoesNotAcknowledgeOrKeepAnyWrites(t *testing.T) {
	f := newSysInfoFixture(t)
	peer := model.Peer{Id: "device", Username: "before"}
	compatCreate(t, &peer)
	book := model.AddressBookCollection{UserId: 1, Name: "target"}
	compatCreate(t, &book)
	old := compatRow("device", f.actor, 0)
	compatCreate(t, &old)
	pool, err := global.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	reachedCommit := false
	faultDB, err := gorm.Open(sqlite.Dialector{Conn: sciterFailCommitPool{ConnPool: pool, db: pool, reached: &reachedCommit}}, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	service.DB = faultDB
	result := sysInfoCall(f, `{"id":"device","username":"after","preset-address-book-name":"target","preset-address-book-alias":"after"}`, nil)
	if !reachedCommit {
		t.Fatal("test did not reach failing commit")
	}
	if result.Code != 503 || strings.Contains(result.Body.String(), "SYSINFO_UPDATED") || strings.Contains(result.Body.String(), "private") {
		t.Fatalf("commit failure falsely acknowledged or leaked: %d %s", result.Code, result.Body.String())
	}
	var got model.Peer
	if err := global.DB.First(&got, peer.RowId).Error; err != nil {
		t.Fatal(err)
	}
	if got.Username != "before" || got.AbId != 0 || got.Alias != "" {
		t.Fatalf("peer escaped failed commit: %+v", got)
	}
	var entries []model.AddressBook
	if err := global.DB.Find(&entries).Error; err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].RowId != old.RowId {
		t.Fatalf("preset writes escaped failed commit: %+v", entries)
	}
}

func TestSysInfoCancellationAfterWriteRollsBackBeforeACK(t *testing.T) {
	f := newSysInfoFixture(t)
	peer := model.Peer{Id: "device", Username: "before"}
	compatCreate(t, &peer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reached := false
	if err := global.DB.Callback().Update().After("gorm:update").Register("sysinfo-cancel-after-write", func(tx *gorm.DB) {
		if tx.Statement.Table == "peers" {
			reached = true
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	result := sysInfoCall(f, `{"id":"device","username":"after"}`, ctx)
	if !reached || result.Code != 408 {
		t.Fatalf("mid-transaction cancellation was acknowledged: reached=%v status=%d %s", reached, result.Code, result.Body.String())
	}
	var got model.Peer
	if err := global.DB.First(&got, peer.RowId).Error; err != nil {
		t.Fatal(err)
	}
	if got.Username != "before" {
		t.Fatal("canceled report committed")
	}
}
