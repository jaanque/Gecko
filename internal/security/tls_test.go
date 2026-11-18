package security

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"
)

func TestMutualTLSHandshakeAndProfileKey(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity failed: %v", err)
	}
	if id.AdminToken == id.ClientToken {
		t.Fatal("admin and client tokens must be distinct")
	}

	// Loading again must preserve fingerprint, mTLS certs, and both tokens.
	id2, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreateIdentity failed: %v", err)
	}
	if id.CertSHA256 != id2.CertSHA256 ||
		id.AdminToken != id2.AdminToken ||
		id.ClientToken != id2.ClientToken ||
		!bytes.Equal(id.AdminCertDER, id2.AdminCertDER) ||
		!bytes.Equal(id.ClientCertDER, id2.ClientCertDER) {
		t.Fatal("expected persisted mTLS identity across reloads")
	}

	// Profile key round-trip (RMT2: compressed + mTLS certs).
	prof := RelayProfile{
		ServerAddr:    "remoto.ejemplo.com:9443",
		CertSHA256:    id.CertSHA256,
		AdminToken:    id.AdminToken,
		ClientToken:   id.ClientToken,
		AdminCertDER:  id.AdminCertDER,
		AdminKeyDER:   id.AdminKeyDER,
		ClientCertDER: id.ClientCertDER,
		ClientKeyDER:  id.ClientKeyDER,
	}
	key, err := EncodeProfileKey(prof)
	if err != nil {
		t.Fatalf("EncodeProfileKey failed: %v", err)
	}
	gotProf, err := DecodeProfileKey(key)
	if err != nil {
		t.Fatalf("DecodeProfileKey failed: %v", err)
	}
	if gotProf.ServerAddr != prof.ServerAddr ||
		gotProf.CertSHA256 != prof.CertSHA256 ||
		gotProf.AdminToken != prof.AdminToken ||
		gotProf.ClientToken != prof.ClientToken ||
		!bytes.Equal(gotProf.AdminCertDER, prof.AdminCertDER) ||
		!bytes.Equal(gotProf.ClientCertDER, prof.ClientCertDER) {
		t.Fatalf("DecodeProfileKey mismatch")
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", ServerTLSConfig(id.Certificate, id.ClientCAs))
	if err != nil {
		t.Fatalf("tls.Listen failed: %v", err)
	}
	defer ln.Close()

	roleCh := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn *tls.Conn) {
				defer conn.Close()
				if err := conn.Handshake(); err != nil {
					return
				}
				if role, _, err := PeerIdentityFromTLS(conn); err == nil {
					roleCh <- role
				}
			}(c.(*tls.Conn))
		}
	}()

	// 1. Valid Admin mTLS cert + server pin succeeds with TLS 1.3 and role "admin".
	admCfg, err := MutualPinnedClientTLSConfig(id.CertSHA256, id.AdminCertDER, id.AdminKeyDER)
	if err != nil {
		t.Fatalf("MutualPinnedClientTLSConfig(admin) failed: %v", err)
	}
	conn, err := tls.Dial("tcp", ln.Addr().String(), admCfg)
	if err != nil {
		t.Fatalf("expected valid admin mTLS connection to succeed: %v", err)
	}
	if err := VerifyServerTLSConn(conn, id.CertSHA256); err != nil {
		t.Fatalf("VerifyServerTLSConn failed: %v", err)
	}
	conn.Close()
	if gotRole := <-roleCh; gotRole != RoleAdmin {
		t.Fatalf("expected role %q, got %q", RoleAdmin, gotRole)
	}

	// 2. Valid Client mTLS cert + server pin succeeds with role "client".
	cliCfg, err := MutualPinnedClientTLSConfig(id.CertSHA256, id.ClientCertDER, id.ClientKeyDER)
	if err != nil {
		t.Fatalf("MutualPinnedClientTLSConfig(client) failed: %v", err)
	}
	cliConn, err := tls.Dial("tcp", ln.Addr().String(), cliCfg)
	if err != nil {
		t.Fatalf("expected valid client mTLS connection to succeed: %v", err)
	}
	cliConn.Close()
	if gotRole := <-roleCh; gotRole != RoleClient {
		t.Fatalf("expected role %q, got %q", RoleClient, gotRole)
	}

	// 3. Unauthenticated client WITHOUT mTLS certificate must fail at TLS handshake.
	noCertCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
	}
	if badConn, err := tls.Dial("tcp", ln.Addr().String(), noCertCfg); err == nil {
		// In TLS 1.3, server alert on missing client cert is received on first Read.
		buf := make([]byte, 1)
		_, rerr := badConn.Read(buf)
		badConn.Close()
		if rerr == nil {
			t.Fatal("expected server to reject connection without mTLS client certificate")
		}
	}

	// 4. Client with a certificate signed by a DIFFERENT CA must be rejected.
	otherID, err := LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity(other) failed: %v", err)
	}
	untrustedCfg, err := MutualPinnedClientTLSConfig(id.CertSHA256, otherID.ClientCertDER, otherID.ClientKeyDER)
	if err != nil {
		t.Fatalf("MutualPinnedClientTLSConfig(other) failed: %v", err)
	}
	if badConn, err := tls.Dial("tcp", ln.Addr().String(), untrustedCfg); err == nil {
		buf := make([]byte, 1)
		_, rerr := badConn.Read(buf)
		badConn.Close()
		if rerr == nil {
			t.Fatal("expected server to reject client certificate from unknown CA")
		}
	}

	// 5. Wrong server pin must fail client-side.
	badPinCfg, err := MutualPinnedClientTLSConfig(strings.Repeat("0", 64), id.ClientCertDER, id.ClientKeyDER)
	if err != nil {
		t.Fatalf("MutualPinnedClientTLSConfig(bad pin) failed: %v", err)
	}
	if badConn, err := tls.Dial("tcp", ln.Addr().String(), badPinCfg); err == nil {
		badConn.Close()
		t.Fatal("expected mismatched server TLS pin to be rejected")
	}
}

