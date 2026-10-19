package oidcprovider

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

const (
	testVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	testNonce    = "nonce-for-this-attempt"
	testClientID = "public-desktop-client"
	testRedirect = "https://api.example.test/api/oidc/callback"
)

type idpOptions struct {
	claims         func(jwt.MapClaims)
	metadata       func(map[string]interface{})
	profile        func(map[string]interface{})
	omitIDToken    bool
	badSignature   bool
	tokenStatus    int
	tokenError     string
	userInfoStatus int
	keysStatus     int
	block          string
	tokenRedirect  bool
}

type tokenRequest struct {
	method, authorization string
	form                  url.Values
}

type testIDP struct {
	server        *httptest.Server
	client        *http.Client
	tokenRequests chan tokenRequest
	entered       chan struct{}
	discoveries   atomic.Int32
	userInfos     atomic.Int32
	redirects     atomic.Int32
}

func newTestIDP(t *testing.T, options idpOptions) *testIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &testIDP{tokenRequests: make(chan tokenRequest, 32), entered: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	blocked := func(w http.ResponseWriter, r *http.Request, phase string) bool {
		if options.block != phase {
			return false
		}
		select {
		case fixture.entered <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		return true
	}
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fixture.discoveries.Add(1)
		if blocked(w, r, "discovery") {
			return
		}
		info := map[string]interface{}{
			"issuer": fixture.server.URL, "authorization_endpoint": fixture.server.URL + "/authorize",
			"token_endpoint": fixture.server.URL + "/token", "userinfo_endpoint": fixture.server.URL + "/userinfo",
			"jwks_uri": fixture.server.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
			"token_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"},
		}
		if options.metadata != nil {
			options.metadata(info)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		fixture.tokenRequests <- tokenRequest{method: r.Method, authorization: r.Header.Get("Authorization"), form: r.PostForm}
		if blocked(w, r, "token") {
			return
		}
		if options.tokenRedirect {
			http.Redirect(w, r, fixture.server.URL+"/unexpected-redirect", http.StatusTemporaryRedirect)
			return
		}
		if options.tokenStatus != 0 {
			if options.tokenError != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(options.tokenStatus)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": options.tokenError, "error_description": "private-provider-error-body"})
				return
			}
			w.WriteHeader(options.tokenStatus)
			_, _ = w.Write([]byte("private-provider-error-body"))
			return
		}
		claims := jwt.MapClaims{
			"iss": fixture.server.URL, "sub": "stable-subject", "aud": testClientID,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "nonce": testNonce,
		}
		if options.claims != nil {
			options.claims(claims)
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "provider-key"
		signed, signErr := token.SignedString(key)
		if signErr != nil {
			t.Error(signErr)
			w.WriteHeader(500)
			return
		}
		if options.badSignature {
			parts := strings.Split(signed, ".")
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			sig[0] ^= 1
			parts[2] = base64.RawURLEncoding.EncodeToString(sig)
			signed = strings.Join(parts, ".")
		}
		response := map[string]interface{}{"access_token": "opaque-access-token", "token_type": "Bearer",
			"expires_in": 60, "refresh_token": "never-use-this-refresh-token"}
		if !options.omitIDToken {
			response["id_token"] = signed
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		if blocked(w, r, "jwks") {
			return
		}
		if options.keysStatus != 0 {
			w.WriteHeader(options.keysStatus)
			_, _ = w.Write([]byte("private-provider-jwks-error"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey,
			KeyID: "provider-key", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		fixture.userInfos.Add(1)
		if r.Header.Get("Authorization") != "Bearer opaque-access-token" {
			t.Error("UserInfo did not use the exchanged access token")
		}
		if blocked(w, r, "userinfo") {
			return
		}
		if options.userInfoStatus != 0 {
			w.WriteHeader(options.userInfoStatus)
			_, _ = w.Write([]byte("private-provider-userinfo-error"))
			return
		}
		profile := map[string]interface{}{"sub": "stable-subject", "preferred_username": "external-user", "name": "External User",
			"picture": "https://images.example.test/avatar", "is_admin": true, "roles": []string{"admin"}}
		if options.profile != nil {
			options.profile(profile)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(profile)
	})
	mux.HandleFunc("/unexpected-redirect", func(w http.ResponseWriter, r *http.Request) {
		fixture.redirects.Add(1)
		w.WriteHeader(500)
	})
	fixture.server = httptest.NewTLSServer(mux)
	fixture.client = fixture.server.Client()
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *testIDP) provider(t *testing.T) *Provider {
	t.Helper()
	p, err := New(Config{Issuer: f.server.URL, ClientID: testClientID, RedirectURL: testRedirect, HTTPClient: f.client})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPublicAuthorizationAndExchangeOverTLS(t *testing.T) {
	f := newTestIDP(t, idpOptions{})
	p := f.provider(t)
	if f.discoveries.Load() != 0 || f.client.Timeout != 0 {
		t.Fatal("constructor performed discovery or mutated caller's HTTP client")
	}
	authorization, err := p.AuthorizationURL(context.Background(), "state-for-attempt", testNonce, testVerifier)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(authorization)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for key, want := range map[string]string{"client_id": testClientID, "redirect_uri": testRedirect, "response_type": "code",
		"scope": "openid profile", "state": "state-for-attempt", "nonce": testNonce, "code_challenge_method": "S256",
		"code_challenge": "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"} {
		if q.Get(key) != want {
			t.Fatalf("authorization %s = %q, want %q", key, q.Get(key), want)
		}
	}
	for _, forbidden := range []string{"client_secret", "code_verifier", "access_type", "refresh_token"} {
		if q.Has(forbidden) {
			t.Fatalf("authorization includes forbidden %s", forbidden)
		}
	}
	identity, err := p.Exchange(context.Background(), "authorization-code", testNonce, testVerifier)
	if err != nil {
		t.Fatal(err)
	}
	if identity != (Identity{Issuer: f.server.URL, Subject: "stable-subject", Username: "external-user", Name: "External User",
		Picture: "https://images.example.test/avatar"}) {
		t.Fatalf("unexpected identity (email is optional): %#v", identity)
	}
	assertPublicTokenRequest(t, <-f.tokenRequests)
	if len(f.tokenRequests) != 0 || f.userInfos.Load() != 1 || f.discoveries.Load() != 1 {
		t.Fatal("unexpected refresh, UserInfo or discovery requests")
	}
}

func assertPublicTokenRequest(t *testing.T, request tokenRequest) {
	t.Helper()
	if request.method != http.MethodPost || request.authorization != "" {
		t.Fatalf("PUBLIC client used unexpected authentication: %#v", request)
	}
	for key, want := range map[string]string{"grant_type": "authorization_code", "code": "authorization-code",
		"client_id": testClientID, "redirect_uri": testRedirect, "code_verifier": testVerifier} {
		if request.form.Get(key) != want {
			t.Fatalf("token %s = %q, want %q", key, request.form.Get(key), want)
		}
	}
	if len(request.form) != 5 || request.form.Has("client_secret") {
		t.Fatal("unexpected token parameters or client secret")
	}
}

func TestPublicRegistrationDoesNotNegotiateDiscoveryAuthenticationMethods(t *testing.T) {
	for _, tc := range []struct {
		name    string
		methods []string
	}{
		{"explicit none", []string{"none"}},
		{"omitted", nil},
		{"Basic advertised", []string{"client_secret_basic"}},
		{"confidential methods advertised", []string{"client_secret_basic", "client_secret_post", "client_secret_jwt",
			"private_key_jwt", "tls_client_auth", "self_signed_tls_client_auth"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestIDP(t, idpOptions{metadata: func(m map[string]interface{}) {
				if tc.methods == nil {
					delete(m, "token_endpoint_auth_methods_supported")
				} else {
					m["token_endpoint_auth_methods_supported"] = tc.methods
				}
			}})
			p := f.provider(t)
			authorization, err := p.AuthorizationURL(context.Background(), "state-for-attempt", testNonce, testVerifier)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(authorization)
			if err != nil || u.Query().Get("code_challenge_method") != "S256" ||
				u.Query().Get("code_challenge") != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
				t.Fatal("discovery changed S256 authorization")
			}
			identity, err := p.Exchange(context.Background(), "authorization-code", testNonce, testVerifier)
			if err != nil || identity.Issuer != f.server.URL || identity.Subject != "stable-subject" {
				t.Fatalf("registered PUBLIC exchange failed: %#v, %v", identity, err)
			}
			if len(f.tokenRequests) != 1 || f.userInfos.Load() != 1 || f.discoveries.Load() != 1 {
				t.Fatal("unexpected token, UserInfo or discovery request count")
			}
			assertPublicTokenRequest(t, <-f.tokenRequests)
		})
	}
}

func TestInvalidClientNeverFallsBackToAnotherAuthenticationMethod(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newTestIDP(t, idpOptions{
				metadata: func(m map[string]interface{}) {
					m["token_endpoint_auth_methods_supported"] = []string{"client_secret_basic", "client_secret_post"}
				},
				tokenStatus: status, tokenError: "invalid_client",
			})
			identity, err := f.provider(t).Exchange(context.Background(), "authorization-code", testNonce, testVerifier)
			if err != ErrUnavailable || identity != (Identity{}) {
				t.Fatalf("token rejection leaked details or returned an identity: %#v, %v", identity, err)
			}
			if len(f.tokenRequests) != 1 || f.userInfos.Load() != 0 {
				t.Fatal("invalid_client retried token authentication or requested UserInfo")
			}
			assertPublicTokenRequest(t, <-f.tokenRequests)
		})
	}
}

func TestRejectsUnverifiedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options idpOptions
	}{
		{"missing ID token", idpOptions{omitIDToken: true}},
		{"wrong nonce", idpOptions{claims: func(c jwt.MapClaims) { c["nonce"] = "another-attempt" }}},
		{"missing nonce", idpOptions{claims: func(c jwt.MapClaims) { delete(c, "nonce") }}},
		{"wrong issuer", idpOptions{claims: func(c jwt.MapClaims) { c["iss"] = "https://another-issuer.test" }}},
		{"wrong audience", idpOptions{claims: func(c jwt.MapClaims) { c["aud"] = "another-client" }}},
		{"expired", idpOptions{claims: func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }}},
		{"missing expiry", idpOptions{claims: func(c jwt.MapClaims) { delete(c, "exp") }}},
		{"future issued at", idpOptions{claims: func(c jwt.MapClaims) { c["iat"] = time.Now().Add(time.Minute).Unix() }}},
		{"missing issued at", idpOptions{claims: func(c jwt.MapClaims) { delete(c, "iat") }}},
		{"string issued at", idpOptions{claims: func(c jwt.MapClaims) { c["iat"] = "1700000000" }}},
		{"string expiry", idpOptions{claims: func(c jwt.MapClaims) { c["exp"] = "9999999999" }}},
		{"empty subject", idpOptions{claims: func(c jwt.MapClaims) { c["sub"] = "" }}},
		{"bad signature", idpOptions{badSignature: true}},
		{"multiple audiences without azp", idpOptions{claims: func(c jwt.MapClaims) { c["aud"] = []string{testClientID, "another-client"} }}},
		{"wrong azp", idpOptions{claims: func(c jwt.MapClaims) {
			c["aud"] = []string{testClientID, "another-client"}
			c["azp"] = "another-client"
		}}},
		{"wrong single audience azp", idpOptions{claims: func(c jwt.MapClaims) { c["azp"] = "another-client" }}},
		{"UserInfo subject mismatch", idpOptions{profile: func(c map[string]interface{}) { c["sub"] = "other-subject" }}},
		{"missing UserInfo subject", idpOptions{profile: func(c map[string]interface{}) { delete(c, "sub") }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestIDP(t, tc.options)
			identity, err := f.provider(t).Exchange(context.Background(), "authorization-code", testNonce, testVerifier)
			if !errors.Is(err, ErrInvalidIdentity) || identity != (Identity{}) {
				t.Fatalf("invalid identity accepted or misclassified: %#v, %v", identity, err)
			}
		})
	}
}

