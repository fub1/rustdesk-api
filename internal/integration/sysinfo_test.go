package integration

import (
	"context"
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
		if err := f.DB.Where("id = ?", created.ID).First(&got).Error; err != nil || got.UserId != owner.Id || got.Uuid != "created-uuid" || got.Username != "" || got.Hostname != "new host" {
			t.Fatalf("new report did not preserve login-log owner lookup: %+v %v", got, err)
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

// The legacy preset's final peer link UPDATE is the last business SQL. Only
// fail that UPDATE, after the base snapshot, book INSERT and cleanup succeeded.
func failSysinfoPresetCommit(t *testing.T, f *fixture, collectionID uint) func() {
	t.Helper()
	if f.Engine == "postgres" {
		transactionExec(t, f.DB, "CREATE TABLE sysinfo_commit_parent (id bigint PRIMARY KEY)")
		transactionExec(t, f.DB, "CREATE TABLE sysinfo_commit_guard (parent_id bigint REFERENCES sysinfo_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)")
		transactionExec(t, f.DB, "CREATE FUNCTION sysinfo_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO sysinfo_commit_guard(parent_id) VALUES (999999); RETURN NEW; END $$")
		transactionExec(t, f.DB, fmt.Sprintf("CREATE TRIGGER sysinfo_fail_commit AFTER UPDATE ON peers FOR EACH ROW WHEN (NEW.ab_id = %d) EXECUTE FUNCTION sysinfo_fail_commit()", collectionID))
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
		if _, present := values["ab_id"]; !present || !reached.CompareAndSwap(false, true) {
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
			t.Error("actual connection termination after final preset UPDATE not reached")
		}
		if err := f.DB.Callback().Update().Remove("sysinfo:kill-before-commit"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClientSysinfoActualCommitRollbackAndRetry(t *testing.T) {
	for _, preset := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy-preset=%t", preset), func(t *testing.T) {
			forEngines(t, func(t *testing.T, f *fixture) {
				peer := model.Peer{Id: "commit-report", Uuid: "commit-uuid", Username: "before", Alias: "before alias", Hostname: "before host"}
				input := clientsysinfo.Input{ID: peer.Id, UUID: sysinfoString(peer.Uuid), Hostname: sysinfoString("after host")}
				var original model.AddressBook
				var book model.AddressBookCollection
				if preset {
					transactionCreate(t, f.DB, &peer)
					book = model.AddressBookCollection{UserId: 1, Name: "legacy-preset-target"}
					transactionCreate(t, f.DB, &book)
					original = model.AddressBook{Id: peer.Id, UserId: 1, CollectionId: 0, Alias: "original entry", Tags: custom_types.AutoJson(`[]`)}
					transactionCreate(t, f.DB, &original)
					input.PresetAddressBookName = sysinfoString(book.Name)
					input.PresetAddressBookAlias = sysinfoString("after alias")
				}
				store, err := clientsysinfo.New(f.DB)
				if err != nil {
					t.Fatal(err)
				}
				var remove func()
				if preset {
					remove = failSysinfoPresetCommit(t, f, book.Id)
				} else {
					remove = failTransactionCommit(t, f, "peers")
				}
				if err := store.Report(context.Background(), input); !errors.Is(err, clientsysinfo.ErrUnavailable) {
					t.Fatalf("real failed COMMIT acknowledged: %v", err)
				}
				remove()
				fresh := f.Reopen(t)
				var got model.Peer
				if preset {
					if err := fresh.First(&got, peer.RowId).Error; err != nil || got.Username != peer.Username || got.Hostname != peer.Hostname || got.Alias != peer.Alias || got.AbId != 0 {
						t.Fatalf("base/preset columns escaped failed COMMIT: %+v %v", got, err)
					}
					var gotEntry model.AddressBook
					if err := fresh.First(&gotEntry, original.RowId).Error; err != nil || gotEntry.Alias != original.Alias || transactionCount(t, fresh, &model.AddressBook{}) != 1 {
						t.Fatalf("preset insert/cleanup escaped failed COMMIT: %+v %v", gotEntry, err)
					}
				} else if transactionCount(t, fresh, &model.Peer{}) != 0 {
					t.Fatal("failed COMMIT left a peer")
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
				if preset && (got.AbId != book.Id || got.Alias != "after alias" || transactionCount(t, fresh, &model.AddressBook{}) != 1) {
					t.Fatal("preset retry did not commit one link and entry")
				}
			})
		})
	}
}
