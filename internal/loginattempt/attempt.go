// Package loginattempt owns bounded, instance-local browser login attempts.
// It starts no goroutines. Expiration is lazy and never extended by callbacks
// or polling. Callers must revalidate a cached result's local session before
// returning it: this cache is not authority for account or session validity.
package loginattempt

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	DefaultTTL      = 5 * time.Minute
	DefaultCapacity = 1024
)

var (
	ErrInvalidInput = errors.New("invalid login attempt input")
	ErrNotFound     = errors.New("login attempt not found")
	ErrExpired      = errors.New("login attempt expired")
	ErrCapacity     = errors.New("login attempt capacity reached")
	ErrRandom       = errors.New("login attempt entropy unavailable")
	ErrPending      = errors.New("login attempt pending")
	ErrClaimed      = errors.New("login callback already claimed")
	ErrInvalidState = errors.New("invalid login attempt transition")
	ErrForbidden    = errors.New("login attempt binding mismatch")
	ErrFailed       = errors.New("login attempt failed")
)

type Purpose string

const (
	PurposeLogin Purpose = "login"
	PurposeBind  Purpose = "bind"
	// PurposeWebLogin is an explicit interactive approval flow, separate from
	// provider callbacks. The trusted adapter authenticates the approving actor.
	PurposeWebLogin Purpose = "web-login"
)

type Status string

const (
	Pending    Status = "pending"
	Processing Status = "processing"
	Authorized Status = "authorized"
	Failed     Status = "failed"
	Issued     Status = "issued"
)

// Client is an adapter-assigned route audience, never a caller's device type.
// Platform is begin-time diagnostic metadata and is not part of poll binding.
type Device struct{ ID, UUID, Client, Platform string }

// Binding is the authenticated local actor that initiated a bind. It must
// never be serialized to the browser or the polling client.
type Binding struct {
	ActorID      uint   `json:"-"`
	SessionToken string `json:"-"`
}

type BeginInput struct {
	Device  Device
	Purpose Purpose
	Binding Binding
	// Provider is an immutable configuration VALUE, not a pointer to mutable
	// live configuration. Provider and Identity payloads remain caller-owned
	// immutable values; arbitrary nested values cannot be deep-copied here.
	Provider any
}

type Attempt struct {
	Code                 string
	State                string `json:"-"`
	Nonce                string `json:"-"`
	Verifier             string `json:"-"`
	CreatedAt, ExpiresAt time.Time
	Device               Device
	Purpose              Purpose
	Status               Status
	Binding              Binding `json:"-"`
	Provider             any     `json:"-"`
	Identity             any     `json:"-"`
}

type PollInput struct {
	Code    string
	Device  Device
	Purpose Purpose
}

// FailureError retains a cause for the trusted adapter, without placing its
// potentially sensitive text in Error(). Map causes to safe public messages.
type FailureError struct{ Cause error }

func (*FailureError) Error() string { return "login attempt failed" }
func (*FailureError) Unwrap() error { return ErrFailed }

type Options struct {
	TTL      time.Duration
	Capacity int
	// Now must be safe for concurrent calls. Zero values use real time and
	// crypto/rand.Reader; the injected reader is serialized by this Store.
	Now    func() time.Time
	Random io.Reader
}

type entry struct {
	gate    chan struct{}
	attempt Attempt
	result  any
	failure error
}

type Store struct {
	mu         sync.Mutex
	byCode     map[string]*entry
	byState    map[string]*entry
	randomGate chan struct{}
	random     io.Reader
	now        func() time.Time
	ttl        time.Duration
	capacity   int
}

// Lease cannot be constructed by external callers. A callback claim is never
// released back to Pending, even if exchanging the one-use provider code fails.
type Lease struct {
	owner    *Store
	entry    *entry
	snapshot Attempt
}

func (l Lease) Attempt() Attempt { return l.snapshot }

