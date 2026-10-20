package http

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/utils"
)

const routeOIDCClientID = "rustdesk-public-client"
const routeOIDCSubject = "550e8400-e29b-41d4-a716-446655440000"
const routeOIDCDevice = "123456789"
const routeOIDCUUID = "native-client-uuid"

type routeAuthorization struct{ nonce, challenge string }
type routeIDP struct {
	server    *httptest.Server
	mu        sync.Mutex
	grants    map[string]routeAuthorization
	tokens    atomic.Int32
	userInfos atomic.Int32
}

// This TLS IdP supplies real Discovery, RSA signatures/JWKS and UserInfo. It
// rejects confidential-client authentication and a missing/wrong PKCE verifier.
func newRouteIDP(t *testing.T) *routeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &routeIDP{grants: make(map[string]routeAuthorization)}
	mux := stdhttp.NewServeMux()
	write := func(w stdhttp.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/.well-known/openid-configuration", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		write(w, map[string]any{"issuer": idp.server.URL, "authorization_endpoint": idp.server.URL + "/authorize", "token_endpoint": idp.server.URL + "/token", "userinfo_endpoint": idp.server.URL + "/userinfo", "jwks_uri": idp.server.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}, "token_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}})
	})
	mux.HandleFunc("/token", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		idp.tokens.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if r.Method != stdhttp.MethodPost || r.Header.Get("Authorization") != "" || r.PostForm.Has("client_secret") || r.PostForm.Get("client_id") != routeOIDCClientID || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != "https://api.example.test/api/oidc/callback" {
			t.Error("public token request used incorrect client authentication or callback")
			w.WriteHeader(400)
			return
		}
		idp.mu.Lock()
		grant, ok := idp.grants[r.PostForm.Get("code")]
		idp.mu.Unlock()
		verifier := r.PostForm.Get("code_verifier")
		challenge := sha256.Sum256([]byte(verifier))
		if !ok || len(verifier) < 43 || base64.RawURLEncoding.EncodeToString(challenge[:]) != grant.challenge {
			t.Error("token exchange did not bind S256 verifier to authorization request")
			w.WriteHeader(400)
			return
		}
		claims := jwt.MapClaims{"iss": idp.server.URL, "sub": routeOIDCSubject, "aud": routeOIDCClientID, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "nonce": grant.nonce}
		if r.PostForm.Get("code") == "bad-nonce" {
			claims["nonce"] = "different-nonce"
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "route-idp-rsa"
		signed, err := token.SignedString(key)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		write(w, map[string]any{"access_token": "idp-access-token-private", "token_type": "Bearer", "expires_in": 60, "id_token": signed})
	})
	mux.HandleFunc("/jwks", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		write(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "route-idp-rsa", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/userinfo", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		idp.userInfos.Add(1)
		if r.Header.Get("Authorization") != "Bearer idp-access-token-private" {
			t.Error("UserInfo did not use provider access token")
			w.WriteHeader(401)
			return
		}
		// Deliberately collide with an existing local administrator and add roles.
		// Neither profile attributes nor these claims may choose/elevate an account.
		write(w, map[string]any{"sub": routeOIDCSubject, "preferred_username": "admin", "email": "admin@example.test", "name": "External User", "is_admin": true, "roles": []string{"admin"}})
	})
	idp.server = httptest.NewTLSServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *routeIDP) register(t *testing.T, rawURL, code string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "https" || u.Host != strings.TrimPrefix(idp.server.URL, "https://") || u.Path != "/authorize" || q.Get("client_id") != routeOIDCClientID || q.Get("redirect_uri") != "https://api.example.test/api/oidc/callback" || q.Get("response_type") != "code" || q.Get("scope") != "openid profile" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge") == "" || q.Has("client_secret") || q.Has("code_verifier") {
		t.Fatalf("incorrect public authorization request: %s", rawURL)
	}
	idp.mu.Lock()
	idp.grants[code] = routeAuthorization{nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	idp.mu.Unlock()
	return q.Get("state")
}

type routeOIDCFixture struct {
	handler stdhttp.Handler
	idp     *routeIDP
	admin   auth.Principal
}

