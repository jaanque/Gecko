package admin

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"testing"
	"time"

	"remote-control/internal/config"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
)

func TestGenerateClientBinary(t *testing.T) {
	id, err := security.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity failed: %v", err)
	}
	hub := NewHub(id, 9443)
	fakeTemplate := []byte("MZ\x90\x00fake-client-exe-template-bytes")

	// Auto-appends default agent port when omitted.
	built, err := hub.GenerateClientBinary(fakeTemplate, "192.168.1.50")
	if err != nil {
		t.Fatalf("GenerateClientBinary failed: %v", err)
	}
	cfg, err := config.ReadFromBytes(built)
	if err != nil {
		t.Fatalf("ReadFromBytes failed: %v", err)
	}
	if cfg.ServerAddr != "192.168.1.50:9443" || cfg.CertSHA256 != id.CertSHA256 || cfg.AuthToken != id.AuthToken {
		t.Fatalf("config mismatch: %+v", cfg)
	}

	if _, err := hub.GenerateClientBinary(nil, "192.168.1.50:9443"); err == nil {
		t.Fatal("expected error with empty template")
	}
}

func TestAgentSessionLifecycle(t *testing.T) {
	id, err := security.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity failed: %v", err)
	}
	hub := NewHub(id, 9443)

	startedCh := make(chan string, 1)
	frameCh := make(chan []byte, 1)
	hub.SetEvents(HubEvents{
		OnSessionStarted: func(sid, clientID, hostname string, w, h int) {
			startedCh <- hostname
		},
		OnFrame: func(sid string, data []byte) {
			cp := make([]byte, len(data))
			copy(cp, data)
			frameCh <- cp
		},
	})

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

	// 1. Bad token must be rejected.
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
		t.Fatalf("expected auth resp for bad token, err=%v type=%d", err, mType)
	}
	var badResp protocol.AuthRespPayload
	_ = json.Unmarshal(data, &badResp)
	if badResp.OK {
		t.Fatal("expected bad token rejection")
	}

	// 2. Valid token registers client.
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
		t.Fatalf("expected OK auth resp, got %+v", okResp)
	}

	// Wait for client to appear online.
	var clientID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list := hub.ListClients()
		if len(list) == 1 && list[0].Hostname == "PC-OFICINA-01" {
			clientID = list[0].ID
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if clientID == "" {
		t.Fatal("client did not register online")
	}

	// 3. Admin requests connect -> Client receives MsgConsentReq -> Client allows -> Streams frame.
	if err := hub.RequestConnect(clientID); err != nil {
		t.Fatalf("RequestConnect failed: %v", err)
	}
	mType, data, err = protocol.ReadMessage(conn)
	if err != nil || mType != protocol.MsgConsentReq {
		t.Fatalf("expected MsgConsentReq, err=%v type=%d", err, mType)
	}
	var creq protocol.ConsentReqPayload
	_ = json.Unmarshal(data, &creq)

	if err := protocol.WriteJSON(conn, protocol.MsgConsentResp, protocol.ConsentRespPayload{
		SessionID: creq.SessionID,
		Allowed:   true,
	}); err != nil {
		t.Fatalf("write consent resp failed: %v", err)
	}

	select {
	case host := <-startedCh:
		if host != "PC-OFICINA-01" {
			t.Fatalf("unexpected started host %q", host)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OnSessionStarted")
	}

	wantFrame := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	if err := protocol.WriteMessage(conn, protocol.MsgFrame, wantFrame); err != nil {
		t.Fatalf("write frame failed: %v", err)
	}
	select {
	case gotFrame := <-frameCh:
		if !bytes.Equal(gotFrame, wantFrame) {
			t.Fatalf("frame mismatch: got %x, want %x", gotFrame, wantFrame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OnFrame")
	}

	// 4. Disconnect notifies client.
	hub.DisconnectSession("test done")
	mType, _, err = protocol.ReadMessage(conn)
	if err != nil || mType != protocol.MsgSessionStop {
		t.Fatalf("expected MsgSessionStop, err=%v type=%d", err, mType)
	}
}