func New(options Options) (*Store, error) {
	if options.TTL < 0 || options.Capacity < 0 {
		return nil, ErrInvalidInput
	}
	if options.TTL == 0 {
		options.TTL = DefaultTTL
	}
	if options.Capacity == 0 {
		options.Capacity = DefaultCapacity
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	s := &Store{byCode: make(map[string]*entry), byState: make(map[string]*entry), randomGate: make(chan struct{}, 1), random: options.Random, now: options.Now, ttl: options.TTL, capacity: options.Capacity}
	s.randomGate <- struct{}{}
	return s, nil
}

func acquire(ctx context.Context, gate chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-gate:
		if err := ctx.Err(); err != nil {
			gate <- struct{}{}
			return err
		}
		return nil
	}
}
func release(gate chan struct{}) { gate <- struct{}{} }

func validPurpose(p Purpose) bool {
	return p == PurposeLogin || p == PurposeBind || p == PurposeWebLogin
}

func (s *Store) credentials(ctx context.Context) ([4]string, error) {
	var tokens [4]string
	if err := acquire(ctx, s.randomGate); err != nil {
		return tokens, err
	}
	defer release(s.randomGate)
	for i := range tokens {
		if err := ctx.Err(); err != nil {
			return tokens, err
		}
		var data [32]byte
		if _, err := io.ReadFull(s.random, data[:]); err != nil {
			return tokens, fmt.Errorf("%w: %v", ErrRandom, err)
		}
		tokens[i] = base64.RawURLEncoding.EncodeToString(data[:])
		for j := 0; j < i; j++ {
			if tokens[i] == tokens[j] {
				return tokens, ErrRandom
			}
		}
	}
	return tokens, ctx.Err()
}

func (s *Store) Begin(ctx context.Context, input BeginInput) (Attempt, error) {
	if err := ctx.Err(); err != nil {
		return Attempt{}, err
	}
	if !validPurpose(input.Purpose) || (input.Binding.ActorID == 0) != (input.Binding.SessionToken == "") || (input.Purpose == PurposeBind && input.Binding.ActorID == 0) {
		return Attempt{}, ErrInvalidInput
	}
	// Entropy is never read while holding the store's index lock. io.Reader
	// itself has no cancellation contract; custom readers must not block forever.
	tokens, err := s.credentials(ctx)
	if err != nil {
		return Attempt{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Attempt{}, err
	}
	now := s.now().UTC()
	s.expire(now)
	if len(s.byCode) >= s.capacity {
		return Attempt{}, ErrCapacity
	}
	if s.byCode[tokens[0]] != nil || s.byState[tokens[0]] != nil || s.byCode[tokens[1]] != nil || s.byState[tokens[1]] != nil {
		return Attempt{}, ErrRandom
	}
	a := Attempt{Code: tokens[0], State: tokens[1], Nonce: tokens[2], Verifier: tokens[3], CreatedAt: now, ExpiresAt: now.Add(s.ttl), Device: input.Device, Purpose: input.Purpose, Status: Pending, Binding: input.Binding, Provider: input.Provider}
	e := &entry{gate: make(chan struct{}, 1), attempt: a}
	e.gate <- struct{}{}
	s.byCode[a.Code], s.byState[a.State] = e, e
	return a, nil
}

// expire only accesses immutable fields. It never waits for an attempt's gate.
// An operation holding an expired entry must still pass live() before use.
func (s *Store) expire(now time.Time) {
	for code, e := range s.byCode {
		if !now.Before(e.attempt.ExpiresAt) {
			delete(s.byCode, code)
			delete(s.byState, e.attempt.State)
		}
	}
}

func (s *Store) lookup(ctx context.Context, key string, state bool) (*entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var e *entry
	if state {
		e = s.byState[key]
	} else {
		e = s.byCode[key]
	}
	now := s.now().UTC()
	s.expire(now)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e == nil {
		return nil, ErrNotFound
	}
	if !now.Before(e.attempt.ExpiresAt) {
		return nil, ErrExpired
	}
	return e, nil
}

// live is called only with e.gate held; index mutations never acquire e.gate.
func (s *Store) live(ctx context.Context, e *entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.now().UTC().Before(e.attempt.ExpiresAt) {
		if s.byCode[e.attempt.Code] == e {
			delete(s.byCode, e.attempt.Code)
		}
		if s.byState[e.attempt.State] == e {
			delete(s.byState, e.attempt.State)
		}
		return ErrExpired
	}
	if s.byCode[e.attempt.Code] != e {
		return ErrNotFound
	}
	return ctx.Err()
}

func (s *Store) ClaimState(ctx context.Context, state string) (Lease, error) {
	e, err := s.lookup(ctx, state, true)
	if err != nil {
		return Lease{}, err
	}
	if err := acquire(ctx, e.gate); err != nil {
		return Lease{}, err
	}
	defer release(e.gate)
	if err := s.live(ctx, e); err != nil {
		return Lease{}, err
	}
	if e.attempt.Purpose == PurposeWebLogin {
		return Lease{}, ErrForbidden
	}
	if e.attempt.Status != Pending {
		return Lease{}, ErrClaimed
	}
	e.attempt.Status = Processing
	return Lease{owner: s, entry: e, snapshot: e.attempt}, nil
}

// CheckLease verifies a claimed callback is still live before a caller performs
// an irreversible operation. A network exchange never extends the attempt TTL.
func (s *Store) CheckLease(ctx context.Context, lease Lease) error {
	if lease.owner != s || lease.entry == nil {
		return ErrInvalidInput
	}
	e := lease.entry
	if err := acquire(ctx, e.gate); err != nil {
		return err
	}
	defer release(e.gate)
	if err := s.live(ctx, e); err != nil {
		return err
	}
	if e.attempt.Status != Processing {
		return ErrInvalidState
	}
	return nil
}

// ContextForLease bounds callback work by the remaining absolute attempt life.
// Use a duration from the store clock so injected clocks are not mistaken for
// the machine's wall clock. Each call consumes the same original TTL.
func (s *Store) ContextForLease(ctx context.Context, lease Lease) (context.Context, context.CancelFunc, error) {
	if err := s.CheckLease(ctx, lease); err != nil {
		return nil, nil, err
	}
	return s.workContext(ctx, lease.snapshot.ExpiresAt)
}

func (s *Store) workContext(ctx context.Context, expiresAt time.Time) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	remaining := expiresAt.Sub(s.now().UTC())
	if remaining <= 0 {
		return nil, nil, ErrExpired
	}
	work, cancel := context.WithTimeout(ctx, remaining)
	return work, cancel, nil
}

