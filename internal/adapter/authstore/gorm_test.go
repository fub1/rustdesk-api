package authstore_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/authstore"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/sessiontoken"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "auth.db")+"?_txlock=immediate&_busy_timeout=5000"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.User{}, &model.UserToken{}, &model.Peer{}); err != nil {
		t.Fatal(err)
	}
	// Use the runtime schema for current-model peer writes.
	if err := database.Update(db, 281, nil); err != nil {
		t.Fatal(err)
	}
	return db
}

func create(t *testing.T, db *gorm.DB, value interface{}) {
	t.Helper()
	if err := db.Create(value).Error; err != nil {
		t.Fatal(err)
	}
}

func execSQL(t *testing.T, db *gorm.DB, sql string) {
	t.Helper()
	if err := db.Exec(sql).Error; err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T, db *gorm.DB, admin bool, token string, expiry time.Time) (model.User, model.UserToken) {
	t.Helper()
	u := model.User{
		Username: "fixture", Email: "u@example.test", Password: "internal-hash", Nickname: "Display", Avatar: "avatar.png",
		GroupId: 7, IsAdmin: &admin, Status: model.COMMON_STATUS_ENABLE, Remark: "remark",
		TimeModel: model.TimeModel{CreatedAt: custom_types.AutoTime(now.Add(-time.Hour)), UpdatedAt: custom_types.AutoTime(now.Add(-time.Minute))},
	}
	create(t, db, &u)
	s := model.UserToken{UserId: u.Id, Token: token, DeviceUuid: "device-uuid", DeviceId: "1234", ExpiredAt: expiry.Unix()}
	create(t, db, &s)
	return u, s
}

