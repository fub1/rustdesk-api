package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/lejianwen/rustdesk-api/v2/config"
	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/http"
	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientassign"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	"github.com/lejianwen/rustdesk-api/v2/internal/lifecycle"
	"github.com/lejianwen/rustdesk-api/v2/lib/cache"
	"github.com/lejianwen/rustdesk-api/v2/lib/jwt"
	"github.com/lejianwen/rustdesk-api/v2/lib/lock"
	"github.com/lejianwen/rustdesk-api/v2/lib/logger"
	"github.com/lejianwen/rustdesk-api/v2/lib/orm"
	"github.com/lejianwen/rustdesk-api/v2/lib/upload"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"github.com/lejianwen/rustdesk-api/v2/utils"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"github.com/spf13/cobra"
	"gorm.io/gorm"
)

const DatabaseVersion = 279

// @title 管理系统API
// @version 1.0
// @description 接口
// @basePath /api
// @securityDefinitions.apikey token
// @in header
// @name api-token
// @securitydefinitions.apikey BearerAuth
// @in header
// @name Authorization

var rootCmd = &cobra.Command{
	Use:   "apimain",
	Short: "RUSTDESK API SERVER",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		return initGlobal(cmd.Context(), cmd.Parent() == nil)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := http.ConfigureFramework(global.Config.Gin.Mode); err != nil {
			return err
		}
		global.LoginLimiter = utils.NewLoginLimiter(utils.SecurityPolicy{
			CaptchaThreshold: global.Config.App.CaptchaThreshold,
			BanThreshold:     global.Config.App.BanThreshold,
			AttemptsWindow:   10 * time.Minute,
			BanDuration:      30 * time.Minute,
		})
		global.LoginLimiter.RegisterProvider(utils.B64StringCaptchaProvider{})
		defer global.LoginLimiter.Close()
		sessions, err := app.NewSessions(global.DB, global.Config.App.TokenExpire, global.Jwt.Key)
		if err != nil {
			return err
		}
		authLogger := global.Logger
		oidcClient, err := app.OIDCHTTPClient(global.Config.Proxy.Enable, global.Config.Proxy.Host)
		if err != nil {
			return err
		}
		defer oidcClient.CloseIdleConnections()
		oidcLogin, err := app.NewOIDC(global.DB, global.Config.App.TokenExpire, global.Jwt.Key, sessions, global.Config.Rustdesk.ApiServer, global.Config.App.WebSso, oidcClient)
		if err != nil {
			return err
		}
		assign, err := clientassign.New(global.DB, clientassign.Options{})
		if err != nil {
			return err
		}
		authHTTP, err := httpapi.New(sessions, httpapi.Options{
			Assign:    assign,
			OIDC:      oidcLogin,
			Translate: http.LegacyTranslator(), Timeout: global.Config.App.AuthTimeout,
			Observe: func(event httpapi.FailureEvent) {
				authLogger.WithFields(map[string]interface{}{"component": "auth", "operation": event.Operation, "result": event.Code, "route": event.Route}).Debug("Authentication request failed")
			},
		})
		if err != nil {
			return err
		}
		handler, err := http.NewHandler(authHTTP)
		if err != nil {
			return err
		}
		return http.RunContext(cmd.Context(), handler, global.Config.Gin.ApiAddr, []lifecycle.Worker{service.AllService.PeerService}, lifecycle.Options{
			OnReady: func(net.Addr) { global.Logger.Info("API SERVER START") },
		})
	},
}

