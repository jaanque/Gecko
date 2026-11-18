package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"remote-control/internal/config"
	"remote-control/internal/security"
)

func TestNormalizeHostPort(t *testing.T) {
	cases := map[string]string{
		"remoto.ejemplo.com":      "remoto.ejemplo.com:9443",
		"remoto.ejemplo.com:8443": "remoto.ejemplo.com:8443",
		" 10.0.0.5 ":              "10.0.0.5:9443",
		"[::1]:9443":              "[::1]:9443",
		"":                        "",
	}
	for in, want := range cases {
		if got := normalizeHostPort(in, "9443"); got != want {
			t.Errorf("normalizeHostPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRelayClientProfilePersistenceAndOfflineGeneration(t *testing.T) {
	id, err := security.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity failed: %v", err)
	}

	dataDir := t.TempDir()
	rc := NewRelayClient(dataDir)
	if _, ok := rc.Profile(); ok {
		t.Fatal("expected no profile initially")
	}

	if _, _, err := rc.GenerateClientBinary([]byte("MZ-stub"), nil, "", ""); err == nil {
		t.Fatal("expected error generating client before server is configured")
	}

	prof := security.RelayProfile{
		ServerAddr:    "127.0.0.1:19443",
		CertSHA256:    id.CertSHA256,
		AdminToken:    id.AdminToken,
		ClientToken:   id.ClientToken,
		AdminCertDER:  id.AdminCertDER,
		AdminKeyDER:   id.AdminKeyDER,
		ClientCertDER: id.ClientCertDER,
		ClientKeyDER:  id.ClientKeyDER,
	}
	if err := rc.Configure(prof); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}
	rc.Close()

	// Reload from disk and verify offline fallback binary generation uses ClientToken (never AdminToken).
	rc2 := NewRelayClient(dataDir)
	gotProf, ok := rc2.Profile()
	if !ok || gotProf.ServerAddr != prof.ServerAddr || gotProf.CertSHA256 != prof.CertSHA256 {
		t.Fatalf("reloaded profile mismatch: ok=%v got=%+v", ok, gotProf)
	}

	bin, credID, err := rc2.GenerateClientBinary([]byte("MZ\x90\x00stub"), nil, "offline", "")
	if err != nil {
		t.Fatalf("offline GenerateClientBinary failed: %v", err)
	}
	if credID != security.DefaultClientCredentialID {
		t.Fatalf("expected fallback default credential ID when offline, got %q", credID)
	}
	cfg, err := config.ReadFromBytes(bin)
	if err != nil {
		t.Fatalf("ReadFromBytes failed: %v", err)
	}
	if cfg.AuthToken != id.ClientToken || cfg.AuthToken == id.AdminToken {
		t.Fatal("offline generated client must embed ClientToken, never AdminToken")
	}
	if !bytes.Equal(cfg.ClientCertDER, id.ClientCertDER) {
		t.Fatal("offline generated client cert mismatch")
	}

	// SendFile without active session must fail fast.
	tmpFile := filepath.Join(t.TempDir(), "test.txt")
	_ = os.WriteFile(tmpFile, []byte("abc"), 0o600)
	if err := rc2.SendFile(tmpFile); err == nil {
		t.Fatal("expected SendFile without active connection/session to fail")
	}
}
