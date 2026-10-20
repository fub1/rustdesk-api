package http

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/config"
	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	"github.com/lejianwen/rustdesk-api/v2/lib/cache"
	"github.com/lejianwen/rustdesk-api/v2/lib/jwt"
	"github.com/lejianwen/rustdesk-api/v2/lib/lock"
	"github.com/lejianwen/rustdesk-api/v2/lib/upload"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"github.com/lejianwen/rustdesk-api/v2/utils"
	"github.com/sirupsen/logrus"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Legacy state is installed once per test and never changed concurrently.
// Resource creation is explicit here, outside NewHandler, and fully cleaned up.
func installLegacyFixture(t *testing.T) *httpapi.Handler {
	t.Helper()
	previousConfig, previousDB, previousLog := global.Config, global.DB, global.Logger
	previousJWT, previousLock, previousCache := global.Jwt, global.Lock, global.Cache
	previousLimiter, previousLocalizer := global.LoginLimiter, global.Localizer
	previousValidator, previousOSS := global.Validator, global.Oss
	sc, sd, sl, sj, sk, ss := service.Config, service.DB, service.Logger, service.Jwt, service.Lock, service.AllService
	mode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() {
		global.Config, global.DB, global.Logger = previousConfig, previousDB, previousLog
		global.Jwt, global.Lock, global.Cache = previousJWT, previousLock, previousCache
		global.LoginLimiter, global.Localizer = previousLimiter, previousLocalizer
		global.Validator, global.Oss = previousValidator, previousOSS
		service.Config, service.DB, service.Logger, service.Jwt, service.Lock, service.AllService = sc, sd, sl, sj, sk, ss
		gin.SetMode(mode)
	})
	resources, err := filepath.Abs("../resources")
	if err != nil {
		t.Fatal(err)
	}
	global.Config = config.Config{Lang: "en", Gin: config.Gin{Mode: gin.ReleaseMode, ResourcesPath: resources}}
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "http.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	global.DB = db
	global.Logger = logrus.New()
	global.Logger.SetOutput(io.Discard)
	global.Jwt, global.Lock = jwt.NewJwt("test-only-key", 0), lock.NewLocal()
	global.Cache = &cache.FileCache{Dir: t.TempDir()}
	global.Oss = &upload.Oss{}
	global.ApiInitValidator()
	global.InitI18n()
	limiter := utils.NewLoginLimiter(utils.SecurityPolicy{CaptchaThreshold: -1})
	t.Cleanup(limiter.Close)
	global.LoginLimiter = limiter
	service.New(&global.Config, global.DB, global.Logger, global.Jwt, global.Lock)
	sessions, err := app.NewSessions(global.DB, global.Config.App.TokenExpire, global.Jwt.Key)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.New(sessions, httpapi.Options{Translate: LegacyTranslator()})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestNewHandlerPreservesHTTPContracts(t *testing.T) {
	sessions := installLegacyFixture(t)
	mode, writer, errorWriter := gin.Mode(), gin.DefaultWriter, gin.DefaultErrorWriter
	g, err := NewHandler(sessions)
	if err != nil {
		t.Fatal(err)
	}
	if gin.Mode() != mode || gin.DefaultWriter != writer || gin.DefaultErrorWriter != errorWriter {
		t.Fatal("handler construction changed Gin process configuration")
	}
	tables, err := global.DB.Migrator().GetTables()
	if err != nil || len(tables) != 0 {
		t.Fatalf("handler migrated database: %v %v", tables, err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
		body         string
	}{
		{"GET", "/missing", 404, "404 not found"},
		{"POST", "/api/currentUser", 401, `{"error":"Unauthorized"}`},
		{"GET", "/api/admin/user/list", 200, `{"code":403,"message":"Please login","data":null}`},
		{"GET", "/api/", 200, `{"code":0,"message":"success","data":"Hello Gwen"}`},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.status {
				t.Fatalf("HTTP %d, body %s", w.Code, w.Body.String())
			}
			if tc.path == "/api/admin/user/list" {
				var response struct {
					Code int `json:"code"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Code != 403 {
					t.Fatalf("admin contract: %s (%v)", w.Body.String(), err)
				}
			} else if strings.TrimSpace(w.Body.String()) != tc.body {
				t.Fatalf("body = %s, want %s", w.Body.String(), tc.body)
			}
		})
	}
}

func TestProductionAuthRouteOwnershipAndPrivileges(t *testing.T) {
	sessions := installLegacyFixture(t)
	if _, err := NewHandler(nil); err == nil {
		t.Fatal("missing instance auth accepted")
	}
	if err := global.DB.AutoMigrate(&model.User{}, &model.UserToken{}, &model.Peer{}, &model.ServerCmd{}, &model.Oauth{}); err != nil {
		t.Fatal(err)
	}
	// Use the runtime schema for current-model peer writes.
	if err := database.Update(global.DB, 281, nil); err != nil {
		t.Fatal(err)
	}
	global.Config.Admin.Hello = "Welcome {{username}}"
	global.Jwt.TokenExpireDuration = time.Hour
	tokens := map[uint]string{}
	for id := uint(1); id <= 2; id++ {
		isAdmin := id == 2
		if err := global.DB.Create(&model.User{IdModel: model.IdModel{Id: id}, Username: fmt.Sprintf("user%d", id), IsAdmin: &isAdmin, Status: model.COMMON_STATUS_ENABLE}).Error; err != nil {
			t.Fatal(err)
		}
		token := global.Jwt.GenerateToken(id)
		tokens[id] = token
		if err := global.DB.Create(&model.UserToken{UserId: id, Token: token, ExpiredAt: time.Now().Add(time.Hour).Unix()}).Error; err != nil {
			t.Fatal(err)
		}
	}
	g, err := NewHandler(sessions)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if token != "" {
			r.Header.Set("api-token", token)
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/api/admin/rustdesk/cmdList", "/api/admin/oauth/list", "/api/admin/user/list"} {
		for _, token := range []string{"", tokens[1]} {
			w := call("GET", path, token)
			var body struct {
				Code int `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != 403 {
				t.Fatalf("sensitive route %s accepted ordinary/anonymous: %s", path, w.Body.String())
			}
		}
		w := call("GET", path, tokens[2])
		var body struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != 0 {
			t.Fatalf("admin route %s: %s", path, w.Body.String())
		}
	}
	if w := call("GET", "/api/admin/user/current", tokens[1]); !strings.Contains(w.Body.String(), `"username":"user1"`) {
		t.Fatalf("ordinary self-management rejected: %s", w.Body.String())
	}
	if w := call("GET", "/api/admin/config/admin", tokens[1]); !strings.Contains(w.Body.String(), "Welcome user1") {
		t.Fatalf("optional auth did not resolve session: %s", w.Body.String())
	}
	if w := call("GET", "/api/admin/config/admin", "bad-token"); strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("invalid optional auth exposed hello: %s", w.Body.String())
	}
	if err := global.DB.Model(&model.User{}).Where("id = ?", 2).Update("is_admin", false).Error; err != nil {
		t.Fatal(err)
	}
	w := call("GET", "/api/admin/rustdesk/cmdList", tokens[2])
	var body struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != 403 {
		t.Fatalf("demoted administrator retained access: %s", w.Body.String())
	}
	// Logout is now registered after authentication, and has exactly one owner.
	w = call("POST", "/api/admin/logout", tokens[1])
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != 0 {
		t.Fatalf("logout: %s", w.Body.String())
	}
	w = call("GET", "/api/admin/user/current", tokens[1])
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != 403 {
		t.Fatalf("admin logout left session valid: %s", w.Body.String())
	}
}

func TestNewHandlerRequiresConsistentLegacyResources(t *testing.T) {
	sessions := installLegacyFixture(t)
	checks := []struct {
		name   string
		remove func() func()
	}{
		{"database", func() func() { old := global.DB; global.DB = nil; return func() { global.DB = old } }},
		{"logger", func() func() { old := global.Logger; global.Logger = nil; return func() { global.Logger = old } }},
		{"JWT", func() func() { old := global.Jwt; global.Jwt = nil; return func() { global.Jwt = old } }},
		{"limiter", func() func() {
			old := global.LoginLimiter
			global.LoginLimiter = nil
			return func() { global.LoginLimiter = old }
		}},
		{"localizer", func() func() {
			old := global.Localizer
			global.Localizer = nil
			return func() { global.Localizer = old }
		}},
		{"validator", func() func() {
			old := global.Validator.ValidStruct
			global.Validator.ValidStruct = nil
			return func() { global.Validator.ValidStruct = old }
		}},
		{"cache", func() func() { old := global.Cache; global.Cache = nil; return func() { global.Cache = old } }},
		{"upload", func() func() { old := global.Oss; global.Oss = nil; return func() { global.Oss = old } }},
		{"services", func() func() {
			old := service.AllService
			service.AllService = nil
			return func() { service.AllService = old }
		}},
		{"service database", func() func() { old := service.DB; service.DB = nil; return func() { service.DB = old } }},
		{"service configuration", func() func() {
			old := service.Config
			service.Config = &config.Config{}
			return func() { service.Config = old }
		}},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			restore := tc.remove()
			defer restore()
			if g, err := NewHandler(sessions); err == nil || g != nil {
				t.Fatal("missing dependency accepted")
			}
		})
	}
}