var resetPwdCmd = &cobra.Command{
	Use:     "reset-admin-pwd [pwd]",
	Example: "reset-admin-pwd 123456",
	Short:   "Reset Admin Password",
	Args:    cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		pwd := args[0]
		admin := service.AllService.UserService.InfoById(1)
		if admin.Id == 0 {
			global.Logger.Warn("user not found! ")
			return
		}
		err := service.AllService.UserService.UpdatePassword(admin, pwd)
		if err != nil {
			global.Logger.Error("reset password fail! ", err)
			return
		}
		global.Logger.Info("reset password success! ")
	},
}
var resetUserPwdCmd = &cobra.Command{
	Use:     "reset-pwd [userId] [pwd]",
	Example: "reset-pwd 2 123456",
	Short:   "Reset User Password",
	Args:    cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		userId := args[0]
		pwd := args[1]
		uid, err := strconv.Atoi(userId)
		if err != nil {
			global.Logger.Warn("userId must be int!")
			return
		}
		if uid <= 0 {
			global.Logger.Warn("userId must be greater than 0! ")
			return
		}
		u := service.AllService.UserService.InfoById(uint(uid))
		if u.Id == 0 {
			global.Logger.Warn("user not found! ")
			return
		}
		err = service.AllService.UserService.UpdatePassword(u, pwd)
		if err != nil {
			global.Logger.Warn("reset password fail! ", err)
			return
		}
		global.Logger.Info("reset password success!")
	},
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&global.ConfigPath, "config", "c", "./conf/config.yaml", "choose config file")
	rootCmd.AddCommand(resetPwdCmd, resetUserPwdCmd)
}

var logFile io.Closer

func execute(ctx context.Context) (err error) {
	defer func() {
		if !errors.Is(err, lifecycle.ErrIncompleteShutdown) {
			err = errors.Join(err, closeConnections())
		}
	}()
	return rootCmd.ExecuteContext(ctx)
}

func closeConnections() error {
	var err error
	if closer, ok := global.Cache.(io.Closer); ok {
		err = errors.Join(err, closer.Close())
	}
	global.Cache = nil
	if global.Redis != nil {
		err = errors.Join(err, global.Redis.Close())
		global.Redis = nil
	}
	if global.DB != nil {
		pool, e := global.DB.DB()
		if e != nil {
			err = errors.Join(err, e)
		} else {
			err = errors.Join(err, pool.Close())
		}
		global.DB = nil
	}
	return err
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := execute(ctx)
	if err != nil {
		if global.Logger != nil {
			global.Logger.Error(err)
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	if logFile != nil && !errors.Is(err, lifecycle.ErrIncompleteShutdown) {
		closeErr := logFile.Close()
		logFile = nil
		if closeErr != nil {
			fmt.Fprintln(os.Stderr, "close application log failed")
			err = errors.Join(err, closeErr)
		}
	}
	if err != nil {
		os.Exit(1)
	}
}

func InitGlobal() (err error) {
	return initGlobal(context.Background(), true)
}

func initGlobal(ctx context.Context, server bool) (err error) {
	// Failed startup must release opened connections and never reach HTTP or
	// background tasks. Workers are initialized only after migrations succeed.
	defer func() {
		if err == nil {
			return
		}
		err = errors.Join(err, closeConnections())
	}()

	//配置解析
	global.Viper, err = config.Load(&global.Config, global.ConfigPath)
	if err != nil {
		return err
	}

	//日志
	global.Logger, logFile, err = logger.Open(&logger.Config{
		Path:         global.Config.Logger.Path,
		Level:        global.Config.Logger.Level,
		ReportCaller: global.Config.Logger.ReportCaller,
	})
	if err != nil {
		return err
	}

	global.InitI18n()

	//gorm
	if global.Config.Gorm.Type == config.TypeMysql {

		dsn := fmt.Sprintf("%s:%s@(%s)/%s?charset=utf8mb4&parseTime=True&loc=Local&tls=%s",
			global.Config.Mysql.Username,
			global.Config.Mysql.Password,
			global.Config.Mysql.Addr,
			global.Config.Mysql.Dbname,
			global.Config.Mysql.Tls,
		)

		global.DB, err = orm.NewMysql(&orm.MysqlConfig{
			Dsn:          dsn,
			MaxIdleConns: global.Config.Gorm.MaxIdleConns,
			MaxOpenConns: global.Config.Gorm.MaxOpenConns,
		}, global.Logger)
	} else if global.Config.Gorm.Type == config.TypePostgresql {
		dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s TimeZone=%s",
			global.Config.Postgresql.Host,
			global.Config.Postgresql.Port,
			global.Config.Postgresql.User,
			global.Config.Postgresql.Password,
			global.Config.Postgresql.Dbname,
			global.Config.Postgresql.Sslmode,
			global.Config.Postgresql.TimeZone,
		)
		global.DB, err = orm.NewPostgresql(&orm.PostgresqlConfig{
			Dsn:          dsn,
			MaxIdleConns: global.Config.Gorm.MaxIdleConns,
			MaxOpenConns: global.Config.Gorm.MaxOpenConns,
		}, global.Logger)
	} else {
		//sqlite
		global.DB, err = orm.NewSqlite(&orm.SqliteConfig{
			MaxIdleConns: global.Config.Gorm.MaxIdleConns,
			MaxOpenConns: global.Config.Gorm.MaxOpenConns,
		}, global.Logger)
	}

	if err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}

	//validator
	global.ApiInitValidator()

	//oss
	global.Oss = &upload.Oss{
		AccessKeyId:     global.Config.Oss.AccessKeyId,
		AccessKeySecret: global.Config.Oss.AccessKeySecret,
		Host:            global.Config.Oss.Host,
		CallbackUrl:     global.Config.Oss.CallbackUrl,
		ExpireTime:      global.Config.Oss.ExpireTime,
		MaxByte:         global.Config.Oss.MaxByte,
	}

	//jwt
	//fmt.Println(global.Config.Jwt.PrivateKey)
	global.Jwt = jwt.NewJwt(global.Config.Jwt.Key, global.Config.Jwt.ExpireDuration)
	//locker
	global.Lock = lock.NewLocal()

	//service
	service.New(&global.Config, global.DB, global.Logger, global.Jwt, global.Lock)

	if err := MigrateContext(ctx, DatabaseVersion); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if server {
		global.Redis = redis.NewClient(&redis.Options{Addr: global.Config.Redis.Addr, Password: global.Config.Redis.Password, DB: global.Config.Redis.Db})
		switch global.Config.Cache.Type {
		case "", cache.TypeFile:
			fc := cache.NewFileCache()
			fc.SetDir(global.Config.Cache.FileDir)
			global.Cache = fc
		case cache.TypeRedis:
			global.Cache = cache.NewRedis(&redis.Options{Addr: global.Config.Cache.RedisAddr, Password: global.Config.Cache.RedisPwd, DB: global.Config.Cache.RedisDb})
		case cache.TypeMem:
			global.Cache = cache.NewMemoryCache(0)
		default:
			return errors.New("unsupported cache type")
		}
	}

	return nil
}

