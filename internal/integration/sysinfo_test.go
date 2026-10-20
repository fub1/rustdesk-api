package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/clientsysinfo"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

func sysinfoString(value string) clientsysinfo.OptionalString {
	return clientsysinfo.OptionalString{Present: true, Value: value}
}

func TestClientSysinfoSnapshotsRealEngines(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		owner := model.User{Username: "sysinfo-owner", GroupId: 1, Status: model.COMMON_STATUS_ENABLE}
		transactionCreate(t, f.DB, &owner)
		peer := model.Peer{Id: "snapshot", Uuid: "snapshot-uuid", UserId: owner.Id, GroupId: 17, AbId: 18, Alias: "managed alias", Cpu: "cpu", Hostname: "host", Memory: "memory", Os: "Linux", Username: "old user", Version: "old", LastOnlineTime: 99, LastOnlineIp: "192.0.2.1", PresetAbId: 19, PresetAbAlias: "preset alias", PresetDevName: "preset host", PresetDevUsrName: "preset user", PresetModifiedAt: 23}
		transactionCreate(t, f.DB, &peer)
		store, err := clientsysinfo.New(f.Reopen(t))
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		input := clientsysinfo.Input{ID: peer.Id, Hostname: sysinfoString(""), CPU: sysinfoString(""), Version: sysinfoString("1.5.0")}
		if err := store.Report(ctx, input); err != nil {
			t.Fatal(err)
		}
		var got model.Peer
		if err := f.DB.First(&got, peer.RowId).Error; err != nil {
			t.Fatal(err)
		}
		want := peer
		want.Username, want.Hostname, want.Cpu, want.Version = "", "", "", "1.5.0"
		got.TimeModel, want.TimeModel = model.TimeModel{}, model.TimeModel{}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("omitted username/explicit empties or protected fields differ: got=%+v want=%+v", got, want)
		}
		if err := store.Report(ctx, input); err != nil {
			t.Fatal("identical retry rejected", err)
		}
		transactionCreate(t, f.DB, &model.LoginLog{UserId: owner.Id, DeviceId: "created", Uuid: "created-uuid"})
		created := clientsysinfo.Input{ID: "created", UUID: sysinfoString("created-uuid"), Hostname: sysinfoString("new host"), OS: sysinfoString("Linux")}
		if err := store.Report(ctx, created); err != nil {
			t.Fatal(err)
		}
		got = model.Peer{}
		if err := f.DB.Where("id = ?", created.ID).First(&got).Error; err != nil || got.UserId != 0 || got.Uuid != "created-uuid" || got.Username != "" || got.Hostname != "new host" {
			t.Fatalf("anonymous report inferred login-log owner or lost basic fields: %+v %v", got, err)
		}
		if err := store.Report(ctx, created); err != nil {
			t.Fatal(err)
		}
		var count int64
		if err := f.DB.Model(&model.Peer{}).Where("id = ?", created.ID).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("retry duplicated peer: %d %v", count, err)
		}
	})
}

func TestClientSysinfoIndependentPoolsPreservePartialFields(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		peer := model.Peer{Id: "parallel", Uuid: "parallel-uuid", Username: "old", Cpu: "old cpu", Memory: "old memory", Alias: "managed"}
		transactionCreate(t, f.DB, &peer)
		inputs := []clientsysinfo.Input{{ID: peer.Id, CPU: sysinfoString("new cpu")}, {ID: peer.Id, Memory: sysinfoString("new memory")}}
		start, results := make(chan struct{}), make(chan error, len(inputs))
		for _, input := range inputs {
			store, err := clientsysinfo.New(f.Reopen(t))
			if err != nil {
				t.Fatal(err)
			}
			go func(input clientsysinfo.Input) {
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				results <- store.Report(ctx, input)
			}(input)
		}
		close(start)
		for range inputs {
			if err := <-results; err != nil {
				t.Fatalf("independent report failed: %v", err)
			}
		}
		var got model.Peer
		if err := f.DB.First(&got, peer.RowId).Error; err != nil || got.Cpu != "new cpu" || got.Memory != "new memory" || got.Username != "" || got.Alias != peer.Alias || got.Uuid != peer.Uuid {
			t.Fatalf("partial reports lost fields: %+v %v", got, err)
		}
	})
}

