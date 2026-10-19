package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

type memoryStore struct {
	user                 User
	session              Session
	lookupErr, renewErr  error
	revokeErr, deleteErr error
	renewed, revoked     int
	deleted              []uint
	renewResult          *Session
}

func validStore() *memoryStore {
	return &memoryStore{
		user:    User{ID: 1, Username: "user", Enabled: true},
		session: Session{ID: 10, UserID: 1, Token: "opaque", ExpiresAt: testNow.Add(2 * time.Hour)},
	}
}

func (s *memoryStore) Lookup(context.Context, string) (User, Session, error) {
	return s.user, s.session, s.lookupErr
}
func (s *memoryStore) Renew(_ context.Context, session Session, expiry, _ time.Time) (Session, error) {
	s.renewed++
	if s.renewErr != nil {
		return Session{}, s.renewErr
	}
	if s.renewResult != nil {
		return *s.renewResult, nil
	}
	s.session.ExpiresAt = expiry
	return s.session, nil
}
func (s *memoryStore) Revoke(context.Context, Session) error {
	s.revoked++
	return s.revokeErr
}
func (s *memoryStore) DeleteSessions(_ context.Context, ids []uint) error {
	s.deleted = append([]uint(nil), ids...)
	return s.deleteErr
}

type verifierFunc func(context.Context, string, time.Time) (TokenClaims, error)

func (f verifierFunc) Verify(c context.Context, token string, now time.Time) (TokenClaims, error) {
	return f(c, token, now)
}

func serviceFor(t *testing.T, store Store, ttl time.Duration, verifier TokenVerifier) *Service {
	t.Helper()
	s, err := New(Dependencies{Store: store, TTL: ttl, Verifier: verifier, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDependenciesAndTTL(t *testing.T) {
	if _, err := New(Dependencies{}); err == nil {
		t.Fatal("missing store accepted")
	}
	if _, err := New(Dependencies{Store: validStore(), TTL: -time.Second}); err == nil {
		t.Fatal("negative TTL accepted")
	}
	store := validStore()
	store.session.ExpiresAt = testNow.Add(time.Second)
	principal, err := serviceFor(t, store, 0, nil).Authenticate(context.Background(), "opaque")
	if err != nil || !principal.Session.ExpiresAt.Equal(testNow.Add(168*time.Hour)) || store.renewed != 1 {
		t.Fatalf("default TTL renewal: %+v, %v, calls=%d", principal, err, store.renewed)
	}
}

func TestAuthenticationRules(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*memoryStore)
		want   error
	}{
		{"enabled", func(*memoryStore) {}, nil},
		{"disabled", func(s *memoryStore) { s.user.Enabled = false }, ErrDisabled},
		{"expired_equal", func(s *memoryStore) { s.session.ExpiresAt = testNow }, ErrUnauthenticated},
		{"expired_before", func(s *memoryStore) { s.session.ExpiresAt = testNow.Add(-time.Second) }, ErrUnauthenticated},
		{"subsecond_expiry", func(s *memoryStore) { s.session.ExpiresAt = testNow.Add(time.Nanosecond) }, ErrUnauthenticated},
		{"missing_user", func(s *memoryStore) { s.user.ID = 0 }, ErrUnauthenticated},
		{"missing_session", func(s *memoryStore) { s.session.ID = 0 }, ErrUnauthenticated},
		{"wrong_user", func(s *memoryStore) { s.session.UserID = 2 }, ErrUnauthenticated},
		{"wrong_token", func(s *memoryStore) { s.session.Token = "other" }, ErrUnauthenticated},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := validStore()
			test.change(store)
			_, err := serviceFor(t, store, time.Hour, nil).Authenticate(context.Background(), "opaque")
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}

func TestJWTIdentityAndExpiryCap(t *testing.T) {
	for _, test := range []struct {
		name   string
		userID uint
		expiry time.Time
		want   error
	}{
		{"cap", 1, testNow.Add(20 * time.Minute), nil},
		{"different_user", 2, testNow.Add(time.Hour), ErrUnauthenticated},
		{"zero_user", 0, testNow.Add(time.Hour), ErrUnauthenticated},
		{"missing_expiry", 1, time.Time{}, ErrUnauthenticated},
		{"equal_expiry", 1, testNow, ErrUnauthenticated},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := validStore()
			store.session.ExpiresAt = testNow.Add(time.Minute)
			verify := verifierFunc(func(context.Context, string, time.Time) (TokenClaims, error) {
				return TokenClaims{UserID: test.userID, ExpiresAt: test.expiry}, nil
			})
			p, err := serviceFor(t, store, 3*time.Hour, verify).Authenticate(context.Background(), "opaque")
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
			if err == nil && (!p.Session.ExpiresAt.Equal(test.expiry) || !store.session.ExpiresAt.Equal(test.expiry)) {
				t.Fatalf("renewal exceeded JWT expiry: %+v", p.Session)
			}
		})
	}
	store := validStore()
	cap := testNow.Add(10 * time.Minute)
	p, err := serviceFor(t, store, time.Hour, verifierFunc(func(context.Context, string, time.Time) (TokenClaims, error) {
		return TokenClaims{UserID: 1, ExpiresAt: cap}, nil
	})).Authenticate(context.Background(), "opaque")
	if err != nil || store.renewed != 0 || !p.Session.ExpiresAt.Equal(cap) {
		t.Fatalf("existing DB expiry must not bypass JWT lifetime: %+v, %v", p, err)
	}
}

