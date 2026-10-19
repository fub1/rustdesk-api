package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/authstore"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/identitystore"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/sessiontoken"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientaudit"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientstrategy"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

func transactionCount(t *testing.T, db *gorm.DB, row interface{}) int64 {
	t.Helper()
	var n int64
	if err := db.Model(row).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func transactionCreate(t *testing.T, db *gorm.DB, row interface{}) {
	t.Helper()
	if err := db.Create(row).Error; err != nil {
		t.Fatal(err)
	}
}

func transactionExec(t *testing.T, db *gorm.DB, statement string) {
	t.Helper()
	if err := db.Exec(statement).Error; err != nil {
		t.Fatal(err)
	}
}

func auditOpen() model.AuditConn {
	return model.AuditConn{Action: model.AuditActionNew, PeerId: "device", Uuid: "uuid", ConnId: 1, SessionId: "18446744073709551615", Nonce: "same-delivery"}
}

func TestAuditIndependentPoolsArbitrateReceipts(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		const writers = 12
		services := make([]*clientaudit.Service, writers)
		for i := range services {
			var err error
			services[i], err = clientaudit.New(f.Reopen(t))
			if err != nil {
				t.Fatal(err)
			}
		}
		start := make(chan struct{})
		results := make(chan error, writers)
		var wg sync.WaitGroup
		for _, service := range services {
			wg.Add(1)
			go func(s *clientaudit.Service) {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				results <- s.Conn(ctx, auditOpen())
			}(service)
		}
		close(start)
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatalf("same committed delivery must resolve outside failed transaction: %v", err)
			}
		}
		if transactionCount(t, f.DB, &model.AuditConn{}) != 1 || transactionCount(t, f.DB, &model.AuditDeliveryReceipt{}) != 1 {
			t.Fatal("cross-pool retry duplicated event/receipt")
		}
		var stored model.AuditConn
		if err := f.DB.First(&stored).Error; err != nil || stored.SessionId != auditOpen().SessionId {
			t.Fatalf("u64 changed: %q, %v", stored.SessionId, err)
		}
		conflict := auditOpen()
		conflict.Ip = "192.0.2.9"
		if err := services[0].Conn(context.Background(), conflict); !errors.Is(err, clientaudit.ErrConflict) {
			t.Fatalf("different payload was acknowledged: %v", err)
		}
		if err := f.DB.Delete(&stored).Error; err != nil {
			t.Fatal(err)
		}
		if err := services[1].Conn(context.Background(), auditOpen()); err != nil {
			t.Fatal(err)
		}
		if transactionCount(t, f.DB, &model.AuditConn{}) != 0 {
			t.Fatal("receipt replay resurrected deleted business row")
		}
	})
}

