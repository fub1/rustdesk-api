package integration

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientassign"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientsysinfo"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

func assignmentValue(s string) clientassign.OptionalString {
	return clientassign.OptionalString{Present: true, Value: s}
}

func assignmentSetup(t *testing.T, f *fixture) (auth.Principal, model.User, model.Peer) {
	t.Helper()
	if err := database.Update(f.DB, 279, nil); err != nil {
		t.Fatal(err)
	}
	yes := true
	admin := model.User{Username: "cli-admin", Status: model.COMMON_STATUS_ENABLE, IsAdmin: &yes, GroupId: 1}
	owner := model.User{Username: "ExactOwner", Status: model.COMMON_STATUS_ENABLE, GroupId: 1}
	transactionCreate(t, f.DB, &owner)
	transactionCreate(t, f.DB, &admin)
	now := time.Now().UTC().Truncate(time.Second)
	token := model.UserToken{UserId: admin.Id, Token: "real-engine-assignment-token", ExpiredAt: now.Add(time.Hour).Unix()}
	transactionCreate(t, f.DB, &token)
	p := auth.Principal{User: auth.User{ID: admin.Id, IsAdmin: true, Enabled: true}, Session: auth.Session{ID: token.Id, UserID: admin.Id, Token: token.Token, ExpiresAt: now.Add(30 * time.Minute)}}
	peer := model.Peer{Id: "assigned-device", Uuid: "exact-UUID", Hostname: "before-host", Username: "before-user", Alias: "managed-alias", Note: "before-note", Cpu: "before-cpu", AbId: 17}
	transactionCreate(t, f.DB, &peer)
	return p, owner, peer
}

func assignmentRead(t *testing.T, db *gorm.DB, id uint) model.Peer {
	t.Helper()
	var row model.Peer
	if err := db.First(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func TestClientAssignmentRealCommitRollbackAndRetry(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		actor, owner, peer := assignmentSetup(t, f)
		before := assignmentRead(t, f.DB, peer.RowId)
		input := clientassign.Input{ID: peer.Id, UUID: peer.Uuid, UserName: assignmentValue(owner.Username), DeviceName: assignmentValue("after-host"), DeviceUsername: assignmentValue("after-user"), Note: assignmentValue("after-note")}
		store, err := clientassign.New(f.DB, clientassign.Options{})
		if err != nil {
			t.Fatal(err)
		}
		// Same physical final peers UPDATE as sysinfo: PostgreSQL deferred failure,
		// MySQL killed connection only after successful UPDATE, before COMMIT.
		remove := failSysinfoUpdateCommit(t, f)
		err = store.Assign(context.Background(), actor, input)
		remove()
		if !errors.Is(err, clientassign.ErrUnavailable) {
			t.Fatalf("failed COMMIT did not fail closed: %v", err)
		}
		fresh := f.Reopen(t)
		if after := assignmentRead(t, fresh, peer.RowId); !reflect.DeepEqual(before, after) {
			t.Fatal("assignment escaped failed COMMIT")
		}
		store, err = clientassign.New(fresh, clientassign.Options{})
		if err != nil {
			t.Fatal(err)
		}
		for n := 0; n < 2; n++ {
			if err := store.Assign(context.Background(), actor, input); err != nil {
				t.Fatal("retry", err)
			}
		}
		got := assignmentRead(t, fresh, peer.RowId)
		if got.UserId != owner.Id || got.Hostname != "after-host" || got.Username != "after-user" || got.Note != "after-note" || got.Alias != peer.Alias || got.AbId != peer.AbId {
			t.Fatal("retry omitted or enlarged the assignment")
		}
		if err := store.Assign(context.Background(), actor, clientassign.Input{ID: peer.Id, UUID: peer.Uuid, Note: assignmentValue("")}); err != nil {
			t.Fatal(err)
		}
		got = assignmentRead(t, fresh, peer.RowId)
		if got.Note != "" || got.Hostname != "after-host" || got.UserId != owner.Id {
			t.Fatal("empty note did not clear independently")
		}
	})
}

