package oidclogin_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/identitystore"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/sessiontoken"
	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/internal/loginattempt"
	"github.com/lejianwen/rustdesk-api/v2/internal/oidclogin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var ctx = context.Background()
var client = loginattempt.Device{ID: "123456789", UUID: "device-uuid", Client: "client", Platform: "Windows"}

type credential struct{ state, nonce, verifier string }
type fakeProtocol struct {
	mu             sync.Mutex
	authorizations map[string]credential
	identity       auth.ExternalIdentity
	exchangeError  error
	exchangeHook   func(context.Context) error
	exchanges      int
}

func (p *fakeProtocol) AuthorizationURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authorizations[state] = credential{state, nonce, verifier}
	return "https://idp.test/authorize?state=" + url.QueryEscape(state), nil
}
func (p *fakeProtocol) Exchange(ctx context.Context, code, nonce, verifier string) (auth.ExternalIdentity, error) {
	if err := ctx.Err(); err != nil {
		return auth.ExternalIdentity{}, err
	}
	p.mu.Lock()
	p.exchanges++
	credentials, ok := p.authorizations[code]
	if !ok || credentials.nonce != nonce || credentials.verifier != verifier {
		p.mu.Unlock()
		return auth.ExternalIdentity{}, errors.New("protocol binding mismatch")
	}
	identity, exchangeErr, hook := p.identity, p.exchangeError, p.exchangeHook
	p.mu.Unlock()
	if exchangeErr != nil {
		return auth.ExternalIdentity{}, exchangeErr
	}
	if hook != nil {
		if err := hook(ctx); err != nil {
			return auth.ExternalIdentity{}, err
		}
	}
	return identity, nil
}
func (p *fakeProtocol) count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.exchanges }

type attemptClock struct {
	mu    sync.Mutex
	value time.Time
}

func (c *attemptClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.value }
func (c *attemptClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = c.value.Add(d)
}

type fixture struct {
	db       *gorm.DB
	service  *oidclogin.Service
	sessions *auth.Service
	issuer   *auth.Issuer
	protocol *fakeProtocol
	clock    *attemptClock
}

func newFixture(t *testing.T, key []byte) *fixture {
	return newFixtureWithOptions(t, key, loginattempt.Options{})
}

