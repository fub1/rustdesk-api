package http

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/config"
	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/lib/cache"
	"github.com/lejianwen/rustdesk-api/v2/lib/jwt"
	"github.com/lejianwen/rustdesk-api/v2/lib/lock"
	"github.com/lejianwen/rustdesk-api/v2/lib/upload"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"github.com/lejianwen/rustdesk-api/v2/utils"
	"github.com/sirupsen/logrus"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Legacy state is installed once per test and never changed concurrently.
// Resource creation is explicit here, outside NewHandler, and fully cleaned up.
func installLegacyFixture(t *testing.T) {
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
}

func TestNewHandlerPreservesHTTPContracts(t *testing.T) {
	installLegacyFixture(t)
	mode, writer, errorWriter := gin.Mode(), gin.DefaultWriter, gin.DefaultErrorWriter
	g, err := NewHandler()
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

func TestNewHandlerRequiresConsistentLegacyResources(t *testing.T) {
	installLegacyFixture(t)
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
			if g, err := NewHandler(); err == nil || g != nil {
				t.Fatal("missing dependency accepted")
			}
		})
	}
}

func TestNewHandlerReportsConfigurationErrors(t *testing.T) {
	installLegacyFixture(t)
	global.Config.Gin.TrustProxy = "not-an-address"
	if _, err := NewHandler(); err == nil || !strings.Contains(err.Error(), "trusted proxies") {
		t.Fatalf("proxy error = %v", err)
	}
	global.Config.Gin.TrustProxy = ""
	global.Config.Gin.ResourcesPath = t.TempDir()
	if _, err := NewHandler(); err == nil || !strings.Contains(err.Error(), "templates") {
		t.Fatalf("template error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(global.Config.Gin.ResourcesPath, "templates"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global.Config.Gin.ResourcesPath, "templates", "broken.html"), []byte("{{if}}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHandler(); err == nil || !strings.Contains(err.Error(), "templates") {
		t.Fatalf("invalid template error = %v", err)
	}
	mode := gin.Mode()
	if err := ConfigureFramework("invalid"); err == nil || gin.Mode() != mode {
		t.Fatal("invalid mode must return an error without mutation")
	}
}

func TestRunReturnsListenError(t *testing.T) {
	installLegacyFixture(t)
	g, err := NewHandler()
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(g, "127.0.0.1:invalid-port"); err == nil {
		t.Fatal("listen error discarded")
	}
}

func TestLegacyRouteInventory(t *testing.T) {
	installLegacyFixture(t)
	for _, tc := range []struct {
		name     string
		features int
	}{{"base", 0}, {"optional", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			global.Config.App.WebClient, global.Config.App.ShowSwagger = tc.features, tc.features
			g, err := NewHandler()
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