func TestClientAssignmentExactTargetsAndStaleAdmission(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		actor, owner, peer := assignmentSetup(t, f)
		group := model.DeviceGroup{Name: "ExactGroup"}
		transactionCreate(t, f.DB, &group)
		store, err := clientassign.New(f.Reopen(t), clientassign.Options{})
		if err != nil {
			t.Fatal(err)
		}
		base := clientassign.Input{ID: peer.Id, UUID: peer.Uuid, Note: assignmentValue("must-not-write")}
		for _, input := range []clientassign.Input{
			{ID: peer.Id, UUID: strings.ToLower(peer.Uuid), Note: assignmentValue("bad")},
			{ID: peer.Id, UUID: peer.Uuid, UserName: assignmentValue(strings.ToLower(owner.Username)), Note: assignmentValue("bad")},
			{ID: peer.Id, UUID: peer.Uuid, UserName: assignmentValue(owner.Username + " "), Note: assignmentValue("bad")},
			{ID: peer.Id, UUID: peer.Uuid, UserName: assignmentValue("ÉxactOwner"), Note: assignmentValue("bad")},
			{ID: peer.Id, UUID: peer.Uuid, DeviceGroupName: assignmentValue("ÉxactGroup"), Note: assignmentValue("bad")},
			{ID: peer.Id, UUID: peer.Uuid, DeviceGroupName: assignmentValue(strings.ToLower(group.Name)), Note: assignmentValue("bad")},
		} {
			before := assignmentRead(t, f.DB, peer.RowId)
			if err := store.Assign(context.Background(), actor, input); err == nil {
				t.Fatal("near-match target accepted")
			}
			if after := assignmentRead(t, f.DB, peer.RowId); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected target changed row")
			}
		}
		beforeDuplicate := assignmentRead(t, f.DB, peer.RowId)
		duplicate := model.DeviceGroup{Name: group.Name}
		transactionCreate(t, f.DB, &duplicate)
		if err := store.Assign(context.Background(), actor, clientassign.Input{ID: peer.Id, UUID: peer.Uuid, DeviceGroupName: assignmentValue(group.Name), Note: assignmentValue("bad")}); err == nil {
			t.Fatal("ambiguous group accepted")
		}
		if after := assignmentRead(t, f.DB, peer.RowId); !reflect.DeepEqual(beforeDuplicate, after) {
			t.Fatal("ambiguous group failure changed row")
		}
		before := assignmentRead(t, f.DB, peer.RowId)
		// A principal already accepted by middleware must not retain old authority.
		if err := f.DB.Model(&model.User{}).Where("id = ?", actor.User.ID).Update("is_admin", false).Error; err != nil {
			t.Fatal(err)
		}
		if err := store.Assign(context.Background(), actor, base); err == nil {
			t.Fatal("stale administrator accepted")
		}
		if err := f.DB.Model(&model.User{}).Where("id = ?", actor.User.ID).Update("is_admin", true).Error; err != nil {
			t.Fatal(err)
		}
		short := actor
		short.Session.ExpiresAt = time.Now().UTC().Add(-time.Second)
		if err := store.Assign(context.Background(), short, base); err == nil {
			t.Fatal("expired signed deadline accepted")
		}
		if err := f.DB.Where("id = ?", actor.Session.ID).Delete(&model.UserToken{}).Error; err != nil {
			t.Fatal(err)
		}
		if err := store.Assign(context.Background(), actor, base); err == nil {
			t.Fatal("revoked middleware session accepted")
		}
		if after := assignmentRead(t, f.DB, peer.RowId); !reflect.DeepEqual(before, after) {
			t.Fatal("failed admission changed row")
		}
	})
}

