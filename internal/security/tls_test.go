package security

import (
	"crypto/tls"
	"strings"
	"testing"
)

func TestPinnedTLSHandshake(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity failed: %v", err)
	}

	// Loading again must preserve the exact same fingerprint and token.
	id2, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreateIdentity failed: %v", err)
	}
	if id.CertSHA256 != id2.CertSHA256 || id.AuthToken != id2.AuthToken {
		t.Fatal("expected persisted identity across reloads")
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", ServerTLSConfig(id.Certificate))
	if err != nil {
		t.Fatalf("tls.Listen failed: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn *tls.Conn) {
				defer conn.Close()
				_ = conn.Handshake()
			}(c.(*tls.Conn))
		}
	}()

	// Valid pin succeeds.
	goodCfg, err := PinnedClientTLSConfig(id.CertSHA256)
	if err != nil {
		t.Fatalf("PinnedClientTLSConfig failed: %v", err)
	}
	conn, err := tls.Dial("tcp", ln.Addr().String(), goodCfg)
	if err != nil {
		t.Fatalf("expected valid pinned TLS connection to succeed: %v", err)
	}
	if conn.ConnectionState().Version != tls.VersionTLS13 {
		t.Fatalf("expected TLS 1.3, got 0x%x", conn.ConnectionState().Version)
	}
	conn.Close()

	// Wrong pin must fail.
	badCfg, err := PinnedClientTLSConfig(strings.Repeat("0", 64))
	if err != nil {
		t.Fatalf("PinnedClientTLSConfig(bad) failed: %v", err)
	}
	if badConn, err := tls.Dial("tcp", ln.Addr().String(), badCfg); err == nil {
		badConn.Close()
		t.Fatal("expected mismatched TLS pin to be rejected")
	}
}