func newFixtureWithOptions(t *testing.T, key []byte, options loginattempt.Options) *fixture {
	t.Helper()
	// Legacy peers use user_id=0 for unbound devices. As in authstore fixtures,
	// do not infer foreign keys from those legacy GORM associations. Explicit
	// identity FK/uniqueness constraints are covered by identitystore tests;
	// this fixture also injects an absent user to verify fail-closed admission.
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "oidc.db")+"?_txlock=immediate&_busy_timeout=5000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&model.User{}, &model.UserToken{}, &model.LoginLog{}, &model.Peer{}, &model.Oauth{}, &model.OIDCIdentity{}); err != nil {
		t.Fatal(err)
	}
	// Use the runtime schema for current-model peer writes.
	if err := database.Update(db, 281, nil); err != nil {
		t.Fatal(err)
	}
	yes := true
	provider := model.Oauth{Op: "ours", OauthType: model.OauthTypeOidc, Issuer: "https://idp.test", ClientId: "rustdesk", AutoRegister: &yes, PkceEnable: &yes, PkceMethod: model.PKCEMethodS256, Scopes: "openid profile"}
	if err := db.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Peer{Id: client.ID, Uuid: client.UUID}).Error; err != nil {
		t.Fatal(err)
	}
	sessions, err := app.NewSessions(db, time.Hour, key)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := app.NewIssuer(db, time.Hour, key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := identitystore.New(db, sessiontoken.NewIssuer(key), time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := &attemptClock{value: time.Now().UTC()}
	if options.Now == nil {
		options.Now = clock.now
	}
	attempts, err := loginattempt.New(options)
	if err != nil {
		t.Fatal(err)
	}
	protocol := &fakeProtocol{authorizations: make(map[string]credential), identity: auth.ExternalIdentity{Issuer: provider.Issuer, Subject: "stable-uuid-sub", Username: "external", Name: "Example"}}
	service, err := oidclogin.New(store, attempts, sessions, issuer, func(config auth.ProviderConfig) (oidclogin.Protocol, error) {
		if config.ID != provider.Id || config.Issuer != provider.Issuer || config.ClientID != provider.ClientId {
			return nil, errors.New("unexpected provider snapshot")
		}
		return protocol, nil
	}, "https://api.test/_admin/#/oauth/")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{db, service, sessions, issuer, protocol, clock}
}

func (f *fixture) start(t *testing.T, device loginattempt.Device, actor *auth.Principal) (oidclogin.Started, string) {
	t.Helper()
	a, err := f.service.Begin(ctx, "ours", device, actor)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(a.URL)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	f.protocol.mu.Lock()
	binding, ok := f.protocol.authorizations[state]
	f.protocol.mu.Unlock()
	if !ok || state == "" || a.Code == state || binding.nonce == "" || binding.verifier == "" || binding.nonce == binding.verifier {
		t.Fatal("independent browser/poll credentials missing")
	}
	return a, state
}
func (f *fixture) authorize(t *testing.T) oidclogin.Started {
	t.Helper()
	a, state := f.start(t, client, nil)
	if err := f.service.Callback(ctx, state, state, ""); err != nil {
		t.Fatal(err)
	}
	return a
}
func (f *fixture) poll(t *testing.T, a oidclogin.Started) auth.Principal {
	t.Helper()
	p, err := f.service.PollEither(ctx, a.Code, client, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func count(t *testing.T, db *gorm.DB, record any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(record).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}
func assertCounts(t *testing.T, f *fixture, sessions, logs, identities int64) {
	t.Helper()
	for _, v := range []struct {
		record any
		want   int64
	}{{&model.UserToken{}, sessions}, {&model.LoginLog{}, logs}, {&model.OIDCIdentity{}, identities}} {
		if got := count(t, f.db, v.record); got != v.want {
			t.Fatalf("%T count=%d want=%d", v.record, got, v.want)
		}
	}
}
func (f *fixture) actor(t *testing.T, name string) auth.Principal {
	t.Helper()
	u := model.User{Username: name, Password: "non-default-hash", Status: model.COMMON_STATUS_ENABLE}
	if err := f.db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	p, err := f.issuer.Issue(ctx, auth.IssueRequest{UserID: u.Id, Client: "webadmin", Method: "account"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOIDCConcurrentPollCommitsOneLocalSession(t *testing.T) {
	for _, kind := range []string{"opaque", "jwt"} {
		t.Run(kind, func(t *testing.T) {
			var key []byte
			if kind == "jwt" {
				key = []byte("test-only-jwt-signing-key")
			}
			f := newFixture(t, key)
			a, state := f.start(t, client, nil)
			if _, err := f.service.PollEither(ctx, a.Code, client, ""); !errors.Is(err, loginattempt.ErrPending) {
				t.Fatal("unapproved attempt issued", err)
			}
			if err := f.service.Callback(ctx, state, state, ""); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			results := make(chan auth.Principal, 24)
			for i := 0; i < 24; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					p, err := f.service.PollEither(ctx, a.Code, client, "127.0.0.1")
					if err != nil {
						t.Error(err)
						return
					}
					results <- p
				}()
			}
			wg.Wait()
			close(results)
			var first auth.Principal
			for p := range results {
				if first.Session.ID == 0 {
					first = p
				}
				if p.Session.ID != first.Session.ID || p.Session.Token != first.Session.Token {
					t.Fatal("concurrent polls minted different sessions")
				}
			}
			if first.Session.ID == 0 || first.User.IsAdmin {
				t.Fatal("login missing or elevated")
			}
			assertCounts(t, f, 1, 1, 1)
			var log model.LoginLog
			if err := f.db.First(&log).Error; err != nil {
				t.Fatal(err)
			}
			if log.UserTokenId != first.Session.ID || log.UserId != first.User.ID {
				t.Fatal("login log does not reference issued session")
			}
			var peer model.Peer
			if err := f.db.First(&peer).Error; err != nil {
				t.Fatal(err)
			}
			if peer.UserId != first.User.ID {
				t.Fatal("device not bound atomically")
			}
			if f.protocol.count() != 1 {
				t.Fatal("provider code exchanged more than once")
			}
			if err := f.service.Callback(ctx, state, state, ""); !errors.Is(err, loginattempt.ErrClaimed) {
				t.Fatal("callback replay accepted", err)
			}
		})
	}
}

func TestPollDeviceRouteAndPurposeCannotBeSubstituted(t *testing.T) {
	f := newFixture(t, nil)
	a := f.authorize(t)
	for _, device := range []loginattempt.Device{{ID: "wrong", UUID: client.UUID, Client: client.Client}, {ID: client.ID, UUID: "wrong", Client: client.Client}, {ID: client.ID, UUID: client.UUID, Client: "admin"}} {
		if p, err := f.service.PollEither(ctx, a.Code, device, ""); !errors.Is(err, loginattempt.ErrForbidden) || p.Session.Token != "" {
			t.Fatal("cross-device/route issued", err)
		}
	}
	if _, err := f.service.Poll(ctx, a.Code, client, true, ""); !errors.Is(err, loginattempt.ErrForbidden) {
		t.Fatal("OIDC polled as web approval", err)
	}
	if _, err := f.service.PollEither(ctx, a.URL, client, ""); err == nil {
		t.Fatal("URL accepted as poll credential")
	}
	assertCounts(t, f, 0, 0, 0)
	withoutPlatform := client
	withoutPlatform.Platform = ""
	if _, err := f.service.PollEither(ctx, a.Code, withoutPlatform, ""); err != nil {
		t.Fatal("native poll requires absent platform", err)
	}
	assertCounts(t, f, 1, 1, 1)
}

func TestBadCallbacksBecomePermanentlyFailed(t *testing.T) {
	for _, mode := range []string{"provider-error", "missing-code", "exchange-error", "wrong-issuer", "empty-subject"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, nil)
			a, state := f.start(t, client, nil)
			code, providerError := state, ""
			switch mode {
			case "provider-error":
				providerError = "access_denied"
			case "missing-code":
				code = ""
			case "exchange-error":
				f.protocol.exchangeError = errors.New("provider secret")
			case "wrong-issuer":
				f.protocol.identity.Issuer = "https://other.test"
			case "empty-subject":
				f.protocol.identity.Subject = ""
			}
			if err := f.service.Callback(ctx, state, code, providerError); err == nil {
				t.Fatal("bad callback accepted")
			}
			if p, err := f.service.PollEither(ctx, a.Code, client, ""); !errors.Is(err, loginattempt.ErrFailed) || p.Session.Token != "" {
				t.Fatal("failed attempt not terminal", err)
			}
			exchanges := f.protocol.count()
			if err := f.service.Callback(ctx, state, state, ""); !errors.Is(err, loginattempt.ErrClaimed) {
				t.Fatal("failed callback reclaimed", err)
			}
			if f.protocol.count() != exchanges {
				t.Fatal("callback replay reexchanged code")
			}
			assertCounts(t, f, 0, 0, 0)
		})
	}
}

func TestIssuanceFaultRollsBackAndPollRetriesWithoutExchange(t *testing.T) {
	f := newFixture(t, nil)
	a := f.authorize(t)
	var fault atomic.Bool
	fault.Store(true)
	if err := f.db.Callback().Create().Before("gorm:create").Register("oidc-test-fault", func(tx *gorm.DB) {
		if tx.Statement.Table == "login_logs" && fault.Load() {
			tx.AddError(errors.New("injected persistence fault"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if p, err := f.service.PollEither(ctx, a.Code, client, ""); !errors.Is(err, auth.ErrUnavailable) || p.Session.Token != "" {
		t.Fatal("issuance failure returned credential", err)
	}
	assertCounts(t, f, 0, 0, 0)
	if count(t, f.db, &model.User{}) != 0 {
		t.Fatal("failed transaction left auto-registered account")
	}
	fault.Store(false)
	p := f.poll(t, a)
	again := f.poll(t, a)
	if p.Session.Token != again.Session.Token || f.protocol.count() != 1 {
		t.Fatal("retry repeated exchange or reminted cached session")
	}
	assertCounts(t, f, 1, 1, 1)
}

func TestCachedResultRevalidatesCurrentSessionAndUser(t *testing.T) {
	for _, mode := range []string{"revoked", "disabled", "deleted", "database-error"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, nil)
			a := f.authorize(t)
			first := f.poll(t, a)
			// Simulate a response lost after commit; repeated polls recover exactly it.
			if got := f.poll(t, a); got.Session.Token != first.Session.Token {
				t.Fatal("response recovery minted a new token")
			}
			var want error
			switch mode {
			case "revoked":
				want = auth.ErrUnauthenticated
				if err := f.sessions.Revoke(ctx, first, first.Session.Token); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				want = auth.ErrDisabled
				if err := f.db.Model(&model.User{}).Where("id = ?", first.User.ID).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
					t.Fatal(err)
				}
			case "deleted":
				want = auth.ErrUnauthenticated
				if err := f.db.Delete(&model.User{}, first.User.ID).Error; err != nil {
					t.Fatal(err)
				}
			case "database-error":
				want = auth.ErrUnavailable
				pool, _ := f.db.DB()
				if err := pool.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if p, err := f.service.PollEither(ctx, a.Code, client, ""); !errors.Is(err, want) || p.Session.Token != "" {
				t.Fatalf("cached result bypassed %s: %v", mode, err)
			}
		})
	}
}

func TestCallbackDoesNotExtendAttemptExpiry(t *testing.T) {
	for _, issued := range []bool{false, true} {
		t.Run(fmt.Sprint(issued), func(t *testing.T) {
			f := newFixture(t, nil)
			a, state := f.start(t, client, nil)
			f.clock.add(4 * time.Minute)
			if err := f.service.Callback(ctx, state, state, ""); err != nil {
				t.Fatal(err)
			}
			if issued {
				f.poll(t, a)
			}
			f.clock.add(time.Minute)
			if p, err := f.service.PollEither(ctx, a.Code, client, ""); !errors.Is(err, loginattempt.ErrExpired) || p.Session.Token != "" {
				t.Fatal("callback/result extended original TTL", err)
			}
			want := int64(0)
			if issued {
				want = 1
			}
			assertCounts(t, f, want, want, want)
		})
	}
}

func TestExplicitBindingRequiresLiveActorAndCannotIssueSession(t *testing.T) {
	for _, mode := range []string{"valid", "revoked", "disabled", "identity-owned-by-other"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, nil)
			actor := f.actor(t, "actor")
			a, state := f.start(t, client, &actor)
			if _, err := f.service.Inspect(ctx, a.Code, actor); err != nil {
				t.Fatal("initiator cannot inspect binding", err)
			}
			other := f.actor(t, "other")
			if _, err := f.service.Inspect(ctx, a.Code, other); !errors.Is(err, loginattempt.ErrForbidden) {
				t.Fatal("another actor inspected bind", err)
			}
			switch mode {
			case "revoked":
				if err := f.sessions.Revoke(ctx, actor, actor.Session.Token); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := f.db.Model(&model.User{}).Where("id = ?", actor.User.ID).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
					t.Fatal(err)
				}
			case "identity-owned-by-other":
				if err := f.db.Create(&model.OIDCIdentity{Issuer: f.protocol.identity.Issuer, Subject: f.protocol.identity.Subject, UserID: other.User.ID}).Error; err != nil {
					t.Fatal(err)
				}
			}
			err := f.service.Callback(ctx, state, state, "")
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid binding actor/identity accepted")
			}
			if mode == "revoked" || mode == "disabled" {
				if _, err := f.service.Begin(ctx, "ours", client, &actor); err == nil {
					t.Fatal("inactive initiating session created a new binding attempt")
				}
			}
			if p, err := f.service.PollEither(ctx, a.Code, client, ""); !errors.Is(err, loginattempt.ErrForbidden) || p.Session.Token != "" {
				t.Fatal("bind attempt issued a new session", err)
			}
			wantSessions := int64(2)
			if mode == "revoked" {
				wantSessions = 1
			}
			wantLinks := int64(0)
			if mode == "valid" || mode == "identity-owned-by-other" {
				wantLinks = 1
			}
			assertCounts(t, f, wantSessions, 2, wantLinks)
			if wantLinks == 1 {
				var link model.OIDCIdentity
				if err := f.db.First(&link).Error; err != nil {
					t.Fatal(err)
				}
				wantUser := actor.User.ID
				if mode == "identity-owned-by-other" {
					wantUser = other.User.ID
				}
				if link.UserID != wantUser {
					t.Fatal("identity binding stolen")
				}
			}
		})
	}
}

