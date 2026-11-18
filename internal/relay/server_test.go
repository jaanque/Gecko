package relay_test

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
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
	defer srv.Close()
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
	clipCh := make(chan string, 1)

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
		OnClipboard: func(text string) {
			clipCh <- text
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

	// Verify generated client binary provisions a UNIQUE revocable client credential (NEVER Admin credentials!).
	genBin, credID, err := admClient.GenerateClientBinaryWithLabel([]byte("MZ\x90\x00fake-template"), nil, "Equipo-Cliente-1")
	if err != nil {
		t.Fatalf("GenerateClientBinaryWithLabel failed: %v", err)
	}
	if credID == "" || credID == security.DefaultClientCredentialID {
		t.Fatalf("expected unique provisioned credential ID while connected, got %q", credID)
	}
	genCfg, err := config.ReadFromBytes(genBin)
	if err != nil {
		t.Fatalf("ReadFromBytes failed: %v", err)
	}
	if genCfg.AuthToken == id.AdminToken || len(genCfg.AuthToken) < 32 {
		t.Fatal("generated client must embed unique client token, never AdminToken")
	}
	if bytes.Equal(genCfg.ClientCertDER, id.AdminCertDER) || bytes.Equal(genCfg.ClientCertDER, id.ClientCertDER) {
		t.Fatal("generated client must embed unique client mTLS cert")
	}

	// 4. Connect a simulated remote PC Client using the newly provisioned unique credential.
	uniqueClientTLS, err := security.MutualPinnedClientTLSConfig(genCfg.CertSHA256, genCfg.ClientCertDER, genCfg.ClientKeyDER)
	if err != nil {
		t.Fatalf("MutualPinnedClientTLSConfig(unique) failed: %v", err)
	}
	cliConn, err := tls.Dial("tcp", ln.Addr().String(), uniqueClientTLS)
	if err != nil {
		t.Fatalf("client dial failed: %v", err)
	}
	defer cliConn.Close()

	if err := protocol.WriteJSON(cliConn, protocol.MsgAuth, protocol.AuthPayload{
		Token:    genCfg.AuthToken,
		Hostname: "PC-REMOTO-EXTERNO",
		OS:       "linux",
		Username: "alice",
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

	// Wait for Admin to see the online PC with OS, Username, and CredentialID populated.
	var target protocol.ClientSummary
	deadline := time.After(3 * time.Second)
	for target.ID == "" {
		select {
		case list := <-clientsCh:
			for _, c := range list {
				if c.Hostname == "PC-REMOTO-EXTERNO" {
					target = c
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for client in admin list")
		}
	}
	if target.OS != "linux" || target.Username != "alice" || target.CredentialID != credID {
		t.Fatalf("unexpected ClientSummary metadata: %+v", target)
	}

	// 5. Admin requests control -> Client allows -> Relay forwards frame, input, clipboard, system action & file.
	if err := admClient.RequestConnect(target.ID); err != nil {
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

	// Bidirectional clipboard sync.
	admClient.SendClipboard("texto-desde-admin")
	mType, data, err = protocol.ReadMessage(cliConn)
	if err != nil || mType != protocol.MsgClipboard {
		t.Fatalf("expected relayed MsgClipboard to client, err=%v type=%d", err, mType)
	}
	var gotClip protocol.ClipboardPayload
	_ = json.Unmarshal(data, &gotClip)
	if gotClip.Text != "texto-desde-admin" {
		t.Fatalf("clipboard to client mismatch: %q", gotClip.Text)
	}

	_ = protocol.WriteJSON(cliConn, protocol.MsgClipboard, protocol.ClipboardPayload{Text: "texto-desde-cliente"})
	select {
	case txt := <-clipCh:
		if txt != "texto-desde-cliente" {
			t.Fatalf("clipboard to admin mismatch: %q", txt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for clipboard from client")
	}

	// System action (Ctrl+Alt+Del).
	admClient.SendSystemAction(protocol.ActionCtrlAltDel)
	mType, data, err = protocol.ReadMessage(cliConn)
	if err != nil || mType != protocol.MsgSystemAction {
		t.Fatalf("expected relayed MsgSystemAction, err=%v type=%d", err, mType)
	}
	var sa protocol.SystemActionPayload
	_ = json.Unmarshal(data, &sa)
	if sa.Action != protocol.ActionCtrlAltDel {
		t.Fatalf("unexpected system action: %+v", sa)
	}

	// End-to-end file transfer Admin -> Relay -> Client.
	srcFile := filepath.Join(t.TempDir(), "nota.txt")
	wantContent := []byte("contenido-de-prueba-gecko-2026")
	if err := os.WriteFile(srcFile, wantContent, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- admClient.SendFile(srcFile)
	}()

	mType, data, err = protocol.ReadMessage(cliConn)
	if err != nil || mType != protocol.MsgFileOffer {
		t.Fatalf("expected MsgFileOffer, err=%v type=%d", err, mType)
	}
	var offer protocol.FileOfferPayload
	_ = json.Unmarshal(data, &offer)
	wantSum := sha256.Sum256(wantContent)
	if offer.Name != "nota.txt" || offer.Size != int64(len(wantContent)) || offer.SHA256 != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("unexpected offer: %+v", offer)
	}
	_ = protocol.WriteJSON(cliConn, protocol.MsgFileAccept, protocol.FileAcceptPayload{
		TransferID: offer.TransferID,
		Accepted:   true,
	})

	var received bytes.Buffer
	for {
		mType, data, err = protocol.ReadMessage(cliConn)
		if err != nil {
			t.Fatalf("reading file stream failed: %v", err)
		}
		if mType == protocol.MsgFileChunk {
			ch, err := protocol.DecodeFileChunk(data)
			if err != nil {
				t.Fatalf("DecodeFileChunk failed: %v", err)
			}
			received.Write(ch.Data)
		} else if mType == protocol.MsgFileDone {
			_ = protocol.WriteJSON(cliConn, protocol.MsgFileDone, protocol.FileDonePayload{
				TransferID: offer.TransferID,
				OK:         true,
				SavedPath:  "/tmp/nota.txt",
			})
			break
		}
	}
	if !bytes.Equal(received.Bytes(), wantContent) {
		t.Fatalf("transferred file bytes mismatch: got %q", received.String())
	}
	if err := <-sendErrCh; err != nil {
		t.Fatalf("SendFile returned error: %v", err)
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

	// 6. Revoke client credential -> client is disconnected immediately and cannot re-authenticate!
	if err := admClient.RevokeClient(credID, target.ID); err != nil {
		t.Fatalf("RevokeClient failed: %v", err)
	}
	// Expect MsgSessionStop or connection close.
	_ = cliConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, rerr := protocol.ReadMessage(cliConn)
		if rerr != nil {
			break
		}
	}

	// Reconnecting with the revoked credential must fail auth.
	revokedConn, err := tls.Dial("tcp", ln.Addr().String(), uniqueClientTLS)
	if err != nil {
		t.Fatalf("dial with revoked cert failed: %v", err)
	}
	defer revokedConn.Close()
	_ = protocol.WriteJSON(revokedConn, protocol.MsgAuth, protocol.AuthPayload{
		Token:    genCfg.AuthToken,
		Hostname: "PC-REVOCADO",
	})
	mType, data, err = protocol.ReadMessage(revokedConn)
	if err != nil || mType != protocol.MsgAuthResp {
		t.Fatalf("expected auth resp for revoked client, err=%v type=%d", err, mType)
	}
	var revResp protocol.AuthRespPayload
	_ = json.Unmarshal(data, &revResp)
	if revResp.OK {
		t.Fatal("revoked client must not be allowed to re-authenticate")
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
	defer srv.Close()
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

func TestRelayPerIPRateLimit(t *testing.T) {
	id, err := security.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity failed: %v", err)
	}
	srv := relay.NewServer(id)
	srv.SetRateLimits(2, 2, 10*time.Second)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", security.ServerTLSConfig(id.Certificate, id.ClientCAs))
	if err != nil {
		t.Fatalf("tls.Listen failed: %v", err)
	}
	defer ln.Close()
	defer srv.Close()
	go func() { _ = srv.Serve(ln) }()

	cliTLS, err := security.MutualPinnedClientTLSConfig(id.CertSHA256, id.ClientCertDER, id.ClientKeyDER)
	if err != nil {
		t.Fatalf("MutualPinnedClientTLSConfig failed: %v", err)
	}

	// First two connections consume the 2-per-window quota.
	for i := 0; i < 2; i++ {
		c, err := tls.Dial("tcp", ln.Addr().String(), cliTLS)
		if err != nil {
			t.Fatalf("dial #%d failed unexpectedly: %v", i, err)
		}
		_ = c.Close()
	}

	// Third connection from the same IP within the window must be closed immediately by the rate limiter.
	c3, err := tls.Dial("tcp", ln.Addr().String(), cliTLS)
	if err == nil {
		_ = c3.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		buf := make([]byte, 1)
		_, rerr := c3.Read(buf)
		_ = c3.Close()
		if rerr == nil {
			t.Fatal("expected third connection exceeding per-IP rate limit to be rejected")
		}
	}
}
