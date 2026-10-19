package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/lejianwen/rustdesk-api/v2/config"
	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/http"
	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
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

const DatabaseVersion = 274

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
		return InitGlobal()
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
		authHTTP, err := httpapi.New(sessions, httpapi.Options{
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
		global.Logger.Info("API SERVER START")
		service.AllService.PeerService.StartHeartbeatFlusher()
		return http.Run(handler, global.Config.Gin.ApiAddr)
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
func main() {
	if err := rootCmd.Execute(); err != nil {
		if global.Logger != nil {
			global.Logger.Error(err)
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func InitGlobal() (err error) {
	// Failed startup must release opened connections and never reach HTTP or
	// background tasks. Workers are initialized only after migrations succeed.
	defer func() {
		if err == nil {
			return
		}
		if global.DB != nil {
			if sqlDB, dbErr := global.DB.DB(); dbErr != nil {
				err = errors.Join(err, dbErr)
			} else {
				err = errors.Join(err, sqlDB.Close())
			}
			global.DB = nil
		}
		if global.Redis != nil {
			err = errors.Join(err, global.Redis.Close())
			global.Redis = nil
		}
	}()

	//配置解析
	global.Viper = config.Init(&global.Config, global.ConfigPath)

	//日志
	global.Logger = logger.New(&logger.Config{
		Path:         global.Config.Logger.Path,
		Level:        global.Config.Logger.Level,
		ReportCaller: global.Config.Logger.ReportCaller,
	})

	global.InitI18n()

	//redis
	global.Redis = redis.NewClient(&redis.Options{
		Addr:     global.Config.Redis.Addr,
		Password: global.Config.Redis.Password,
		DB:       global.Config.Redis.Db,
	})

	//cache
	if global.Config.Cache.Type == cache.TypeFile {
		fc := cache.NewFileCache()
		fc.SetDir(global.Config.Cache.FileDir)
		global.Cache = fc
	} else if global.Config.Cache.Type == cache.TypeRedis {
		global.Cache = cache.NewRedis(&redis.Options{
			Addr:     global.Config.Cache.RedisAddr,
			Password: global.Config.Cache.RedisPwd,
			DB:       global.Config.Cache.RedisDb,
		})
	}
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

	if err := DatabaseAutoUpdate(); err != nil {
		return err
	}

	return nil
}

func DatabaseAutoUpdate() error {
	return Migrate(DatabaseVersion)
}

func Migrate(version uint) error {
	var initialPassword string
	err := database.Update(global.DB, version, func(tx *gorm.DB) error {
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