func newService(t *testing.T, db *gorm.DB, verifier auth.TokenVerifier) (*auth.Service, *authstore.Store) {
	t.Helper()
	store, err := authstore.New(db)
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.New(auth.Dependencies{Store: store, Verifier: verifier, TTL: time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return service, store
}

func TestLookupMapsCompleteLegacyUserAndSession(t *testing.T) {
	db := openDB(t)
	u, session := fixture(t, db, false, "opaque", now.Add(2*time.Hour))
	s, _ := newService(t, db, nil)
	p, err := s.Authenticate(context.Background(), "opaque")
	if err != nil {
		t.Fatal(err)
	}
	if p.User.ID != u.Id || p.User.Username != u.Username || p.User.Email != u.Email || p.User.Nickname != u.Nickname || p.User.Avatar != u.Avatar || p.User.GroupID != u.GroupId || p.User.PasswordHash != u.Password || p.User.Remark != u.Remark || p.User.IsAdmin || !p.User.Enabled {
		t.Fatalf("incomplete user mapping: %+v", p.User)
	}
	if !p.User.CreatedAt.Equal(time.Time(u.CreatedAt)) || !p.User.UpdatedAt.Equal(time.Time(u.UpdatedAt)) || p.User.CreatedAt.Location() != time.UTC {
		t.Fatalf("user timestamps changed: %+v", p.User)
	}
	if p.Session.ID != session.Id || p.Session.UserID != u.Id || p.Session.Token != "opaque" || p.Session.DeviceUUID != session.DeviceUuid || p.Session.DeviceID != session.DeviceId || p.Session.ExpiresAt.Unix() != session.ExpiredAt || p.Session.ExpiresAt.Location() != time.UTC {
		t.Fatalf("incomplete session mapping: %+v", p.Session)
	}
	if err := db.Model(&model.User{}).Where("id = ?", u.Id).Update("is_admin", true).Error; err != nil {
		t.Fatal(err)
	}
	p, err = s.Authenticate(context.Background(), "opaque")
	if err != nil || !p.User.IsAdmin {
		t.Fatalf("role was cached: %+v, %v", p.User, err)
	}
}

func TestAuthenticationRejectsMissingDisabledExpiredAndAmbiguousRecords(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*gorm.DB, model.User, model.UserToken)
		want   error
	}{
		{"missing_session", func(db *gorm.DB, _ model.User, s model.UserToken) {
			if err := db.Delete(&s).Error; err != nil {
				t.Fatal(err)
			}
		}, auth.ErrUnauthenticated},
		{"missing_user", func(db *gorm.DB, u model.User, _ model.UserToken) {
			if err := db.Delete(&u).Error; err != nil {
				t.Fatal(err)
			}
		}, auth.ErrUnauthenticated},
		{"disabled", func(db *gorm.DB, u model.User, _ model.UserToken) {
			if err := db.Model(&u).Update("status", model.COMMON_STATUS_DISABLED).Error; err != nil {
				t.Fatal(err)
			}
		}, auth.ErrDisabled},
		{"expiry_equal", func(db *gorm.DB, _ model.User, s model.UserToken) {
			if err := db.Model(&s).Update("expired_at", now.Unix()).Error; err != nil {
				t.Fatal(err)
			}
		}, auth.ErrUnauthenticated},
		{"duplicate_token", func(db *gorm.DB, u model.User, s model.UserToken) {
			create(t, db, &model.UserToken{UserId: u.Id, Token: s.Token, ExpiredAt: now.Add(time.Hour).Unix()})
		}, auth.ErrUnauthenticated},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openDB(t)
			u, row := fixture(t, db, false, "opaque", now.Add(time.Hour))
			test.mutate(db, u, row)
			s, _ := newService(t, db, nil)
			if _, err := s.Authenticate(context.Background(), "opaque"); !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}

func TestRenewalCannotResurrectRevokedSession(t *testing.T) {
	db := openDB(t)
	_, row := fixture(t, db, false, "opaque", now.Add(time.Minute))
	s, store := newService(t, db, nil)
	u, stale, err := store.Lookup(context.Background(), "opaque")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Renew(context.Background(), stale, now.Add(time.Hour), now); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("revoked renewal: %v", err)
	}
	if _, err := s.Authenticate(context.Background(), "opaque"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("revoked token accepted: %v", err)
	}
	var count int64
	if err := db.Model(&model.UserToken{}).Where("id = ? AND user_id = ?", row.Id, u.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("revoked row resurrected: count=%d, err=%v", count, err)
	}
}

func TestRenewalWritesBoundedExpiryAndPropagatesFailure(t *testing.T) {
	db := openDB(t)
	_, row := fixture(t, db, false, "opaque", now.Add(time.Minute))
	s, _ := newService(t, db, nil)
	execSQL(t, db, "CREATE TRIGGER fail_renew BEFORE UPDATE ON user_tokens BEGIN SELECT RAISE(ABORT, 'renew failed'); END")
	if _, err := s.Authenticate(context.Background(), "opaque"); !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("failed renewal authenticated: %v", err)
	}
	execSQL(t, db, "DROP TRIGGER fail_renew")
	p, err := s.Authenticate(context.Background(), "opaque")
	if err != nil || !p.Session.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("renewal retry: %+v, %v", p.Session, err)
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.ExpiredAt != now.Add(time.Hour).Unix() {
		t.Fatalf("renewal not persisted: %+v, %v", row, err)
	}
}

func TestConcurrentRenewalDoesNotShortenExistingExpiry(t *testing.T) {
	db := openDB(t)
	fixture(t, db, false, "opaque", now.Add(time.Minute))
	_, store := newService(t, db, nil)
	_, stale, err := store.Lookup(context.Background(), "opaque")
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Hour)
	if _, err := store.Renew(context.Background(), stale, later, now); err != nil {
		t.Fatal(err)
	}
	got, err := store.Renew(context.Background(), stale, now.Add(time.Hour), now)
	if err != nil || !got.ExpiresAt.Equal(later) {
		t.Fatalf("concurrent renewal shortened/rejected session: %+v, %v", got, err)
	}
}

