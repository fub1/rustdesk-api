package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

type cancelingStore struct {
	*memoryStore
	operation string
	cancel    context.CancelFunc
}

func (s *cancelingStore) Lookup(ctx context.Context, token string) (User, Session, error) {
	if s.operation == "lookup" {
		s.cancel()
		return User{}, Session{}, ErrUnauthenticated
	}
	return s.memoryStore.Lookup(ctx, token)
}
func (s *cancelingStore) Renew(ctx context.Context, row Session, expiry, now time.Time) (Session, error) {
	if s.operation == "renew" {
		s.cancel()
		return Session{}, errors.New("driver stopped during renewal")
	}
	return s.memoryStore.Renew(ctx, row, expiry, now)
}
func (s *cancelingStore) Revoke(ctx context.Context, row Session) error {
	if s.operation == "revoke" {
		s.cancel()
		return errors.New("driver stopped during revoke")
	}
	return s.memoryStore.Revoke(ctx, row)
}
func (s *cancelingStore) DeleteSessions(ctx context.Context, ids []uint) error {
	if s.operation == "delete" {
		s.cancel()
		return errors.New("driver stopped during deletion")
	}
	return s.memoryStore.DeleteSessions(ctx, ids)
}

func TestRequestCancellationTakesPrecedenceOverLateDependencyErrors(t *testing.T) {
	for _, operation := range []string{"verifier", "lookup", "renew", "revoke", "delete"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &cancelingStore{memoryStore: validStore(), operation: operation, cancel: cancel}
			store.user.IsAdmin = true
			if operation == "renew" {
				store.session.ExpiresAt = testNow.Add(time.Minute)
			}
			var verifier TokenVerifier
			if operation == "verifier" {
				verifier = verifierFunc(func(context.Context, string, time.Time) (TokenClaims, error) {
					cancel()
					return TokenClaims{}, ErrUnauthenticated
				})
			}
			s := serviceFor(t, store, time.Hour, verifier)
			principal := Principal{User: store.user, Session: store.session}
			var err error
			switch operation {
			case "revoke":
				err = s.Revoke(ctx, principal, "opaque")
			case "delete":
				err = s.RevokeSessions(ctx, principal, []uint{10})
			default:
				_, err = s.Authenticate(ctx, "opaque")
			}
			if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrUnavailable) {
				t.Fatalf("late error overrode request cancellation: %v", err)
			}
		})
	}
}
