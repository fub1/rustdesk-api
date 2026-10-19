package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/app"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

func TestHandlerRejectsMissingDependenciesAndNegativeTimeout(t *testing.T) {
	f := newInstallation(t, "user", false)
	sessions, err := app.NewSessions(f.db, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	translate := func(_, id string) string { return id }
	for _, tc := range []struct {
		sessions bool
		options  httpapi.Options
	}{
		{false, httpapi.Options{Translate: translate}},
		{true, httpapi.Options{}},
		{true, httpapi.Options{Translate: translate, Timeout: -time.Second}},
	} {
		dependency := sessions
		if !tc.sessions {
			dependency = nil
		}
		if h, err := httpapi.New(dependency, tc.options); err == nil || h != nil {
			t.Fatal("invalid authentication dependencies accepted")
		}
	}
}

func TestHTTPDeadlineAndSafeFailureEvents(t *testing.T) {
	for _, path := range []string{"/api/currentUser", "/api/admin/user/current", "/api/admin/config/admin"} {
		t.Run(path, func(t *testing.T) {
			var events []httpapi.FailureEvent
			f := newInstallation(t, "user", false, httpapi.Options{Timeout: 25 * time.Millisecond, Observe: func(e httpapi.FailureEvent) { events = append(events, e) }})
			addSession(t, f, 1, "sensitive-session-value", f.now.Add(2*time.Hour))
			if err := f.db.Callback().Query().Before("gorm:query").Register("slow-auth-query", func(tx *gorm.DB) {
				if tx.Statement.Table == "user_tokens" {
					<-tx.Statement.Context.Done()
					tx.AddError(errors.New("driver failure with sensitive-session-value"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			method, header, token := "POST", "Authorization", "Bearer sensitive-session-value"
			if strings.Contains(path, "admin") {
				method, header, token = "GET", "api-token", "sensitive-session-value"
			}
			start := time.Now()
			w := request(f, method, path+"?code=sensitive-query-value", header, token, "")
			if w.Code != 504 || time.Since(start) > 2*time.Second {
				t.Fatalf("deadline result=%d %s", w.Code, w.Body.String())
			}
			if len(events) != 1 || events[0].Code != "deadline" || events[0].Operation != "authenticate" || events[0].Route != path {
				t.Fatalf("failure events=%+v", events)
			}
			serialized, _ := json.Marshal(events)
			if strings.Contains(string(serialized)+w.Body.String(), "sensitive-") {
				t.Fatal("credential/cause/query leaked into failure event or response")
			}
		})
	}
}

func TestHTTPCancellationInterruptsInFlightAuthentication(t *testing.T) {
	f := newInstallation(t, "user", false)
	addSession(t, f, 1, "valid", f.now.Add(2*time.Hour))
	started := make(chan struct{})
	if err := f.db.Callback().Query().Before("gorm:query").Register("wait-for-cancel", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_tokens" {
			close(started)
			<-tx.Statement.Context.Done()
			tx.AddError(tx.Statement.Context.Err())
		}
	}); err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/api/currentUser", nil).WithContext(parent)
	r.Header.Set("Authorization", "Bearer valid")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); f.routes.ServeHTTP(w, r) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("authentication did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request cancellation did not reach DB operation")
	}
	if w.Code != 408 {
		t.Fatalf("cancellation became a login failure: %d %s", w.Code, w.Body.String())
	}
}

func TestHTTPMutationDeadlinesDoNotReportSuccess(t *testing.T) {
	for _, operation := range []string{"renew", "logout", "admin_logout", "bulk_revoke"} {
		t.Run(operation, func(t *testing.T) {
			f := newInstallation(t, "user", false, httpapi.Options{Timeout: 25 * time.Millisecond})
			expiry := f.now.Add(2 * time.Hour)
			if operation == "renew" {
				expiry = f.now.Add(time.Minute)
			}
			addSession(t, f, 1, "target", expiry)
			addSession(t, f, 2, "admin", f.now.Add(2*time.Hour))
			block := func(tx *gorm.DB) {
				if tx.Statement.Table == "user_tokens" {
					<-tx.Statement.Context.Done()
					tx.AddError(errors.New("driver target secret"))
				}
			}
			if operation == "renew" {
				if err := f.db.Callback().Update().Before("gorm:update").Register("slow-renew", block); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.db.Callback().Delete().Before("gorm:delete").Register("slow-revoke", block); err != nil {
					t.Fatal(err)
				}
			}
			path, header, token, body := "/api/currentUser", "Authorization", "Bearer target", ""
			if operation == "logout" {
				path = "/api/logout"
			}
			if operation == "admin_logout" {
				path, header, token = "/api/admin/logout", "api-token", "target"
			}
			if operation == "bulk_revoke" {
				path, header, token, body = "/api/admin/user_token/batchDelete", "api-token", "admin", `{"ids":[1]}`
			}
			w := request(f, "POST", path, header, token, body)
			if w.Code != 504 {
				t.Fatalf("%s returned false success/login failure: %d %s", operation, w.Code, w.Body.String())
			}
			var target model.UserToken
			if err := f.db.First(&target, 1).Error; err != nil || target.Token != "target" || target.ExpiredAt != expiry.Unix() {
				t.Fatalf("failed mutation partially persisted: row=%+v error=%v", target, err)
			}
		})
	}
}

func TestDefaultDeadlineDoesNotReplaceEarlierRequestDeadline(t *testing.T) {
	f := newInstallation(t, "user", false)
	addSession(t, f, 1, "valid", f.now.Add(2*time.Hour))
	var observed time.Time
	if err := f.db.Callback().Query().Before("gorm:query").Register("inspect-deadline", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_tokens" {
			observed, _ = tx.Statement.Context.Deadline()
		}
	}); err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	deadline, _ := parent.Deadline()
	r := httptest.NewRequest("POST", "/api/currentUser", nil).WithContext(parent)
	r.Header.Set("Authorization", "Bearer valid")
	w := httptest.NewRecorder()
	f.routes.ServeHTTP(w, r)
	if w.Code != 200 || observed.IsZero() || observed.After(deadline) {
		t.Fatalf("parent deadline lost: status=%d observed=%v parent=%v", w.Code, observed, deadline)
	}
}
