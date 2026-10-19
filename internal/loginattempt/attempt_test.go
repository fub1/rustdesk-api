package loginattempt_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	la "github.com/lejianwen/rustdesk-api/v2/internal/loginattempt"
)

var background = context.Background()
var device = la.Device{ID: "123", UUID: "uuid", Client: "client", Platform: "Windows"}

type clock struct {
	mu    sync.Mutex
	value time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.value }
func (c *clock) add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.value = c.value.Add(d) }

func fixture(t *testing.T, capacity int) (*la.Store, *clock) {
	t.Helper()
	c := &clock{value: time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)}
	s, err := la.New(la.Options{Capacity: capacity, Now: c.now, Random: rand.New(rand.NewSource(7))})
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}
func begin(t *testing.T, s *la.Store) la.Attempt {
	t.Helper()
	a, err := s.Begin(background, la.BeginInput{Device: device, Purpose: la.PurposeLogin, Provider: "provider-value"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func authorize(t *testing.T, s *la.Store, a la.Attempt) la.Lease {
	t.Helper()
	lease, err := s.ClaimState(background, a.State)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(background, lease, "verified-identity"); err != nil {
		t.Fatal(err)
	}
	return lease
}
func poll(a la.Attempt) la.PollInput {
	return la.PollInput{Code: a.Code, Device: a.Device, Purpose: a.Purpose}
}

type result struct{ Token string }

func TestCallbackBudgetUsesRemainingStoreClockLifetime(t *testing.T) {
	s, c := fixture(t, 0)
	a := begin(t, s)
	lease, err := s.ClaimState(background, a.State)
	if err != nil {
		t.Fatal(err)
	}
	c.add(la.DefaultTTL - 80*time.Millisecond)
	work, cancel, err := s.ContextForLease(background, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	deadline, ok := work.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 80*time.Millisecond {
		t.Fatal("callback budget used wall-clock timestamp or refreshed the original TTL")
	}
	<-work.Done()
	if !errors.Is(work.Err(), context.DeadlineExceeded) {
		t.Fatal(work.Err())
	}
	// The fake clock is intentionally historical relative to a future test run.
	// Its remaining duration, rather than its absolute timestamp, is authoritative.
	c.add(80 * time.Millisecond)
	if err := s.CheckLease(background, lease); !errors.Is(err, la.ErrExpired) {
		t.Fatalf("expired lease: %v", err)
	}
	if _, _, err := s.ContextForLease(background, lease); !errors.Is(err, la.ErrNotFound) && !errors.Is(err, la.ErrExpired) {
		t.Fatalf("expired budget: %v", err)
	}
}

func TestPollWorkBudgetConsumesOriginalTTL(t *testing.T) {
	s, c := fixture(t, 0)
	a := begin(t, s)
	authorize(t, s, a)
	c.add(la.DefaultTTL - 60*time.Millisecond)
	_, err := s.Poll(background, poll(a), func(work context.Context, _ la.Attempt) (any, error) {
		deadline, ok := work.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 60*time.Millisecond {
			t.Fatal("issue work did not consume the remaining original TTL")
		}
		<-work.Done()
		return nil, work.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("poll work deadline: %v", err)
	}
}

func TestLeaseBudgetRejectsForeignCompletedAndCanceledLeases(t *testing.T) {
	s, _ := fixture(t, 0)
	a := begin(t, s)
	lease, err := s.ClaimState(background, a.State)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := fixture(t, 0)
	if err := other.CheckLease(background, lease); !errors.Is(err, la.ErrInvalidInput) {
		t.Fatal(err)
	}
	if err := s.CheckLease(background, la.Lease{}); !errors.Is(err, la.ErrInvalidInput) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(background)
	cancel()
	if _, _, err := s.ContextForLease(canceled, lease); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.Complete(background, lease, "identity"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckLease(background, lease); !errors.Is(err, la.ErrInvalidState) {
		t.Fatal(err)
	}
}

func TestIndependentCredentialsAndLeaseCopies(t *testing.T) {
	s, _ := fixture(t, 0)
	a := begin(t, s)
	seen := map[string]bool{}
	for _, token := range []string{a.Code, a.State, a.Nonce, a.Verifier} {
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(decoded) != 32 || seen[token] {
			t.Fatalf("invalid or reused credential: length=%d error=%v", len(decoded), err)
		}
		seen[token] = true
	}
	if a.Status != la.Pending || a.ExpiresAt.Sub(a.CreatedAt) != la.DefaultTTL {
		t.Fatalf("wrong initial attempt: %+v", a)
	}
	if _, err := s.ClaimState(background, a.Code); !errors.Is(err, la.ErrNotFound) {
		t.Fatalf("poll code used as callback state: %v", err)
	}
	if _, err := s.Poll(background, la.PollInput{Code: a.State, Device: a.Device, Purpose: a.Purpose}, nil); !errors.Is(err, la.ErrNotFound) {
		t.Fatalf("state used as poll code: %v", err)
	}
	lease, err := s.ClaimState(background, a.State)
	if err != nil {
		t.Fatal(err)
	}
	copy := lease.Attempt()
	copy.Device.ID = "mutated"
	copy.State = "changed"
	if got := lease.Attempt(); got.Device != device || got.State != a.State {
		t.Fatal("lease exposes mutable attempt fields")
	}
	if _, err := s.ClaimState(background, a.State); !errors.Is(err, la.ErrClaimed) {
		t.Fatalf("callback replay: %v", err)
	}
	if err := s.Complete(background, lease, "identity"); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(background, lease, "another"); !errors.Is(err, la.ErrInvalidState) {
		t.Fatalf("second completion: %v", err)
	}
	if err := s.Fail(background, lease, errors.New("late")); !errors.Is(err, la.ErrInvalidState) {
		t.Fatalf("late failure: %v", err)
	}
	other, _ := fixture(t, 0)
	if err := other.Complete(background, lease, "identity"); !errors.Is(err, la.ErrInvalidInput) {
		t.Fatalf("foreign lease: %v", err)
	}
	if err := s.Complete(background, la.Lease{}, "identity"); !errors.Is(err, la.ErrInvalidInput) {
		t.Fatalf("forged lease: %v", err)
	}
}

func TestDeviceAndPurposeBindings(t *testing.T) {
	s, _ := fixture(t, 0)
	a := begin(t, s)
	for _, altered := range []la.Device{
		{ID: "different", UUID: device.UUID, Client: device.Client, Platform: device.Platform},
		{ID: device.ID, UUID: "different", Client: device.Client, Platform: device.Platform},
		{ID: device.ID, UUID: device.UUID, Client: "different", Platform: device.Platform},
	} {
		q := poll(a)
		q.Device = altered
		if _, err := s.Poll(background, q, nil); !errors.Is(err, la.ErrForbidden) {
			t.Fatalf("cross-device poll: %v", err)
		}
	}
	// Native clients omit Platform from polling; only stable device identity and
	// the server-assigned route audience participate in the binding.
	withoutPlatform := poll(a)
	withoutPlatform.Device.Platform = ""
	if _, err := s.Poll(background, withoutPlatform, nil); !errors.Is(err, la.ErrPending) {
		t.Fatalf("platform metadata incorrectly required: %v", err)
	}
	q := poll(a)
	q.Purpose = la.PurposeBind
	if _, err := s.Poll(background, q, nil); !errors.Is(err, la.ErrForbidden) {
		t.Fatalf("cross-purpose poll: %v", err)
	}
	if _, err := s.Poll(background, poll(a), nil); !errors.Is(err, la.ErrPending) {
		t.Fatalf("pending: %v", err)
	}
	lease, err := s.ClaimState(background, a.State)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Poll(background, poll(a), nil); !errors.Is(err, la.ErrPending) {
		t.Fatalf("processing: %v", err)
	}
	cause := errors.New("sensitive-provider-error")
	if err := s.Fail(background, lease, cause); err != nil {
		t.Fatal(err)
	}
	_, err = s.Poll(background, poll(a), func(context.Context, la.Attempt) (any, error) { t.Fatal("failed attempt issued"); return nil, nil })
	var failed *la.FailureError
	if !errors.Is(err, la.ErrFailed) || !errors.As(err, &failed) || failed.Cause != cause || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("terminal failure lost or leaked: %v", err)
	}
	if _, err := s.ClaimState(background, a.State); !errors.Is(err, la.ErrClaimed) {
		t.Fatalf("failed callback reclaimed: %v", err)
	}
}

func TestConcurrentCallbackAndPollingIssueOnlyOnce(t *testing.T) {
	s, _ := fixture(t, 0)
	a := begin(t, s)
	var claims atomic.Int32
	var wg sync.WaitGroup
	winner := make(chan la.Lease, 1)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := s.ClaimState(background, a.State)
			if err == nil {
				claims.Add(1)
				winner <- lease
			} else if !errors.Is(err, la.ErrClaimed) {
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatalf("callback claims=%d", claims.Load())
	}
	if err := s.Complete(background, <-winner, "identity"); err != nil {
		t.Fatal(err)
	}
	var issued atomic.Int32
	issue := func(ctx context.Context, a la.Attempt) (any, error) {
		issued.Add(1)
		if a.Identity != "identity" || a.Status != la.Authorized {
			t.Error("issuer lacks verified identity")
		}
		time.Sleep(10 * time.Millisecond)
		return result{"one-session"}, nil
	}
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.Poll(background, poll(a), issue)
			if err != nil || got != (result{"one-session"}) {
				t.Errorf("poll result=%v error=%v", got, err)
			}
		}()
	}
	wg.Wait()
	if issued.Load() != 1 {
		t.Fatalf("sessions minted=%d", issued.Load())
	}
	got, err := s.Poll(background, poll(a), nil)
	if err != nil || got != (result{"one-session"}) {
		t.Fatalf("lost-response recovery=%v %v", got, err)
	}
}

func TestIssuanceFailureCanRetryWithoutReexchangingCode(t *testing.T) {
	s, _ := fixture(t, 0)
	a := begin(t, s)
	authorize(t, s, a)
	calls := 0
	issue := func(context.Context, la.Attempt) (any, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("temporary database error")
		}
		return result{"session"}, nil
	}
	if _, err := s.Poll(background, poll(a), issue); err == nil {
		t.Fatal("issuance failure ignored")
	}
	if _, err := s.ClaimState(background, a.State); !errors.Is(err, la.ErrClaimed) {
		t.Fatalf("callback could exchange code twice: %v", err)
	}
	for i := 0; i < 2; i++ {
		if got, err := s.Poll(background, poll(a), issue); err != nil || got != (result{"session"}) {
			t.Fatalf("retry result=%v error=%v", got, err)
		}
	}
	if calls != 2 {
		t.Fatalf("issuer calls=%d", calls)
	}
}

func TestSlowAttemptDoesNotBlockOtherAttemptsAndWaitCanCancel(t *testing.T) {
	s, _ := fixture(t, 0)
	a := begin(t, s)
	authorize(t, s, a)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.Poll(background, poll(a), func(context.Context, la.Attempt) (any, error) { close(entered); <-release; return result{"a"}, nil })
		done <- err
	}()
	<-entered
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	otherDone := make(chan error, 1)
	go func() {
		b, err := s.Begin(background, la.BeginInput{Device: device, Purpose: la.PurposeLogin})
		if err == nil {
			var l la.Lease
			l, err = s.ClaimState(background, b.State)
			if err == nil {
				err = s.Complete(background, l, "b")
			}
		}
		otherDone <- err
	}()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("one issuer blocks all attempts")
	}
	ctx, cancel := context.WithTimeout(background, 25*time.Millisecond)
	defer cancel()
	if _, err := s.Poll(ctx, poll(a), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gate wait ignored cancellation: %v", err)
	}
}

