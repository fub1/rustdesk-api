package oidcprovider

import (
	"context"
	"errors"
	"io"
	"net/http"
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

// net/http deliberately preserves EOF even after Client.Timeout expires;
// cancelTimerBody.Read does not wrap EOF as a typed timeout. Model that allowed
// response-body outcome directly, rather than claiming the standard library
// must always return net.Error. Production owns the request context deadline.
type eofAtDeadlineBody struct {
	ctx    context.Context
	prefix *strings.Reader
}

func (b *eofAtDeadlineBody) Read(p []byte) (int, error) {
	if b.prefix.Len() != 0 {
		return b.prefix.Read(p)
	}
	<-b.ctx.Done()
	return 0, io.EOF
}
func (*eofAtDeadlineBody) Close() error { return nil }

type eofAtDeadlineTransport struct {
	base           http.RoundTripper
	path           string
	reached        atomic.Bool
	requestContext context.Context
}

func (tr *eofAtDeadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != tr.path {
		return tr.base.RoundTrip(req)
	}
	tr.reached.Store(true)
	tr.requestContext = req.Context()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, ContentLength: -1, Request: req,
		Body: &eofAtDeadlineBody{ctx: req.Context(), prefix: strings.NewReader(`{"private-provider-eof":`)}}, nil
}
func TestBodyEOFAtClientDeadlineHasNoTypedCauseBeforeSDK(t *testing.T) {
	tr := &eofAtDeadlineTransport{path: "/"}
	client := &http.Client{Timeout: 60 * time.Millisecond, Transport: tr}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://provider.example.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal("fixture did not return successful headers", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || string(data) != `{"private-provider-eof":` || !tr.reached.Load() || !errors.Is(tr.requestContext.Err(), context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("EOF control did not establish an untyped body outcome after request deadline: body-read-success=%t request-expired=%t caller-active=%t", err == nil, errors.Is(tr.requestContext.Err(), context.DeadlineExceeded), ctx.Err() == nil)
	}
}
func TestPhaseDeadlineSurvivesResponseBodyEOFAtEveryNetworkPhase(t *testing.T) {
	paths := map[string]string{"discovery": "/.well-known/openid-configuration", "token": "/token", "jwks": "/jwks", "userinfo": "/userinfo"}
	for _, phase := range []string{"discovery", "token", "jwks", "userinfo"} {
		t.Run(phase, func(t *testing.T) {
			f := newTestIDP(t, idpOptions{})
			transport := &eofAtDeadlineTransport{base: f.client.Transport, path: paths[phase]}
			f.client.Transport = transport
			f.client.Timeout = 60 * time.Millisecond
			provider := f.provider(t)
			if phase != "discovery" {
				if err := provider.Prepare(context.Background()); err != nil {
					t.Fatal("prepare", err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			identity, err := provider.Exchange(ctx, "authorization-code", testNonce, testVerifier)
			if !transport.reached.Load() || transport.requestContext == nil || !errors.Is(transport.requestContext.Err(), context.DeadlineExceeded) {
				t.Fatal("EOF response did not follow the actual request deadline")
			}
			if identity != (Identity{}) || !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || strings.Contains(err.Error(), "private-provider-eof") {
				t.Fatalf("phase lost EOF deadline or leaked data: deadline=%t unavailable=%t caller-active=%t identity-empty=%t", errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrUnavailable), ctx.Err() == nil, identity == (Identity{}))
			}
		})
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
