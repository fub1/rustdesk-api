package authstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/authstore"
	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/sessiontoken"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

func issuanceDB(t *testing.T) (*gorm.DB, model.User) {
	db := openDB(t)
	if err := db.AutoMigrate(&model.LoginLog{}); err != nil {
		t.Fatal(err)
	}
	user := model.User{Username: "login-user", Password: "current-hash", Status: model.COMMON_STATUS_ENABLE}
	create(t, db, &user)
	create(t, db, &model.Peer{Id: "device", Uuid: "uuid", UserId: 0})
	return db, user
}

func TestIssueCommitsSessionLogAndDevice(t *testing.T) {
	db, u := issuanceDB(t)
	store, _ := authstore.New(db)
	issuer, _ := auth.NewIssuer(store, sessiontoken.NewIssuer(nil), time.Hour, func() time.Time { return now })
	p, err := issuer.Issue(context.Background(), auth.IssueRequest{UserID: u.Id, ExpectedPassword: &u.Password, DeviceUUID: "uuid", DeviceID: "device", Client: "app", Method: "account"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Session.ID == 0 || p.Session.Token == "" || p.Session.ExpiresAt != now.Add(time.Hour) {
		t.Fatalf("invalid issued session: id=%d", p.Session.ID)
	}
	var log model.LoginLog
	if err := db.First(&log).Error; err != nil {
		t.Fatal(err)
	}
	if log.UserId != u.Id || log.UserTokenId != p.Session.ID {
		t.Fatal("login log does not reference actual session")
	}
	var peer model.Peer
	db.First(&peer)
	if peer.UserId != u.Id {
		t.Fatal("device binding not committed")
	}
}

func TestIssueRollsBackEveryWriteFailure(t *testing.T) {
	for _, target := range []string{"user_tokens", "login_logs", "peers"} {
		t.Run(target, func(t *testing.T) {
			db, u := issuanceDB(t)
			callback := func(tx *gorm.DB) {
				if tx.Statement.Table == target {
					tx.AddError(errors.New("simulated persistence failure secret"))
				}
			}
			if target == "peers" {
				db.Callback().Update().Before("gorm:update").Register("issue-fault", callback)
			} else {
				db.Callback().Create().Before("gorm:create").Register("issue-fault", callback)
			}
			store, _ := authstore.New(db)
			issuer, _ := auth.NewIssuer(store, sessiontoken.NewIssuer(nil), time.Hour, nil)
			p, err := issuer.Issue(context.Background(), auth.IssueRequest{UserID: u.Id, DeviceUUID: "uuid"})
			if !errors.Is(err, auth.ErrUnavailable) || p.Session.Token != "" {
				t.Fatal("failure returned a credential")
			}
			var tokens, logs int64
			db.Model(&model.UserToken{}).Count(&tokens)
			db.Model(&model.LoginLog{}).Count(&logs)
			var peer model.Peer
			db.First(&peer)
			if tokens != 0 || logs != 0 || peer.UserId != 0 {
				t.Fatalf("partial commit tokens=%d logs=%d peer=%d", tokens, logs, peer.UserId)
			}
		})
	}
}

func TestIssueRechecksUserAndPassword(t *testing.T) {
	for _, mode := range []string{"deleted", "disabled", "password-changed", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			db, u := issuanceDB(t)
			store, _ := authstore.New(db)
			issuer, _ := auth.NewIssuer(store, sessiontoken.NewIssuer(nil), time.Hour, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "deleted":
				db.Delete(&u)
			case "disabled":
				db.Model(&u).Update("status", model.COMMON_STATUS_DISABLED)
			case "password-changed":
				db.Model(&model.User{}).Where("id = ?", u.Id).Update("password", "new-hash")
			case "canceled":
				cancel()
			}
			p, err := issuer.Issue(ctx, auth.IssueRequest{UserID: u.Id, ExpectedPassword: &u.Password})
			if err == nil || p.Session.Token != "" {
				t.Fatal("stale or canceled login issued token")
			}
			var count int64
			db.Model(&model.UserToken{}).Count(&count)
			if count != 0 {
				t.Fatal("failed login persisted session")
			}
		})
	}
}