func TestSuccessfulIssueSurvivesResponseCancellation(t *testing.T) {
	s, _ := fixture(t, 0)
	a := begin(t, s)
	authorize(t, s, a)
	ctx, cancel := context.WithCancel(background)
	_, err := s.Poll(ctx, poll(a), func(context.Context, la.Attempt) (any, error) { cancel(); return result{"committed"}, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled response: %v", err)
	}
	got, err := s.Poll(background, poll(a), func(context.Context, la.Attempt) (any, error) {
		t.Fatal("committed result was minted again")
		return nil, nil
	})
	if err != nil || got != (result{"committed"}) {
		t.Fatalf("result recovery=%v %v", got, err)
	}
}

func TestAbsoluteExpiryCapacityAndForget(t *testing.T) {
	s, c := fixture(t, 1)
	a := begin(t, s)
	if _, err := s.Begin(background, la.BeginInput{Device: device, Purpose: la.PurposeLogin}); !errors.Is(err, la.ErrCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	c.add(4 * time.Minute)
	lease, err := s.ClaimState(background, a.State)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(background, lease, "identity"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Poll(background, poll(a), func(context.Context, la.Attempt) (any, error) { return result{"session"}, nil }); err != nil {
		t.Fatal(err)
	}
	c.add(time.Minute)
	if _, err := s.Poll(background, poll(a), nil); !errors.Is(err, la.ErrExpired) {
		t.Fatalf("result extended original TTL: %v", err)
	}
	b := begin(t, s)
	lease, err = s.ClaimState(background, b.State)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(background, b.Code); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(background, lease, "identity"); !errors.Is(err, la.ErrNotFound) {
		t.Fatalf("forgotten lease remains valid: %v", err)
	}
	begin(t, s)
}

func TestExpiredInFlightIssueCannotRemoveReplacement(t *testing.T) {
	// Reusing credentials is astronomically unlikely with crypto/rand, but a
	// stale entry must still never remove a replacement from either index.
	credentials := make([]byte, 128)
	if _, err := rand.New(rand.NewSource(9)).Read(credentials); err != nil {
		t.Fatal(err)
	}
	c := &clock{value: time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)}
	s, err := la.New(la.Options{Capacity: 1, Now: c.now, Random: bytes.NewReader(append(credentials, credentials...))})
	if err != nil {
		t.Fatal(err)
	}
	a := begin(t, s)
	authorize(t, s, a)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.Poll(background, poll(a), func(context.Context, la.Attempt) (any, error) {
			close(entered)
			<-release
			return result{"expired-session"}, nil
		})
		done <- err
	}()
	<-entered
	c.add(la.DefaultTTL)
	b, err := s.Begin(background, la.BeginInput{Device: device, Purpose: la.PurposeLogin})
	close(release)
	if issueErr := <-done; !errors.Is(issueErr, la.ErrExpired) {
		t.Fatalf("expired in-flight issue returned result: %v", issueErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if b.Code != a.Code || b.State != a.State {
		t.Fatal("test did not exercise replacement indexes")
	}
	if _, err := s.Poll(background, poll(b), nil); !errors.Is(err, la.ErrPending) {
		t.Fatalf("old issuer removed replacement poll index: %v", err)
	}
	lease, err := s.ClaimState(background, b.State)
	if err != nil {
		t.Fatalf("old issuer removed replacement callback index: %v", err)
	}
	c.add(la.DefaultTTL)
	if err := s.Complete(background, lease, "late identity"); !errors.Is(err, la.ErrExpired) {
		t.Fatalf("callback completion extended original TTL: %v", err)
	}
}

func TestBindingInspectionDoesNotExposeSecretsToJSON(t *testing.T) {
	s, _ := fixture(t, 0)
	binding := la.Binding{ActorID: 7, SessionToken: "private-session"}
	a, err := s.Begin(background, la.BeginInput{Device: device, Purpose: la.PurposeBind, Binding: binding, Provider: "private-provider"})
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []la.Binding{{}, {ActorID: 8, SessionToken: binding.SessionToken}, {ActorID: 7, SessionToken: "other"}} {
		if _, err := s.Inspect(background, a.Code, actor); !errors.Is(err, la.ErrForbidden) {
			t.Fatalf("wrong binding actor: %v", err)
		}
	}
	copy, err := s.Inspect(background, a.Code, binding)
	if err != nil {
		t.Fatal(err)
	}
	copy.Device.ID = "mutated"
	got, err := s.Inspect(background, a.Code, binding)
	if err != nil || got.Device != device {
		t.Fatal("inspection mutates store")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{a.State, a.Nonce, a.Verifier, "private-session", "private-provider"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("secret serialized: %s", encoded)
		}
	}
}

type failOnce struct {
	inner  io.Reader
	failed bool
}

func (r *failOnce) Read(p []byte) (int, error) {
	if !r.failed {
		r.failed = true
		return 0, errors.New("entropy failed")
	}
	return r.inner.Read(p)
}

func TestInvalidOptionsAndEntropyFailureDoNotReserveCapacity(t *testing.T) {
	for _, options := range []la.Options{{TTL: -time.Second}, {Capacity: -1}} {
		if s, err := la.New(options); err == nil || s != nil {
			t.Fatal("invalid options accepted")
		}
	}
	r := &failOnce{inner: rand.New(rand.NewSource(1))}
	s, err := la.New(la.Options{Capacity: 1, Random: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(background, la.BeginInput{Device: device, Purpose: la.PurposeLogin}); !errors.Is(err, la.ErrRandom) {
		t.Fatalf("entropy failure: %v", err)
	}
	begin(t, s)
	duplicate, err := la.New(la.Options{Random: bytes.NewReader(make([]byte, 128))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := duplicate.Begin(background, la.BeginInput{Device: device, Purpose: la.PurposeLogin}); !errors.Is(err, la.ErrRandom) {
		t.Fatalf("identical state/poll accepted: %v", err)
	}
	defaults, err := la.New(la.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a := begin(t, defaults)
	if a.Code == a.State {
		t.Fatal("production randomness reused code/state")
	}
}

func TestCanceledAPIsDoNotChangeAttempt(t *testing.T) {
	s, _ := fixture(t, 0)
	a := begin(t, s)
	ctx, cancel := context.WithCancel(background)
	cancel()
	if _, err := s.Begin(ctx, la.BeginInput{Purpose: la.PurposeLogin}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.ClaimState(ctx, a.State); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	lease, err := s.ClaimState(background, a.State)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, lease, "identity"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.Fail(ctx, lease, errors.New("failed")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, a.Code); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.Poll(ctx, poll(a), nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.Inspect(ctx, a.Code, la.Binding{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.Metadata(ctx, a.Code); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.Approve(ctx, a.Code, "actor"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.Complete(background, lease, "identity"); err != nil {
		t.Fatal("canceled operation changed state", err)
	}
}

func TestWebApprovalIsAtomicAndSeparateFromOIDCCallback(t *testing.T) {
	s, _ := fixture(t, 0)
	oidc := begin(t, s)
	for _, processing := range []bool{false, true} {
		if processing {
			if _, err := s.ClaimState(background, oidc.State); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Approve(background, oidc.Code, "actor"); !errors.Is(err, la.ErrForbidden) {
			t.Fatalf("OIDC attempt approved through code: %v", err)
		}
		if _, err := s.Metadata(background, oidc.Code); !errors.Is(err, la.ErrForbidden) {
			t.Fatalf("OIDC attempt exposed to approval UI: %v", err)
		}
	}
	a, err := s.Begin(background, la.BeginInput{Device: device, Purpose: la.PurposeWebLogin, Provider: "webauth"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimState(background, a.State); !errors.Is(err, la.ErrForbidden) {
		t.Fatalf("interactive approval claimed as provider callback: %v", err)
	}
	copy, err := s.Metadata(background, a.Code)
	if err != nil || copy.Purpose != la.PurposeWebLogin || copy.Provider != "webauth" {
		t.Fatalf("metadata: %+v %v", copy, err)
	}
	copy.Device.ID = "mutation"
	var approvals atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Approve(background, a.Code, "validated-actor"); err == nil {
				approvals.Add(1)
			} else if !errors.Is(err, la.ErrInvalidState) {
				t.Errorf("approve: %v", err)
			}
		}()
	}
	wg.Wait()
	if approvals.Load() != 1 {
		t.Fatalf("approvals=%d", approvals.Load())
	}
	wrongPurpose := poll(a)
	wrongPurpose.Purpose = la.PurposeLogin
	if _, err := s.Poll(background, wrongPurpose, nil); !errors.Is(err, la.ErrForbidden) {
		t.Fatalf("web approval polled as OIDC login: %v", err)
	}
	got, err := s.Poll(background, poll(a), func(_ context.Context, attempt la.Attempt) (any, error) {
		if attempt.Identity != "validated-actor" || attempt.Device != device {
			t.Fatal("approval identity or immutable metadata changed")
		}
		return result{"web-session"}, nil
	})
	if err != nil || got != (result{"web-session"}) {
		t.Fatalf("approved poll: %v %v", got, err)
	}
}