// PG's deferred foreign key makes INSERT succeed and COMMIT fail. MySQL has
// no deferred FK: terminate the actual server connection only AFTER its final
// INSERT succeeded, leaving the server to roll back the uncommitted transaction.
// This is not a simulated Commit implementation or a statement-time SIGNAL.
func failTransactionCommit(t *testing.T, f *fixture, target string) func() {
	t.Helper()
	if f.Engine == "postgres" {
		transactionExec(t, f.DB, "CREATE TABLE integration_commit_parent (id bigint PRIMARY KEY)")
		transactionExec(t, f.DB, "CREATE TABLE integration_commit_guard (parent_id bigint REFERENCES integration_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)")
		transactionExec(t, f.DB, "CREATE FUNCTION integration_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO integration_commit_guard(parent_id) VALUES (999999); RETURN NEW; END $$")
		transactionExec(t, f.DB, "CREATE TRIGGER integration_fail_commit AFTER INSERT ON "+target+" FOR EACH ROW EXECUTE FUNCTION integration_fail_commit()")
		return func() {
			transactionExec(t, f.DB, "DROP TRIGGER integration_fail_commit ON "+target)
			transactionExec(t, f.DB, "DROP FUNCTION integration_fail_commit()")
			transactionExec(t, f.DB, "DROP TABLE integration_commit_guard")
			transactionExec(t, f.DB, "DROP TABLE integration_commit_parent")
		}
	}
	control := f.Reopen(t)
	var fired atomic.Bool
	var killed atomic.Bool
	if err := f.DB.Callback().Create().After("gorm:create").Register("integration:kill-before-commit", func(tx *gorm.DB) {
		if tx.Statement.Table != target || tx.Error != nil || !fired.CompareAndSwap(false, true) {
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
		if !fired.Load() {
			t.Error("commit failure hook never reached successful final INSERT")
		}
		if !killed.Load() {
			t.Error("actual MySQL transaction connection was not terminated")
		}
		if err := f.DB.Callback().Create().Remove("integration:kill-before-commit"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuditRealCommitFailureDoesNotConsumeReceipt(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		service, err := clientaudit.New(f.DB)
		if err != nil {
			t.Fatal(err)
		}
		remove := failTransactionCommit(t, f, "audit_conns")
		if err := service.Conn(context.Background(), auditOpen()); !errors.Is(err, clientaudit.ErrUnavailable) {
			t.Fatalf("failed COMMIT received success: %v", err)
		}
		remove()
		fresh := f.Reopen(t)
		if transactionCount(t, fresh, &model.AuditConn{}) != 0 || transactionCount(t, fresh, &model.AuditDeliveryReceipt{}) != 0 {
			t.Fatal("failed commit persisted receipt/event")
		}
		retry, _ := clientaudit.New(fresh)
		for i := 0; i < 3; i++ {
			if err := retry.Conn(context.Background(), auditOpen()); err != nil {
				t.Fatal(err)
			}
		}
		if transactionCount(t, fresh, &model.AuditConn{}) != 1 || transactionCount(t, fresh, &model.AuditDeliveryReceipt{}) != 1 {
			t.Fatal("cross-connection retry after failed commit duplicated/lost data")
		}
	})
}

func identityConfig(t *testing.T, db *gorm.DB) (*identitystore.Store, auth.ProviderConfig) {
	t.Helper()
	enabled := true
	row := model.Oauth{Op: "integration", OauthType: "oidc", Issuer: "https://identity.test", ClientId: "integration", AutoRegister: &enabled, PkceEnable: &enabled, PkceMethod: "S256"}
	transactionCreate(t, db, &row)
	s, err := identitystore.New(db, sessiontoken.NewIssuer(nil), time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Provider(context.Background(), row.Op)
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestIdentityRealCommitFailureRollsBackAdmission(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		s, c := identityConfig(t, f.DB)
		initialUsers := transactionCount(t, f.DB, &model.User{})
		remove := failTransactionCommit(t, f, "login_logs")
		identity := auth.ExternalIdentity{Issuer: c.Issuer, Subject: "new-sub", Username: "new"}
		p, err := s.Login(context.Background(), identity, c, auth.IssueRequest{})
		if !errors.Is(err, auth.ErrUnavailable) || p.Session.Token != "" {
			t.Fatalf("failed admission COMMIT exposed credential: %v", err)
		}
		remove()
		fresh := f.Reopen(t)
		if transactionCount(t, fresh, &model.User{}) != initialUsers {
			t.Fatal("failed admission left orphan user")
		}
		for _, row := range []interface{}{&model.OIDCIdentity{}, &model.UserToken{}, &model.LoginLog{}} {
			if transactionCount(t, fresh, row) != 0 {
				t.Fatal("failed admission left partial identity/session/log")
			}
		}
		retry, _ := identitystore.New(fresh, sessiontoken.NewIssuer(nil), time.Hour, nil)
		p, err = retry.Login(context.Background(), identity, c, auth.IssueRequest{})
		if err != nil || p.Session.Token == "" {
			t.Fatalf("admission retry failed: %v", err)
		}
		if transactionCount(t, fresh, &model.User{}) != initialUsers+1 || transactionCount(t, fresh, &model.OIDCIdentity{}) != 1 {
			t.Fatal("admission retry produced wrong identity ownership")
		}
	})
}

func TestIdentityConcurrentAdmissionAndByteExactKey(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, c := identityConfig(t, f.DB)
		initialUsers := transactionCount(t, f.DB, &model.User{})
		const writers = 8
		stores := make([]*identitystore.Store, writers)
		for i := range stores {
			var err error
			stores[i], err = identitystore.New(f.Reopen(t), sessiontoken.NewIssuer(nil), time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
		}
		type outcome struct {
			p   auth.Principal
			err error
		}
		start, results := make(chan struct{}), make(chan outcome, writers)
		var wg sync.WaitGroup
		for _, store := range stores {
			wg.Add(1)
			go func(s *identitystore.Store) {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				p, err := s.Login(ctx, auth.ExternalIdentity{Issuer: c.Issuer, Subject: "same-sub", Email: "admin@example.test"}, c, auth.IssueRequest{})
				results <- outcome{p, err}
			}(store)
		}
		close(start)
		wg.Wait()
		close(results)
		var owner uint
		for result := range results {
			if result.err != nil {
				t.Fatalf("provider lock did not serialize admission: %v", result.err)
			}
			if owner == 0 {
				owner = result.p.User.ID
			}
			if result.p.User.ID != owner || owner == 1 || result.p.User.IsAdmin {
				t.Fatal("identity ownership split or administrator reused")
			}
		}
		if transactionCount(t, f.DB, &model.User{}) != initialUsers+1 || transactionCount(t, f.DB, &model.OIDCIdentity{}) != 1 || transactionCount(t, f.DB, &model.UserToken{}) != writers {
			t.Fatal("concurrent admission persisted wrong counts")
		}
		// Exercise the production migration's MySQL VARBINARY conversion, not a
		// test-only AutoMigrate under SQLite's inherently binary text equality.
		for _, pair := range [][2]string{{c.Issuer, "SAME-SUB"}, {c.Issuer, "same-sub "}, {"https://IDENTITY.test", "same-sub"}, {c.Issuer, "é"}, {c.Issuer, "e\u0301"}} {
			transactionCreate(t, f.DB, &model.OIDCIdentity{Issuer: pair[0], Subject: pair[1], UserID: owner})
		}
		duplicate := model.OIDCIdentity{Issuer: c.Issuer, Subject: "same-sub", UserID: 1}
		if err := f.DB.Create(&duplicate).Error; err == nil {
			t.Fatal("exact issuer/sub duplicate bypassed database unique index")
		}
		if transactionCount(t, f.DB, &model.OIDCIdentity{}) != 6 {
			t.Fatal("byte-distinct identities collapsed")
		}
		var link model.OIDCIdentity
		if err := f.DB.Where("issuer = ? AND sub = ?", c.Issuer, "same-sub").First(&link).Error; err != nil || link.UserID != owner {
			t.Fatalf("duplicate changed owner: %v", err)
		}
		if err := f.DB.Create(&model.OIDCIdentity{Issuer: c.Issuer, Subject: "orphan", UserID: 999999}).Error; err == nil {
			t.Fatal("identity user FK absent")
		}
	})
}

func TestSessionConcurrentRenewAndRevokeCannotResurrect(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		a, _ := authstore.New(f.Reopen(t))
		b, _ := authstore.New(f.Reopen(t))
		for i := 0; i < 8; i++ {
			now := time.Now().UTC().Truncate(time.Second)
			row := model.UserToken{UserId: 1, Token: fmt.Sprintf("session-%d", i), ExpiredAt: now.Add(time.Minute).Unix()}
			transactionCreate(t, f.DB, &row)
			_, current, err := a.Lookup(context.Background(), row.Token)
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			results := make(chan error, 2)
			go func() {
				<-start
				_, err := a.Renew(context.Background(), current, now.Add(time.Hour), now)
				results <- err
			}()
			go func() { <-start; results <- b.Revoke(context.Background(), current) }()
			close(start)
			for n := 0; n < 2; n++ {
				if err := <-results; err != nil && !errors.Is(err, auth.ErrUnauthenticated) {
					t.Fatalf("unexpected renew/revoke error: %v", err)
				}
			}
			if _, _, err := a.Lookup(context.Background(), row.Token); !errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatalf("revoked session resurrected: %v", err)
			}
		}
	})
}