func newRouteOIDCFixture(t *testing.T) *routeOIDCFixture {
	t.Helper()
	installLegacyFixture(t)
	// Match the legacy schema's unbound peer user_id=0 semantics; explicit
	// identity FK/uniqueness checks are exercised by identitystore's DB suite.
	global.DB.Config.DisableForeignKeyConstraintWhenMigrating = true
	if err := global.DB.AutoMigrate(&model.User{}, &model.UserToken{}, &model.LoginLog{}, &model.Peer{}, &model.Oauth{}, &model.OIDCIdentity{}, &model.OperationLog{}); err != nil {
		t.Fatal(err)
	}
	// Use the runtime schema for current-model peer writes.
	if err := database.Update(global.DB, 281, nil); err != nil {
		t.Fatal(err)
	}
	pool, err := global.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	idp := newRouteIDP(t)
	yes := true
	provider := model.Oauth{Op: "ours", OauthType: model.OauthTypeOidc, Issuer: idp.server.URL, ClientId: routeOIDCClientID, AutoRegister: &yes, PkceEnable: &yes, PkceMethod: model.PKCEMethodS256, Scopes: "openid profile"}
	if err := global.DB.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	if err := global.DB.Create(&model.Peer{Id: routeOIDCDevice, Uuid: routeOIDCUUID}).Error; err != nil {
		t.Fatal(err)
	}
	hash, err := utils.EncryptPassword("custom-secret-password")
	if err != nil {
		t.Fatal(err)
	}
	admin := model.User{Username: "admin", Email: "admin@example.test", Password: hash, IsAdmin: &yes, Status: model.COMMON_STATUS_ENABLE}
	if err := global.DB.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	sessions, err := app.NewSessions(global.DB, time.Hour, global.Jwt.Key)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := app.NewIssuer(global.DB, time.Hour, global.Jwt.Key)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := issuer.Issue(context.Background(), auth.IssueRequest{UserID: admin.Id, Client: "webadmin", Method: "account"})
	if err != nil {
		t.Fatal(err)
	}
	oidc, err := app.NewOIDC(global.DB, time.Hour, global.Jwt.Key, sessions, "https://api.example.test", true, idp.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	authHTTP, err := httpapi.New(sessions, httpapi.Options{Translate: LegacyTranslator(), OIDC: oidc})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(authHTTP)
	if err != nil {
		t.Fatal(err)
	}
	return &routeOIDCFixture{handler, idp, principal}
}

func (f *routeOIDCFixture) call(method, path, body, token string, admin bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		if admin {
			r.Header.Set("api-token", token)
		} else {
			r.Header.Set("Authorization", "Bearer "+token)
		}
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

type routeOIDCStarted struct{ Code, URL string }

func (f *routeOIDCFixture) begin(t *testing.T, admin bool, op string) routeOIDCStarted {
	t.Helper()
	path := "/api/oidc/auth"
	if admin {
		path = "/api/admin/oidc/auth"
	}
	w := f.call("POST", path, fmt.Sprintf(`{"op":%q,"id":%q,"uuid":%q,"deviceInfo":{"os":"Windows","type":"webadmin"}}`, op, routeOIDCDevice, routeOIDCUUID), "", false)
	var a routeOIDCStarted
	if admin {
		var envelope struct {
			Code int
			Data routeOIDCStarted
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Code != 0 {
			t.Fatalf("admin begin: HTTP %d %s", w.Code, w.Body.String())
		}
		a = envelope.Data
	} else if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || a.Code == "" || a.URL == "" {
		t.Fatalf("begin: HTTP %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("login attempt response lacks credential cache/referrer protection")
	}
	return a
}
func (f *routeOIDCFixture) query(code string, admin bool) *httptest.ResponseRecorder {
	path := "/api/oidc/auth-query"
	if admin {
		path = "/api/admin/oidc/auth-query"
	}
	return f.call("GET", path+"?"+url.Values{"code": {code}, "id": {routeOIDCDevice}, "uuid": {routeOIDCUUID}}.Encode(), "", "", false)
}
func routeCount(t *testing.T, row any) int64 {
	t.Helper()
	var n int64
	if err := global.DB.Model(row).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestProductionOIDCRoutesPublicPKCEAndLocalSession(t *testing.T) {
	f := newRouteOIDCFixture(t)
	a := f.begin(t, false, "ours")
	state := f.idp.register(t, a.URL, "valid-code")
	if state == a.Code {
		t.Fatal("provider state doubles as poll credential")
	}
	pending := f.query(a.Code, false)
	var pendingBody struct{ Error string }
	if json.Unmarshal(pending.Body.Bytes(), &pendingBody) != nil || pending.Code != 200 || pendingBody.Error != "No authed oidc is found" {
		t.Fatalf("pending native contract: %d %s", pending.Code, pending.Body.String())
	}
	for _, suffix := range []string{"confirm", "bindConfirm"} {
		w := f.call("POST", "/api/admin/oauth/"+suffix, fmt.Sprintf(`{"code":%q}`, a.Code), f.admin.Session.Token, true)
		var envelope struct{ Code int }
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Code == 0 {
			t.Fatalf("legacy %s bypassed OIDC: %s", suffix, w.Body.String())
		}
	}
	w := f.call("GET", "/api/oidc/callback?"+url.Values{"state": {state}, "code": {"valid-code"}}.Encode(), "", "", false)
	if w.Code != 200 {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	if f.idp.tokens.Load() != 1 || f.idp.userInfos.Load() != 1 {
		t.Fatal("callback skipped real token/UserInfo exchange")
	}
	// A duplicate callback must never exchange an already consumed code again.
	f.call("GET", "/api/oidc/callback?"+url.Values{"state": {state}, "code": {"valid-code"}}.Encode(), "", "", false)
	if f.idp.tokens.Load() != 1 {
		t.Fatal("duplicate callback re-exchanged provider code")
	}
	if cross := f.query(a.Code, true); strings.Contains(cross.Body.String(), `"access_token"`) || strings.Contains(cross.Body.String(), `"token"`) {
		t.Fatalf("native attempt crossed admin route: %s", cross.Body.String())
	}
	response := f.query(a.Code, false)
	var login struct {
		AccessToken string `json:"access_token"`
		User        struct {
			Name    string `json:"name"`
			IsAdmin bool   `json:"is_admin"`
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &login); err != nil || response.Code != 200 || login.AccessToken == "" || login.User.Name == "" {
		t.Fatalf("native poll: %d %s", response.Code, response.Body.String())
	}
	if login.AccessToken == "idp-access-token-private" || login.User.IsAdmin {
		t.Fatal("provider credential/role used as local session")
	}
	var link model.OIDCIdentity
	if err := global.DB.First(&link).Error; err != nil {
		t.Fatal(err)
	}
	if link.Issuer != f.idp.server.URL || link.Subject != routeOIDCSubject || link.UserID == f.admin.User.ID {
		t.Fatal("issuer/sub identity linked by profile email/name")
	}
	if routeCount(t, &model.OIDCIdentity{}) != 1 || routeCount(t, &model.UserToken{}) != 2 || routeCount(t, &model.LoginLog{}) != 2 {
		t.Fatal("login did not persist exactly one local session/log")
	}
	if repeat := f.query(a.Code, false); !strings.Contains(repeat.Body.String(), login.AccessToken) {
		t.Fatal("lost response did not recover same local token")
	}
	current := f.call("POST", "/api/currentUser", "{}", login.AccessToken, false)
	if current.Code != 200 || !strings.Contains(current.Body.String(), login.User.Name) {
		t.Fatalf("local token currentUser: %d %s", current.Code, current.Body.String())
	}
	logout := f.call("POST", "/api/logout", "{}", login.AccessToken, false)
	if logout.Code != 200 {
		t.Fatalf("logout: %d %s", logout.Code, logout.Body.String())
	}
	if stale := f.query(a.Code, false); stale.Code == 200 && strings.Contains(stale.Body.String(), login.AccessToken) {
		t.Fatal("cached poll returned revoked token")
	}
	if current := f.call("POST", "/api/currentUser", "{}", login.AccessToken, false); current.Code != 401 {
		t.Fatalf("logout left local token valid: %d %s", current.Code, current.Body.String())
	}
}

func TestProductionOIDCBadCallbackIsTerminal(t *testing.T) {
	f := newRouteOIDCFixture(t)
	a := f.begin(t, false, "ours")
	state := f.idp.register(t, a.URL, "bad-nonce")
	w := f.call("GET", "/api/oidc/callback?"+url.Values{"state": {state}, "code": {"bad-nonce"}}.Encode(), "", "", false)
	if strings.Contains(w.Body.String(), "idp-access-token-private") {
		t.Fatal("callback leaked provider token")
	}
	if f.idp.tokens.Load() != 1 || f.idp.userInfos.Load() != 0 {
		t.Fatal("bad nonce was not rejected before UserInfo")
	}
	f.call("GET", "/api/oidc/callback?"+url.Values{"state": {state}, "code": {"bad-nonce"}}.Encode(), "", "", false)
	if f.idp.tokens.Load() != 1 {
		t.Fatal("failed callback could repeat exchange")
	}
	if poll := f.query(a.Code, false); strings.Contains(poll.Body.String(), `"access_token"`) {
		t.Fatalf("bad callback issued local session: %s", poll.Body.String())
	}
	if routeCount(t, &model.OIDCIdentity{}) != 0 || routeCount(t, &model.UserToken{}) != 1 || routeCount(t, &model.LoginLog{}) != 1 {
		t.Fatal("bad callback mutated local admission")
	}
}

func TestProductionOIDCAdminEnvelopeAndRouteAudience(t *testing.T) {
	f := newRouteOIDCFixture(t)
	a := f.begin(t, true, "ours")
	state := f.idp.register(t, a.URL, "admin-code")
	pending := f.query(a.Code, true)
	var pendingBody struct{ Error string }
	if json.Unmarshal(pending.Body.Bytes(), &pendingBody) != nil || pendingBody.Error != "No authed oidc is found" {
		t.Fatalf("admin pending compatibility: %s", pending.Body.String())
	}
	f.call("GET", "/api/oidc/callback?"+url.Values{"state": {state}, "code": {"admin-code"}}.Encode(), "", "", false)
	if cross := f.query(a.Code, false); strings.Contains(cross.Body.String(), `"access_token"`) {
		t.Fatalf("admin attempt crossed client route: %s", cross.Body.String())
	}
	w := f.query(a.Code, true)
	var response struct {
		Code int
		Data struct {
			Token      string
			Username   string
			RouteNames []string `json:"route_names"`
		}
	}
	if json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Code != 0 || response.Data.Token == "" || response.Data.Username == "" {
		t.Fatalf("admin login envelope: %d %s", w.Code, w.Body.String())
	}
	for _, route := range response.Data.RouteNames {
		if route == "*" {
			t.Fatal("IdP role claim granted administrator routes")
		}
	}
	current := f.call("GET", "/api/admin/user/current", "", response.Data.Token, true)
	if !strings.Contains(current.Body.String(), response.Data.Username) {
		t.Fatalf("admin self-management session invalid: %s", current.Body.String())
	}
	sensitive := f.call("GET", "/api/admin/user/list", "", response.Data.Token, true)
	var forbidden struct{ Code int }
	if json.Unmarshal(sensitive.Body.Bytes(), &forbidden) != nil || forbidden.Code != 403 {
		t.Fatalf("OIDC ordinary account gained administration: %s", sensitive.Body.String())
	}
	logout := f.call("POST", "/api/admin/logout", "{}", response.Data.Token, true)
	var result struct{ Code int }
	if json.Unmarshal(logout.Body.Bytes(), &result) != nil || result.Code != 0 {
		t.Fatalf("admin logout: %s", logout.Body.String())
	}
	if stale := f.query(a.Code, true); strings.Contains(stale.Body.String(), response.Data.Token) {
		t.Fatal("admin cached poll returned revoked token")
	}
}

func assertSafeOIDCInfo(t *testing.T, body []byte) {
	t.Helper()
	var envelope struct {
		Code int
		Data map[string]json.RawMessage
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Code != 0 || envelope.Data == nil {
		t.Fatalf("approval metadata: %s", body)
	}
	allowed := map[string]bool{"id": true, "uuid": true, "device_os": true, "device_type": true, "action": true, "status": true}
	for key := range envelope.Data {
		if !allowed[key] {
			t.Fatalf("approval metadata exposes unexpected field %q: %s", key, body)
		}
	}
}

func TestProductionOIDCBindingInfoIsSanitizedAndRequiresInitiator(t *testing.T) {
	f := newRouteOIDCFixture(t)
	w := f.call("POST", "/api/admin/oauth/bind", `{"op":"ours","id":"bind-device","uuid":"bind-uuid","deviceInfo":{"os":"Windows"}}`, f.admin.Session.Token, true)
	var started struct {
		Code int
		Data routeOIDCStarted
	}
	if json.Unmarshal(w.Body.Bytes(), &started) != nil || started.Code != 0 || started.Data.Code == "" {
		t.Fatalf("bind begin: %s", w.Body.String())
	}
	state := f.idp.register(t, started.Data.URL, "bind-code")
	infoPath := "/api/admin/oauth/info?code=" + url.QueryEscape(started.Data.Code)
	if anonymous := f.call("GET", infoPath, "", "", true); strings.Contains(anonymous.Body.String(), "bind-device") {
		t.Fatal("anonymous request read binding metadata")
	}
	info := f.call("GET", infoPath, "", f.admin.Session.Token, true)
	assertSafeOIDCInfo(t, info.Body.Bytes())
	// Even the same local actor's other authenticated session does not own the
	// initiating session's bind attempt.
	issuer, err := app.NewIssuer(global.DB, time.Hour, global.Jwt.Key)
	if err != nil {
		t.Fatal(err)
	}
	otherSession, err := issuer.Issue(context.Background(), auth.IssueRequest{UserID: f.admin.User.ID, Client: "webadmin", Method: "account"})
	if err != nil {
		t.Fatal(err)
	}
	otherInfo := f.call("GET", infoPath, "", otherSession.Session.Token, true)
	var otherResponse struct{ Code int }
	if json.Unmarshal(otherInfo.Body.Bytes(), &otherResponse) != nil || otherResponse.Code == 0 || strings.Contains(otherInfo.Body.String(), "bind-device") {
		t.Fatal("another authenticated session read binding metadata")
	}
	for _, secret := range []string{state, f.admin.Session.Token, "code_verifier", "client_secret", "nonce", "password"} {
		if strings.Contains(info.Body.String(), secret) {
			t.Fatal("binding metadata leaked credentials")
		}
	}
	for _, suffix := range []string{"confirm", "bindConfirm"} {
		confirmation := f.call("POST", "/api/admin/oauth/"+suffix, fmt.Sprintf(`{"code":%q}`, started.Data.Code), f.admin.Session.Token, true)
		var result struct{ Code int }
		if json.Unmarshal(confirmation.Body.Bytes(), &result) != nil || result.Code == 0 {
			t.Fatalf("legacy %s authorized OIDC bind: %s", suffix, confirmation.Body.String())
		}
	}
	if routeCount(t, &model.OIDCIdentity{}) != 0 {
		t.Fatal("legacy confirmation wrote identity")
	}
	f.call("GET", "/api/oidc/callback?"+url.Values{"state": {state}, "code": {"bind-code"}}.Encode(), "", "", false)
	var identity model.OIDCIdentity
	if err := global.DB.First(&identity).Error; err != nil {
		t.Fatal(err)
	}
	if identity.UserID != f.admin.User.ID || identity.Issuer != f.idp.server.URL || identity.Subject != routeOIDCSubject {
		t.Fatal("explicit binding did not use initiating actor")
	}
	if routeCount(t, &model.UserToken{}) != 2 || routeCount(t, &model.LoginLog{}) != 2 {
		t.Fatal("bind callback issued extra session")
	}
	if poll := f.query(started.Data.Code, true); strings.Contains(poll.Body.String(), `"token"`) {
		t.Fatal("bind attempt polled as login")
	}
}

func TestProductionWebSSOConfirmRejectsEmptyAndDefaultPasswords(t *testing.T) {
	f := newRouteOIDCFixture(t)
	defaultBcrypt, err := utils.EncryptPassword(utils.DefaultInitPassword)
	if err != nil {
		t.Fatal(err)
	}
	emptyBcrypt, err := utils.EncryptPassword("")
	if err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"", utils.Md5("rustdesk-api"), utils.Md5(utils.DefaultInitPassword + "rustdesk-api"), defaultBcrypt, emptyBcrypt} {
		if err := global.DB.Model(&model.User{}).Where("id = ?", f.admin.User.ID).Update("password", hash).Error; err != nil {
			t.Fatal(err)
		}
		a := f.begin(t, false, "webauth")
		info := f.call("GET", "/api/admin/oauth/info?code="+url.QueryEscape(a.Code), "", f.admin.Session.Token, true)
		assertSafeOIDCInfo(t, info.Body.Bytes())
		confirmation := f.call("POST", "/api/admin/oauth/confirm", fmt.Sprintf(`{"code":%q}`, a.Code), f.admin.Session.Token, true)
		var result struct{ Code int }
		if json.Unmarshal(confirmation.Body.Bytes(), &result) != nil || result.Code != 403 {
			t.Fatalf("unsafe password approved webSSO: %s", confirmation.Body.String())
		}
		pending := f.query(a.Code, false)
		var resultPending struct{ Error string }
		if json.Unmarshal(pending.Body.Bytes(), &resultPending) != nil || resultPending.Error != "No authed oidc is found" {
			t.Fatalf("denied confirmation changed attempt: %s", pending.Body.String())
		}
	}
	if f.idp.tokens.Load() != 0 || routeCount(t, &model.UserToken{}) != 1 || routeCount(t, &model.LoginLog{}) != 1 {
		t.Fatal("denied webSSO confirmation issued credential or contacted IdP")
	}
}