func TestInteractiveWebApprovalCannotBypassOIDC(t *testing.T) {
	f := newFixture(t, nil)
	actor := f.actor(t, "approver")
	oidc, state := f.start(t, client, nil)
	if err := f.service.ApproveWeb(ctx, oidc.Code, actor); !errors.Is(err, loginattempt.ErrForbidden) {
		t.Fatal("OIDC accepted interactive approval", err)
	}
	if err := f.service.Callback(ctx, state, state, ""); err != nil {
		t.Fatal(err)
	}
	web, err := f.service.Begin(ctx, "webauth", client, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.ApproveWeb(ctx, web.Code, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Poll(ctx, web.Code, client, false, ""); !errors.Is(err, loginattempt.ErrForbidden) {
		t.Fatal("web approved attempt accepted OIDC purpose", err)
	}
	p := f.poll(t, web)
	if p.User.ID != actor.User.ID {
		t.Fatal("web approval issued for another actor")
	}
	if again := f.poll(t, web); again.Session.Token != p.Session.Token {
		t.Fatal("web poll minted twice")
	}
	if err := f.sessions.Revoke(ctx, actor, actor.Session.Token); err != nil {
		t.Fatal(err)
	}
	second, err := f.service.Begin(ctx, "webauth", client, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.ApproveWeb(ctx, second.Code, actor); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("revoked actor approved web login", err)
	}
	assertCounts(t, f, 1, 2, 0)
}

func TestWebApprovalRechecksActorBeforeIssuingSession(t *testing.T) {
	for _, mode := range []string{"revoked", "disabled", "password-changed"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, nil)
			actor := f.actor(t, "approver")
			a, err := f.service.Begin(ctx, "webauth", client, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.ApproveWeb(ctx, a.Code, actor); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "revoked":
				if err := f.sessions.Revoke(ctx, actor, actor.Session.Token); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := f.db.Model(&model.User{}).Where("id = ?", actor.User.ID).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
					t.Fatal(err)
				}
			case "password-changed":
				// Changing the local password does not revoke the existing approving
				// session; issuance must use the freshly authenticated password hash.
				if err := f.db.Model(&model.User{}).Where("id = ?", actor.User.ID).Update("password", "changed-hash").Error; err != nil {
					t.Fatal(err)
				}
			}
			p, err := f.service.PollEither(ctx, a.Code, client, "")
			if mode == "password-changed" {
				if err != nil || p.User.PasswordHash != "changed-hash" {
					t.Fatal("issuance used stale approving actor", err)
				}
				assertCounts(t, f, 2, 2, 0)
				return
			}
			want := auth.ErrUnauthenticated
			wantSessions := int64(0)
			if mode == "disabled" {
				want = auth.ErrDisabled
				wantSessions = 1
			}
			if !errors.Is(err, want) || p.Session.Token != "" {
				t.Fatal("stale web approval issued a session", err)
			}
			assertCounts(t, f, wantSessions, 1, 0)
		})
	}
}