func TestIdentityConcurrentBindingKeepsOneOwner(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, config := identityConfig(t, f.DB)
		other := model.User{Username: "other-user", Password: "hash", Status: model.COMMON_STATUS_ENABLE}
		transactionCreate(t, f.DB, &other)
		owners := []uint{1, other.Id}
		actors := make([]auth.Principal, 2)
		stores := make([]*identitystore.Store, 2)
		for i, owner := range owners {
			row := model.UserToken{UserId: owner, Token: fmt.Sprintf("binding-%d", i), ExpiredAt: time.Now().Add(time.Hour).Unix()}
			transactionCreate(t, f.DB, &row)
			actors[i] = auth.Principal{User: auth.User{ID: owner}, Session: auth.Session{ID: row.Id, UserID: owner, Token: row.Token, ExpiresAt: time.Unix(row.ExpiredAt, 0)}}
			stores[i], _ = identitystore.New(f.Reopen(t), sessiontoken.NewIssuer(nil), time.Hour, nil)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		for i := range actors {
			go func(i int) {
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				results <- stores[i].Bind(ctx, auth.ExternalIdentity{Issuer: config.Issuer, Subject: "contested"}, config, actors[i])
			}(i)
		}
		close(start)
		var success, forbidden int
		for i := 0; i < 2; i++ {
			switch err := <-results; {
			case err == nil:
				success++
			case errors.Is(err, auth.ErrForbidden):
				forbidden++
			default:
				t.Fatalf("unexpected binding result: %v", err)
			}
		}
		if success != 1 || forbidden != 1 || transactionCount(t, f.DB, &model.OIDCIdentity{}) != 1 {
			t.Fatal("simultaneous binding did not preserve one owner")
		}
	})
}