func (s *Store) transition(ctx context.Context, lease Lease, identity any, failure error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lease.owner != s || lease.entry == nil {
		return ErrInvalidInput
	}
	e := lease.entry
	if err := acquire(ctx, e.gate); err != nil {
		return err
	}
	defer release(e.gate)
	if err := s.live(ctx, e); err != nil {
		return err
	}
	if e.attempt.Status != Processing {
		return ErrInvalidState
	}
	if failure != nil {
		e.failure, e.attempt.Status = failure, Failed
	} else {
		e.attempt.Identity, e.attempt.Status = identity, Authorized
	}
	return nil
}

func (s *Store) Complete(ctx context.Context, lease Lease, identity any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if identity == nil {
		return ErrInvalidInput
	}
	return s.transition(ctx, lease, identity, nil)
}
func (s *Store) Fail(ctx context.Context, lease Lease, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cause == nil {
		return ErrInvalidInput
	}
	return s.transition(ctx, lease, nil, cause)
}

// Poll issues at most once after authorization and caches the immutable result.
// issue must honor ctx and must not reenter this attempt. Errors leave the
// attempt Authorized for retry; successful issuance stays cached even if ctx
// is canceled just after issue returns. No other attempt waits on this gate.
// The adapter must authenticate/revalidate EVERY returned cached session.
func (s *Store) Poll(ctx context.Context, input PollInput, issue func(context.Context, Attempt) (any, error)) (any, error) {
	e, err := s.lookup(ctx, input.Code, false)
	if err != nil {
		return nil, err
	}
	if err := acquire(ctx, e.gate); err != nil {
		return nil, err
	}
	defer release(e.gate)
	if err := s.live(ctx, e); err != nil {
		return nil, err
	}
	bound := e.attempt.Device
	if bound.ID != input.Device.ID || bound.UUID != input.Device.UUID || bound.Client != input.Device.Client || e.attempt.Purpose != input.Purpose {
		return nil, ErrForbidden
	}
	switch e.attempt.Status {
	case Pending, Processing:
		return nil, ErrPending
	case Failed:
		return nil, &FailureError{Cause: e.failure}
	case Issued:
		return e.result, nil
	case Authorized:
		if issue == nil {
			return nil, ErrInvalidInput
		}
		work, cancel, err := s.workContext(ctx, e.attempt.ExpiresAt)
		if err != nil {
			return nil, err
		}
		result, err := issue(work, e.attempt)
		workErr := work.Err()
		cancel()
		if err == nil {
			e.result, e.attempt.Status = result, Issued
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if workErr != nil {
			return nil, workErr
		}
		if err != nil {
			return nil, err
		}
		if err := s.live(ctx, e); err != nil {
			return nil, err
		}
		return result, nil
	default:
		return nil, ErrInvalidState
	}
}

// Inspect exposes a copy only to the same authenticated binding actor/session.
// The adapter must emit a sanitized DTO, never marshal Attempt or Binding.
func (s *Store) Inspect(ctx context.Context, code string, actor Binding) (Attempt, error) {
	if err := ctx.Err(); err != nil {
		return Attempt{}, err
	}
	if actor.ActorID == 0 || actor.SessionToken == "" {
		return Attempt{}, ErrForbidden
	}
	e, err := s.lookup(ctx, code, false)
	if err != nil {
		return Attempt{}, err
	}
	if err := acquire(ctx, e.gate); err != nil {
		return Attempt{}, err
	}
	defer release(e.gate)
	if err := s.live(ctx, e); err != nil {
		return Attempt{}, err
	}
	if e.attempt.Binding != actor {
		return Attempt{}, ErrForbidden
	}
	return e.attempt, nil
}

// Metadata is an internal capability for an authenticated approval UI. It only
// exposes web-login attempts. The adapter must authenticate first, enforce its
// provider/actor policy, and emit selected safe DTO fields, never this Attempt.
func (s *Store) Metadata(ctx context.Context, code string) (Attempt, error) {
	e, err := s.lookup(ctx, code, false)
	if err != nil {
		return Attempt{}, err
	}
	if err := acquire(ctx, e.gate); err != nil {
		return Attempt{}, err
	}
	defer release(e.gate)
	if err := s.live(ctx, e); err != nil {
		return Attempt{}, err
	}
	if e.attempt.Purpose != PurposeWebLogin {
		return Attempt{}, ErrForbidden
	}
	return e.attempt, nil
}

// Approve authorizes only an unclaimed interactive web-login attempt. This is
// not an HTTP authentication boundary: the trusted adapter must first validate
// the approving actor and provider policy. OIDC attempts cannot use this path.
func (s *Store) Approve(ctx context.Context, code string, identity any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if identity == nil {
		return ErrInvalidInput
	}
	e, err := s.lookup(ctx, code, false)
	if err != nil {
		return err
	}
	if err := acquire(ctx, e.gate); err != nil {
		return err
	}
	defer release(e.gate)
	if err := s.live(ctx, e); err != nil {
		return err
	}
	if e.attempt.Purpose != PurposeWebLogin {
		return ErrForbidden
	}
	if e.attempt.Status != Pending {
		return ErrInvalidState
	}
	e.attempt.Identity, e.attempt.Status = identity, Authorized
	return nil
}

// Forget discards a failed BeginURL construction or explicitly abandoned attempt.
// Calling code owns this internal capability; it is not an unauthenticated HTTP API.
func (s *Store) Forget(ctx context.Context, code string) error {
	e, err := s.lookup(ctx, code, false)
	if err != nil {
		return err
	}
	if err := acquire(ctx, e.gate); err != nil {
		return err
	}
	defer release(e.gate)
	if err := s.live(ctx, e); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byCode, e.attempt.Code)
	delete(s.byState, e.attempt.State)
	return ctx.Err()
}