func TestDependencyFailuresAndCancellation(t *testing.T) {
	store := validStore()
	store.lookupErr = errors.New("database failed")
	if _, err := serviceFor(t, store, time.Hour, nil).Authenticate(context.Background(), "opaque"); !errors.Is(err, ErrUnavailable) || !errors.Is(err, store.lookupErr) {
		t.Fatalf("lookup error lost: %v", err)
	}
	store = validStore()
	store.session.ExpiresAt = testNow.Add(time.Second)
	store.renewErr = errors.New("renewal failed")
	if _, err := serviceFor(t, store, time.Hour, nil).Authenticate(context.Background(), "opaque"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("renewal failure was accepted: %v", err)
	}
	store.renewErr = ErrUnauthenticated
	if _, err := serviceFor(t, store, time.Hour, nil).Authenticate(context.Background(), "opaque"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked during renewal was accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := serviceFor(t, validStore(), time.Hour, nil).Authenticate(ctx, "opaque"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := serviceFor(t, validStore(), time.Hour, nil).Authenticate(ctx, "opaque"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline lost: %v", err)
	}
}

func TestConcurrentRenewalAlreadyExtendedByAnotherRequest(t *testing.T) {
	store := validStore()
	store.session.ExpiresAt = testNow.Add(time.Minute)
	other := store.session
	other.ExpiresAt = testNow.Add(time.Hour + time.Second)
	store.renewResult = &other
	p, err := serviceFor(t, store, time.Hour, nil).Authenticate(context.Background(), "opaque")
	if err != nil || !p.Session.ExpiresAt.Equal(other.ExpiresAt) {
		t.Fatalf("concurrent renewal rejected a live session: %+v, %v", p.Session, err)
	}
}

func TestRevokeAndAdministratorRules(t *testing.T) {
	store := validStore()
	s := serviceFor(t, store, time.Hour, nil)
	p, err := s.Authenticate(context.Background(), "opaque")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(context.Background(), p, "wrong"); !errors.Is(err, ErrUnauthenticated) || store.revoked != 0 {
		t.Fatalf("wrong token revoked a session: %v", err)
	}
	if err := s.Revoke(context.Background(), p, "opaque"); err != nil || store.revoked != 1 {
		t.Fatalf("revoke: %v", err)
	}
	if err := RequireAdministrator(p); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ordinary user: %v", err)
	}
	store.user.IsAdmin = true
	p.User.IsAdmin = true
	if err := RequireAdministrator(p); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSessions(context.Background(), p, []uint{0}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero ID: %v", err)
	}
	if err := s.RevokeSessions(context.Background(), p, nil); err != nil || store.deleted != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if err := s.RevokeSessions(context.Background(), p, []uint{10, 999}); err != nil || len(store.deleted) != 2 {
		t.Fatalf("admin delete: %v", err)
	}
	store.user.IsAdmin = false
	if err := s.RevokeSessions(context.Background(), p, []uint{10}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stale admin principal: %v", err)
	}
	store.user.Enabled = false
	if err := s.RevokeSessions(context.Background(), p, []uint{10}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled admin: %v", err)
	}
}