// Pause the bind after its first actor lock. Under the old session-first order,
// let issuance acquire the user and reach its session query before releasing
// the bind: that creates a deterministic real-engine deadlock. With user-first
// validation, issuance instead waits for that user without holding the session.
func TestSessionWebIssuanceAndBindingUseConsistentLockOrder(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		_, config := identityConfig(t, f.DB)
		now := time.Now().UTC().Truncate(time.Second)
		row := model.UserToken{UserId: 1, Token: "approving-browser", ExpiredAt: now.Add(time.Hour).Unix()}
		transactionCreate(t, f.DB, &row)
		actor := auth.Principal{User: auth.User{ID: 1}, Session: auth.Session{ID: row.Id, UserID: 1, Token: row.Token, ExpiresAt: time.Unix(row.ExpiredAt, 0)}}
		bindDB, bindErrors := observeTransactions(t, f.Reopen(t))
		issueDB, issueErrors := observeTransactions(t, f.Reopen(t))
		binding, _ := identitystore.New(bindDB, sessiontoken.NewIssuer(nil), time.Hour, nil)
		issuing, _ := authstore.New(issueDB)
		locked, release := make(chan string, 1), make(chan struct{})
		userAttempted, sessionAttempted := make(chan struct{}), make(chan struct{})
		var bindOnce, userOnce, sessionOnce sync.Once
		if err := bindDB.Callback().Query().After("gorm:query").Register("integration:hold-actor-session", func(tx *gorm.DB) {
			if (tx.Statement.Table == "users" || tx.Statement.Table == "user_tokens") && tx.Error == nil {
				bindOnce.Do(func() {
					locked <- tx.Statement.Table
					select {
					case <-release:
					case <-tx.Statement.Context.Done():
						tx.AddError(tx.Statement.Context.Err())
					}
				})
			}
		}); err != nil {
			t.Fatal(err)
		}
		if err := issueDB.Callback().Query().Before("gorm:query").Register("integration:source-session-started", func(tx *gorm.DB) {
			if tx.Statement.Table == "user_tokens" {
				sessionOnce.Do(func() { close(sessionAttempted) })
			}
			if tx.Statement.Table == "users" {
				userOnce.Do(func() { close(userAttempted) })
			}
		}); err != nil {
			t.Fatal(err)
		}
		defer bindDB.Callback().Query().Remove("integration:hold-actor-session")
		defer issueDB.Callback().Query().Remove("integration:source-session-started")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		results := make(chan error, 2)
		go func() {
			results <- binding.Bind(ctx, auth.ExternalIdentity{Issuer: config.Issuer, Subject: "bound-during-web-login"}, config, actor)
		}()
		var firstActorLock string
		select {
		case firstActorLock = <-locked:
		case <-ctx.Done():
			close(release)
			t.Fatal("bind never acquired its first actor lock")
		}
		go func() {
			_, err := issuing.Issue(ctx, auth.IssuedSession{Request: auth.IssueRequest{UserID: 1, SourceSession: &actor.Session}, Token: "new-web-session", Now: now, ExpiresAt: now.Add(time.Hour)})
			results <- err
		}()
		attempted := userAttempted
		if firstActorLock == "user_tokens" {
			attempted = sessionAttempted
		}
		select {
		case <-attempted:
		case <-ctx.Done():
			close(release)
			t.Fatal("issuance never reached the competing actor query")
		}
		close(release)
		var failures []error
		for i := 0; i < 2; i++ {
			if err := <-results; err != nil {
				failures = append(failures, err)
			}
		}
		if len(failures) != 0 {
			t.Fatalf("opposite actor/user locks prevented concurrent binding and issuance: %v (engine deadlock=%v)", failures, bindErrors.retryable() || issueErrors.retryable())
		}
		if transactionCount(t, f.DB, &model.OIDCIdentity{}) != 1 || transactionCount(t, f.DB, &model.UserToken{}) != 2 || transactionCount(t, f.DB, &model.LoginLog{}) != 1 {
			t.Fatal("successful concurrent operations lost writes")
		}
	})
}