func TestMultipleAudiencesRequireMatchingAuthorizedParty(t *testing.T) {
	f := newTestIDP(t, idpOptions{claims: func(c jwt.MapClaims) { c["aud"] = []string{testClientID, "another-client"}; c["azp"] = testClientID }})
	if _, err := f.provider(t).Exchange(context.Background(), "authorization-code", testNonce, testVerifier); err != nil {
		t.Fatal(err)
	}
}

func TestDependencyFailuresAreSanitizedAndNeverIdentities(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options idpOptions
	}{
		{"token", idpOptions{tokenStatus: 400}},
		{"JWKS", idpOptions{keysStatus: 503}},
		{"UserInfo", idpOptions{userInfoStatus: 401}},
		{"redirect", idpOptions{tokenRedirect: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestIDP(t, tc.options)
			identity, err := f.provider(t).Exchange(context.Background(), "authorization-code", testNonce, testVerifier)
			if !errors.Is(err, ErrUnavailable) || identity != (Identity{}) || strings.Contains(err.Error(), "private-provider") {
				t.Fatalf("provider failure leaked or misclassified: %#v, %v", identity, err)
			}
			if len(f.tokenRequests) != 1 || f.redirects.Load() != 0 {
				t.Fatal("token request retried or redirected with credentials")
			}
		})
	}
}

