package relay_test

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"testing"
	"time"

	"remote-control/internal/admin"
	"remote-control/internal/config"
	"remote-control/internal/protocol"
	"remote-control/internal/relay"
	"remote-control/internal/security"
)

func TestFullBridgeLifecycleAndRoleIsolation(t *testing.T) {
	id, err := security.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity failed: %v", err)
	}

	srv := relay.NewServer(id)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", security.ServerTLSConfig(id.Certificate, id.ClientCAs))
	if err != nil {
		t.Fatalf("tls.Listen failed: %v", err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln) }()

	clientTLS, err := security.MutualPinnedClientTLSConfig(id.CertSHA256, id.ClientCertDER, id.ClientKeyDER)
	if err != nil {
		t.Fatalf("MutualPinnedClientTLSConfig(client) failed: %v", err)
	}
	adminTLS, err := security.MutualPinnedClientTLSConfig(id.CertSHA256, id.AdminCertDER, id.AdminKeyDER)
	if err != nil {
		t.Fatalf("MutualPinnedClientTLSConfig(admin) failed: %v", err)
	}

	// 0. mTLS enforcement: peer WITHOUT client certificate must be rejected at TLS handshake.
	noCertConn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
	})
	if err == nil {
		_, _, rerr := protocol.ReadMessage(noCertConn)
		noCertConn.Close()
		if rerr == nil {
			t.Fatal("expected unauthenticated peer without mTLS cert to be rejected")
		}
	}

	// 1. Role isolation: Client mTLS cert MUST NOT authenticate as Admin (even with valid AdminToken!).
	badAdm, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	_ = protocol.WriteJSON(badAdm, protocol.MsgAdminAuth, protocol.AdminAuthPayload{Token: id.AdminToken})
	mType, data, err := protocol.ReadMessage(badAdm)
	badAdm.Close()
	if err != nil || mType != protocol.MsgAuthResp {
		t.Fatalf("expected auth resp, err=%v type=%d", err, mType)
	}
	var badResp protocol.AuthRespPayload
	_ = json.Unmarshal(data, &badResp)
	if badResp.OK {
		t.Fatal("client mTLS cert must be rejected on MsgAdminAuth even with admin token")
	}

	// 2. Role isolation: Admin mTLS cert MUST NOT register as Client (even with ClientToken!).
	badCli, err := tls.Dial("tcp", ln.Addr().String(), adminTLS)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	_ = protocol.WriteJSON(badCli, protocol.MsgAuth, protocol.AuthPayload{Token: id.ClientToken, Hostname: "FAKE"})
	mType, data, err = protocol.ReadMessage(badCli)
	badCli.Close()
	if err != nil || mType != protocol.MsgAuthResp {
		t.Fatalf("expected auth resp, err=%v type=%d", err, mType)
	}
	badResp = protocol.AuthRespPayload{}
	_ = json.Unmarshal(data, &badResp)
	if badResp.OK {
		t.Fatal("admin mTLS cert must be rejected on MsgAuth even with client token")
	}

	// 3. Connect real Admin RelayClient.
	admClient := admin.NewRelayClient(t.TempDir())
	defer admClient.Close()

	connectedCh := make(chan bool, 4)
	clientsCh := make(chan []protocol.ClientSummary, 8)
	startedCh := make(chan int, 1)
	frameCh := make(chan []byte, 1)

	admClient.SetEvents(admin.RelayEvents{
		OnRelayStatus: func(connected bool, _ string) {
			if connected {
				select {
				case connectedCh <- true:
				default:
				}
			}
		},
		OnClientsChanged: func(clients []protocol.ClientSummary) {
			clientsCh <- clients
		},
		OnSessionStarted: func(_, _, _ string, _, _, monCount int) {
			startedCh <- monCount
		},
		OnFrame: func(_ string, frame []byte) {
			cp := make([]byte, len(frame))
			copy(cp, frame)
			frameCh <- cp
		},
	})

	if err := admClient.Configure(security.RelayProfile{
		ServerAddr:    ln.Addr().String(),
		CertSHA256:    id.CertSHA256,
		AdminToken:    id.AdminToken,
		ClientToken:   id.ClientToken,
		AdminCertDER:  id.AdminCertDER,
		AdminKeyDER:   id.AdminKeyDER,
		ClientCertDER: id.ClientCertDER,
		ClientKeyDER:  id.ClientKeyDER,
	}); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}

	select {
	case <-connectedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("admin did not connect to relay")
	}

	// Verify generated client binary uses ClientToken + Client mTLS cert (NEVER Admin credentials!).
	genBin, err := admClient.GenerateClientBinary([]byte("MZ\x90\x00fake-template"), nil)
	if err != nil {
		t.Fatalf("GenerateClientBinary failed: %v", err)
	}
	genCfg, err := config.ReadFromBytes(genBin)
	if err != nil {
		t.Fatalf("ReadFromBytes failed: %v", err)
	}
	if genCfg.AuthToken != id.ClientToken || genCfg.AuthToken == id.AdminToken {
		t.Fatal("generated client must embed ClientToken, never AdminToken")
	}
	if !bytes.Equal(genCfg.ClientCertDER, id.ClientCertDER) || bytes.Equal(genCfg.ClientCertDER, id.AdminCertDER) {
		t.Fatal("generated client must embed client mTLS cert, never admin mTLS cert")
	}

	// 4. Connect a simulated remote PC Client.
	cliConn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("client dial failed: %v", err)
	}
	defer cliConn.Close()

	if err := protocol.WriteJSON(cliConn, protocol.MsgAuth, protocol.AuthPayload{
		Token:    id.ClientToken,
		Hostname: "PC-REMOTO-EXTERNO",
		Width:    1920,
		Height:   1080,
		Monitors: []protocol.MonitorInfo{
			{Index: 0, Width: 1920, Height: 1080, Primary: true},
			{Index: 1, Width: 1920, Height: 1080, Primary: false},
		},
	}); err != nil {
		t.Fatalf("client auth write failed: %v", err)
	}
	mType, data, err = protocol.ReadMessage(cliConn)
	if err != nil || mType != protocol.MsgAuthResp {
		t.Fatalf("expected client auth resp, err=%v type=%d", err, mType)
	}
	var okResp protocol.AuthRespPayload
	if json.Unmarshal(data, &okResp) != nil || !okResp.OK {
		t.Fatalf("client auth failed: %+v", okResp)
	}

	// Wait for Admin to see the online PC.
	var targetID string
	deadline := time.After(3 * time.Second)
	for targetID == "" {
		select {
		case list := <-clientsCh:
			for _, c := range list {
				if c.Hostname == "PC-REMOTO-EXTERNO" {
					targetID = c.ID
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for client in admin list")
		}
	}

	// 5. Admin requests control -> Client allows -> Relay forwards frame & input both ways.
	if err := admClient.RequestConnect(targetID); err != nil {
		t.Fatalf("RequestConnect failed: %v", err)
	}
	mType, data, err = protocol.ReadMessage(cliConn)
	if err != nil || mType != protocol.MsgConsentReq {
		t.Fatalf("expected MsgConsentReq, err=%v type=%d", err, mType)
	}
	var creq protocol.ConsentReqPayload
	_ = json.Unmarshal(data, &creq)

	_ = protocol.WriteJSON(cliConn, protocol.MsgConsentResp, protocol.ConsentRespPayload{
		SessionID: creq.SessionID,
		Allowed:   true,
	})

	select {
	case monCount := <-startedCh:
		if monCount != 2 {
			t.Fatalf("expected 2 monitors, got %d", monCount)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnSessionStarted")
	}

	// Frame Client -> Relay -> Admin.
	wantFrame := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	_ = protocol.WriteMessage(cliConn, protocol.MsgFrame, wantFrame)
	select {
	case got := <-frameCh:
		if !bytes.Equal(got, wantFrame) {
			t.Fatalf("relayed frame mismatch: got %x want %x", got, wantFrame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for relayed frame")
	}

	// Input Admin -> Relay -> Client.
	admClient.SendSelectMonitor(-1)
	mType, data, err = protocol.ReadMessage(cliConn)
	if err != nil || mType != protocol.MsgStreamConfig {
		t.Fatalf("expected relayed MsgStreamConfig, err=%v type=%d", err, mType)
	}
	var sc protocol.StreamConfigPayload
	_ = json.Unmarshal(data, &sc)
	if sc.Monitor == nil || *sc.Monitor != -1 {
		t.Fatalf("expected monitor -1, got %+v", sc.Monitor)
	}

	// Disconnect Admin -> Relay -> Client.
	admClient.DisconnectSession("done")
	mType, _, err = protocol.ReadMessage(cliConn)
	if err != nil || mType != protocol.MsgSessionStop {
		t.Fatalf("expected MsgSessionStop, err=%v type=%d", err, mType)
	}
}

func TestRelayClientReconfigureDoesNotClobberConn(t *testing.T) {
	id, err := security.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity failed: %v", err)
	}
	srv := relay.NewServer(id)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", security.ServerTLSConfig(id.Certificate, id.ClientCAs))
	if err != nil {
		t.Fatalf("tls.Listen failed: %v", err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln) }()

	admClient := admin.NewRelayClient(t.TempDir())
	defer admClient.Close()

	connectedCh := make(chan bool, 8)
	admClient.SetEvents(admin.RelayEvents{
		OnRelayStatus: func(connected bool, _ string) {
			if connected {
				select {
				case connectedCh <- true:
				default:
				}
			}
		},
	})

	// Trigger a first connection, then immediately reconfigure/restart multiple times.
	prof := security.RelayProfile{
		ServerAddr:    ln.Addr().String(),
		CertSHA256:    id.CertSHA256,
		AdminToken:    id.AdminToken,
		ClientToken:   id.ClientToken,
		AdminCertDER:  id.AdminCertDER,
		AdminKeyDER:   id.AdminKeyDER,
		ClientCertDER: id.ClientCertDER,
		ClientKeyDER:  id.ClientKeyDER,
	}
	for i := 0; i < 4; i++ {
		if err := admClient.Configure(prof); err != nil {
			t.Fatalf("Configure #%d failed: %v", i, err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	select {
	case <-connectedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("admin did not reconnect")
	}

	// Give any previous canceled loops time to finish exiting and ensure they never clear c.conn.
	time.Sleep(150 * time.Millisecond)
	if err := admClient.RequestConnect("nonexistent-id"); err != nil {
		t.Fatalf("expected active connection after reconfigure, got error: %v", err)
	}
}