func TestNewHandlerReportsConfigurationErrors(t *testing.T) {
	sessions := installLegacyFixture(t)
	global.Config.Gin.TrustProxy = "not-an-address"
	if _, err := NewHandler(sessions); err == nil || !strings.Contains(err.Error(), "trusted proxies") {
		t.Fatalf("proxy error = %v", err)
	}
	global.Config.Gin.TrustProxy = ""
	global.Config.Gin.ResourcesPath = t.TempDir()
	if _, err := NewHandler(sessions); err == nil || !strings.Contains(err.Error(), "templates") {
		t.Fatalf("template error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(global.Config.Gin.ResourcesPath, "templates"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global.Config.Gin.ResourcesPath, "templates", "broken.html"), []byte("{{if}}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHandler(sessions); err == nil || !strings.Contains(err.Error(), "templates") {
		t.Fatalf("invalid template error = %v", err)
	}
	mode := gin.Mode()
	if err := ConfigureFramework("invalid"); err == nil || gin.Mode() != mode {
		t.Fatal("invalid mode must return an error without mutation")
	}
}

func TestRunReturnsListenError(t *testing.T) {
	sessions := installLegacyFixture(t)
	g, err := NewHandler(sessions)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(g, "127.0.0.1:invalid-port"); err == nil {
		t.Fatal("listen error discarded")
	}
}

func TestLegacyRouteInventory(t *testing.T) {
	sessions := installLegacyFixture(t)
	for _, tc := range []struct {
		name     string
		features int
	}{{"base", 0}, {"optional", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			global.Config.App.WebClient, global.Config.App.ShowSwagger = tc.features, tc.features
			g, err := NewHandler(sessions)
			if err != nil {
				t.Fatal(err)
			}
			var routes []string
			for _, route := range g.Routes() {
				routes = append(routes, route.Method+" "+route.Path)
			}
			sort.Strings(routes)
			actual := strings.Join(routes, "\n") + "\n"
			path := filepath.Join("testdata", "routes-"+tc.name+".txt")
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if actual != strings.ReplaceAll(string(want), "\r\n", "\n") {
				t.Fatalf("legacy route inventory changed; review method/path ownership\n%s", actual)
			}
		})
	}
}