func TestCancellationAndHTTPTimeoutAtEveryNetworkPhase(t *testing.T) {
	for _, phase := range []string{"discovery", "token", "jwks", "userinfo"} {
		for _, cancelRequest := range []bool{true, false} {
			name := phase + "/client-timeout"
			if cancelRequest {
				name = phase + "/cancel"
			}
			t.Run(name, func(t *testing.T) {
				f := newTestIDP(t, idpOptions{block: phase})
				if !cancelRequest {
					f.client.Timeout = 60 * time.Millisecond
				}
				p := f.provider(t)
				if phase != "discovery" {
					if err := p.Prepare(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				result := make(chan error, 1)
				start := time.Now()
				go func() { _, err := p.Exchange(ctx, "authorization-code", testNonce, testVerifier); result <- err }()
				select {
				case <-f.entered:
				case <-time.After(time.Second):
					t.Fatal("network phase not reached")
				}
				want := context.DeadlineExceeded
				if cancelRequest {
					cancel()
					want = context.Canceled
				}
				select {
				case err := <-result:
					if !errors.Is(err, ErrUnavailable) || !errors.Is(err, want) || time.Since(start) > time.Second {
						t.Fatalf("network request not promptly bounded: %v after %v", err, time.Since(start))
					}
				case <-time.After(time.Second):
					t.Fatal("network call ignored cancellation or HTTP client timeout")
				}
			})
		}
	}
}

func TestConfigurationAndPKCECannotDowngrade(t *testing.T) {
	for _, config := range []Config{
		{Issuer: "http://issuer.test", ClientID: testClientID, RedirectURL: testRedirect},
		{Issuer: "https://issuer.test", ClientID: testClientID, RedirectURL: "http://api.test/callback"},
		{Issuer: "https://user:secret@issuer.test", ClientID: testClientID, RedirectURL: testRedirect},
		{Issuer: "https://issuer.test?code=secret", ClientID: testClientID, RedirectURL: testRedirect},
		{Issuer: "https://issuer.test", ClientID: "", RedirectURL: testRedirect},
	} {
		if _, err := New(config); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("insecure configuration accepted: %v", err)
		}
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]interface{})
	}{
		{"plain only", func(m map[string]interface{}) { m["code_challenge_methods_supported"] = []string{"plain"} }},
		{"malformed authentication methods", func(m map[string]interface{}) {
			m["token_endpoint_auth_methods_supported"] = "client_secret_basic"
		}},
		{"unsigned", func(m map[string]interface{}) { m["id_token_signing_alg_values_supported"] = []string{"none"} }},
		{"symmetric", func(m map[string]interface{}) { m["id_token_signing_alg_values_supported"] = []string{"HS256"} }},
		{"HTTP token endpoint", func(m map[string]interface{}) { m["token_endpoint"] = "http://issuer.test/token" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestIDP(t, idpOptions{metadata: tc.change})
			if err := f.provider(t).Prepare(context.Background()); !errors.Is(err, ErrConfiguration) {
				t.Fatalf("provider downgrade accepted: %v", err)
			}
		})
	}
	f := newTestIDP(t, idpOptions{})
	p := f.provider(t)
	for _, verifier := range []string{"", "short", strings.Repeat("a", 129), strings.Repeat("a", 42) + "!"} {
		if _, err := p.AuthorizationURL(context.Background(), "state", testNonce, verifier); !errors.Is(err, ErrInput) {
			t.Fatal("invalid PKCE verifier accepted")
		}
	}
	if f.discoveries.Load() != 0 {
		t.Fatal("invalid PKCE input contacted provider")
	}
}

func TestConcurrentPreparationHasNoSharedRequestContext(t *testing.T) {
	f := newTestIDP(t, idpOptions{})
	p := f.provider(t)
	var callers sync.WaitGroup
	for i := 0; i < 4; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			if err := p.Prepare(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	callers.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Prepare(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("prepared provider ignored canceled caller: %v", err)
	}
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatalf("another caller's cancellation poisoned provider: %v", err)
	}
}
