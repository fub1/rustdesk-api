// Package auth owns session authentication independently of HTTP, persistence
// libraries and legacy application globals.
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrUnauthenticated = errors.New("unauthenticated session")
	ErrDisabled        = errors.New("user disabled")
	ErrUnavailable     = errors.New("authentication unavailable")
	ErrForbidden       = errors.New("administrator required")
	ErrInvalidInput    = errors.New("invalid authentication input")
)

const DefaultTTL = 168 * time.Hour

type User struct {
	ID, GroupID                       uint
	Username, Email, Nickname, Avatar string
	IsAdmin, Enabled                  bool
	PasswordHash, Remark              string
	CreatedAt, UpdatedAt              time.Time
}

type Session struct {
	ID, UserID                  uint
	Token, DeviceUUID, DeviceID string
	ExpiresAt                   time.Time
}

type Principal struct {
	User    User
	Session Session
}

// Store must reject ambiguous raw tokens. Renew may only update an existing,
// unexpired matching session; it must never recreate a revoked row. Revoke must
// atomically remove the matching session and its legacy device binding.
type Store interface {
	Lookup(ctx context.Context, token string) (User, Session, error)
	Renew(ctx context.Context, session Session, expiresAt, now time.Time) (Session, error)
	Revoke(ctx context.Context, session Session) error
	DeleteSessions(ctx context.Context, ids []uint) error
}

type TokenClaims struct {
	UserID    uint
	ExpiresAt time.Time
}

type TokenVerifier interface {
	Verify(ctx context.Context, token string, now time.Time) (TokenClaims, error)
}

type Dependencies struct {
	Store    Store
	Verifier TokenVerifier
	Now      func() time.Time
	TTL      time.Duration
}

type Service struct {
	store    Store
	verifier TokenVerifier
	now      func() time.Time
	ttl      time.Duration
}

func New(deps Dependencies) (*Service, error) {
	if deps.Store == nil {
		return nil, errors.New("auth store is required")
	}
	if deps.TTL < 0 {
		return nil, errors.New("auth TTL must not be negative")
	}
	if deps.TTL == 0 {
		deps.TTL = DefaultTTL
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Service{store: deps.Store, verifier: deps.Verifier, now: deps.Now, ttl: deps.TTL}, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	if err := ctx.Err(); err != nil {
		return Principal{}, err
	}
	if token == "" {
		return Principal{}, ErrUnauthenticated
	}
	now := s.now().UTC().Truncate(time.Second)
	var claims TokenClaims
	if s.verifier != nil {
		var err error
		claims, err = s.verifier.Verify(ctx, token, now)
		if err != nil {
			return Principal{}, dependencyError("verify session token", err)
		}
		if err := ctx.Err(); err != nil {
			return Principal{}, err
		}
		claims.ExpiresAt = claims.ExpiresAt.UTC().Truncate(time.Second)
		if claims.UserID == 0 || !claims.ExpiresAt.After(now) {
			return Principal{}, ErrUnauthenticated
		}
	}
	user, session, err := s.store.Lookup(ctx, token)
	if err != nil {
		return Principal{}, dependencyError("lookup session", err)
	}
	if err := ctx.Err(); err != nil {
		return Principal{}, err
	}
	session.ExpiresAt = session.ExpiresAt.UTC().Truncate(time.Second)
	if user.ID == 0 || session.ID == 0 || session.UserID != user.ID || session.Token != token || !session.ExpiresAt.After(now) {
		return Principal{}, ErrUnauthenticated
	}
	if s.verifier != nil && claims.UserID != session.UserID {
		return Principal{}, ErrUnauthenticated
	}
	if !user.Enabled {
		return Principal{}, ErrDisabled
	}
	effectiveExpiry := session.ExpiresAt.UTC().Truncate(time.Second)
	if s.verifier != nil && claims.ExpiresAt.Before(effectiveExpiry) {
		effectiveExpiry = claims.ExpiresAt.UTC().Truncate(time.Second)
	}
	if effectiveExpiry.Sub(now) < s.ttl/3 {
		expiresAt := now.Add(s.ttl).UTC().Truncate(time.Second)
		if s.verifier != nil && claims.ExpiresAt.Before(expiresAt) {
			expiresAt = claims.ExpiresAt.UTC().Truncate(time.Second)
		}
		if expiresAt.After(session.ExpiresAt) {
			renewed, err := s.store.Renew(ctx, session, expiresAt, now)
			if err != nil {
				return Principal{}, dependencyError("renew session", err)
			}
			renewed.ExpiresAt = renewed.ExpiresAt.UTC().Truncate(time.Second)
			if renewed.ID != session.ID || renewed.UserID != user.ID || renewed.Token != token || !renewed.ExpiresAt.After(now) {
				return Principal{}, ErrUnauthenticated
			}
			session = renewed
			effectiveExpiry = renewed.ExpiresAt.UTC().Truncate(time.Second)
			if s.verifier != nil && claims.ExpiresAt.Before(effectiveExpiry) {
				effectiveExpiry = claims.ExpiresAt
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Principal{}, err
	}
	session.ExpiresAt = effectiveExpiry
	return Principal{User: user, Session: session}, nil
}

func (s *Service) Revoke(ctx context.Context, principal Principal, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if token == "" || principal.User.ID == 0 || principal.Session.ID == 0 || principal.Session.UserID != principal.User.ID || principal.Session.Token != token {
		return ErrUnauthenticated
	}
	if err := s.store.Revoke(ctx, principal.Session); err != nil {
		return dependencyError("revoke session", err)
	}
	return ctx.Err()
}

// RequireAdministrator checks an authenticated principal using its database
// user role, never a JWT role claim or a frontend route name.
func RequireAdministrator(principal Principal) error {
	if principal.User.ID == 0 || principal.Session.ID == 0 || principal.Session.UserID != principal.User.ID {
		return ErrUnauthenticated
	}
	if !principal.User.Enabled {
		return ErrDisabled
	}
	if !principal.User.IsAdmin {
		return ErrForbidden
	}
	return nil
}

// RevokeSessions retains the legacy administrator ID-delete behavior: missing
// rows are idempotent and these administrative deletes do not unbind peers.
func (s *Service) RevokeSessions(ctx context.Context, actor Principal, ids []uint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := RequireAdministrator(actor); err != nil {
		return err
	}
	current, err := s.Authenticate(ctx, actor.Session.Token)
	if err != nil {
		return err
	}
	if current.User.ID != actor.User.ID || current.Session.ID != actor.Session.ID {
		return ErrUnauthenticated
	}
	if err := RequireAdministrator(current); err != nil {
		return err
	}
	for _, id := range ids {
		if id == 0 {
			return ErrInvalidInput
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if err := s.store.DeleteSessions(ctx, ids); err != nil {
		return dependencyError("delete sessions", err)
	}
	return ctx.Err()
}

func dependencyError(operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrDisabled) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrInvalidInput) {
		return err
	}
	return errors.Join(ErrUnavailable, fmt.Errorf("%s: %w", operation, err))
}