// An existing peer report ends with one basic snapshot UPDATE. Inject actual
// commit failure after that final SQL and verify rollback through a fresh pool.
func failSysinfoUpdateCommit(t *testing.T, f *fixture) func() {
	t.Helper()
	if f.Engine == "postgres" {
		transactionExec(t, f.DB, "CREATE TABLE sysinfo_commit_parent (id bigint PRIMARY KEY)")
		transactionExec(t, f.DB, "CREATE TABLE sysinfo_commit_guard (parent_id bigint REFERENCES sysinfo_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)")
		transactionExec(t, f.DB, "CREATE FUNCTION sysinfo_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO sysinfo_commit_guard(parent_id) VALUES (999999); RETURN NEW; END $$")
		transactionExec(t, f.DB, "CREATE TRIGGER sysinfo_fail_commit AFTER UPDATE ON peers FOR EACH ROW EXECUTE FUNCTION sysinfo_fail_commit()")
		return func() {
			transactionExec(t, f.DB, "DROP TRIGGER sysinfo_fail_commit ON peers")
			transactionExec(t, f.DB, "DROP FUNCTION sysinfo_fail_commit()")
			transactionExec(t, f.DB, "DROP TABLE sysinfo_commit_guard")
			transactionExec(t, f.DB, "DROP TABLE sysinfo_commit_parent")
		}
	}
	control := f.Reopen(t)
	var reached, killed atomic.Bool
	if err := f.DB.Callback().Update().After("gorm:update").Register("sysinfo:kill-before-commit", func(tx *gorm.DB) {
		values, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok || tx.Statement.Table != "peers" || tx.Error != nil {
			return
		}
		if _, present := values["username"]; !present || !reached.CompareAndSwap(false, true) {
			return
		}
		var id uint64
		if err := tx.Statement.ConnPool.QueryRowContext(tx.Statement.Context, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
			tx.AddError(err)
			return
		}
		if err := control.Exec(fmt.Sprintf("KILL CONNECTION %d", id)).Error; err != nil {
			tx.AddError(err)
			return
		}
		killed.Store(true)
	}); err != nil {
		t.Fatal(err)
	}
	return func() {
		if !reached.Load() || !killed.Load() {
			t.Error("actual connection termination after final basic UPDATE not reached")
		}
		if err := f.DB.Callback().Update().Remove("sysinfo:kill-before-commit"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClientSysinfoActualCommitRollbackAndRetry(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			forEngines(t, func(t *testing.T, f *fixture) {
				peer := model.Peer{Id: "commit-report", Uuid: "commit-uuid", UserId: 1, Username: "before", Alias: "managed", Hostname: "before host"}
				if existing {
					transactionCreate(t, f.DB, &peer)
				}
				input := clientsysinfo.Input{ID: peer.Id, UUID: sysinfoString(peer.Uuid), Hostname: sysinfoString("after host")}
				store, err := clientsysinfo.New(f.DB)
				if err != nil {
					t.Fatal(err)
				}
				var remove func()
				if existing {
					remove = failSysinfoUpdateCommit(t, f)
				} else {
					remove = failTransactionCommit(t, f, "peers")
				}
				if err := store.Report(context.Background(), input); !errors.Is(err, clientsysinfo.ErrUnavailable) {
					t.Fatalf("real failed COMMIT acknowledged: %v", err)
				}
				remove()
				fresh := f.Reopen(t)
				var got model.Peer
				if existing {
					if err := fresh.First(&got, peer.RowId).Error; err != nil || got.Username != peer.Username || got.Hostname != peer.Hostname || got.Alias != peer.Alias || got.UserId != peer.UserId {
						t.Fatalf("basic UPDATE escaped failed COMMIT: %+v %v", got, err)
					}
				} else if transactionCount(t, fresh, &model.Peer{}) != 0 {
					t.Fatal("failed COMMIT retained new peer")
				}
				retry, err := clientsysinfo.New(fresh)
				if err != nil {
					t.Fatal(err)
				}
				if err := retry.Report(context.Background(), input); err != nil {
					t.Fatal("retry poisoned by failed COMMIT", err)
				}
				got = model.Peer{}
				if err := fresh.Where("id = ?", peer.Id).First(&got).Error; err != nil || got.Hostname != "after host" || got.Username != "" {
					t.Fatalf("snapshot retry not persisted: %+v %v", got, err)
				}
				if existing && (got.UserId != peer.UserId || got.Alias != peer.Alias) || !existing && got.UserId != 0 {
					t.Fatal("basic retry changed ownership or management columns")
				}
			})
		})
	}
}