func TestPerClientCredentialIssuanceAndRevocation(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity failed: %v", err)
	}
	if !id.IsNew {
		t.Fatal("expected IsNew=true on initial identity creation")
	}

	cred, err := id.IssueClientCredential("Portatil-Soporte")
	if err != nil {
		t.Fatalf("IssueClientCredential failed: %v", err)
	}
	if cred.ID == "" || len(cred.ClientToken) < 32 {
		t.Fatalf("invalid issued credential: %+v", cred)
	}

	leaf, err := x509.ParseCertificate(cred.ClientCertDER)
	if err != nil {
		t.Fatalf("ParseCertificate failed: %v", err)
	}
	gotID, ok := id.VerifyClientCredential(leaf, cred.ClientToken)
	if !ok || gotID != cred.ID {
		t.Fatalf("VerifyClientCredential failed: got (%q, %v), want (%q, true)", gotID, ok, cred.ID)
	}
	if _, ok := id.VerifyClientCredential(leaf, id.ClientToken); ok {
		t.Fatal("unique client cert must not accept legacy shared token")
	}

	// Reload identity from disk: IsNew must be false and issued credential must remain valid.
	reloaded, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.IsNew {
		t.Fatal("expected IsNew=false on reload")
	}
	if gotID, ok := reloaded.VerifyClientCredential(leaf, cred.ClientToken); !ok || gotID != cred.ID {
		t.Fatalf("reloaded VerifyClientCredential failed: (%q, %v)", gotID, ok)
	}

	// Revoke and verify immediate rejection + persistence across reload.
	if err := reloaded.RevokeClient(cred.ID); err != nil {
		t.Fatalf("RevokeClient failed: %v", err)
	}
	if _, ok := reloaded.VerifyClientCredential(leaf, cred.ClientToken); ok {
		t.Fatal("revoked client credential must be rejected")
	}

	reloaded2, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("reload2 failed: %v", err)
	}
	if _, ok := reloaded2.VerifyClientCredential(leaf, cred.ClientToken); ok {
		t.Fatal("revoked client credential must remain rejected after reload")
	}
	list := reloaded2.ListIssuedClients()
	if len(list) != 1 || !list[0].Revoked || list[0].Label != "Portatil-Soporte" {
		t.Fatalf("unexpected issued list: %+v", list)
	}
}