func TestRevokeUnbindsOnlyLegacyMatchingPeer(t *testing.T) {
	db := openDB(t)
	u, _ := fixture(t, db, false, "opaque", now.Add(time.Hour))
	first := model.Peer{Id: "first", Uuid: "device-uuid", UserId: u.Id}
	second := model.Peer{Id: "second", Uuid: "device-uuid", UserId: u.Id}
	other := model.Peer{Id: "other-user", Uuid: "device-uuid", UserId: 999}
	for _, peer := range []*model.Peer{&first, &second, &other} {
		create(t, db, peer)
	}
	s, _ := newService(t, db, nil)
	p, err := s.Authenticate(context.Background(), "opaque")
	if err != nil {
		t.Fatal(err)
	}
	p.Session.DeviceUUID = "untrusted-context-uuid"
	if err := s.Revoke(context.Background(), p, "opaque"); err != nil {
		t.Fatal(err)
	}
	for _, peer := range []*model.Peer{&first, &second, &other} {
		if err := db.First(peer, peer.RowId).Error; err != nil {
			t.Fatal(err)
		}
	}
	if first.UserId != 0 || second.UserId != u.Id || other.UserId != 999 {
		t.Fatalf("wrong peer unbound: %+v / %+v / %+v", first, second, other)
	}
}

func TestRevokeRollsBackOnDeleteAndPeerErrors(t *testing.T) {
	for _, scenario := range []string{"delete", "peer_query", "peer_update"} {
		t.Run(scenario, func(t *testing.T) {
			db := openDB(t)
			u, _ := fixture(t, db, false, "opaque", now.Add(time.Hour))
			create(t, db, &model.Peer{Id: "peer", Uuid: "device-uuid", UserId: u.Id})
			s, _ := newService(t, db, nil)
			p, err := s.Authenticate(context.Background(), "opaque")
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "delete":
				execSQL(t, db, "CREATE TRIGGER fail_revoke BEFORE DELETE ON user_tokens BEGIN SELECT RAISE(ABORT, 'delete failed'); END")
			case "peer_query":
				execSQL(t, db, "DROP TABLE peers")
			case "peer_update":
				execSQL(t, db, "CREATE TRIGGER fail_revoke BEFORE UPDATE ON peers BEGIN SELECT RAISE(ABORT, 'unbind failed'); END")
			}
			if err := s.Revoke(context.Background(), p, "opaque"); !errors.Is(err, auth.ErrUnavailable) {
				t.Fatalf("revoke error swallowed: %v", err)
			}
			if _, err := s.Authenticate(context.Background(), "opaque"); err != nil {
				t.Fatalf("failed transaction deleted session: %v", err)
			}
		})
	}
}

