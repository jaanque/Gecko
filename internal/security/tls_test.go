package security

import (
	"crypto/tls"
	"strings"
	"testing"
)

func TestPinnedTLSHandshakeAndProfileKey(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity failed: %v", err)
	}
	if id.AdminToken == id.ClientToken {
		t.Fatal("admin and client tokens must be distinct")
	}

	// Loading again must preserve fingerprint and both tokens.
	id2, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreateIdentity failed: %v", err)
	}
	if id.CertSHA256 != id2.CertSHA256 || id.AdminToken != id2.AdminToken || id.ClientToken != id2.ClientToken {
		t.Fatal("expected persisted identity across reloads")
	}

	// Profile key round-trip.
	prof := RelayProfile{
		ServerAddr:  "remoto.ejemplo.com:9443",
		CertSHA256:  id.CertSHA256,
		AdminToken:  id.AdminToken,
		ClientToken: id.ClientToken,
	}
	key, err := EncodeProfileKey(prof)
	if err != nil {
		t.Fatalf("EncodeProfileKey failed: %v", err)
	}
	gotProf, err := DecodeProfileKey(key)
	if err != nil || gotProf != prof {
		t.Fatalf("DecodeProfileKey mismatch: got %+v err=%v", gotProf, err)
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

	// Valid pin succeeds with TLS 1.3.
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