func TestClientAssignmentConcurrentPoolsAndAnonymousReports(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		actor, owner, peer := assignmentSetup(t, f)
		a, err := clientassign.New(f.Reopen(t), clientassign.Options{})
		if err != nil {
			t.Fatal(err)
		}
		b, err := clientassign.New(f.Reopen(t), clientassign.Options{})
		if err != nil {
			t.Fatal(err)
		}
		report, err := clientsysinfo.New(f.Reopen(t))
		if err != nil {
			t.Fatal(err)
		}
		start, results := make(chan struct{}), make(chan error, 3)
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			results <- a.Assign(ctx, actor, clientassign.Input{ID: peer.Id, UUID: peer.Uuid, UserName: assignmentValue(owner.Username), Note: assignmentValue("durable-note")})
		}()
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			results <- b.Assign(ctx, actor, clientassign.Input{ID: peer.Id, UUID: peer.Uuid, DeviceName: assignmentValue("cli-host")})
		}()
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			results <- report.Report(ctx, clientsysinfo.Input{ID: peer.Id, CPU: sysinfoString("reported-cpu")})
		}()
		close(start)
		for i := 0; i < 3; i++ {
			if err := <-results; err != nil {
				t.Fatal("independent pool competition", err)
			}
		}
		got := assignmentRead(t, f.DB, peer.RowId)
		if got.UserId != owner.Id || got.Note != "durable-note" || got.Hostname != "cli-host" || got.Cpu != "reported-cpu" || got.Alias != peer.Alias || got.AbId != peer.AbId {
			t.Fatal("concurrent partial writes lost fields or management boundaries")
		}

		// Opposite actor/target-owner orders must still acquire users in the
		// same ascending order, before their separate sessions and target peer.
		if err := f.DB.Model(&model.User{}).Where("id = ?", owner.Id).Update("is_admin", true).Error; err != nil {
			t.Fatal(err)
		}
		token := model.UserToken{UserId: owner.Id, Token: "second-assignment-token", ExpiredAt: time.Now().Add(time.Hour).Unix()}
		transactionCreate(t, f.DB, &token)
		other := auth.Principal{User: auth.User{ID: owner.Id, IsAdmin: true, Enabled: true}, Session: auth.Session{ID: token.Id, UserID: owner.Id, Token: token.Token, ExpiresAt: time.Now().Add(30 * time.Minute)}}
		crossStart, crossResults := make(chan struct{}), make(chan error, 2)
		go func() {
			<-crossStart
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			crossResults <- a.Assign(ctx, actor, clientassign.Input{ID: peer.Id, UUID: peer.Uuid, UserName: assignmentValue(owner.Username)})
		}()
		go func() {
			<-crossStart
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			crossResults <- b.Assign(ctx, other, clientassign.Input{ID: peer.Id, UUID: peer.Uuid, UserName: assignmentValue("cli-admin")})
		}()
		close(crossStart)
		for i := 0; i < 2; i++ {
			if err := <-crossResults; err != nil {
				t.Fatal("cross-administrator owner assignment", err)
			}
		}
		got = assignmentRead(t, f.DB, peer.RowId)
		if (got.UserId != owner.Id && got.UserId != actor.User.ID) || got.Note != "durable-note" || got.Alias != peer.Alias {
			t.Fatal("cross-administrator update enlarged its scope")
		}
	})
}

func TestClientAssignmentNote279RealMigrationRetry(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		peer := model.Peer{Id: "legacy279", Hostname: "preserved"}
		transactionCreate(t, f.DB, &peer)
		if err := f.DB.Migrator().DropColumn(&model.Peer{}, "Note"); err != nil {
			t.Fatal(err)
		}
		transactionExec(t, f.DB, "ALTER TABLE versions ADD CONSTRAINT reject_note279_version CHECK (version <> 279)")
		if err := database.Update(f.DB, 279, nil); err == nil {
			t.Fatal("failed version write acknowledged")
		}
		var v model.Version
		if err := f.DB.Last(&v).Error; err != nil || v.Version != 278 {
			t.Fatal("failed upgrade advanced version", err)
		}
		if f.Engine == "postgres" && f.DB.Migrator().HasColumn(&model.Peer{}, "Note") {
			t.Fatal("transactional note DDL escaped failed version write")
		}
		drop := "ALTER TABLE versions DROP CONSTRAINT reject_note279_version"
		if f.Engine == "mysql" {
			drop = "ALTER TABLE versions DROP CHECK reject_note279_version"
		}
		transactionExec(t, f.DB, drop)
		if err := database.Update(f.Reopen(t), 279, nil); err != nil {
			t.Fatal("upgrade retry", err)
		}
		got := assignmentRead(t, f.DB, peer.RowId)
		if got.Note != "" || got.Hostname != "preserved" {
			t.Fatal("old row note backfill changed other data")
		}
		transactionExec(t, f.DB, "INSERT INTO peers (id) VALUES ('default-note279')")
		var raw model.Peer
		if err := f.DB.Where("id = ?", "default-note279").First(&raw).Error; err != nil || raw.Note != "" {
			t.Fatal("SQL note default missing", err)
		}
		if err := f.DB.Exec("UPDATE peers SET note = NULL WHERE id = ?", peer.Id).Error; err == nil {
			t.Fatal("NULL device note accepted")
		}
		if err := database.Update(f.DB, 279, nil); err != nil {
			t.Fatal(err)
		}
		if transactionCount(t, f.DB, &model.Version{}) != 2 {
			t.Fatal("retry or repeated startup duplicated version")
		}
	})
}