func TestInstanceIsolationAndCancellation(t *testing.T) {
	dbA, dbB := openDB(t), openDB(t)
	fixture(t, dbA, false, "same-token", now.Add(time.Hour))
	fixture(t, dbB, true, "same-token", now.Add(time.Hour))
	a, _ := newService(t, dbA, nil)
	b, _ := newService(t, dbB, nil)
	pa, err := a.Authenticate(context.Background(), "same-token")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := b.Authenticate(context.Background(), "same-token")
	if err != nil {
		t.Fatal(err)
	}
	if pa.User.IsAdmin || !pb.User.IsAdmin {
		t.Fatal("instances share role state")
	}
	if err := a.Revoke(context.Background(), pa, "same-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Authenticate(context.Background(), "same-token"); err != nil {
		t.Fatalf("other instance revoked: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Authenticate(ctx, "same-token"); !errors.Is(err, context.Canceled) {
		t.Fatalf("authenticate cancellation: %v", err)
	}
	if err := b.Revoke(ctx, pb, "same-token"); !errors.Is(err, context.Canceled) {
		t.Fatalf("revoke cancellation: %v", err)
	}
	if _, err := b.Authenticate(context.Background(), "same-token"); err != nil {
		t.Fatalf("canceled revoke had effects: %v", err)
	}
}

func TestJWTDatabaseAuthorityIdentityAndRenewalCap(t *testing.T) {
	key := []byte("sqlite-jwt-session-test-key")
	verifier, err := sessiontoken.NewHS256(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"valid", "wrong_user", "no_session", "expired", "bad_signature"} {
		t.Run(scenario, func(t *testing.T) {
			db := openDB(t)
			uid := uint(1)
			if scenario == "wrong_user" {
				uid = 99
			}
			expiry := now.Add(10 * time.Minute)
			if scenario == "expired" {
				expiry = now
			}
			signingKey := key
			if scenario == "bad_signature" {
				signingKey = []byte("other-key")
			}
			raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"user_id": uid, "exp": expiry.Unix()}).SignedString(signingKey)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "no_session" {
				fixture(t, db, false, raw, now.Add(time.Minute))
			}
			s, _ := newService(t, db, verifier)
			p, err := s.Authenticate(context.Background(), raw)
			if scenario != "valid" {
				if !errors.Is(err, auth.ErrUnauthenticated) {
					t.Fatalf("invalid JWT/session accepted: %v", err)
				}
				return
			}
			if err != nil || !p.Session.ExpiresAt.Equal(expiry) {
				t.Fatalf("JWT renewal cap: %+v, %v", p.Session, err)
			}
			var row model.UserToken
			if err := db.First(&row).Error; err != nil || row.ExpiredAt != expiry.Unix() {
				t.Fatalf("DB exceeded JWT expiry: %+v, %v", row, err)
			}
		})
	}
}

func TestAdministratorDeleteSessionsIsScopedAndDoesNotUnbind(t *testing.T) {
	db := openDB(t)
	u, actor := fixture(t, db, true, "admin-token", now.Add(time.Hour))
	target := model.UserToken{UserId: u.Id, Token: "target", DeviceUuid: "device-uuid", ExpiredAt: now.Add(time.Hour).Unix()}
	create(t, db, &target)
	peer := model.Peer{Id: "peer", Uuid: "device-uuid", UserId: u.Id}
	create(t, db, &peer)
	s, _ := newService(t, db, nil)
	p, err := s.Authenticate(context.Background(), actor.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSessions(context.Background(), p, []uint{target.Id, 999}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSessions(context.Background(), p, []uint{target.Id}); err != nil {
		t.Fatalf("repeat revoke not idempotent: %v", err)
	}
	if err := s.RevokeSessions(context.Background(), p, nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if err := db.First(&peer, peer.RowId).Error; err != nil || peer.UserId != u.Id {
		t.Fatalf("ID deletion changed binding: %+v, %v", peer, err)
	}
	if err := db.Model(&u).Update("is_admin", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSessions(context.Background(), p, []uint{actor.Id}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("stale admin accepted: %v", err)
	}
	if _, err := s.Authenticate(context.Background(), actor.Token); err != nil {
		t.Fatalf("unauthorized delete had effects: %v", err)
	}
}

func TestStoreDoesNotLogRawTokens(t *testing.T) {
	db := openDB(t)
	const raw = "credential-must-not-appear-in-query-log"
	fixture(t, db, false, raw, now.Add(time.Minute))
	var logs bytes.Buffer
	db.Logger = logger.New(log.New(&logs, "", 0), logger.Config{LogLevel: logger.Info})
	s, _ := newService(t, db, nil)
	p, err := s.Authenticate(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(context.Background(), p, raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), raw) {
		t.Fatalf("raw token leaked to ORM log: %s", logs.String())
	}
}

func TestClosedDatabaseFailsUnavailable(t *testing.T) {
	db := openDB(t)
	fixture(t, db, false, "opaque", now.Add(time.Hour))
	s, _ := newService(t, db, nil)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(context.Background(), "opaque"); !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("closed DB accepted: %v", err)
	}
}
