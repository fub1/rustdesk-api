// Package oidclogin coordinates the browser protocol, bounded attempts and
// transactional local admission. It has no HTTP/database/framework globals.
package oidclogin

import (
	"context"
	"errors"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/loginattempt"
)

type Protocol interface {
	AuthorizationURL(context.Context, string, string, string) (string, error)
	Exchange(context.Context, string, string, string) (auth.ExternalIdentity, error)
}
type Store interface {
	Provider(context.Context, string) (auth.ProviderConfig, error)
	Login(context.Context, auth.ExternalIdentity, auth.ProviderConfig, auth.IssueRequest) (auth.Principal, error)
	Bind(context.Context, auth.ExternalIdentity, auth.ProviderConfig, auth.Principal) error
	Unbind(context.Context, auth.ProviderConfig, auth.Principal) error
	Links(context.Context, auth.Principal) ([]auth.ProviderLink, error)
}
type Factory func(auth.ProviderConfig) (Protocol, error)
type Service struct {
	store    Store
	attempts *loginattempt.Store
	sessions *auth.Service
	issuer   *auth.Issuer
	factory  Factory
	webURL   string
}
type snapshot struct {
	config   auth.ProviderConfig
	protocol Protocol
	web      bool
}
type Started struct{ Code, URL string }
type WebApproval struct{ Actor auth.Principal }

func New(store Store, attempts *loginattempt.Store, sessions *auth.Service, issuer *auth.Issuer, factory Factory, webURL string) (*Service, error) {
	if store == nil || attempts == nil || sessions == nil || issuer == nil || factory == nil {
		return nil, errors.New("invalid OIDC login dependencies")
	}
	return &Service{store: store, attempts: attempts, sessions: sessions, issuer: issuer, factory: factory, webURL: webURL}, nil
}

func (s *Service) Begin(ctx context.Context, op string, device loginattempt.Device, actor *auth.Principal) (Started, error) {
	purpose := loginattempt.PurposeLogin
	binding := loginattempt.Binding{}
	if actor != nil {
		current, err := s.sessions.Authenticate(ctx, actor.Session.Token)
		if err != nil {
			return Started{}, err
		}
		if current.User.ID != actor.User.ID {
			return Started{}, auth.ErrUnauthenticated
		}
		binding = loginattempt.Binding{ActorID: current.User.ID, SessionToken: current.Session.Token}
		purpose = loginattempt.PurposeBind
	}
	var config auth.ProviderConfig
	var protocol Protocol
	web := op == "webauth"
	if web {
		if actor != nil || s.webURL == "" {
			return Started{}, auth.ErrInvalidInput
		}
		purpose = loginattempt.PurposeWebLogin
	} else {
		var err error
		config, err = s.store.Provider(ctx, op)
		if err != nil {
			return Started{}, err
		}
		protocol, err = s.factory(config)
		if err != nil {
			return Started{}, err
		}
	}
	a, err := s.attempts.Begin(ctx, loginattempt.BeginInput{Device: device, Purpose: purpose, Binding: binding, Provider: snapshot{config: config, protocol: protocol, web: web}})
	if err != nil {
		return Started{}, err
	}
	var url string
	if web {
		url = s.webURL + a.Code
	} else {
		url, err = protocol.AuthorizationURL(ctx, a.State, a.Nonce, a.Verifier)
	}
	if err != nil {
		_ = s.attempts.Forget(context.WithoutCancel(ctx), a.Code)
		return Started{}, err
	}
	return Started{Code: a.Code, URL: url}, nil
}

func (s *Service) Callback(ctx context.Context, state, code, providerError string, authorizationIssuer ...string) error {
	lease, err := s.attempts.ClaimState(ctx, state)
	if err != nil {
		return err
	}
	a := lease.Attempt()
	snap, ok := a.Provider.(snapshot)
	fail := func(err error) error { _ = s.attempts.Fail(context.WithoutCancel(ctx), lease, err); return err }
	work, cancel, err := s.attempts.ContextForLease(ctx, lease)
	if err != nil {
		return fail(err)
	}
	defer cancel()
	if !ok || snap.web || snap.protocol == nil || providerError != "" || code == "" {
		return fail(auth.ErrUnauthenticated)
	}
	if len(authorizationIssuer) > 1 || len(authorizationIssuer) == 1 && authorizationIssuer[0] != "" && authorizationIssuer[0] != snap.config.Issuer {
		return fail(auth.ErrUnauthenticated)
	}
	identity, err := snap.protocol.Exchange(work, code, a.Nonce, a.Verifier)
	if err != nil {
		return fail(err)
	}
	if identity.Issuer != snap.config.Issuer || identity.Subject == "" {
		return fail(auth.ErrUnauthenticated)
	}
	if err := s.attempts.CheckLease(work, lease); err != nil {
		return fail(err)
	}
	if a.Purpose == loginattempt.PurposeBind {
		actor, err := s.sessions.Authenticate(work, a.Binding.SessionToken)
		if err != nil {
			return fail(err)
		}
		if actor.User.ID != a.Binding.ActorID {
			return fail(auth.ErrUnauthenticated)
		}
		bindingWork, cancelBinding := context.WithDeadline(work, actor.Session.ExpiresAt)
		defer cancelBinding()
		if err := s.attempts.CheckLease(bindingWork, lease); err != nil {
			return fail(err)
		}
		if err := s.store.Bind(bindingWork, identity, snap.config, actor); err != nil {
			return fail(err)
		}
	}
	if err := s.attempts.Complete(work, lease, identity); err != nil {
		return fail(err)
	}
	return nil
}

