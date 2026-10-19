package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/config"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/utils"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func issuanceDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "issuance.db")+"?_txlock=immediate&_busy_timeout=5000"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.AutoMigrate(&model.User{}, &model.UserToken{}, &model.LoginLog{}, &model.Peer{}, &model.OIDCIdentity{},
		&model.UserThird{}, &model.AddressBook{}, &model.AddressBookCollection{}, &model.AddressBookCollectionRule{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func issuanceService(t *testing.T, db *gorm.DB, ttl time.Duration) *UserService {
	t.Helper()
	oldConfig, oldDB, oldLogger, oldJWT, oldLock, oldAll := Config, DB, Logger, Jwt, Lock, AllService
	t.Cleanup(func() {
		Config, DB, Logger, Jwt, Lock, AllService = oldConfig, oldDB, oldLogger, oldJWT, oldLock, oldAll
	})
	return New(&config.Config{App: config.App{TokenExpire: ttl}}, db, nil, nil, nil).UserService
}

func issuanceCreate(t *testing.T, db *gorm.DB, row interface{}) {
	t.Helper()
	if err := db.Create(row).Error; err != nil {
		t.Fatal(err)
	}
}

func issuanceSQL(t *testing.T, db *gorm.DB, query string) {
	t.Helper()
	if err := db.Exec(query).Error; err != nil {
		t.Fatal(err)
	}
}

func issuanceUser(t *testing.T, db *gorm.DB) model.User {
	t.Helper()
	admin := false
	u := model.User{Username: "bridge-user", Password: "verified-password-hash", IsAdmin: &admin, Status: model.COMMON_STATUS_ENABLE}
	issuanceCreate(t, db, &u)
	return u
}

func issuanceCount(t *testing.T, db *gorm.DB, row interface{}) int64 {
	t.Helper()
	var count int64
	if err := db.Model(row).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func TestLegacyLoginRollsBackEveryWriteFailure(t *testing.T) {
	for _, fault := range []struct{ name, ddl string }{
		{"session", "CREATE TRIGGER fail_issue BEFORE INSERT ON user_tokens BEGIN SELECT RAISE(ABORT, 'session fault'); END"},
		{"log", "CREATE TRIGGER fail_issue BEFORE INSERT ON login_logs BEGIN SELECT RAISE(ABORT, 'log fault'); END"},
		{"peer", "CREATE TRIGGER fail_issue BEFORE UPDATE ON peers BEGIN SELECT RAISE(ABORT, 'peer fault'); END"},
	} {
		t.Run(fault.name, func(t *testing.T) {
			db := issuanceDB(t)
			us := issuanceService(t, db, time.Hour)
			u := issuanceUser(t, db)
			peer := model.Peer{Id: "device", Uuid: "uuid", UserId: 99}
			issuanceCreate(t, db, &peer)
			issuanceSQL(t, db, fault.ddl)
			ut, err := us.Login(context.Background(), &u, &model.LoginLog{Uuid: "uuid", DeviceId: "device"})
			if ut != nil || !errors.Is(err, auth.ErrUnavailable) {
				t.Fatalf("token=%v error=%v", ut, err)
			}
			if issuanceCount(t, db, &model.UserToken{}) != 0 || issuanceCount(t, db, &model.LoginLog{}) != 0 {
				t.Fatal("partial issuance committed")
			}
			if err := db.First(&peer, peer.RowId).Error; err != nil {
				t.Fatal(err)
			}
			if peer.UserId != 99 {
				t.Fatal("peer ownership changed on rollback")
			}
		})
	}
}

func TestLegacyLoginUsesConfirmedUserAndCorrectLogReference(t *testing.T) {
	db := issuanceDB(t)
	us := issuanceService(t, db, time.Hour)
	u := issuanceUser(t, db)
	issuanceCreate(t, db, &model.UserToken{UserId: u.Id, Token: "existing", ExpiredAt: time.Now().Add(time.Hour).Unix()})
	if err := db.Model(&model.User{}).Where("id = ?", u.Id).Updates(map[string]interface{}{"is_admin": true, "nickname": "current-name"}).Error; err != nil {
		t.Fatal(err)
	}
	log := &model.LoginLog{Client: "test", Type: model.LoginLogTypeAccount}
	ut, err := us.Login(context.Background(), &u, log)
	if err != nil || ut == nil {
		t.Fatalf("login: %v", err)
	}
	if u.IsAdmin == nil || !*u.IsAdmin || u.Nickname != "current-name" {
		t.Fatal("stale user returned")
	}
	var persisted model.LoginLog
	if err := db.First(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.UserTokenId != ut.Id || log.UserTokenId != ut.Id || ut.Id == ut.UserId {
		t.Fatalf("log references user rather than session: log=%+v session=%+v", persisted, ut)
	}
	if len(ut.Token) < 40 {
		t.Fatal("opaque token lacks expected random material")
	}
	if err := db.Model(&model.User{}).Where("id = ?", u.Id).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
		t.Fatal(err)
	}
	if token, err := us.Login(context.Background(), &u, log); token != nil || !errors.Is(err, auth.ErrDisabled) {
		t.Fatalf("disabled user login: %v, %v", token, err)
	}
}

func TestLegacyLoginRejectsChangedPasswordAndCancellation(t *testing.T) {
	db := issuanceDB(t)
	us := issuanceService(t, db, time.Hour)
	u := issuanceUser(t, db)
	if err := db.Model(&model.User{}).Where("id = ?", u.Id).Update("password", "new-hash").Error; err != nil {
		t.Fatal(err)
	}
	if token, err := us.Login(context.Background(), &u, &model.LoginLog{}); token != nil || !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("stale password login: %v, %v", token, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if token, err := us.Login(ctx, &u, &model.LoginLog{}); token != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled login: %v, %v", token, err)
	}
	if issuanceCount(t, db, &model.UserToken{}) != 0 {
		t.Fatal("failed login committed session")
	}
}

func TestLegacyLoginInstanceIsolationAndTimeoutSnapshot(t *testing.T) {
	firstDB := issuanceDB(t)
	first := issuanceService(t, firstDB, time.Hour)
	u := issuanceUser(t, firstDB)
	secondDB := issuanceDB(t)
	_ = issuanceService(t, secondDB, 2*time.Hour)
	ut, err := first.Login(context.Background(), &u, &model.LoginLog{})
	if err != nil || ut == nil {
		t.Fatalf("first instance login: %v", err)
	}
	if issuanceCount(t, secondDB, &model.UserToken{}) != 0 {
		t.Fatal("cross-instance session write")
	}
	remaining := time.Until(time.Unix(ut.ExpiredAt, 0))
	if remaining < 59*time.Minute || remaining > time.Hour {
		t.Fatalf("wrong instance TTL: %v", remaining)
	}
	first.authTimeout = 25 * time.Millisecond
	if err := firstDB.Callback().Query().Before("gorm:query").Register("issuance_wait", func(tx *gorm.DB) { <-tx.Statement.Context.Done() }); err != nil {
		t.Fatal(err)
	}
	defer firstDB.Callback().Query().Remove("issuance_wait")
	if token, err := first.Login(context.Background(), &u, &model.LoginLog{}); token != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded login: %v, %v", token, err)
	}
}

func TestLegacyPasswordChangeRollsBackRevocationFailure(t *testing.T) {
	db := issuanceDB(t)
	us := issuanceService(t, db, time.Hour)
	u := issuanceUser(t, db)
	oldHash := u.Password
	issuanceCreate(t, db, &model.UserToken{UserId: u.Id, Token: "existing"})
	issuanceSQL(t, db, "CREATE TRIGGER fail_revoke BEFORE DELETE ON user_tokens BEGIN SELECT RAISE(ABORT, 'revoke fault'); END")
	if err := us.UpdatePasswordContext(context.Background(), &u, "new-secret"); err == nil {
		t.Fatal("revocation failure ignored")
	}
	var current model.User
	if err := db.First(&current, u.Id).Error; err != nil {
		t.Fatal(err)
	}
	if current.Password != oldHash || u.Password != oldHash || issuanceCount(t, db, &model.UserToken{}) != 1 {
		t.Fatal("password changed without session revocation")
	}
	issuanceSQL(t, db, "DROP TRIGGER fail_revoke")
	if err := us.UpdatePassword(&u, "new-secret"); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&current, u.Id).Error; err != nil {
		t.Fatal(err)
	}
	if ok, _, err := utils.VerifyPassword(current.Password, "new-secret"); err != nil || !ok {
		t.Fatal("new password not committed")
	}
	if issuanceCount(t, db, &model.UserToken{}) != 0 {
		t.Fatal("sessions remain after password change")
	}
	stale := u
	stale.Password = oldHash
	if err := us.UpdatePassword(&stale, "stale-secret"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("stale password update allowed: %v", err)
	}
}

func TestLegacyOAuthEmailRegistrationDisabled(t *testing.T) {
	us := &UserService{}
	if err, user := us.RegisterByOauth(&model.OauthUser{Email: "existing@example.test"}, "provider"); err == nil || user != nil {
		t.Fatal("legacy email admission remains enabled")
	}
}

func TestLegacyDeleteRollsBackIdentitySessionAndPeerCleanup(t *testing.T) {
	db := issuanceDB(t)
	us := issuanceService(t, db, time.Hour)
	u := issuanceUser(t, db)
	identity := model.OIDCIdentity{Issuer: "https://issuer.example.test", Subject: "subject", UserID: u.Id}
	issuanceCreate(t, db, &identity)
	issuanceCreate(t, db, &model.UserThird{UserId: u.Id, Op: "legacy"})
	issuanceCreate(t, db, &model.UserToken{UserId: u.Id, Token: "existing"})
	peer := model.Peer{Id: "device", Uuid: "uuid", UserId: u.Id}
	issuanceCreate(t, db, &peer)
	issuanceSQL(t, db, "CREATE TRIGGER fail_unlink BEFORE UPDATE ON peers BEGIN SELECT RAISE(ABORT, 'unlink fault'); END")
	if err := us.DeleteContext(context.Background(), &u); err == nil {
		t.Fatal("delete ignored peer cleanup failure")
	}
	for _, row := range []interface{}{&model.User{}, &model.OIDCIdentity{}, &model.UserThird{}, &model.UserToken{}} {
		if issuanceCount(t, db, row) != 1 {
			t.Fatalf("partial deletion of %T", row)
		}
	}
	issuanceSQL(t, db, "DROP TRIGGER fail_unlink")
	if err := us.DeleteContext(context.Background(), &u); err != nil {
		t.Fatal(err)
	}
	for _, row := range []interface{}{&model.User{}, &model.OIDCIdentity{}, &model.UserThird{}, &model.UserToken{}} {
		if issuanceCount(t, db, row) != 0 {
			t.Fatalf("deletion left %T", row)
		}
	}
	if err := db.First(&peer, peer.RowId).Error; err != nil {
		t.Fatal(err)
	}
	if peer.UserId != 0 {
		t.Fatal("deleted user's peer remains linked")
	}
}

func TestLegacyDeleteLastAdministratorAndCancellation(t *testing.T) {
	db := issuanceDB(t)
	us := issuanceService(t, db, time.Hour)
	u := issuanceUser(t, db)
	if err := db.Model(&model.User{}).Where("id = ?", u.Id).Update("is_admin", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := us.Delete(&u); err == nil {
		t.Fatal("last administrator deleted using stale ordinary-user snapshot")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := us.DeleteContext(ctx, &u); !errors.Is(err, context.Canceled) {
		t.Fatalf("delete cancellation: %v", err)
	}
	if err := us.UpdatePasswordContext(ctx, &u, "new-secret"); !errors.Is(err, context.Canceled) {
		t.Fatalf("password cancellation: %v", err)
	}
	if issuanceCount(t, db, &model.User{}) != 1 {
		t.Fatal("failed delete changed user")
	}
}

// A deferred foreign key fails at COMMIT rather than at the write statement.
// This verifies the bridge never treats an unsuccessful commit as a login or
// password-change success.
func TestLegacyIssuanceAndPasswordCommitFailures(t *testing.T) {
	for _, operation := range []string{"login", "password"} {
		t.Run(operation, func(t *testing.T) {
			db := issuanceDB(t)
			conn, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			conn.SetMaxOpenConns(1)
			issuanceSQL(t, db, "PRAGMA foreign_keys = ON")
			issuanceSQL(t, db, "CREATE TABLE commit_guards (user_id INTEGER REFERENCES users(id) DEFERRABLE INITIALLY DEFERRED)")
			us := issuanceService(t, db, time.Hour)
			u := issuanceUser(t, db)
			originalHash := u.Password
			if operation == "login" {
				issuanceSQL(t, db, "CREATE TRIGGER fail_commit AFTER INSERT ON login_logs BEGIN INSERT INTO commit_guards(user_id) VALUES(999999); END")
				if token, err := us.Login(context.Background(), &u, &model.LoginLog{}); token != nil || err == nil {
					t.Fatalf("commit failure returned token=%v error=%v", token, err)
				}
				if issuanceCount(t, db, &model.UserToken{}) != 0 || issuanceCount(t, db, &model.LoginLog{}) != 0 {
					t.Fatal("failed commit left issuance writes")
				}
			} else {
				issuanceCreate(t, db, &model.UserToken{UserId: u.Id, Token: "existing"})
				issuanceSQL(t, db, "CREATE TRIGGER fail_commit AFTER UPDATE ON users BEGIN INSERT INTO commit_guards(user_id) VALUES(999999); END")
				if err := us.UpdatePassword(&u, "new-secret"); err == nil {
					t.Fatal("password commit failure ignored")
				}
				var current model.User
				if err := db.First(&current, u.Id).Error; err != nil {
					t.Fatal(err)
				}
				if current.Password != originalHash || u.Password != originalHash || issuanceCount(t, db, &model.UserToken{}) != 1 {
					t.Fatal("failed commit changed password or sessions")
				}
			}
		})
	}
}
