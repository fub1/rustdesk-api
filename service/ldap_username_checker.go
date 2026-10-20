package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/lejianwen/rustdesk-api/v2/config"
)

// NewLDAPUsernameChecker snapshots the existing directory namespace check for
// administrator account creation. It does not authenticate or synchronize users.
// Directory failures fail closed; request cancellation closes the actual socket.
func NewLDAPUsernameChecker(snapshot config.Ldap, budget time.Duration) func(context.Context, string) (bool, error) {
	if budget <= 0 {
		budget = 5 * time.Second
	}
	return func(parent context.Context, username string) (bool, error) {
		ctx, cancel := context.WithTimeout(parent, budget)
		defer cancel()
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !snapshot.Enable {
			return false, nil
		}
		raw, encrypted, err := dialLDAPNamespace(ctx, snapshot)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, ErrLdapSearchFailed
		}
		defer raw.Close()
		deadline, _ := ctx.Deadline()
		if err := raw.SetDeadline(deadline); err != nil {
			return false, ErrLdapSearchFailed
		}
		done := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			_ = raw.Close()
			close(done)
		})
		defer func() {
			if !stop() {
				<-done
			}
		}()
		conn := ldap.NewConn(raw, encrypted)
		conn.SetTimeout(time.Until(deadline))
		conn.Start()
		defer conn.Close()
		if err := conn.Bind(snapshot.BindDn, snapshot.BindPassword); err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, ErrLdapSearchFailed
		}
		legacy := &LdapService{}
		request := legacy.buildUserSearchRequest(&snapshot,
			legacy.filterField(legacy.fieldUsername(&snapshot), ldap.EscapeFilter(username)))
		result, err := conn.Search(request)
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if err != nil {
			return false, ErrLdapSearchFailed
		}
		return len(result.Entries) > 0, nil
	}
}

func dialLDAPNamespace(ctx context.Context, cfg config.Ldap) (net.Conn, bool, error) {
	u, err := url.Parse(cfg.Url)
	if err != nil {
		return nil, false, ErrUrlParseFailed
	}
	dialer := &net.Dialer{}
	if u.Scheme == "ldapi" {
		path := u.Path
		if path == "" || path == "/" {
			path = "/var/run/slapd/ldapi"
		}
		conn, err := dialer.DialContext(ctx, "unix", path)
		return conn, false, err
	}
	port := u.Port()
	if port == "" {
		port = "389"
		if u.Scheme == "ldaps" {
			port = "636"
		}
	}
	address := net.JoinHostPort(u.Hostname(), port)
	switch u.Scheme {
	case "ldap", "cldap":
		network := "tcp"
		if u.Scheme == "cldap" {
			network = "udp"
		}
		conn, err := dialer.DialContext(ctx, network, address)
		return conn, false, err
	case "ldaps":
		settings := &tls.Config{InsecureSkipVerify: !cfg.TlsVerify}
		if cfg.TlsCaFile != "" {
			data, err := os.ReadFile(cfg.TlsCaFile)
			if err != nil {
				return nil, false, ErrFileReadFailed
			}
			settings.RootCAs = x509.NewCertPool()
			if !settings.RootCAs.AppendCertsFromPEM(data) {
				return nil, false, ErrLdapTlsFailed
			}
		}
		conn, err := (&tls.Dialer{NetDialer: dialer, Config: settings}).DialContext(ctx, "tcp", address)
		return conn, true, err
	default:
		return nil, false, ErrUrlParseFailed
	}
}