func TestBindExchangeCrossingActualTTLDoesNotPersistIdentity(t *testing.T) {
	const ttl = 80 * time.Millisecond
	f := newFixtureWithOptions(t, nil, loginattempt.Options{TTL: ttl, Now: time.Now})
	actor := f.actor(t, "binding-actor")
	a, state := f.start(t, client, &actor)
	var receivedBudget time.Duration
	f.protocol.exchangeHook = func(work context.Context) error {
		deadline, ok := work.Deadline()
		if !ok {
			t.Fatal("exchange received no attempt deadline")
		}
		receivedBudget = time.Until(deadline)
		// Return an identity after the flow expired even if a dependency ignores
		// cancellation. The explicit live-lease check must still precede Bind.
		timer := time.NewTimer(2 * ttl)
		defer timer.Stop()
		<-timer.C
		return nil
	}
	err := f.service.Callback(context.Background(), state, state, "")
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, loginattempt.ErrExpired) {
		t.Fatalf("late callback error=%v", err)
	}
	if receivedBudget <= 0 || receivedBudget > ttl {
		t.Fatalf("exchange budget=%v", receivedBudget)
	}
	assertCounts(t, f, 1, 1, 0)
	if _, err := f.service.Inspect(ctx, a.Code, actor); err == nil {
		t.Fatal("expired binding remains inspectable")
	}
}

