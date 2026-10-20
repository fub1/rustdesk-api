package oidcprovider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Flush successful headers before blocking the body. This deterministically
// selects SDK response-body handling instead of the Client.Do header failure.
func streamUntilCancelled(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"private-provider-body":`)
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

func TestHTTPClientBodyDeadlineHasTypedCauseBeforeSDK(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(streamUntilCancelled))
	defer s.Close()
	client := s.Client()
	client.Timeout = 60 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := client.Do(req)
	if err != nil {
		t.Fatal("fixture did not return successful headers")
	}
	defer r.Body.Close()
	_, err = io.ReadAll(r.Body)
	var typed net.Error
	if !errors.As(err, &typed) || !typed.Timeout() || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatal("body timeout did not retain typed deadline while caller remained active")
	}
}

func TestResponseBodyTimeoutKeepsTypedDeadlineAtEveryNetworkPhase(t *testing.T) {
	paths := map[string]string{"discovery": "/.well-known/openid-configuration", "token": "/token", "jwks": "/jwks", "userinfo": "/userinfo"}
	for _, phase := range []string{"discovery", "token", "jwks", "userinfo"} {
		t.Run(phase, func(t *testing.T) {
			f := newTestIDP(t, idpOptions{})
			original := f.server.Config.Handler
			var reached atomic.Bool
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == paths[phase] {
					reached.Store(true)
					streamUntilCancelled(w, r)
					return
				}
				original.ServeHTTP(w, r)
			})
			f.client.Timeout = 60 * time.Millisecond
			p := f.provider(t)
			if phase != "discovery" {
				if err := p.Prepare(context.Background()); err != nil {
					t.Fatal("prepare", err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			identity, err := p.Exchange(ctx, "authorization-code", testNonce, testVerifier)
			if !reached.Load() {
				t.Fatal("streaming response phase was not reached")
			}
			if identity != (Identity{}) || !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || strings.Contains(err.Error(), "private-provider-body") {
				t.Fatalf("body deadline lost or leaked: deadline=%t unavailable=%t caller-active=%t identity-empty=%t", errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrUnavailable), ctx.Err() == nil, identity == (Identity{}))
			}
		})
	}
}

func TestNetworkPhaseKeepsParentAndNormalizedBudgetIndependent(t *testing.T) {
	for _, configured := range []time.Duration{0, 60 * time.Millisecond, 20 * time.Second} {
		t.Run(configured.String(), func(t *testing.T) {
			callerClient := &http.Client{Timeout: configured}
			p, err := New(Config{Issuer: "https://issuer.example.test", ClientID: testClientID, RedirectURL: testRedirect, HTTPClient: callerClient})
			if err != nil {
				t.Fatal(err)
			}
			budget := configured
			if budget <= 0 || budget > 10*time.Second {
				budget = 10 * time.Second
			}
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			before := time.Now()
			first, cancelFirst := networkPhase(parent, p.client)
			after := time.Now()
			deadline, ok := first.Deadline()
			if !ok || deadline.Before(before.Add(budget)) || deadline.After(after.Add(budget)) {
				t.Fatal("phase did not use the normalized independent request budget")
			}
			if callerClient.Timeout != configured || p.client.Timeout != budget || p.client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
				t.Fatal("phase mutated client settings or redirect restriction")
			}
			cancelFirst()
			if parent.Err() != nil {
				t.Fatal("one phase cancelled its parent")
			}
			second, cancelSecond := networkPhase(parent, p.client)
			defer cancelSecond()
			if second.Err() != nil {
				t.Fatal("another phase inherited the prior phase cancellation")
			}
			cancelParent()
			if !errors.Is(second.Err(), context.Canceled) {
				t.Fatal("phase ignored caller cancellation")
			}
		})
	}
	parentDeadline := time.Now().Add(time.Minute)
	parent, cancelParent := context.WithDeadline(context.Background(), parentDeadline)
	defer cancelParent()
	phase, cancelPhase := networkPhase(parent, &http.Client{Timeout: 2 * time.Minute})
	defer cancelPhase()
	deadline, ok := phase.Deadline()
	if !ok || !deadline.Equal(parentDeadline) {
		t.Fatal("phase extended the caller deadline")
	}
}

// This old-style transport intentionally ignores Request.Context. Retaining
// http.Client.Timeout must still invoke the supported CancelRequest fallback.
type legacyCancellationTransport struct {
	cancelled chan struct{}
	once      sync.Once
}

func (tr *legacyCancellationTransport) RoundTrip(*http.Request) (*http.Response, error) {
	<-tr.cancelled
	return nil, errors.New("private legacy transport cancellation")
}
func (tr *legacyCancellationTransport) CancelRequest(*http.Request) {
	tr.once.Do(func() { close(tr.cancelled) })
}
func TestNetworkPhasePreservesClientLegacyCancellationFallback(t *testing.T) {
	transport := &legacyCancellationTransport{cancelled: make(chan struct{})}
	t.Cleanup(func() { transport.CancelRequest(nil) })
	caller := &http.Client{Timeout: 60 * time.Millisecond, Transport: transport}
	p, err := New(Config{Issuer: "https://issuer.example.test", ClientID: testClientID, RedirectURL: testRedirect, HTTPClient: caller})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- p.Prepare(ctx) }()
	select {
	case err := <-result:
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || strings.Contains(err.Error(), "private legacy") {
			t.Fatal("legacy cancellation fallback lost classification or leaked details")
		}
	case <-time.After(time.Second):
		transport.CancelRequest(nil)
		t.Fatal("phase removed the http.Client legacy cancellation fallback")
	}
}

func TestExchangeKeepsSeparateBudgetsForSequentialNetworkRequests(t *testing.T) {
	f := newTestIDP(t, idpOptions{})
	const budget = 2 * time.Second
	const delay = 800 * time.Millisecond
	var reached atomic.Int32
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" || r.URL.Path == "/jwks" || r.URL.Path == "/userinfo" {
			reached.Add(1)
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
		}
		original.ServeHTTP(w, r)
	})
	f.client.Timeout = budget
	p := f.provider(t)
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	identity, err := p.Exchange(ctx, "authorization-code", testNonce, testVerifier)
	if err != nil || identity.Subject != "stable-subject" || reached.Load() != 3 || time.Since(start) <= budget {
		t.Fatal("independent request budgets became one total Exchange timeout", err, reached.Load())
	}
}

func TestActiveCallerProviderFailuresNeverBecomeDeadlines(t *testing.T) {
	paths := map[string]string{"discovery": "/.well-known/openid-configuration", "token": "/token", "jwks": "/jwks", "userinfo": "/userinfo"}
	for _, phase := range []string{"discovery", "token", "jwks", "userinfo"} {
		for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
			t.Run(phase+"/"+http.StatusText(status), func(t *testing.T) {
				f := newTestIDP(t, idpOptions{})
				var hits atomic.Int32
				original := f.server.Config.Handler
				f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == paths[phase] {
						hits.Add(1)
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						_, _ = io.WriteString(w, `{"private-provider-bad":`)
						return
					}
					original.ServeHTTP(w, r)
				})
				f.client.Timeout = 2 * time.Second
				p := f.provider(t)
				if phase != "discovery" {
					if err := p.Prepare(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				identity, err := p.Exchange(ctx, "authorization-code", testNonce, testVerifier)
				if err != ErrUnavailable || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || identity != (Identity{}) || hits.Load() != 1 || strings.Contains(err.Error(), "private-provider-bad") {
					t.Fatal("active caller HTTP/JSON failure changed classification, retried, or leaked data")
				}
			})
		}
	}
}