func DatabaseAutoUpdate() error {
	return Migrate(DatabaseVersion)
}

func Migrate(version uint) error {
	return MigrateContext(context.Background(), version)
}

func MigrateContext(ctx context.Context, version uint) error {
	if global.DB == nil {
		return errors.New("migration database is required")
	}
	var initialPassword string
	err := database.Update(global.DB.WithContext(ctx), version, func(tx *gorm.DB) error {
		localizer := global.Localizer("")
		defaultGroup, err := localizer.LocalizeMessage(&i18n.Message{ID: "DefaultGroup"})
		if err != nil {
			return fmt.Errorf("localize default group: %w", err)
		}
		shareGroup, err := localizer.LocalizeMessage(&i18n.Message{ID: "ShareGroup"})
		if err != nil {
			return fmt.Errorf("localize shared group: %w", err)
		}
		pwd := utils.RandomString(8)
		hash, err := utils.EncryptPassword(pwd)
		if err != nil {
			return fmt.Errorf("hash initial admin password: %w", err)
		}
		if err := database.SeedInitialData(tx, defaultGroup, shareGroup, hash); err != nil {
			return err
		}
		initialPassword = pwd
		return nil
	})
	if err != nil {
		return err
	}
	// Publish credentials only once their data and version have committed.
	if initialPassword != "" {
		global.Logger.Info("Admin Password Is: ", initialPassword)
	}
	return nil
}
