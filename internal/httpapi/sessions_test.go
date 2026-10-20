package httpapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/authstore"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/sessiontoken"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestMain(m *testing.M) { gin.SetMode(gin.TestMode); os.Exit(m.Run()) }

type installation struct {
	db     *gorm.DB
	routes *gin.Engine
	now    time.Time
}

// These installations coexist without importing or changing global/service.
func newInstallation(t *testing.T, name string, signed bool, options ...httpapi.Options) *installation {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "sessions.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&model.User{}, &model.UserToken{}, &model.Peer{}); err != nil {
		t.Fatal(err)
	}
	// Use the runtime schema for current-model peer writes.
	if err := database.Update(db, 281, nil); err != nil {
		t.Fatal(err)
	}
	for id := uint(1); id <= 2; id++ {
		admin := id == 2
		if err := db.Create(&model.User{IdModel: model.IdModel{Id: id}, Username: name + string(rune('0'+id)), Password: "private-password-hash", IsAdmin: &admin, Status: model.COMMON_STATUS_ENABLE}).Error; err != nil {
			t.Fatal(err)
		}
	}
	store, err := authstore.New(db)
	if err != nil {
		t.Fatal(err)
	}
	var verifier auth.TokenVerifier
	if signed {
		verifier, err = sessiontoken.NewHS256([]byte("shared-local-session-signing-key"))
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	sessions, err := auth.New(auth.Dependencies{Store: store, Verifier: verifier, Now: func() time.Time { return now }, TTL: 3 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	opts := httpapi.Options{}
	if len(options) > 0 {
		opts = options[0]
	}
	if opts.Translate == nil {
		opts.Translate = func(_, id string) string { return id }
	}
	h, err := httpapi.New(sessions, opts)
	if err != nil {
		t.Fatal(err)
	}
	g := gin.New()
	client := g.Group("/api").Use(h.ClientAuth())
	client.POST("/currentUser", h.ClientCurrent)
	client.GET("/user/info", h.ClientCurrent)
	client.POST("/logout", h.ClientLogout)
	g.GET("/api/admin/config/admin", h.OptionalAdminAuth(), func(c *gin.Context) {
		p, ok := httpapi.Current(c)
		if ok {
			c.JSON(200, gin.H{"hello": p.User.Username})
		} else {
			c.JSON(200, gin.H{"title": name})
		}
	})
	admin := g.Group("/api/admin").Use(h.AdminAuth())
	admin.GET("/user/current", h.AdminCurrent)
	admin.POST("/logout", h.AdminLogout)
	admin.GET("/restricted", h.RequireAdmin(), func(c *gin.Context) { c.JSON(200, gin.H{"allowed": true}) })
	admin.POST("/user_token/delete", h.RequireAdmin(), h.DeleteSession)
	admin.POST("/user_token/batchDelete", h.RequireAdmin(), h.DeleteSessions)
	return &installation{db: db, routes: g, now: now}
}

func addSession(t *testing.T, f *installation, user uint, token string, expiry time.Time) uint {
	t.Helper()
	row := model.UserToken{UserId: user, Token: token, ExpiredAt: expiry.Unix()}
	if err := f.db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row.Id
}

func signedToken(t *testing.T, method jwt.SigningMethod, user uint, expiry *time.Time, key string, nonce string) string {
	t.Helper()
	claims := jwt.MapClaims{"user_id": user, "jti": nonce}
	if expiry != nil {
		claims["exp"] = expiry.Unix()
	}
	token, err := jwt.NewWithClaims(method, claims).SignedString([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func request(f *installation, method, path, header, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if header != "" {
		r.Header.Set(header, token)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.routes.ServeHTTP(w, r)
	return w
}

func requireCode(t *testing.T, w *httptest.ResponseRecorder, status, code int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	var result struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Code != code {
		t.Fatalf("body=%s want code=%d error=%v", w.Body.String(), code, err)
	}
}

func TestIndependentHTTPValidationAndLogout(t *testing.T) {
	for _, signed := range []bool{false, true} {
		t.Run(map[bool]string{false: "opaque", true: "JWT"}[signed], func(t *testing.T) {
			a, b := newInstallation(t, "alice", signed), newInstallation(t, "bob", signed)
			ta, tb := "opaque-alice", "opaque-bob"
			if signed {
				ea, eb := a.now.Add(time.Hour), b.now.Add(time.Hour)
				ta = signedToken(t, jwt.SigningMethodHS256, 1, &ea, "shared-local-session-signing-key", "alice")
				tb = signedToken(t, jwt.SigningMethodHS256, 1, &eb, "shared-local-session-signing-key", "bob")
			}
			addSession(t, a, 1, ta, a.now.Add(time.Hour))
			addSession(t, b, 1, tb, b.now.Add(time.Hour))
			for _, tc := range []struct {
				f           *installation
				token, name string
			}{{a, ta, "alice1"}, {b, tb, "bob1"}} {
				w := request(tc.f, "POST", "/api/currentUser", "Authorization", "Bearer "+tc.token, "")
				if w.Code != 200 || !strings.Contains(w.Body.String(), tc.name) || strings.Contains(w.Body.String(), "private-password-hash") {
					t.Fatalf("current identity: %d %s", w.Code, w.Body.String())
				}
				w = request(tc.f, "GET", "/api/admin/user/current", "api-token", tc.token, "")
				requireCode(t, w, 200, 0)
				if !strings.Contains(w.Body.String(), tc.name) || strings.Contains(w.Body.String(), `"route_names":["*"]`) {
					t.Fatalf("ordinary admin current: %s", w.Body.String())
				}
				requireCode(t, request(tc.f, "GET", "/api/admin/restricted", "api-token", tc.token, ""), 200, 403)
			}
			if w := request(b, "POST", "/api/currentUser", "Authorization", "Bearer "+ta, ""); w.Code != 401 {
				t.Fatalf("cross-installation token accepted: %s", w.Body.String())
			}
			requireCode(t, request(a, "POST", "/api/admin/logout", "api-token", ta, ""), 200, 0)
			if w := request(a, "POST", "/api/currentUser", "Authorization", "Bearer "+ta, ""); w.Code != 401 {
				t.Fatal("logout failed to revoke client access")
			}
			requireCode(t, request(a, "GET", "/api/admin/user/current", "api-token", ta, ""), 200, 403)
			if w := request(b, "POST", "/api/currentUser", "Authorization", "Bearer "+tb, ""); w.Code != 200 {
				t.Fatal("logout affected other installation")
			}
		})
	}
}

func TestHeaderParsingAndExpiration(t *testing.T) {
	f := newInstallation(t, "user", false)
	addSession(t, f, 1, "valid", f.now.Add(2*time.Hour))
	for _, header := range []string{"valid", "Basic valid", "Bearer", "Bearer valid extra", "Bearer valid,another"} {
		if w := request(f, "POST", "/api/currentUser", "Authorization", header, ""); w.Code != 401 {
			t.Fatalf("header %q accepted", header)
		}
	}
	if w := request(f, "POST", "/api/currentUser", "Authorization", "bEaReR valid", ""); w.Code != 200 {
		t.Fatal("case-insensitive Bearer rejected")
	}
	r := httptest.NewRequest("POST", "/api/currentUser", nil)
	r.Header.Add("Authorization", "Bearer valid")
	r.Header.Add("Authorization", "Bearer valid")
	w := httptest.NewRecorder()
	f.routes.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("ambiguous headers accepted")
	}
	addSession(t, f, 1, "boundary", f.now)
	if w := request(f, "POST", "/api/currentUser", "Authorization", "Bearer boundary", ""); w.Code != 401 {
		t.Fatal("exact expiry boundary accepted")
	}
}

func TestJWTValidationIsSharedByBothHeaders(t *testing.T) {
	f := newInstallation(t, "user", true)
	future, past := f.now.Add(time.Hour), f.now.Add(-time.Second)
	for _, tc := range []struct {
		name   string
		method jwt.SigningMethod
		user   uint
		expiry *time.Time
		key    string
	}{
		{"bad signature", jwt.SigningMethodHS256, 1, &future, "wrong-key"},
		{"wrong algorithm", jwt.SigningMethodHS384, 1, &future, "shared-local-session-signing-key"},
		{"expired", jwt.SigningMethodHS256, 1, &past, "shared-local-session-signing-key"},
		{"missing expiry", jwt.SigningMethodHS256, 1, nil, "shared-local-session-signing-key"},
		{"wrong user", jwt.SigningMethodHS256, 2, &future, "shared-local-session-signing-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := signedToken(t, tc.method, tc.user, tc.expiry, tc.key, tc.name)
			addSession(t, f, 1, token, future)
			if w := request(f, "POST", "/api/currentUser", "Authorization", "Bearer "+token, ""); w.Code != 401 {
				t.Fatalf("invalid client JWT accepted: %s", w.Body.String())
			}
			requireCode(t, request(f, "GET", "/api/admin/user/current", "api-token", token, ""), 200, 403)
		})
	}
}

func TestUserStateAndCurrentRoleTakeEffectOnNextRequest(t *testing.T) {
	f := newInstallation(t, "user", false)
	addSession(t, f, 2, "admin", f.now.Add(2*time.Hour))
	if w := request(f, "GET", "/api/admin/restricted", "api-token", "admin", ""); !strings.Contains(w.Body.String(), "allowed") {
		t.Fatal("administrator denied")
	}
	if err := f.db.Model(&model.User{}).Where("id = ?", 2).Update("is_admin", false).Error; err != nil {
		t.Fatal(err)
	}
	requireCode(t, request(f, "GET", "/api/admin/restricted", "api-token", "admin", ""), 200, 403)
	if err := f.db.Model(&model.User{}).Where("id = ?", 2).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
		t.Fatal(err)
	}
	if w := request(f, "POST", "/api/currentUser", "Authorization", "Bearer admin", ""); w.Code != 401 {
		t.Fatal("disabled user accepted")
	}
	if w := request(f, "GET", "/api/admin/user/current", "api-token", "admin", ""); w.Code != 401 {
		t.Fatal("disabled user accepted in admin")
	}
	if w := request(f, "GET", "/api/admin/config/admin", "api-token", "admin", ""); strings.Contains(w.Body.String(), "hello") {
		t.Fatal("optional auth exposed disabled identity")
	}
}

func TestAdministrativeRevocationUsesInstanceStore(t *testing.T) {
	a, b := newInstallation(t, "a", false), newInstallation(t, "b", false)
	id := addSession(t, a, 1, "target", a.now.Add(2*time.Hour))
	addSession(t, b, 1, "other", b.now.Add(2*time.Hour))
	addSession(t, a, 2, "admin", a.now.Add(2*time.Hour))
	requireCode(t, request(a, "POST", "/api/admin/user_token/delete", "api-token", "target", `{"id":1}`), 200, 403)
	requireCode(t, request(a, "POST", "/api/admin/user_token/delete", "api-token", "admin", `{"id":0}`), 200, 101)
	body, _ := json.Marshal(map[string]uint{"id": id})
	requireCode(t, request(a, "POST", "/api/admin/user_token/delete", "api-token", "admin", string(body)), 200, 0)
	if w := request(a, "POST", "/api/currentUser", "Authorization", "Bearer target", ""); w.Code != 401 {
		t.Fatal("target session not revoked")
	}
	if w := request(b, "POST", "/api/currentUser", "Authorization", "Bearer other", ""); w.Code != 200 {
		t.Fatal("deleted session in other database")
	}
	addSession(t, a, 1, "batch-target", a.now.Add(2*time.Hour))
	requireCode(t, request(a, "POST", "/api/admin/user_token/batchDelete", "api-token", "admin", `{"ids":[3]}`), 200, 0)
	if w := request(a, "POST", "/api/currentUser", "Authorization", "Bearer batch-target", ""); w.Code != 401 {
		t.Fatal("batch target not revoked")
	}
}

func TestDependencyFailuresAreNotAuthenticationFailures(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		f := newInstallation(t, "user", false)
		addSession(t, f, 1, "valid", f.now.Add(2*time.Hour))
		pool, _ := f.db.DB()
		_ = pool.Close()
		for _, tc := range []struct{ method, path, header, token string }{{"POST", "/api/currentUser", "Authorization", "Bearer valid"}, {"GET", "/api/admin/user/current", "api-token", "valid"}, {"GET", "/api/admin/config/admin", "api-token", "valid"}} {
			w := request(f, tc.method, tc.path, tc.header, tc.token, "")
			if w.Code != 503 {
				t.Fatalf("DB failure became authentication result: %d %s", w.Code, w.Body.String())
			}
		}
	})
	t.Run("revoke", func(t *testing.T) {
		f := newInstallation(t, "user", false)
		addSession(t, f, 1, "valid-secret-marker", f.now.Add(2*time.Hour))
		if err := f.db.Callback().Delete().Before("gorm:delete").Register("revoke-failure", func(tx *gorm.DB) { tx.AddError(errors.New("SQL contains valid-secret-marker")) }); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct{ path, header, token string }{{"/api/logout", "Authorization", "Bearer valid-secret-marker"}, {"/api/admin/logout", "api-token", "valid-secret-marker"}} {
			w := request(f, "POST", tc.path, tc.header, tc.token, "")
			if w.Code != 503 || bytes.Contains(w.Body.Bytes(), []byte("valid-secret-marker")) {
				t.Fatalf("unsafe failure result: %d %s", w.Code, w.Body.String())
			}
		}
		var count int64
		if err := f.db.Model(&model.UserToken{}).Count(&count).Error; err != nil || count != 1 {
			t.Fatal("failed revoke removed session")
		}
	})
}
