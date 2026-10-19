package clientaudit_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/clientaudit"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func auditDB(t *testing.T) *gorm.DB {
	t.Helper()
	return openAuditDB(t, filepath.Join(t.TempDir(), "audit.db"))
}

func openAuditDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(path+"?_txlock=immediate&_busy_timeout=5000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&model.AuditConn{}, &model.AuditFile{}, &model.AuditDeliveryReceipt{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func writer(t *testing.T, db *gorm.DB) *clientaudit.Service {
	t.Helper()
	s, err := clientaudit.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func exec(t *testing.T, db *gorm.DB, sql string) {
	t.Helper()
	if err := db.Exec(sql).Error; err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, db *gorm.DB, row interface{}) int64 {
	t.Helper()
	var n int64
	if err := db.Model(row).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func openEvent() model.AuditConn {
	return model.AuditConn{Action: "new", PeerId: "controlled", Uuid: "device-uuid", ConnId: 1, SessionId: "0", Nonce: "open-nonce", Ip: "192.0.2.1"}
}

func TestConcurrentRetriesStoreOneOpenAndFilePerWriter(t *testing.T) {
	db := auditDB(t)
	s := writer(t, db)
	file := model.AuditFile{PeerId: "controlled", Uuid: "device-uuid", ConnId: 1, Nonce: "file-nonce", Info: `{"num":1,"files":[["file.txt",12]]}`}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Conn(context.Background(), openEvent()); err != nil {
				t.Error(err)
			}
			if err := s.File(context.Background(), file); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if count(t, db, &model.AuditConn{}) != 1 || count(t, db, &model.AuditFile{}) != 1 {
		t.Fatal("duplicate retry stored")
	}
	// Nonces are scoped by controlled device, not globally trusted identities.
	other := openEvent()
	other.Uuid = "other-device"
	if err := s.Conn(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if count(t, db, &model.AuditConn{}) != 2 {
		t.Fatal("another device was swallowed by nonce")
	}
	file.Path = "changed"
	if err := s.File(context.Background(), file); !errors.Is(err, clientaudit.ErrConflict) {
		t.Fatalf("changed file nonce: %v", err)
	}
}

func TestFailedAuditWritesDoNotBurnNonceAndRetrySucceeds(t *testing.T) {
	for _, table := range []string{"audit_conns", "audit_files"} {
		t.Run(table, func(t *testing.T) {
			db := auditDB(t)
			s := writer(t, db)
			exec(t, db, fmt.Sprintf("CREATE TRIGGER audit_fault BEFORE INSERT ON %s BEGIN SELECT RAISE(ABORT, 'private-database-error'); END", table))
			write := func() error {
				if table == "audit_conns" {
					return s.Conn(context.Background(), openEvent())
				}
				return s.File(context.Background(), model.AuditFile{PeerId: "controlled", Uuid: "uuid", ConnId: 1, Nonce: "file-nonce", Info: `{}`})
			}
			if err := write(); !errors.Is(err, clientaudit.ErrUnavailable) {
				t.Fatalf("write failure=%v", err)
			}
			exec(t, db, "DROP TRIGGER audit_fault")
			if err := write(); err != nil {
				t.Fatal(err)
			}
			if err := write(); err != nil {
				t.Fatal(err)
			}
			row := interface{}(&model.AuditConn{})
			if table == "audit_files" {
				row = &model.AuditFile{}
			}
			if count(t, db, row) != 1 {
				t.Fatal("failed nonce was burned or successful retry duplicated")
			}
		})
	}
}

func TestConnectionAssociationUsesUUIDAndExactSessionAndWritesZeros(t *testing.T) {
	db := auditDB(t)
	s := writer(t, db)
	first := openEvent()
	if err := s.Conn(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	// The older zero-session open must not steal a later exact session's
	// authorization updates. The newest candidate open becomes associated.
	other := first
	other.Nonce = "open-second"
	if err := s.Conn(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	patch := first
	patch.Action = ""
	patch.SessionId = "18446744073709551615"
	patch.Nonce = "auth-first"
	patch.Type = 4
	patch.PrimaryAuth = 3
	patch.TwoFactor = 2
	if err := s.Conn(context.Background(), patch); err != nil {
		t.Fatal(err)
	}
	patch.Type, patch.PrimaryAuth, patch.TwoFactor = 0, 0, 0
	patch.Nonce = "auth-zero"
	if err := s.Conn(context.Background(), patch); err != nil {
		t.Fatal(err)
	}
	var rows []model.AuditConn
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows[1].SessionId != patch.SessionId || rows[1].Type != 0 || rows[1].PrimaryAuth != 0 || rows[1].TwoFactor != 0 || rows[0].SessionId != "0" {
		t.Fatal("zero values lost or exact-session update touched older zero-session connection")
	}
	patch.Uuid = "wrong-device"
	if err := s.Conn(context.Background(), patch); !errors.Is(err, clientaudit.ErrMissingConnection) {
		t.Fatalf("wrong-device connection: %v", err)
	}
	// An already-associated different session is never a fallback candidate.
	third := first
	third.Uuid = "third"
	third.Nonce = "third-open"
	if err := s.Conn(context.Background(), third); err != nil {
		t.Fatal(err)
	}
	third.Action = ""
	third.SessionId = "111"
	third.Nonce = "third-auth"
	if err := s.Conn(context.Background(), third); err != nil {
		t.Fatal(err)
	}
	third.SessionId = "222"
	third.Nonce = "third-other-auth"
	if err := s.Conn(context.Background(), third); !errors.Is(err, clientaudit.ErrMissingConnection) {
		t.Fatalf("cross-session update: %v", err)
	}
}

func TestMissingConnectionAndQueryOrUpdateFaultAreNotSuccess(t *testing.T) {
	db := auditDB(t)
	s := writer(t, db)
	patch := openEvent()
	patch.Action = ""
	patch.SessionId = "9"
	patch.Nonce = "auth-nonce"
	if err := s.Conn(context.Background(), patch); !errors.Is(err, clientaudit.ErrMissingConnection) {
		t.Fatal(err)
	}
	if err := s.Conn(context.Background(), openEvent()); err != nil {
		t.Fatal(err)
	}
	exec(t, db, "CREATE TRIGGER update_fault BEFORE UPDATE ON audit_conns BEGIN SELECT RAISE(ABORT, 'private-update-error'); END")
	if err := s.Conn(context.Background(), patch); !errors.Is(err, clientaudit.ErrUnavailable) {
		t.Fatal(err)
	}
	exec(t, db, "DROP TRIGGER update_fault")
	if err := s.Conn(context.Background(), patch); err != nil {
		t.Fatal(err)
	}
	exec(t, db, "DROP TABLE audit_conns")
	patch.Nonce = "auth-query-after-drop"
	if err := s.Conn(context.Background(), patch); !errors.Is(err, clientaudit.ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestCloseRetryPreservesFirstCloseTimestamp(t *testing.T) {
	db := auditDB(t)
	s := writer(t, db)
	if err := s.Conn(context.Background(), openEvent()); err != nil {
		t.Fatal(err)
	}
	closed := openEvent()
	closed.Action = "close"
	closed.Nonce = "close-nonce"
	if err := s.Conn(context.Background(), closed); err != nil {
		t.Fatal(err)
	}
	// Distinct from server time so overwriting it on a retry is observable.
	exec(t, db, "UPDATE audit_conns SET close_time = 12345")
	if err := s.Conn(context.Background(), closed); err != nil {
		t.Fatal(err)
	}
	var row model.AuditConn
	if err := db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.CloseTime != 12345 {
		t.Fatal("close retry changed the stored close time")
	}
}

func TestAuditContextCancelsInflightWriteAndDatabaseWait(t *testing.T) {
	db := auditDB(t)
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	s := writer(t, db)
	entered := make(chan struct{})
	var paused atomic.Bool
	if err := db.Callback().Create().Before("gorm:create").Register("wait-cancel", func(tx *gorm.DB) {
		if tx.Statement.Table == "audit_conns" && paused.CompareAndSwap(false, true) {
			close(entered)
			<-tx.Statement.Context.Done()
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Create().Remove("wait-cancel")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Conn(ctx, openEvent()) }()
	<-entered
	waiting, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if err := s.Conn(waiting, openEvent()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if count(t, db, &model.AuditConn{}) != 0 {
		t.Fatal("canceled audit committed")
	}
	if count(t, db, &model.AuditDeliveryReceipt{}) != 0 {
		t.Fatal("canceled audit burned its receipt")
	}
	if err := s.Conn(context.Background(), openEvent()); err != nil {
		t.Fatal(err)
	}
}

func TestCrossStoreSQLiteHandlesCommitOneReceiptAndEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	firstDB, secondDB := openAuditDB(t, path), openAuditDB(t, path)
	first, second := writer(t, firstDB), writer(t, secondDB)
	file := model.AuditFile{PeerId: "controlled", Uuid: "device-uuid", ConnId: 1, Nonce: "open-nonce", Info: `{}`}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			s := first
			if index%2 != 0 {
				s = second
			}
			if err := s.Conn(context.Background(), openEvent()); err != nil {
				t.Error(err)
			}
			if err := s.File(context.Background(), file); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if count(t, firstDB, &model.AuditConn{}) != 1 || count(t, secondDB, &model.AuditFile{}) != 1 || count(t, firstDB, &model.AuditDeliveryReceipt{}) != 2 {
		t.Fatal("cross-store duplicate or endpoint key collision")
	}
	var rows []model.AuditDeliveryReceipt
	if err := firstDB.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if len(row.Key) != 64 || len(row.PayloadHash) != 64 {
			t.Fatal("receipt is not a fixed SHA256 key/hash")
		}
	}
}

func TestConcurrentDifferentPayloadsShareOneReceiptKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conflicting.db")
	firstDB, secondDB := openAuditDB(t, path), openAuditDB(t, path)
	first, second := writer(t, firstDB), writer(t, secondDB)
	results := make(chan error, 2)
	a, b := openEvent(), openEvent()
	b.Ip = "192.0.2.2"
	go func() { results <- first.Conn(context.Background(), a) }()
	go func() { results <- second.Conn(context.Background(), b) }()
	var successful, conflicts int
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			successful++
		} else if errors.Is(err, clientaudit.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successful != 1 || conflicts != 1 || count(t, firstDB, &model.AuditConn{}) != 1 || count(t, firstDB, &model.AuditDeliveryReceipt{}) != 1 {
		t.Fatal("conflicting concurrent request was acknowledged")
	}
}

func TestReceiptsSurviveBusinessChangesAndDeletionWithoutRecreatingRows(t *testing.T) {
	db := auditDB(t)
	s := writer(t, db)
	open := openEvent()
	if err := s.Conn(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	auth := open
	auth.Action = ""
	auth.Nonce = "auth"
	auth.SessionId = "18446744073709551615"
	auth.Type = 4
	if err := s.Conn(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	close := auth
	close.Action = "close"
	close.Nonce = "close"
	if err := s.Conn(context.Background(), close); err != nil {
		t.Fatal(err)
	}
	// Open's original zero-session payload is compared against its receipt,
	// rather than against the mutable connection row updated by authorization.
	if err := s.Conn(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	file := model.AuditFile{PeerId: open.PeerId, Uuid: open.Uuid, ConnId: open.ConnId, Nonce: "file", Info: `{}`}
	if err := s.File(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	exec(t, db, "DELETE FROM audit_conns")
	exec(t, db, "DELETE FROM audit_files")
	for _, event := range []model.AuditConn{open, auth, close} {
		if err := s.Conn(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.File(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	if count(t, db, &model.AuditConn{}) != 0 || count(t, db, &model.AuditFile{}) != 0 || count(t, db, &model.AuditDeliveryReceipt{}) != 4 {
		t.Fatal("replayed request resurrected a deleted business audit")
	}
}

func TestReceiptFingerprintsCoverConnectionSemantics(t *testing.T) {
	for name, change := range map[string]func(*model.AuditConn){
		"action": func(a *model.AuditConn) { a.Action = "close" }, "conn": func(a *model.AuditConn) { a.ConnId++ },
		"session": func(a *model.AuditConn) { a.SessionId = "18446744073709551615" }, "peer": func(a *model.AuditConn) { a.FromPeer = "other" },
		"name": func(a *model.AuditConn) { a.FromName = "other" }, "ip": func(a *model.AuditConn) { a.Ip = "192.0.2.9" },
		"type": func(a *model.AuditConn) { a.Type = 4 }, "primary": func(a *model.AuditConn) { a.PrimaryAuth = 4 },
		"factor": func(a *model.AuditConn) { a.TwoFactor = 2 }, "reference": func(a *model.AuditConn) { a.ConnAuditRef = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			db := auditDB(t)
			s := writer(t, db)
			original := openEvent()
			if err := s.Conn(context.Background(), original); err != nil {
				t.Fatal(err)
			}
			changed := original
			change(&changed)
			if err := s.Conn(context.Background(), changed); !errors.Is(err, clientaudit.ErrConflict) {
				t.Fatalf("changed %s was accepted: %v", name, err)
			}
			if count(t, db, &model.AuditConn{}) != 1 || count(t, db, &model.AuditDeliveryReceipt{}) != 1 {
				t.Fatal("conflict changed committed data")
			}
		})
	}
}

func TestReceiptReadAfterFailureRunsOutsideAbortedTransaction(t *testing.T) {
	db := auditDB(t)
	s := writer(t, db)
	if err := s.Conn(context.Background(), openEvent()); err != nil {
		t.Fatal(err)
	}
	var checked bool
	if err := db.Callback().Query().Before("gorm:query").Register("check-outside-tx", func(query *gorm.DB) {
		if query.Statement.Table != "audit_delivery_receipts" {
			return
		}
		checked = true
		if _, inside := query.Statement.ConnPool.(*sql.Tx); inside {
			t.Error("receipt recheck queried an aborted transaction")
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Query().Remove("check-outside-tx")
	if err := s.Conn(context.Background(), openEvent()); err != nil || !checked {
		t.Fatalf("receipt recheck: checked=%v error=%v", checked, err)
	}
}

func TestReceiptInsertFailureAndNoNonceBestEffort(t *testing.T) {
	db := auditDB(t)
	s := writer(t, db)
	exec(t, db, "CREATE TRIGGER receipt_fault BEFORE INSERT ON audit_delivery_receipts BEGIN SELECT RAISE(ABORT, 'private-receipt-error'); END")
	if err := s.Conn(context.Background(), openEvent()); !errors.Is(err, clientaudit.ErrUnavailable) {
		t.Fatal(err)
	}
	if count(t, db, &model.AuditConn{}) != 0 || count(t, db, &model.AuditDeliveryReceipt{}) != 0 {
		t.Fatal("failed receipt wrote a business event")
	}
	exec(t, db, "DROP TRIGGER receipt_fault")
	if err := s.Conn(context.Background(), openEvent()); err != nil {
		t.Fatal(err)
	}
	noNonce := openEvent()
	noNonce.Nonce = ""
	for i := 0; i < 2; i++ {
		if err := s.Conn(context.Background(), noNonce); err != nil {
			t.Fatal(err)
		}
	}
	if count(t, db, &model.AuditConn{}) != 3 || count(t, db, &model.AuditDeliveryReceipt{}) != 1 {
		t.Fatal("no-nonce requests were given an invented idempotency key")
	}
}

func TestReceiptsSeparateReconnectionFromHistoricalReplay(t *testing.T) {
	db := auditDB(t)
	s := writer(t, db)
	open := openEvent()
	if err := s.Conn(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	auth := open
	auth.Action = ""
	auth.Nonce = "auth-first"
	auth.SessionId = "18446744073709551615"
	auth.Type = 4
	if err := s.Conn(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	close := auth
	close.Action = "close"
	close.Nonce = "close-first"
	if err := s.Conn(context.Background(), close); err != nil {
		t.Fatal(err)
	}
	// A new controlled process and the reconnecting controller can reuse both
	// conn_id and session_id, but each actual delivery has a distinct nonce.
	reopened := open
	reopened.Nonce = "open-second"
	if err := s.Conn(context.Background(), reopened); err != nil {
		t.Fatal(err)
	}
	reauth := auth
	reauth.Nonce = "auth-second"
	reauth.Type = 0
	if err := s.Conn(context.Background(), reauth); err != nil {
		t.Fatal(err)
	}
	if err := s.Conn(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	if err := s.Conn(context.Background(), close); err != nil {
		t.Fatal(err)
	}
	var rows []model.AuditConn
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].CloseTime == 0 || rows[0].Type != 4 || rows[1].CloseTime != 0 || rows[1].Type != 0 || rows[1].SessionId != reauth.SessionId {
		t.Fatal("new reconnect or historical retry targeted the wrong connection")
	}
	if count(t, db, &model.AuditDeliveryReceipt{}) != 5 {
		t.Fatal("replay inserted another receipt")
	}
}

func TestCrashReconnectAssociatesAuthAndCloseWithLatestOpen(t *testing.T) {
	db := auditDB(t)
	s := writer(t, db)
	oldOpen := openEvent()
	if err := s.Conn(context.Background(), oldOpen); err != nil {
		t.Fatal(err)
	}
	oldAuth := oldOpen
	oldAuth.Action = ""
	oldAuth.Nonce = "auth-before-crash"
	oldAuth.SessionId = "18446744073709551615"
	oldAuth.FromPeer, oldAuth.FromName = "controller", "before-crash"
	oldAuth.Type, oldAuth.PrimaryAuth, oldAuth.TwoFactor = 4, 3, 2
	if err := s.Conn(context.Background(), oldAuth); err != nil {
		t.Fatal(err)
	}
	// No close was delivered before the controlled process crashed. The new
	// process reuses conn_id; the reconnecting controller reuses its session.
	newOpen := oldOpen
	newOpen.Nonce = "open-after-crash"
	if err := s.Conn(context.Background(), newOpen); err != nil {
		t.Fatal(err)
	}
	newAuth := oldAuth
	newAuth.Nonce = "auth-after-crash"
	newAuth.FromName = "after-crash"
	newAuth.Type, newAuth.PrimaryAuth, newAuth.TwoFactor = 0, 0, 0
	if err := s.Conn(context.Background(), newAuth); err != nil {
		t.Fatal(err)
	}
	// Old committed deliveries are identified by receipts before association;
	// they cannot change the new row or recreate another open.
	for _, replay := range []model.AuditConn{oldOpen, oldAuth} {
		if err := s.Conn(context.Background(), replay); err != nil {
			t.Fatal(err)
		}
	}
	var rows []model.AuditConn
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].FromName != "before-crash" || rows[0].Type != 4 || rows[0].CloseTime != 0 ||
		rows[1].FromName != "after-crash" || rows[1].SessionId != newAuth.SessionId || rows[1].Type != 0 || rows[1].PrimaryAuth != 0 || rows[1].TwoFactor != 0 || rows[1].CloseTime != 0 {
		t.Fatal("crash reconnect authorization targeted old live connection")
	}
	close := newAuth
	close.Action, close.Nonce = "close", "close-after-crash"
	if err := s.Conn(context.Background(), close); err != nil {
		t.Fatal(err)
	}
	rows = nil
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].CloseTime != 0 || rows[1].CloseTime == 0 {
		t.Fatal("crash reconnect close targeted old live connection")
	}
	if count(t, db, &model.AuditDeliveryReceipt{}) != 5 {
		t.Fatal("historical replay added receipts")
	}
}

func TestCloseAfterCrashBeforeAuthorizationUsesLatestZeroOpen(t *testing.T) {
	db := auditDB(t)
	s := writer(t, db)
	old := openEvent()
	old.SessionId = "77"
	if err := s.Conn(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	newOpen := openEvent()
	newOpen.Nonce = "open-after-crash"
	if err := s.Conn(context.Background(), newOpen); err != nil {
		t.Fatal(err)
	}
	// LoginRequest has supplied the session, but the authorization audit did
	// not reach storage. Close must associate the latest zero-session open.
	close := old
	close.Action, close.Nonce = "close", "close-before-auth"
	if err := s.Conn(context.Background(), close); err != nil {
		t.Fatal(err)
	}
	var rows []model.AuditConn
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].CloseTime != 0 || rows[1].CloseTime == 0 || rows[1].SessionId != "77" {
		t.Fatal("early close preferred old exact session over new open")
	}
}

func TestAuditInstancesUseTheirOwnDatabase(t *testing.T) {
	firstDB, secondDB := auditDB(t), auditDB(t)
	first, second := writer(t, firstDB), writer(t, secondDB)
	if err := first.Conn(context.Background(), openEvent()); err != nil {
		t.Fatal(err)
	}
	if count(t, secondDB, &model.AuditConn{}) != 0 {
		t.Fatal("audit crossed installations")
	}
	if err := second.Conn(context.Background(), openEvent()); err != nil {
		t.Fatal(err)
	}
}
