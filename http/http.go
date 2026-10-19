package http

import (
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/render"
	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/http/middleware"
	"github.com/lejianwen/rustdesk-api/v2/http/router"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"github.com/sirupsen/logrus"
)

// NewHandler borrows the resources installed by the legacy bootstrap. It does
// not open connections, migrate, start workers or listen. Existing controllers
// still use package globals; this is not a multi-instance dependency container.
func NewHandler() (*gin.Engine, error) {
	if err := validateLegacyResources(); err != nil {
		return nil, err
	}
	pattern := filepath.Join(global.Config.Gin.ResourcesPath, "templates", "*")
	templates, err := template.ParseGlob(pattern)
	if err != nil {
		return nil, fmt.Errorf("load HTTP templates: %w", err)
	}
	g := gin.New()

	//[WARNING] You trusted all proxies, this is NOT safe. We recommend you to set a value.
	//Please check https://pkg.go.dev/github.com/gin-gonic/gin#readme-don-t-trust-all-proxies for details.
	if global.Config.Gin.TrustProxy != "" {
		pro := strings.Split(global.Config.Gin.TrustProxy, ",")
		err := g.SetTrustedProxies(pro)
		if err != nil {
			return nil, fmt.Errorf("configure trusted proxies: %w", err)
		}
	}

	var recoveryWriter io.Writer = gin.DefaultErrorWriter
	if global.Config.Gin.Mode == gin.ReleaseMode {
		recoveryWriter = recoveryLogWriter{global.Logger}
	}
	g.NoRoute(func(c *gin.Context) {
		c.String(http.StatusNotFound, "404 not found")
	})
	g.Use(middleware.Logger(), middleware.Limiter(), gin.RecoveryWithWriter(recoveryWriter), middleware.Gzip())
	g.SetHTMLTemplate(templates)
	if gin.IsDebugging() {
		// Preserve Gin's template reload behavior after validating the files.
		g.HTMLRender = render.HTMLDebug{Glob: pattern}
	}
	router.WebInit(g)
	router.Init(g)
	router.ApiInit(g)
	return g, nil
}

// ConfigureFramework is a process-wide bootstrap operation, separate from
// handler construction so tests and callers do not mutate Gin's global mode.
func ConfigureFramework(mode string) error {
	switch mode {
	case "", gin.DebugMode, gin.ReleaseMode, gin.TestMode:
		gin.SetMode(mode)
		return nil
	default:
		return fmt.Errorf("invalid Gin mode %q", mode)
	}
}

// ApiInit is retained for callers of the legacy entry point.
func ApiInit() error {
	if err := ConfigureFramework(global.Config.Gin.Mode); err != nil {
		return err
	}
	g, err := NewHandler()
	if err != nil {
		return err
	}
	return Run(g, global.Config.Gin.ApiAddr)
}

func validateLegacyResources() error {
	if global.DB == nil || global.Logger == nil || global.Jwt == nil ||
		global.LoginLimiter == nil || global.Localizer == nil || global.Lock == nil ||
		global.Cache == nil || global.Oss == nil || global.Validator.ValidStruct == nil ||
		global.Validator.ValidVar == nil || service.AllService == nil {
		return errors.New("HTTP handler requires initialized legacy resources")
	}
	if service.Config != &global.Config || service.DB != global.DB ||
		service.Logger != global.Logger || service.Jwt != global.Jwt || service.Lock != global.Lock {
		return errors.New("HTTP handler requires one consistent legacy service installation")
	}
	return nil
}

// Unlike logrus.WriterLevel this synchronous adapter owns no pipe or goroutine.
type recoveryLogWriter struct{ logger *logrus.Logger }

func (w recoveryLogWriter) Write(p []byte) (int, error) {
	w.logger.Error(strings.TrimSpace(string(p)))
	return len(p), nil
}