func TestClientSysinfoManagementPresetRejectionRealEngines(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		peer := model.Peer{Id: "preset-target", UserId: 1, Alias: "managed", Username: "before", Hostname: "before host"}
		transactionCreate(t, f.DB, &peer)
		book := model.AddressBookCollection{UserId: 1, Name: "legacy-target"}
		transactionCreate(t, f.DB, &book)
		entry := model.AddressBook{Id: peer.Id, UserId: 1, Tags: custom_types.AutoJson(`["sentinel"]`), Alias: "entry sentinel"}
		transactionCreate(t, f.DB, &entry)
		tag := model.Tag{UserId: 1, Name: "sentinel", Color: 7}
		transactionCreate(t, f.DB, &tag)
		beforePeers, beforeBooks, beforeEntries, beforeTags := transactionCount(t, f.DB, &model.Peer{}), transactionCount(t, f.DB, &model.AddressBookCollection{}), transactionCount(t, f.DB, &model.AddressBook{}), transactionCount(t, f.DB, &model.Tag{})
		var original model.Peer
		if err := f.DB.First(&original, peer.RowId).Error; err != nil {
			t.Fatal(err)
		}
		store, err := clientsysinfo.New(f.DB)
		if err != nil {
			t.Fatal(err)
		}
		inputs := []clientsysinfo.Input{{ID: peer.Id, Username: sysinfoString("after"), PresetAddressBookName: sysinfoString(book.Name)}, {ID: peer.Id, PresetAddressBookAlias: sysinfoString("after alias")}}
		for _, key := range []string{"preset-address-book-name", "preset-address-book-tag", "preset-address-book-alias", "preset-address-book-password", "preset-address-book-note", "preset-user-name", "preset-strategy-name", "preset-device-group-name", "preset-note", "preset-future-option"} {
			for _, id := range []string{peer.Id, "unregistered"} {
				body, err := json.Marshal(map[string]string{"id": id, "username": "after", key: "untrusted"})
				if err != nil {
					t.Fatal(err)
				}
				var input clientsysinfo.Input
				if err := json.Unmarshal(body, &input); err != nil {
					t.Fatal(err)
				}
				inputs = append(inputs, input)
			}
		}
		for _, input := range inputs {
			if err := store.Report(context.Background(), input); !errors.Is(err, clientsysinfo.ErrManagementPreset) {
				t.Fatalf("management report was not explicitly rejected: %v", err)
			}
		}
		if transactionCount(t, f.DB, &model.Peer{}) != beforePeers || transactionCount(t, f.DB, &model.AddressBookCollection{}) != beforeBooks || transactionCount(t, f.DB, &model.AddressBook{}) != beforeEntries || transactionCount(t, f.DB, &model.Tag{}) != beforeTags {
			t.Fatal("rejected management report changed row counts")
		}
		var got model.Peer
		if err := f.DB.First(&got, peer.RowId).Error; err != nil || !reflect.DeepEqual(got, original) {
			t.Fatalf("rejected report changed existing peer: %+v %v", got, err)
		}
		var gotEntry model.AddressBook
		var gotTag model.Tag
		var gotBook model.AddressBookCollection
		if err := f.DB.First(&gotEntry, entry.RowId).Error; err != nil || gotEntry.Alias != entry.Alias || string(gotEntry.Tags) != string(entry.Tags) {
			t.Fatal("rejected report changed AB sentinel", err)
		}
		if err := f.DB.First(&gotTag, tag.Id).Error; err != nil || gotTag.Name != tag.Name || gotTag.Color != tag.Color {
			t.Fatal("rejected report changed tag sentinel", err)
		}
		if err := f.DB.First(&gotBook, book.Id).Error; err != nil || gotBook.Name != book.Name || gotBook.UserId != book.UserId {
			t.Fatal("rejected report changed book sentinel", err)
		}
		if err := store.Report(context.Background(), clientsysinfo.Input{ID: peer.Id, Hostname: sysinfoString("safe host")}); err != nil {
			t.Fatal("rejected preset poisoned normal report", err)
		}
	})
}