func (s *Service) Poll(ctx context.Context, code string, device loginattempt.Device, web bool, ip string) (auth.Principal, error) {
	purpose := loginattempt.PurposeLogin
	if web {
		purpose = loginattempt.PurposeWebLogin
	}
	result, err := s.attempts.Poll(ctx, loginattempt.PollInput{Code: code, Device: device, Purpose: purpose}, func(ctx context.Context, a loginattempt.Attempt) (any, error) {
		snap, ok := a.Provider.(snapshot)
		if !ok {
			return nil, auth.ErrInvalidInput
		}
		request := auth.IssueRequest{DeviceID: a.Device.ID, DeviceUUID: a.Device.UUID, Client: a.Device.Client, Platform: a.Device.Platform, IP: ip, Method: "oauth"}
		if a.Device.Client == "admin" {
			request.Client = "webadmin"
			request.DeviceUUID = ""
		} else {
			request.Client = "app"
		}
		if snap.web {
			approval, ok := a.Identity.(WebApproval)
			if !ok {
				return nil, auth.ErrInvalidInput
			}
			current, err := s.sessions.Authenticate(ctx, approval.Actor.Session.Token)
			if err != nil {
				return nil, err
			}
			if current.User.ID != approval.Actor.User.ID {
				return nil, auth.ErrUnauthenticated
			}
			request.UserID = current.User.ID
			request.ExpectedPassword = &current.User.PasswordHash
			request.SourceSession = &current.Session
			request.Method = "account"
			issuanceWork, cancelIssuance := context.WithDeadline(ctx, current.Session.ExpiresAt)
			defer cancelIssuance()
			return s.issuer.Issue(issuanceWork, request)
		}
		identity, ok := a.Identity.(auth.ExternalIdentity)
		if !ok {
			return nil, auth.ErrInvalidInput
		}
		return s.store.Login(ctx, identity, snap.config, request)
	})
	if err != nil {
		return auth.Principal{}, err
	}
	p, ok := result.(auth.Principal)
	if !ok {
		return auth.Principal{}, auth.ErrUnavailable
	}
	current, err := s.sessions.Authenticate(ctx, p.Session.Token)
	if err != nil {
		return auth.Principal{}, err
	}
	if current.User.ID != p.User.ID || current.Session.ID != p.Session.ID {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return current, nil
}

// PollEither retains the existing poll wire format (which has no op/purpose).
// A purpose mismatch cannot issue; only the exact original flow is retried.
func (s *Service) PollEither(ctx context.Context, code string, device loginattempt.Device, ip string) (auth.Principal, error) {
	p, err := s.Poll(ctx, code, device, false, ip)
	if errors.Is(err, loginattempt.ErrForbidden) {
		return s.Poll(ctx, code, device, true, ip)
	}
	return p, err
}

func (s *Service) Unbind(ctx context.Context, op string, actor auth.Principal) error {
	config, err := s.store.Provider(ctx, op)
	if err != nil {
		return err
	}
	current, err := s.sessions.Authenticate(ctx, actor.Session.Token)
	if err != nil {
		return err
	}
	if current.User.ID != actor.User.ID {
		return auth.ErrUnauthenticated
	}
	return s.store.Unbind(ctx, config, current)
}

func (s *Service) Inspect(ctx context.Context, code string, actor auth.Principal) (loginattempt.Attempt, error) {
	// Only authenticated callers reach this application operation.
	current, err := s.sessions.Authenticate(ctx, actor.Session.Token)
	if err != nil {
		return loginattempt.Attempt{}, err
	}
	if current.User.ID != actor.User.ID {
		return loginattempt.Attempt{}, auth.ErrUnauthenticated
	}
	a, err := s.attempts.Inspect(ctx, code, loginattempt.Binding{ActorID: actor.User.ID, SessionToken: actor.Session.Token})
	if err == nil {
		return a, nil
	}
	return s.attempts.Metadata(ctx, code)
}

func (s *Service) ApproveWeb(ctx context.Context, code string, actor auth.Principal) error {
	current, err := s.sessions.Authenticate(ctx, actor.Session.Token)
	if err != nil {
		return err
	}
	if current.User.ID != actor.User.ID {
		return auth.ErrUnauthenticated
	}
	a, err := s.attempts.Metadata(ctx, code)
	if err != nil {
		return err
	}
	snap, ok := a.Provider.(snapshot)
	if !ok || !snap.web {
		return auth.ErrInvalidInput
	}
	return s.attempts.Approve(ctx, code, WebApproval{Actor: current})
}

func (s *Service) MyOauth(ctx context.Context, actor auth.Principal) ([]auth.ProviderLink, error) {
	current, err := s.sessions.Authenticate(ctx, actor.Session.Token)
	if err != nil {
		return nil, err
	}
	if current.User.ID != actor.User.ID {
		return nil, auth.ErrUnauthenticated
	}
	return s.store.Links(ctx, current)
}
