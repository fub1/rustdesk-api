package identitystore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/internal/adapter/authstore"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type Store struct {
	db     *gorm.DB
	tokens auth.TokenIssuer
	ttl    time.Duration
	now    func() time.Time
}

func New(db *gorm.DB, tokens auth.TokenIssuer, ttl time.Duration, now func() time.Time) (*Store, error) {
	if db == nil || db.Error != nil || tokens == nil || ttl < 0 {
		return nil, errors.New("invalid identity store dependencies")
	}
	if ttl == 0 {
		ttl = auth.DefaultTTL
	}
	if ttl < time.Second {
		return nil, errors.New("session lifetime must be at least one second")
	}
	if now == nil {
		now = time.Now
	}
	return &Store{db: db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}), tokens: tokens, ttl: ttl, now: now}, nil
}

func lock(tx *gorm.DB) *gorm.DB {
	if tx.Dialector.Name() != "sqlite" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return tx
}

func (s *Store) Provider(ctx context.Context, op string) (auth.ProviderConfig, error) {
	var rows []model.Oauth
	if op == "" {
		return auth.ProviderConfig{}, auth.ErrInvalidInput
	}
	if err := s.db.WithContext(ctx).Where("op = ?", op).Limit(2).Find(&rows).Error; err != nil {
		return auth.ProviderConfig{}, auth.ErrUnavailable
	}
	if len(rows) != 1 {
		return auth.ProviderConfig{}, auth.ErrInvalidInput
	}
	return provider(rows[0])
}

func provider(row model.Oauth) (auth.ProviderConfig, error) {
	if row.Id == 0 || row.OauthType != model.OauthTypeOidc || row.ClientId == "" || row.ClientSecret != "" || row.Issuer == "" {
		return auth.ProviderConfig{}, auth.ErrInvalidInput
	}
	scopes := strings.FieldsFunc(row.Scopes, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' })
	for _, scope := range scopes {
		if scope != "openid" && scope != "profile" {
			return auth.ProviderConfig{}, auth.ErrInvalidInput
		}
	}
	if row.PkceEnable != nil && !*row.PkceEnable || row.PkceMethod != "" && row.PkceMethod != model.PKCEMethodS256 {
		return auth.ProviderConfig{}, auth.ErrInvalidInput
	}
	return auth.ProviderConfig{ID: row.Id, Op: row.Op, Issuer: row.Issuer, ClientID: row.ClientId, AutoRegister: row.AutoRegister != nil && *row.AutoRegister}, nil
}

func checkProvider(tx *gorm.DB, expected auth.ProviderConfig) (auth.ProviderConfig, error) {
	var row model.Oauth
	if err := lock(tx).First(&row, expected.ID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return auth.ProviderConfig{}, auth.ErrInvalidInput
		}
		return auth.ProviderConfig{}, err
	}
	actual, err := provider(row)
	if err != nil || actual.ID != expected.ID || actual.Op != expected.Op || actual.Issuer != expected.Issuer || actual.ClientID != expected.ClientID || actual.AutoRegister != expected.AutoRegister {
		return auth.ProviderConfig{}, auth.ErrInvalidInput
	}
	return actual, nil
}

func valid(identity auth.ExternalIdentity, config auth.ProviderConfig) bool {
	return identity.Issuer == config.Issuer && identity.Issuer != "" && len(identity.Issuer) <= 512 && identity.Subject != "" && len(identity.Subject) <= 255
}

func (s *Store) Login(ctx context.Context, identity auth.ExternalIdentity, config auth.ProviderConfig, request auth.IssueRequest) (auth.Principal, error) {
	if !valid(identity, config) {
		return auth.Principal{}, auth.ErrInvalidInput
	}
	// Username is the verified UserInfo preferred_username. Display mapping is
	// local to this API; it does not require the provider to publish name.
	nickname := strings.TrimSpace(identity.Username)
	var result auth.Principal
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, err := checkProvider(tx, config)
		if err != nil {
			return err
		}
		var link model.OIDCIdentity
		err = lock(tx).Where("issuer = ? AND sub = ?", identity.Issuer, identity.Subject).First(&link).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if !current.AutoRegister {
				return auth.ErrForbidden
			}
			// No email/username lookup can select an existing account. New identities
			// always receive a new ordinary account with no local password.
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				return err
			}
			username := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(identity.Username), " ", ""))
			if len(username) > 64 {
				username = ""
			}
			if username == "" {
				username = "oidc"
			}
			username += "_" + hex.EncodeToString(random[:])
			admin := false
			user := model.User{Username: username, Nickname: nickname, Email: identity.Email, Avatar: identity.Picture, GroupId: 1, IsAdmin: &admin, Status: model.COMMON_STATUS_ENABLE}
			if err := tx.Create(&user).Error; err != nil {
				return err
			}
			link = model.OIDCIdentity{Issuer: identity.Issuer, Subject: identity.Subject, UserID: user.Id}
			if err := tx.Create(&link).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		request.UserID = link.UserID
		request.ExpectedPassword = nil
		now := s.now().UTC().Truncate(time.Second)
		expiry := now.Add(s.ttl).Truncate(time.Second)
		token, err := s.tokens.Issue(ctx, link.UserID, now, expiry)
		if err != nil {
			return err
		}
		result, err = authstore.IssueInTransaction(tx, auth.IssuedSession{Request: request, Token: token, Now: now, ExpiresAt: expiry})
		if err != nil {
			return err
		}
		// Issuance has validated and locked this exact local user until commit.
		// Backfill only an empty display name, preserving local edits and all
		// identity/account fields. The session, login log and nickname commit
		// together; a failed issuance or nickname write leaves none of them.
		if result.User.Nickname == "" && nickname != "" {
			update := tx.Model(&model.User{}).Where("id = ? AND nickname = ?", link.UserID, "").UpdateColumn("nickname", nickname)
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return auth.ErrUnavailable
			}
			result.User.Nickname = nickname
		}
		return nil
	})
	if err != nil {
		return auth.Principal{}, classify(err)
	}
	return result, nil
}

