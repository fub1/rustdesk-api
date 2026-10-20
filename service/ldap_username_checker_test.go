package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/config"
)

// These are real LDAP wire responses, independent of the production checker.
func namespaceTLV(tag byte, value []byte) []byte {
	prefix := []byte{tag, byte(len(value))}
	if len(value) >= 128 {
		prefix = []byte{tag, 0x82, byte(len(value) >> 8), byte(len(value))}
	}
	return append(prefix, value...)
}
func namespacePacket(id, tag byte, payload []byte) []byte {
	return namespaceTLV(0x30, append([]byte{0x02, 0x01, id}, namespaceTLV(tag, payload)...))
}
func namespaceRequest(c net.Conn) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(c, header[:]); err != nil {
		return 0, nil, err
	}
	length := int(header[1])
	if length >= 128 {
		count := length & 127
		if count == 0 || count > 2 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		encoded := make([]byte, count)
		if _, err := io.ReadFull(c, encoded); err != nil {
			return 0, nil, err
		}
		length = 0
		for _, part := range encoded {
			length = length<<8 | int(part)
		}
	}
	if header[0] != 0x30 || length < 5 || length > 16384 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	data := make([]byte, length)
	_, err := io.ReadFull(c, data)
	if err != nil || data[0] != 0x02 || data[1] != 1 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return data[2], data, nil
}
func namespaceFixture(t *testing.T, mode string, tlsConfig *tls.Config) (config.Ldap, <-chan struct{}, <-chan []byte) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	scheme := "ldap"
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
		scheme = "ldaps"
	}
	phase, request, finished := make(chan struct{}), make(chan []byte, 1), make(chan struct{})
	go func() {
		defer close(finished)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		id, _, err := namespaceRequest(conn)
		if err != nil {
			return
		}
		if mode == "bind-block" {
			close(phase)
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		result := []byte{0x0a, 1, 0, 4, 0, 4, 0}
		if _, err := conn.Write(namespacePacket(id, 0x61, result)); err != nil {
			return
		}
		id, data, err := namespaceRequest(conn)
		if err != nil {
			return
		}
		request <- data
		close(phase)
		if mode == "search-block" {
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		if mode == "found" {
			entry := append(namespaceTLV(4, []byte("uid=found,dc=test")), namespaceTLV(0x30, nil)...)
			_, _ = conn.Write(namespacePacket(id, 0x64, entry))
		}
		if mode == "search-error" {
			result[2] = 50 // LDAP insufficientAccessRights.
		}
		_, _ = conn.Write(namespacePacket(id, 0x65, result))
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("directory fixture did not stop after the checker closed its connection")
		}
	})
	return config.Ldap{Enable: true, Url: scheme + "://" + listener.Addr().String(), BindDn: "cn=service", BindPassword: "fixture-private-password", BaseDn: "dc=test", User: config.LdapUser{Filter: "(objectClass=person)"}}, phase, request
}

func TestLDAPNamespaceCheckerSnapshotAndRealSearch(t *testing.T) {
	for _, mode := range []string{"found", "absent", "search-error"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _, request := namespaceFixture(t, mode, nil)
			check := NewLDAPUsernameChecker(cfg, 2*time.Second)
			cfg.Url, cfg.User.Filter = "invalid://changed", "(wrong=changed)"
			exists, err := check(context.Background(), "snapshot-user")
			if mode == "search-error" {
				if exists || !errors.Is(err, ErrLdapSearchFailed) {
					t.Fatal("directory failure was treated as an available username", err)
				}
			} else if err != nil || exists != (mode == "found") {
				t.Fatal("directory search result lost", exists, err)
			}
			if raw := <-request; !bytes.Contains(raw, []byte("person")) || !bytes.Contains(raw, []byte("snapshot-user")) || bytes.Contains(raw, []byte("changed")) {
				t.Fatal("checker did not use the captured directory configuration")
			}
		})
	}
}

func TestLDAPNamespaceCheckerCancellationAndBudget(t *testing.T) {
	for _, mode := range []string{"bind-block", "search-block"} {
		for _, cancelRequest := range []bool{false, true} {
			t.Run(mode+map[bool]string{true: "/cancel", false: "/timeout"}[cancelRequest], func(t *testing.T) {
				cfg, reached, _ := namespaceFixture(t, mode, nil)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				budget := 125 * time.Millisecond
				if cancelRequest {
					budget = 2 * time.Second
				}
				check := NewLDAPUsernameChecker(cfg, budget)
				result := make(chan error, 1)
				go func() { _, err := check(ctx, "bounded-user"); result <- err }()
				select {
				case <-reached:
				case <-time.After(time.Second):
					t.Fatal("actual LDAP operation was not reached")
				}
				if cancelRequest {
					cancel()
				}
				select {
				case err := <-result:
					if err == nil || (cancelRequest && !errors.Is(err, context.Canceled)) {
						t.Fatal("unbounded or successful cancelled directory check", err)
					}
				case <-time.After(time.Second):
					t.Fatal("LDAP operation ignored cancellation or budget")
				}
			})
		}
	}
}

func TestLDAPNamespaceCheckerTLSAndDisabledDirectory(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	cfg, _, _ := namespaceFixture(t, "found", server.TLS.Clone())
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.TLS.Certificates[0].Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.TlsVerify, cfg.TlsCaFile = true, caFile
	if exists, err := NewLDAPUsernameChecker(cfg, time.Second)(context.Background(), "tls-user"); err != nil || !exists {
		t.Fatal("verified directory TLS search failed", exists, err)
	}
	if exists, err := NewLDAPUsernameChecker(config.Ldap{}, 0)(context.Background(), "local-user"); err != nil || exists {
		t.Fatal("disabled directory did not leave local creation available", exists, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewLDAPUsernameChecker(config.Ldap{}, 0)(ctx, "local-user"); !errors.Is(err, context.Canceled) {
		t.Fatal("disabled directory ignored request cancellation", err)
	}
}