func TestBindRechecksLeaseAfterExchangeWithInjectedClock(t *testing.T) {
	f := newFixture(t, nil)
	actor := f.actor(t, "binding-actor")
	_, state := f.start(t, client, &actor)
	f.protocol.exchangeHook = func(context.Context) error { f.clock.add(loginattempt.DefaultTTL); return nil }
	if err := f.service.Callback(ctx, state, state, ""); !errors.Is(err, loginattempt.ErrExpired) {
		t.Fatalf("expired callback: %v", err)
	}
	assertCounts(t, f, 1, 1, 0)
}

func TestPollAttemptDeadlineRollsBackSQLiteAdmission(t *testing.T) {
	const ttl = 100 * time.Millisecond
	f := newFixtureWithOptions(t, nil, loginattempt.Options{TTL: ttl, Now: time.Now})
	a := f.authorize(t)
	var reachedLog bool
	var receivedBudget time.Duration
	if err := f.db.Callback().Create().Before("gorm:create").Register("attempt-deadline", func(tx *gorm.DB) {
		if tx.Statement.Table != "login_logs" {
			return
		}
		reachedLog = true
		deadline, ok := tx.Statement.Context.Deadline()
		if !ok {
			t.Error("database transaction received no attempt deadline")
			return
		}
		receivedBudget = time.Until(deadline)
		// The user, identity and session have already been inserted in this
		// transaction. Let the propagated attempt deadline cancel the final write.
		<-tx.Statement.Context.Done()
	}); err != nil {
		t.Fatal(err)
	}
	defer f.db.Callback().Create().Remove("attempt-deadline")
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	p, err := f.service.PollEither(parent, a.Code, client, "")
	if !errors.Is(err, context.DeadlineExceeded) || p.Session.Token != "" {
		t.Fatalf("poll=%+v error=%v", p.Session, err)
	}
	if !reachedLog || receivedBudget <= 0 || receivedBudget > ttl || time.Since(started) > time.Second {
		t.Fatalf("attempt deadline not propagated to SQLite: reached=%v budget=%v elapsed=%v", reachedLog, receivedBudget, time.Since(started))
	}
	assertCounts(t, f, 0, 0, 0)
	if count(t, f.db, &model.User{}) != 0 {
		t.Fatal("expired admission committed an account")
	}
	var peer model.Peer
	if err := f.db.First(&peer).Error; err != nil {
		t.Fatal(err)
	}
	if peer.UserId != 0 {
		t.Fatal("expired admission committed peer ownership")
	}
}