func checkActor(tx *gorm.DB, actor auth.Principal, now time.Time) error {
	if actor.User.ID == 0 || actor.Session.Token == "" || actor.Session.UserID != actor.User.ID || !actor.Session.ExpiresAt.After(now) {
		return auth.ErrUnauthenticated
	}
	// Issuance and password changes lock the user before its sessions. Keep
	// actor validation in the same order so a binding cannot deadlock a web
	// issuance holding the user while waiting for this approving session.
	var user model.User
	if err := lock(tx).First(&user, actor.User.ID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return auth.ErrUnauthenticated
		}
		return err
	}
	if user.Status != model.COMMON_STATUS_ENABLE {
		return auth.ErrDisabled
	}
	var sessions []model.UserToken
	if err := lock(tx).Where("token = ?", actor.Session.Token).Limit(2).Find(&sessions).Error; err != nil {
		return err
	}
	if len(sessions) != 1 || sessions[0].UserId != actor.User.ID || sessions[0].Id != actor.Session.ID || sessions[0].ExpiredAt <= now.Unix() {
		return auth.ErrUnauthenticated
	}
	return nil
}

func (s *Store) Bind(ctx context.Context, identity auth.ExternalIdentity, config auth.ProviderConfig, actor auth.Principal) error {
	if !valid(identity, config) {
		return auth.ErrInvalidInput
	}
	return classify(s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := checkProvider(tx, config); err != nil {
			return err
		}
		if err := checkActor(tx, actor, s.now().UTC()); err != nil {
			return err
		}
		var link model.OIDCIdentity
		err := lock(tx).Where("issuer = ? AND sub = ?", identity.Issuer, identity.Subject).First(&link).Error
		if err == nil {
			if link.UserID != actor.User.ID {
				return auth.ErrForbidden
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Create(&model.OIDCIdentity{Issuer: identity.Issuer, Subject: identity.Subject, UserID: actor.User.ID}).Error
	}))
}

func (s *Store) Unbind(ctx context.Context, config auth.ProviderConfig, actor auth.Principal) error {
	return classify(s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := checkProvider(tx, config); err != nil {
			return err
		}
		if err := checkActor(tx, actor, s.now().UTC()); err != nil {
			return err
		}
		// Keep an OIDC-only account's final login method; recovery uses the existing
		// administrator password reset instead of silently stranding the account.
		var user model.User
		if err := tx.First(&user, actor.User.ID).Error; err != nil {
			return err
		}
		if user.Password == "" {
			var providers []model.Oauth
			if err := tx.Where("oauth_type = ? AND issuer <> ?", model.OauthTypeOidc, config.Issuer).Find(&providers).Error; err != nil {
				return err
			}
			usable := false
			for _, candidate := range providers {
				other, err := provider(candidate)
				if err != nil {
					continue
				}
				var links int64
				if err := tx.Model(&model.OIDCIdentity{}).Where("user_id = ? AND issuer = ?", actor.User.ID, other.Issuer).Count(&links).Error; err != nil {
					return err
				}
				if links > 0 {
					usable = true
					break
				}
			}
			if !usable {
				return auth.ErrForbidden
			}
		}
		return tx.Where("user_id = ? AND issuer = ?", actor.User.ID, config.Issuer).Delete(&model.OIDCIdentity{}).Error
	}))
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, auth.ErrUnauthenticated, auth.ErrDisabled, auth.ErrForbidden, auth.ErrInvalidInput} {
		if errors.Is(err, known) {
			return known
		}
	}
	return auth.ErrUnavailable
}

func (s *Store) Links(ctx context.Context, actor auth.Principal) ([]auth.ProviderLink, error) {
	var result []auth.ProviderLink
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := checkActor(tx, actor, s.now().UTC()); err != nil {
			return err
		}
		var rows []model.Oauth
		if err := tx.Where("oauth_type = ?", model.OauthTypeOidc).Order("id").Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			config, err := provider(row)
			if err != nil {
				continue
			}
			var n int64
			if err := tx.Model(&model.OIDCIdentity{}).Where("user_id = ? AND issuer = ?", actor.User.ID, config.Issuer).Count(&n).Error; err != nil {
				return err
			}
			result = append(result, auth.ProviderLink{Op: config.Op, Linked: n > 0})
		}
		return nil
	})
	return result, classify(err)
}
