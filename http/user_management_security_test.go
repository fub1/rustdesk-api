package http

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	"github.com/lejianwen/rustdesk-api/v2/internal/useradmin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/utils"
	"gorm.io/gorm"
)

func installUserManagementStore(t *testing.T, f definitionFixture, db *gorm.DB) {
	t.Helper()
	sessions, err := app.NewSessions(f.db, time.Hour, global.Jwt.Key)
	if err != nil {
		t.Fatal(err)
	}
	users, err := useradmin.New(db, useradmin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h, err := httpapi.New(sessions, httpapi.Options{Translate: LegacyTranslator(), Users: users})
	if err != nil {
		t.Fatal(err)
	}
	f.handler, err = NewHandler(h)
	if err != nil {
		t.Fatal(err)
	}
}
func userManagementFixture(t *testing.T) definitionFixture {
	t.Helper()
	f := newDefinitionFixture(t)
	hash, err := utils.EncryptPassword("old-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&model.User{}).Where("id IN ?", []uint{f.admin.Id, f.owner.Id}).Update("password", hash).Error; err != nil {
		t.Fatal(err)
	}
	compatCreate(t, &model.UserToken{UserId: f.owner.Id, Token: global.Jwt.GenerateToken(f.owner.Id), ExpiredAt: time.Now().Add(time.Hour).Unix()})
	installUserManagementStore(t, f, f.db)
	return f
}

type userManagementState struct {
	Users       []model.User
	Sessions    []model.UserToken
	Peers       []model.Peer
	Books       []model.AddressBook
	Collections []model.AddressBookCollection
	Tags        []model.Tag
	Rules       []model.AddressBookCollectionRule
	Identities  []model.OIDCIdentity
	Thirds      []model.UserThird
}

func userManagementSnapshot(t *testing.T, f definitionFixture) userManagementState {
	t.Helper()
	var s userManagementState
	for _, rows := range []any{&s.Users, &s.Sessions, &s.Peers, &s.Books, &s.Collections, &s.Tags, &s.Rules, &s.Identities, &s.Thirds} {
		if err := f.db.Order("1").Find(rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	return s
}

type userManagementCase struct {
	path, body    string
	actor, target model.User
}

func userManagementOperation(f definitionFixture, name string) userManagementCase {
	c := userManagementCase{actor: f.admin, target: f.owner}
	switch name {
	case "create":
		c.path = "create"
		c.body = `{"username":"New HTTP User","group_id":1,"status":1,"nickname":"new display","password":"ignored-input","created_at":"2000-01-01","unknown":"ignored"}`
	case "update":
		c.path = "update"
		c.body = fmt.Sprintf(`{"id":%d,"username":"owner","group_id":1,"status":1,"nickname":"changed display","password":"ignored-input"}`, f.owner.Id)
	case "disable":
		c.path = "update"
		c.body = fmt.Sprintf(`{"id":%d,"username":"owner","group_id":1,"status":2}`, f.owner.Id)
	case "delete":
		c.path = "delete"
		c.body = fmt.Sprintf(`{"id":%d}`, f.owner.Id)
	case "reset":
		c.path = "changePwd"
		c.body = fmt.Sprintf(`{"id":%d,"password":"new-secret"}`, f.owner.Id)
	case "self":
		c.path = "changeCurPwd"
		c.actor = f.owner
		c.body = `{"old_password":"old-secret","new_password":"new-secret","id":999}`
	default:
		panic(name)
	}
	c.path = "/api/admin/user/" + c.path
	return c
}
func assertUserManagementControl(t *testing.T, f definitionFixture, c userManagementCase, name string) {
	t.Helper()
	var user model.User
	if name == "create" {
		if err := f.db.Where("username = ?", "newhttpuser").First(&user).Error; err != nil || user.Nickname != "new display" || user.GroupId != 1 {
			t.Fatal("created account not persisted", err)
		}
		ok, _, err := utils.VerifyPassword(user.Password, utils.DefaultInitPassword)
		if err != nil || !ok {
			t.Fatal("create accepted a protected password", err)
		}
		return
	}
	err := f.db.First(&user, c.target.Id).Error
	if name == "delete" {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatal("deleted account remains", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if name == "update" {
		ok, _, err := utils.VerifyPassword(user.Password, "old-secret")
		if err != nil || !ok || user.Nickname != "changed display" {
			t.Fatal("metadata update changed protected password or lost field", err)
		}
		return
	}
	if name == "disable" {
		if user.Status != model.COMMON_STATUS_DISABLED {
			t.Fatal("disable not persisted")
		}
	} else {
		ok, _, err := utils.VerifyPassword(user.Password, "new-secret")
		if err != nil || !ok {
			t.Fatal("password not persisted", err)
		}
	}
	var count int64
	if err := f.db.Model(&model.UserToken{}).Where("user_id = ?", c.target.Id).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("all target sessions were not revoked", count, err)
	}
}
func TestUserManagementHTTPPostAuthenticationChanges(t *testing.T) {
	for _, name := range []string{"create", "update", "delete", "reset", "self"} {
		for _, change := range []string{"revoked", "disabled", "demoted", "expired"} {
			if name == "self" && change == "demoted" {
				continue
			}
			t.Run(name+"/"+change, func(t *testing.T) {
				f := userManagementFixture(t)
				c := userManagementOperation(f, name)
				var afterChange userManagementState
				fired := false
				reader := &collectionChangingReader{Reader: strings.NewReader(c.body), change: func() error {
					fired = true
					var err error
					switch change {
					case "revoked":
						err = f.db.Where("token = ?", f.tokens[c.actor.Id]).Delete(&model.UserToken{}).Error
					case "disabled":
						err = f.db.Model(&model.User{}).Where("id = ?", c.actor.Id).Update("status", model.COMMON_STATUS_DISABLED).Error
					case "demoted":
						err = f.db.Model(&model.User{}).Where("id = ?", c.actor.Id).Update("is_admin", false).Error
					case "expired":
						err = f.db.Model(&model.UserToken{}).Where("token = ?", f.tokens[c.actor.Id]).Update("expired_at", time.Now().Add(-time.Minute).Unix()).Error
					}
					if err == nil {
						afterChange = userManagementSnapshot(t, f)
					}
					return err
				}}
				r := httptest.NewRequest("POST", c.path, reader)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("api-token", f.tokens[c.actor.Id])
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				definitionResult(t, w, 200, 403)
				if !fired || !reflect.DeepEqual(afterChange, userManagementSnapshot(t, f)) {
					t.Fatal("post-authentication change retained business writes")
				}
				if err := f.db.Model(&model.User{}).Where("id = ?", c.actor.Id).Updates(map[string]any{"status": model.COMMON_STATUS_ENABLE, "is_admin": c.actor.IsAdmin}).Error; err != nil {
					t.Fatal(err)
				}
				if err := f.db.Where("token = ?", f.tokens[c.actor.Id]).Delete(&model.UserToken{}).Error; err != nil {
					t.Fatal(err)
				}
				compatCreate(t, &model.UserToken{UserId: c.actor.Id, Token: f.tokens[c.actor.Id], ExpiredAt: time.Now().Add(time.Hour).Unix()})
				definitionResult(t, f.request(context.Background(), "POST", c.path, c.body, f.tokens[c.actor.Id]), 200, 0)
				assertUserManagementControl(t, f, c, name)
			})
		}
	}
}

type userHTTPCommitPool struct {
	gorm.ConnPool
	db      *sql.DB
	reached *bool
}
type userHTTPFailedCommit struct {
	*sql.Tx
	reached *bool
}

func (p userHTTPCommitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &userHTTPFailedCommit{Tx: tx, reached: p.reached}, nil
}
func (tx *userHTTPFailedCommit) Commit() error {
	*tx.reached = true
	_ = tx.Tx.Rollback()
	return errors.New("private user management commit failure")
}
func TestUserManagementHTTPSQLAndActualCommitFailures(t *testing.T) {
	for _, name := range []string{"create", "update", "disable", "delete", "reset", "self"} {
		for _, fault := range []string{"sql", "commit"} {
			t.Run(name+"/"+fault, func(t *testing.T) {
				f := userManagementFixture(t)
				c := userManagementOperation(f, name)
				before := userManagementSnapshot(t, f)
				reached := false
				if fault == "sql" {
					event, table := "DELETE", "user_tokens"
					if name == "create" {
						event, table = "INSERT", "users"
					}
					if name == "update" {
						event, table = "UPDATE", "users"
					}
					if name == "delete" {
						event, table = "DELETE", "users"
					}
					if err := f.db.Exec(fmt.Sprintf("CREATE TRIGGER user_management_fault BEFORE %s ON %s BEGIN SELECT RAISE(ABORT, 'private user fault'); END", event, table)).Error; err != nil {
						t.Fatal(err)
					}
				} else {
					pool, err := f.db.DB()
					if err != nil {
						t.Fatal(err)
					}
					faultDB := f.db.Session(&gorm.Session{NewDB: true, Context: context.Background()})
					faultDB.Statement.ConnPool = userHTTPCommitPool{ConnPool: pool, db: pool, reached: &reached}
					if f.db.Statement.ConnPool == faultDB.Statement.ConnPool {
						t.Fatal("fault injection changed original statement pool")
					}
					installUserManagementStore(t, f, faultDB)
				}
				w := f.request(context.Background(), "POST", c.path, c.body, f.tokens[c.actor.Id])
				definitionResult(t, w, 503, 503)
				if strings.Contains(w.Body.String(), "private") || (fault == "commit" && !reached) || !reflect.DeepEqual(before, userManagementSnapshot(t, f)) {
					t.Fatal("failed mutation leaked details or retained writes", w.Body.String(), reached)
				}
				if fault == "sql" {
					if err := f.db.Exec("DROP TRIGGER user_management_fault").Error; err != nil {
						t.Fatal(err)
					}
				} else {
					installUserManagementStore(t, f, f.db)
				}
				definitionResult(t, f.request(context.Background(), "POST", c.path, c.body, f.tokens[c.actor.Id]), 200, 0)
				assertUserManagementControl(t, f, c, name)
			})
		}
	}
}
func TestUserManagementHTTPDisableEnableNeverRestoresSessions(t *testing.T) {
	f := userManagementFixture(t)
	c := userManagementOperation(f, "disable")
	var before []model.UserToken
	if err := f.db.Where("user_id = ?", f.owner.Id).Find(&before).Error; err != nil || len(before) != 2 {
		t.Fatal("two-session control missing", err)
	}
	for _, row := range before {
		definitionResult(t, f.request(context.Background(), "GET", "/api/admin/user/current", "", row.Token), 200, 0)
	}
	definitionResult(t, f.request(context.Background(), "POST", c.path, c.body, f.tokens[c.actor.Id]), 200, 0)
	assertUserManagementControl(t, f, c, "disable")
	enable := fmt.Sprintf(`{"id":%d,"username":"owner","group_id":1,"status":1}`, f.owner.Id)
	definitionResult(t, f.call("POST", "/api/admin/user/update", enable), 200, 0)
	for _, row := range before {
		token := row.Token
		definitionResult(t, f.request(context.Background(), "GET", "/api/admin/user/current", "", token), 200, 403)
	}
	fresh := global.Jwt.GenerateToken(f.owner.Id)
	compatCreate(t, &model.UserToken{UserId: f.owner.Id, Token: fresh, ExpiredAt: time.Now().Add(time.Hour).Unix()})
	definitionResult(t, f.request(context.Background(), "GET", "/api/admin/user/current", "", fresh), 200, 0)
}
func TestUserManagementHTTPSelfActionsAndLastEnabledAdmin(t *testing.T) {
	for _, action := range []string{"disable", "demote", "delete", "password"} {
		t.Run(action, func(t *testing.T) {
			f := userManagementFixture(t)
			// A disabled administrator cannot satisfy the safety invariant.
			yes := true
			disabled := model.User{Username: "disabled-admin", GroupId: 1, IsAdmin: &yes, Status: model.COMMON_STATUS_DISABLED}
			compatCreate(t, &disabled)
			body := fmt.Sprintf(`{"id":%d,"username":"administrator","group_id":1,"status":2}`, f.admin.Id)
			path := "/api/admin/user/update"
			if action == "demote" {
				body = fmt.Sprintf(`{"id":%d,"username":"administrator","group_id":1,"status":1,"is_admin":false}`, f.admin.Id)
			}
			if action == "delete" {
				path = "/api/admin/user/delete"
				body = fmt.Sprintf(`{"id":%d}`, f.admin.Id)
			}
			if action == "password" {
				path = "/api/admin/user/changeCurPwd"
				body = `{"old_password":"old-secret","new_password":"new-secret"}`
			} else {
				before := userManagementSnapshot(t, f)
				definitionResult(t, f.call("POST", path, body), 200, 101)
				if !reflect.DeepEqual(before, userManagementSnapshot(t, f)) {
					t.Fatal("last enabled administrator removal changed state")
				}
				// Metadata edit with omitted is_admin must retain the last admin role.
				metadata := fmt.Sprintf(`{"id":%d,"username":"administrator","group_id":1,"status":1,"nickname":"renamed"}`, f.admin.Id)
				definitionResult(t, f.call("POST", "/api/admin/user/update", metadata), 200, 0)
				if err := f.db.Model(&model.User{}).Where("id = ?", f.owner.Id).Update("is_admin", true).Error; err != nil {
					t.Fatal(err)
				}
			}
			definitionResult(t, f.call("POST", path, body), 200, 0)
			var current model.User
			err := f.db.First(&current, f.admin.Id).Error
			switch action {
			case "delete":
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					t.Fatal("self deletion not committed", err)
				}
			case "disable":
				if err != nil || current.Status != 2 {
					t.Fatal("self disable not committed", err)
				}
			case "demote":
				if err != nil || current.IsAdmin == nil || *current.IsAdmin {
					t.Fatal("self demotion not committed", err)
				}
			case "password":
				ok, _, e := utils.VerifyPassword(current.Password, "new-secret")
				if err != nil || e != nil || !ok {
					t.Fatal("self password not committed", err, e)
				}
			}
			if action != "demote" {
				definitionResult(t, f.call("GET", "/api/admin/user/current", ""), 200, 403)
			} else {
				definitionResult(t, f.call("GET", "/api/admin/user/current", ""), 200, 0)
				definitionResult(t, f.call("POST", "/api/admin/user/delete", fmt.Sprintf(`{"id":%d}`, f.hidden.Id)), 200, 403)
			}
		})
	}
}
func TestUserManagementHTTPLDAPPreflightIsBoundedAndCreateOnly(t *testing.T) {
	for _, mode := range []string{"exists", "network-error", "deadline", "clear"} {
		t.Run(mode, func(t *testing.T) {
			f := userManagementFixture(t)
			before := userManagementSnapshot(t, f)
			calls := 0
			sessions, err := app.NewSessions(f.db, time.Hour, global.Jwt.Key)
			if err != nil {
				t.Fatal(err)
			}
			users, err := useradmin.New(f.db, useradmin.Options{})
			if err != nil {
				t.Fatal(err)
			}
			h, err := httpapi.New(sessions, httpapi.Options{Translate: LegacyTranslator(), Users: users, CheckUsernameInLDAP: func(ctx context.Context, name string) (bool, error) {
				calls++
				deadline, ok := ctx.Deadline()
				names := []string{"New HTTP User", "newhttpuser"}
				if !ok || time.Until(deadline) > 5*time.Second || calls > len(names) || name != names[calls-1] {
					t.Fatal("LDAP preflight lost raw/persisted name or operation bound")
				}
				switch mode {
				case "exists":
					return true, nil
				case "network-error":
					return false, errors.New("private LDAP connection error")
				case "deadline":
					return false, context.DeadlineExceeded
				}
				return false, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			f.handler, err = NewHandler(h)
			if err != nil {
				t.Fatal(err)
			}
			c := userManagementOperation(f, "create")
			w := f.call("POST", c.path, c.body)
			status, code := 200, 0
			switch mode {
			case "exists":
				code = 101
			case "network-error":
				status, code = 503, 503
			case "deadline":
				status, code = 504, 504
			}
			definitionResult(t, w, status, code)
			expectedCalls := 1
			if mode == "clear" {
				expectedCalls = 2
			}
			if calls != expectedCalls || strings.Contains(w.Body.String(), "private") {
				t.Fatal("LDAP preflight calls or public error incorrect")
			}
			if mode == "clear" {
				assertUserManagementControl(t, f, c, "create")
			} else if !reflect.DeepEqual(before, userManagementSnapshot(t, f)) {
				t.Fatal("rejected LDAP preflight retained writes")
			}
			c = userManagementOperation(f, "update")
			definitionResult(t, f.call("POST", c.path, c.body), 200, 0)
			if calls != expectedCalls {
				t.Fatal("non-create metadata update called LDAP preflight")
			}
		})
	}
}

func TestUserManagementHTTPLDAPChecksPersistedUsernameAndRejectsEmpty(t *testing.T) {
	for _, mode := range []string{"normalized-conflict", "normalized-empty"} {
		t.Run(mode, func(t *testing.T) {
			f := userManagementFixture(t)
			before := userManagementSnapshot(t, f)
			calls := []string{}
			occupied := mode == "normalized-conflict"
			var shared context.Context
			sessions, err := app.NewSessions(f.db, time.Hour, global.Jwt.Key)
			if err != nil {
				t.Fatal(err)
			}
			users, err := useradmin.New(f.db, useradmin.Options{})
			if err != nil {
				t.Fatal(err)
			}
			h, err := httpapi.New(sessions, httpapi.Options{Translate: LegacyTranslator(), Users: users, CheckUsernameInLDAP: func(ctx context.Context, name string) (bool, error) {
				calls = append(calls, name)
				if shared == nil {
					shared = ctx
				} else if shared != ctx {
					t.Fatal("raw and persisted namespace checks did not share one operation budget")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("namespace check has no request deadline")
				}
				return occupied && name == "alice", nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			f.handler, err = NewHandler(h)
			if err != nil {
				t.Fatal(err)
			}
			body := `{"username":"a lice","group_id":1,"status":1,"nickname":"namespace control"}`
			if mode == "normalized-empty" {
				body = `{"username":"   ","group_id":1,"status":1}`
			}
			definitionResult(t, f.call("POST", "/api/admin/user/create", body), 200, 101)
			if !reflect.DeepEqual(before, userManagementSnapshot(t, f)) {
				t.Fatal("namespace rejection retained account/session/related writes")
			}
			expected := []string{"a lice", "alice"}
			if mode == "normalized-empty" {
				expected = nil
			}
			if len(calls) != len(expected) || (len(expected) != 0 && !reflect.DeepEqual(calls, expected)) {
				t.Fatal("namespace lookup did not check the actual persisted name or empty input dialed LDAP", calls)
			}
			// Same valid actor and intended username can create once the namespace
			// is available; the stored row must use the checked canonical key.
			occupied, shared, calls = false, nil, nil
			definitionResult(t, f.call("POST", "/api/admin/user/create", `{"username":"a lice","group_id":1,"status":1,"nickname":"namespace control"}`), 200, 0)
			if !reflect.DeepEqual(calls, []string{"a lice", "alice"}) {
				t.Fatal("positive control did not check both namespace names", calls)
			}
			var created model.User
			if err := f.db.Where("username = ?", "alice").First(&created).Error; err != nil || created.Nickname != "namespace control" || created.GroupId != 1 || created.Status != model.COMMON_STATUS_ENABLE {
				t.Fatal("positive control did not persist the checked username", err)
			}
			ok, _, err := utils.VerifyPassword(created.Password, utils.DefaultInitPassword)
			if err != nil || !ok {
				t.Fatal("positive namespace control did not retain valid initial password", err)
			}
		})
	}
}

func TestUserManagementHTTPLegacyFormAndProtectedFields(t *testing.T) {
	f := userManagementFixture(t)
	before := userManagementSnapshot(t, f)
	for _, body := range []string{
		`{"id":22,"username":"valid","group_id":1,"status":1}`,
		`{"username":"a","group_id":1,"status":1}`,
		`{"username":"` + strings.Repeat("界", 33) + `","group_id":1,"status":1}`,
		`{"username":"valid","status":1}`,
		`{"username":"valid","group_id":1,"status":0}`,
		`{"username":"valid","group_id":1,"status":-1}`,
		`{"username":"valid","group_id":1,"status":1} {}`,
		`null`,
	} {
		definitionResult(t, f.call("POST", "/api/admin/user/create", body), 200, 101)
		if !reflect.DeepEqual(before, userManagementSnapshot(t, f)) {
			t.Fatal("invalid legacy form retained writes")
		}
	}
	if err := f.db.Model(&model.User{}).Where("id = ?", f.owner.Id).Updates(map[string]any{"nickname": "preserved", "email": "preserved@example.test"}).Error; err != nil {
		t.Fatal(err)
	}
	var original model.User
	if err := f.db.First(&original, f.owner.Id).Error; err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"id":%d,"username":"owner","group_id":1,"status":1,"nickname":"","email":"","is_admin":null,"password":"ignored","created_at":"2000-01-01","updated_at":"2000-01-01","unknown":"ignored"}`, f.owner.Id)
	definitionResult(t, f.call("POST", "/api/admin/user/update", body), 200, 0)
	var updated model.User
	if err := f.db.First(&updated, f.owner.Id).Error; err != nil {
		t.Fatal(err)
	}
	if updated.Nickname != original.Nickname || updated.Email != original.Email || updated.Password != original.Password || !time.Time(updated.CreatedAt).Equal(time.Time(original.CreatedAt)) || updated.IsAdmin == nil || *updated.IsAdmin {
		t.Fatal("legacy nonzero patch or protected fields changed")
	}
	for _, body := range []string{fmt.Sprintf(`{"id":%d,"password":"abc"}`, f.owner.Id), fmt.Sprintf(`{"id":%d,"password":"%s"}`, f.owner.Id, strings.Repeat("x", 33))} {
		definitionResult(t, f.call("POST", "/api/admin/user/changePwd", body), 200, 101)
	}
}
