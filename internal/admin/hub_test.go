package admin

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"remote-control/internal/config"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
)

func TestGenerateClientEndpoint(t *testing.T) {
	id, err := security.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity failed: %v", err)
	}
	hub := NewHub(id)
	fakeTemplate := []byte("MZ\x90\x00fake-client-exe-template-bytes")
	uiToken := "0123456789abcdef0123456789abcdef"

	srv := NewWebServer(hub, id, fakeTemplate, 9443, uiToken, fstest.MapFS{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Unauthorized request must be rejected.
	reqNoAuth, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/generate-client", bytes.NewBufferString(`{"server_addr":"192.168.1.10:9443"}`))
	respNoAuth, err := http.DefaultClient.Do(reqNoAuth)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	respNoAuth.Body.Close()
	if respNoAuth.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without token, got %d", respNoAuth.StatusCode)
	}

	// Authorized request returns a valid executable with embedded config trailer.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/generate-client", bytes.NewBufferString(`{"server_addr":"192.168.1.10:9443"}`))
	req.Header.Set("X-Session-Token", uiToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authorized request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("reading response body failed: %v", err)
	}
	cfg, err := config.ReadFromBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("generated client config invalid: %v", err)
	}
	if cfg.ServerAddr != "192.168.1.10:9443" || cfg.CertSHA256 != id.CertSHA256 || cfg.AuthToken != id.AuthToken {
		t.Fatalf("generated config mismatch: %+v", cfg)
	}
}

func TestAgentRegistrationAndAuthRejection(t *testing.T) {
	id, err := security.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity failed: %v", err)
	}
	hub := NewHub(id)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", security.ServerTLSConfig(id.Certificate))
	if err != nil {
		t.Fatalf("tls.Listen failed: %v", err)
	}
	defer ln.Close()
	go func() { _ = hub.ServeAgents(ln) }()

	clientTLS, err := security.PinnedClientTLSConfig(id.CertSHA256)
	if err != nil {
		t.Fatalf("PinnedClientTLSConfig failed: %v", err)
	}

	// 1. Bad token should be rejected.
	badConn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	_ = protocol.WriteJSON(badConn, protocol.MsgAuth, protocol.AuthPayload{
		Token:    "wrong-token",
		Hostname: "BAD-PC",
	})
	mType, data, err := protocol.ReadMessage(badConn)
	badConn.Close()
	if err != nil || mType != protocol.MsgAuthResp {
		t.Fatalf("expected auth response for bad token, err=%v type=%d", err, mType)
	}
	var badResp protocol.AuthRespPayload
	_ = json.Unmarshal(data, &badResp)
	if badResp.OK {
		t.Fatal("expected bad token to be rejected")
	}

	// 2. Valid token should register the client as online.
	conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if err := protocol.WriteJSON(conn, protocol.MsgAuth, protocol.AuthPayload{
		Token:    id.AuthToken,
		Hostname: "PC-OFICINA-01",
		OS:       "windows",
		Width:    1920,
		Height:   1080,
	}); err != nil {
		t.Fatalf("write auth failed: %v", err)
	}

	mType, data, err = protocol.ReadMessage(conn)
	if err != nil || mType != protocol.MsgAuthResp {
		t.Fatalf("expected auth resp, err=%v type=%d", err, mType)
	}
	var okResp protocol.AuthRespPayload
	if err := json.Unmarshal(data, &okResp); err != nil || !okResp.OK {
		t.Fatalf("expected OK auth response, got %+v", okResp)
	}

	// Poll briefly for Hub list to show client.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list := hub.ListClients()
		if len(list) == 1 && list[0].Hostname == "PC-OFICINA-01" && list[0].Status == "online" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("client did not appear online: %+v", hub.ListClients())
}