// Observe database errors before a store replaces them with a public sentinel.
// In particular, ErrUnavailable alone is NOT evidence of serialization failure.
type transactionErrors struct {
	mu     sync.Mutex
	values []error
}

func (p *transactionErrors) record(err error) {
	if err != nil {
		p.mu.Lock()
		p.values = append(p.values, err)
		p.mu.Unlock()
	}
}
func (p *transactionErrors) retryable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, err := range p.values {
		var state interface{ SQLState() string }
		if errors.As(err, &state) && (state.SQLState() == "40001" || state.SQLState() == "40P01") {
			return true
		}
		var mysql *mysqldriver.MySQLError
		if errors.As(err, &mysql) && (mysql.Number == 1213 || mysql.Number == 1205) {
			return true
		}
	}
	return false
}

type observedPool struct {
	*sql.DB
	errors *transactionErrors
}

func (p observedPool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.DB.BeginTx(ctx, options)
	p.errors.record(err)
	if err != nil {
		return nil, err
	}
	return &observedTransaction{Tx: tx, errors: p.errors}, nil
}

type observedTransaction struct {
	*sql.Tx
	errors *transactionErrors
}

func (tx *observedTransaction) Commit() error {
	err := tx.Tx.Commit()
	tx.errors.record(err)
	return err
}
func (tx *observedTransaction) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	result, err := tx.Tx.ExecContext(ctx, query, args...)
	tx.errors.record(err)
	return result, err
}
func (tx *observedTransaction) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	result, err := tx.Tx.QueryContext(ctx, query, args...)
	tx.errors.record(err)
	return result, err
}
func observeTransactions(t *testing.T, db *gorm.DB) (*gorm.DB, *transactionErrors) {
	t.Helper()
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	p := &transactionErrors{}
	observed := db.Session(&gorm.Session{NewDB: true, Initialized: true})
	wrapped := observedPool{DB: pool, errors: p}
	observed.Config.ConnPool = wrapped
	observed.Statement.ConnPool = wrapped
	// Some drivers report a failed SELECT while iterating Rows instead of
	// QueryContext. GORM has consumed Rows by this callback, so retain that error.
	if err := observed.Callback().Query().After("gorm:query").Register("integration:observe-row-errors", func(tx *gorm.DB) { p.record(tx.Error) }); err != nil {
		t.Fatal(err)
	}
	return observed, p
}