func TestBrowserProofExpiryBoundsSQLiteBindingAndWebIssuance(t *testing.T) {
	for _, purpose := range []string{"bind", "web"} {
		t.Run(purpose, func(t *testing.T) {
			key := []byte("browser-proof-expiry-test-key")
			f := newFixture(t, key)
			user := model.User{Username: "short-lived-browser", Password: "verified-hash", Status: model.COMMON_STATUS_ENABLE}
			if err := f.db.Create(&user).Error; err != nil {
				t.Fatal(err)
			}
			shortIssuer, err := app.NewIssuer(f.db, 2*time.Second, key)
			if err != nil {
				t.Fatal(err)
			}
			actor, err := shortIssuer.Issue(ctx, auth.IssueRequest{UserID: user.Id})
			if err != nil {
				t.Fatal(err)
			}
			var started oidclogin.Started
			var state string
			if purpose == "bind" {
				started, state = f.start(t, client, &actor)
			} else {
				started, err = f.service.Begin(ctx, "webauth", client, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.service.ApproveWeb(ctx, started.Code, actor); err != nil {
					t.Fatal(err)
				}
			}
			target := "oidc_identities"
			if purpose == "web" {
				target = "user_tokens"
			}
			var reached bool
			if err := f.db.Callback().Create().Before("gorm:create").Register("proof-deadline", func(tx *gorm.DB) {
				if tx.Statement.Table != target {
					return
				}
				reached = true
				deadline, ok := tx.Statement.Context.Deadline()
				if !ok || deadline.After(actor.Session.ExpiresAt) {
					t.Error("SQLite operation outlives the authorizing browser proof")
					tx.AddError(errors.New("missing proof deadline"))
					return
				}
				<-tx.Statement.Context.Done()
			}); err != nil {
				t.Fatal(err)
			}
			defer f.db.Callback().Create().Remove("proof-deadline")
			parent, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			if purpose == "bind" {
				err = f.service.Callback(parent, state, state, "")
			} else {
				var p auth.Principal
				p, err = f.service.PollEither(parent, started.Code, client, "")
				if p.Session.Token != "" {
					t.Fatal("expired browser proof returned child credential")
				}
			}
			if !reached || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("browser proof deadline: reached=%v error=%v", reached, err)
			}
			assertCounts(t, f, 1, 1, 0)
		})
	}
}
