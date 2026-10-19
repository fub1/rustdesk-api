package auth

import (
	"context"
	"errors"
	"time"
)

// IssueRequest is a verified login decision, not a substitute for authenticating
// a password or an external identity. Persistence rechecks the current user.
type IssueRequest struct {
	UserID                                             uint
	ExpectedPassword                                   *string
	DeviceID, DeviceUUID, Client, Platform, IP, Method string
}

type IssuedSession struct {
	Request        IssueRequest
	Token          string
	Now, ExpiresAt time.Time
}

type IssueStore interface {
	Issue(context.Context, IssuedSession) (Principal, error)
}

type TokenIssuer interface {
	Issue(context.Context, uint, time.Time, time.Time) (string, error)
}

type Issuer struct {
	store  IssueStore
	tokens TokenIssuer
	ttl    time.Duration
	now    func() time.Time
}

func NewIssuer(store IssueStore, tokens TokenIssuer, ttl time.Duration, now func() time.Time) (*Issuer, error) {
	if store == nil || tokens == nil || ttl < 0 {
		return nil, errors.New("invalid session issuance dependencies")
	}
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < time.Second {
		return nil, errors.New("session lifetime must be at least one second")
	}
	if now == nil {
		now = time.Now
	}
	return &Issuer{store: store, tokens: tokens, ttl: ttl, now: now}, nil
}

func (s *Issuer) Issue(ctx context.Context, request IssueRequest) (Principal, error) {
	if err := ctx.Err(); err != nil {
		return Principal{}, err
	}
	if request.UserID == 0 {
		return Principal{}, ErrUnauthenticated
	}
	now := s.now().UTC().Truncate(time.Second)
	expiry := now.Add(s.ttl).Truncate(time.Second)
	token, err := s.tokens.Issue(ctx, request.UserID, now, expiry)
	if err != nil {
		return Principal{}, dependencyError("generate session", err)
	}
	if token == "" {
		return Principal{}, ErrUnavailable
	}
	p, err := s.store.Issue(ctx, IssuedSession{Request: request, Token: token, Now: now, ExpiresAt: expiry})
	// A successful commit remains a commit even if the caller has just disconnected.
	// OIDC attempt storage retains this result for bounded response recovery.
	if err != nil {
		return Principal{}, dependencyError("persist session", err)
	}
	return p, nil
}