func TestStrategySerializableCompetitionAndDurableClear(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		peer := model.Peer{Id: "strategy-device", Uuid: "strategy-uuid"}
		transactionCreate(t, f.DB, &peer)
		policy := model.PeerStrategy{PeerId: peer.Id, ConfigOptions: custom_types.AutoJson(`{"managed":"value"}`)}
		transactionCreate(t, f.DB, &policy)
		const writers = 10
		type worker struct {
			store  *clientstrategy.Store
			errors *transactionErrors
		}
		workers := make([]worker, writers)
		for i := range workers {
			db, observed := observeTransactions(t, f.Reopen(t))
			store, err := clientstrategy.New(db)
			if err != nil {
				t.Fatal(err)
			}
			workers[i] = worker{store, observed}
		}
		type outcome struct {
			result    clientstrategy.Result
			err       error
			retryable bool
		}
		start := make(chan struct{})
		results := make(chan outcome, writers)
		var wg sync.WaitGroup
		for _, w := range workers {
			wg.Add(1)
			go func(w worker) {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				result, err := w.store.Heartbeat(ctx, peer.Id, peer.Uuid, 0)
				results <- outcome{result, err, w.errors.retryable()}
			}(w)
		}
		close(start)
		wg.Wait()
		close(results)
		var revision int64
		for result := range results {
			if result.err != nil {
				if !errors.Is(result.err, clientstrategy.ErrUnavailable) || !result.retryable {
					t.Fatalf("non-serialization failure hidden by concurrency: %v", result.err)
				}
				if result.result.Revision != 0 || result.result.ConfigOptions != nil {
					t.Fatal("failed transaction published strategy")
				}
				continue
			}
			if revision == 0 {
				revision = result.result.Revision
			}
			if result.result.Revision != revision || revision <= 0 || result.result.ConfigOptions["managed"] != "value" {
				t.Fatal("successful concurrent snapshots differ")
			}
		}
		if revision == 0 {
			t.Fatal("no transaction committed")
		}
		if transactionCount(t, f.DB, &model.PeerStrategyState{}) != 1 {
			t.Fatal("duplicate/missing durable history")
		}
		store, _ := clientstrategy.New(f.Reopen(t))
		got, err := store.Heartbeat(context.Background(), peer.Id, peer.Uuid, revision)
		if err != nil || got.Changed {
			t.Fatalf("known revision was not stable after retry: %v", err)
		}
		if err := f.DB.Delete(&policy).Error; err != nil {
			t.Fatal(err)
		}
		clear, err := store.Heartbeat(context.Background(), peer.Id, peer.Uuid, revision)
		if err != nil || !clear.Changed || clear.ConfigOptions["managed"] != "" || clear.Revision == revision {
			t.Fatalf("removed policy did not clear committed history: %v", err)
		}
		if _, exists := clear.ConfigOptions["preset-device-name"]; exists {
			t.Fatal("unassigned preset was claimed")
		}
	})
}
