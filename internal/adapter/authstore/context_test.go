package authstore_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

// The callback pauses at an actual GORM operation until its request context is
// canceled. All reads/writes still use a real, independently owned SQLite DB.
func blockOperation(t *testing.T, db *gorm.DB, operation, table string, entered chan<- struct{}) func() {
	t.Helper()
	var signal sync.Once
	callback := func(tx *gorm.DB) {
		if tx.Statement.Table != table {
			return
		}
		signal.Do(func() { close(entered) })
		<-tx.Statement.Context.Done()
	}
	var remove func() error
	switch operation {
	case "query":
		if err := db.Callback().Query().Before("gorm:query").Register("test:context", callback); err != nil {
			t.Fatal(err)
		}
		remove = func() error { return db.Callback().Query().Remove("test:context") }
	case "update":
		if err := db.Callback().Update().Before("gorm:update").Register("test:context", callback); err != nil {
			t.Fatal(err)
		}
		remove = func() error { return db.Callback().Update().Remove("test:context") }
	case "delete":
		if err := db.Callback().Delete().Before("gorm:delete").Register("test:context", callback); err != nil {
			t.Fatal(err)
		}
		remove = func() error { return db.Callback().Delete().Remove("test:context") }
	default:
		t.Fatalf("unknown operation %s", operation)
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			if err := remove(); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(cleanup)
	return cleanup
}

func TestInFlightContextsFailClosedWithoutPartialWrites(t *testing.T) {
	for _, operation := range []string{"session_query", "user_query", "renew", "revoke", "admin_delete"} {
		for _, mode := range []string{"cancel", "deadline"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				db := openDB(t)
				u, actor := fixture(t, db, operation == "admin_delete", "opaque", now.Add(time.Minute))
				peer := model.Peer{Id: "peer", Uuid: actor.DeviceUuid, UserId: u.Id}
				create(t, db, &peer)
				target := model.UserToken{UserId: u.Id, Token: "target", ExpiredAt: now.Add(time.Hour).Unix()}
				create(t, db, &target)
				s, _ := newService(t, db, nil)
				var principal auth.Principal
				if operation == "revoke" || operation == "admin_delete" {
					var err error
					principal, err = s.Authenticate(context.Background(), actor.Token)
					if err != nil {
						t.Fatal(err)
					}
				}
				var before []model.UserToken
				if err := db.Order("id").Find(&before).Error; err != nil {
					t.Fatal(err)
				}
				callback, table := "query", "user_tokens"
				switch operation {
				case "user_query":
					table = "users"
				case "renew":
					callback = "update"
				case "revoke", "admin_delete":
					callback = "delete"
				}
				entered := make(chan struct{})
				remove := blockOperation(t, db, callback, table, entered)
				var ctx context.Context
				var cancel context.CancelFunc
				want := context.Canceled
				if mode == "deadline" {
					ctx, cancel = context.WithTimeout(context.Background(), 500*time.Millisecond)
					want = context.DeadlineExceeded
				} else {
					ctx, cancel = context.WithCancel(context.Background())
				}
				defer cancel()
				finished := make(chan error, 1)
				go func() {
					switch operation {
					case "revoke":
						finished <- s.Revoke(ctx, principal, actor.Token)
					case "admin_delete":
						finished <- s.RevokeSessions(ctx, principal, []uint{target.Id})
					default:
						_, err := s.Authenticate(ctx, actor.Token)
						finished <- err
					}
				}()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("request never reached the database operation")
				}
				if mode == "cancel" {
					cancel()
				}
				select {
				case err := <-finished:
					if !errors.Is(err, want) || errors.Is(err, auth.ErrUnauthenticated) {
						t.Fatalf("request context misclassified: %v, want %v", err, want)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("database operation ignored request cancellation")
				}
				remove()
				var after []model.UserToken
				if err := db.Order("id").Find(&after).Error; err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("canceled transaction changed sessions: before=%+v after=%+v", before, after)
				}
				if err := db.First(&peer, peer.RowId).Error; err != nil || peer.UserId != u.Id {
					t.Fatalf("canceled operation changed device binding: %+v, %v", peer, err)
				}
			})
		}
	}
}

func TestDatabaseReadFaultsAreUnavailableNotUnauthorized(t *testing.T) {
	for _, table := range []string{"users", "user_tokens"} {
		t.Run(table, func(t *testing.T) {
			db := openDB(t)
			fixture(t, db, false, "opaque", now.Add(time.Hour))
			s, _ := newService(t, db, nil)
			execSQL(t, db, "DROP TABLE "+table)
			_, err := s.Authenticate(context.Background(), "opaque")
			if !errors.Is(err, auth.ErrUnavailable) || errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatalf("schema/query error misclassified: %v", err)
			}
		})
	}
}

func TestFailedAdministratorBatchDeleteIsAtomic(t *testing.T) {
	db := openDB(t)
	u, actor := fixture(t, db, true, "admin", now.Add(time.Hour))
	first := model.UserToken{UserId: u.Id, Token: "first", ExpiredAt: now.Add(time.Hour).Unix()}
	second := model.UserToken{UserId: u.Id, Token: "second", ExpiredAt: now.Add(time.Hour).Unix()}
	create(t, db, &first)
	create(t, db, &second)
	s, _ := newService(t, db, nil)
	principal, err := s.Authenticate(context.Background(), actor.Token)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, "CREATE TRIGGER fail_batch BEFORE DELETE ON user_tokens WHEN OLD.token = 'second' BEGIN SELECT RAISE(ABORT, 'injected batch failure'); END")
	err = s.RevokeSessions(context.Background(), principal, []uint{first.Id, second.Id})
	if !errors.Is(err, auth.ErrUnavailable) || errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("batch error lost: %v", err)
	}
	var count int64
	if err := db.Model(&model.UserToken{}).Where("id IN ?", []uint{first.Id, second.Id}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("partial batch deletion: count=%d, err=%v", count, err)
	}
	execSQL(t, db, "DROP TRIGGER fail_batch")
	if err := s.RevokeSessions(context.Background(), principal, []uint{first.Id, second.Id}); err != nil {
		t.Fatalf("retry after rolled back deletion: %v", err)
	}
}

func TestFailedRenewalLeavesOriginalExpiry(t *testing.T) {
	db := openDB(t)
	_, row := fixture(t, db, false, "opaque", now.Add(time.Minute))
	s, _ := newService(t, db, nil)
	execSQL(t, db, "CREATE TRIGGER fail_renew_state BEFORE UPDATE ON user_tokens BEGIN SELECT RAISE(ABORT, 'injected renewal failure'); END")
	if _, err := s.Authenticate(context.Background(), "opaque"); !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("renewal fault classification: %v", err)
	}
	var unchanged model.UserToken
	if err := db.First(&unchanged, row.Id).Error; err != nil || unchanged.ExpiredAt != row.ExpiredAt {
		t.Fatalf("failed renewal changed stored lifetime: %+v, %v", unchanged, err)
	}
}
