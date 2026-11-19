package admin

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"remote-control/internal/p2p"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
)

// RelayEvents delivers real-time Relay state and remote session callbacks to the desktop UI.
type RelayEvents struct {
	OnRelayStatus    func(connected bool, detail string)
	OnClientsChanged func(clients []protocol.ClientSummary)
	OnSessionPending func(sessionID, clientID, hostname string)
	OnSessionStarted func(sessionID, clientID, hostname string, width, height, monitorCount int)
	OnSessionDenied  func(sessionID, clientID, hostname, reason string)
	OnSessionEnded   func(sessionID, hostname, reason string)
	OnSessionError   func(reason string)
	OnFrame          func(sessionID string, frameData []byte)
	OnCursor         func(pkt protocol.CursorPacket)
	OnLatency        func(rttMs int)
	OnClipboard      func(text string)
	OnFileStatus     func(name string, ok bool, detail string)
}

// RelayClient maintains an outbound TLS 1.3 connection from AdminRemoto.exe to RelayServidor.
type RelayClient struct {
	profilePath string

	mu            sync.RWMutex
	profile       security.RelayProfile
	hasProfile    bool
	events        RelayEvents
	conn          net.Conn
	writeMu       sync.Mutex
	activeSession string
	loopGen       uint64
	loopCancel    context.CancelFunc

	provMu sync.Mutex
	provCh chan protocol.AdminProvisionRespPayload

	fileMu       sync.Mutex
	fileAcceptCh map[string]chan protocol.FileAcceptPayload
	fileDoneCh   map[string]chan protocol.FileDonePayload
}

// NewRelayClient constructs a RelayClient and loads any saved RelayProfile from dataDir.
func NewRelayClient(dataDir string) *RelayClient {
	rc := &RelayClient{
		profilePath:  filepath.Join(dataDir, "admin_profile.json"),
		fileAcceptCh: make(map[string]chan protocol.FileAcceptPayload),
		fileDoneCh:   make(map[string]chan protocol.FileDonePayload),
	}
	if data, err := os.ReadFile(rc.profilePath); err == nil {
		if dec, err := security.UnprotectData(data); err == nil {
			var p security.RelayProfile
			if json.Unmarshal(dec, &p) == nil && p.Validate() == nil {
				rc.profile = p
				rc.hasProfile = true
			}
		}
	}
	return rc
}

// SetEvents registers the desktop UI callbacks.
func (c *RelayClient) SetEvents(ev RelayEvents) {
	c.mu.Lock()
	c.events = ev
	c.mu.Unlock()
}

// Profile returns the current saved RelayProfile (and whether one is configured).
func (c *RelayClient) Profile() (security.RelayProfile, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.profile, c.hasProfile
}

// Configure saves a new RelayProfile to disk and reconnects to the Relay server immediately.
func (c *RelayClient) Configure(p security.RelayProfile) error {
	p.ServerAddr = normalizeHostPort(p.ServerAddr, "9443")
	p.CertSHA256 = strings.ToLower(strings.TrimSpace(p.CertSHA256))
	p.AdminToken = strings.TrimSpace(p.AdminToken)
	p.ClientToken = strings.TrimSpace(p.ClientToken)

	if err := p.Validate(); err != nil {
		return err
	}
	if _, err := security.MutualPinnedClientTLSConfig(p.CertSHA256, p.AdminCertDER, p.AdminKeyDER); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(c.profilePath), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	toWrite, err := security.ProtectData(raw)
	if err != nil {
		toWrite = raw
	}
	if err := os.WriteFile(c.profilePath, toWrite, 0o600); err != nil {
		return err
	}

	c.mu.Lock()
	c.profile = p
	c.hasProfile = true
	c.mu.Unlock()

	c.Start()
	return nil
}

// Start begins (or restarts) the background auto-reconnecting Relay connection loop.
func (c *RelayClient) Start() {
	c.mu.Lock()
	c.loopGen++
	gen := c.loopGen
	if c.loopCancel != nil {
		c.loopCancel()
		c.loopCancel = nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	if !c.hasProfile {
		ev := c.events
		c.mu.Unlock()
		if ev.OnRelayStatus != nil {
			ev.OnRelayStatus(false, "Sin servidor configurado")
		}
		return
	}
	prof := c.profile
	ctx, cancel := context.WithCancel(context.Background())
	c.loopCancel = cancel
	c.mu.Unlock()

	go c.runLoop(ctx, gen, prof)
}

// Close shuts down the Relay connection cleanly.
func (c *RelayClient) Close() {
	c.DisconnectSession("Administrador cerrado")
	c.mu.Lock()
	c.loopGen++
	if c.loopCancel != nil {
		c.loopCancel()
		c.loopCancel = nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()
}

// RequestConnect asks the Relay to request remote control of clientID.
func (c *RelayClient) RequestConnect(clientID string) error {
	conn := c.currentConn()
	if conn == nil {
		return errors.New("no hay conexión con el servidor puente")
	}
	return c.writeJSON(conn, protocol.MsgAdminConnect, protocol.AdminConnectPayload{ClientID: clientID})
}

// DisconnectSession terminates any active or pending remote session.
func (c *RelayClient) DisconnectSession(reason string) {
	c.mu.Lock()
	conn := c.conn
	sid := c.activeSession
	c.activeSession = ""
	c.mu.Unlock()

	if conn != nil {
		_ = c.writeJSON(conn, protocol.MsgSessionStop, protocol.SessionStopPayload{
			SessionID: sid,
			Reason:    reason,
		})
	}
}

// SendFrameAck forwards a frame acknowledgment to the active client through the Relay.
func (c *RelayClient) SendFrameAck(frameID uint32) {
	if conn := c.currentConn(); conn != nil {
		_ = c.writeMessage(conn, protocol.MsgFrameAck, protocol.EncodeFrameAck(frameID))
	}
}

// SendMouse forwards a mouse event to the active client through the Relay.
func (c *RelayClient) SendMouse(ev protocol.MouseEvent) {
	if conn := c.currentConn(); conn != nil {
		_ = c.writeMessage(conn, protocol.MsgMouse, protocol.EncodeMouse(ev))
	}
}

// SendKey forwards a keyboard event to the active client through the Relay.
func (c *RelayClient) SendKey(ev protocol.KeyEvent) {
	if conn := c.currentConn(); conn != nil {
		_ = c.writeMessage(conn, protocol.MsgKey, protocol.EncodeKey(ev))
	}
}

// SendClipboard synchronizes UTF-8 clipboard text to the active remote client.
func (c *RelayClient) SendClipboard(text string) {
	if len(text) == 0 || len(text) > protocol.MaxClipboardPayloadSize-64 {
		return
	}
	if conn := c.currentConn(); conn != nil {
		_ = c.writeJSON(conn, protocol.MsgClipboard, protocol.ClipboardPayload{Text: text})
	}
}

// SendSystemAction sends a system action (e.g. protocol.ActionCtrlAltDel) to the active client.
func (c *RelayClient) SendSystemAction(action string) {
	if action == "" {
		return
	}
	if conn := c.currentConn(); conn != nil {
		_ = c.writeJSON(conn, protocol.MsgSystemAction, protocol.SystemActionPayload{Action: action})
	}
}

// SendElevateReq requests the remote client to elevate via UAC or restart as an elevated service agent.
func (c *RelayClient) SendElevateReq() {
	c.mu.RLock()
	sid := c.activeSession
	c.mu.RUnlock()
	if sid == "" {
		return
	}
	if conn := c.currentConn(); conn != nil {
		_ = c.writeJSON(conn, protocol.MsgElevateReq, protocol.ElevateReqPayload{SessionID: sid})
	}
}

// SendStreamConfig updates quality on the active client through the Relay.
func (c *RelayClient) SendStreamConfig(quality int, keyframe bool) {
	if conn := c.currentConn(); conn != nil {
		_ = c.writeJSON(conn, protocol.MsgStreamConfig, protocol.StreamConfigPayload{
			Quality:  quality,
			Keyframe: keyframe,
		})
	}
}

// SendSelectMonitor switches active monitor on the active client through the Relay.
func (c *RelayClient) SendSelectMonitor(monitorIndex int) {
	if conn := c.currentConn(); conn != nil {
		m := monitorIndex
		_ = c.writeJSON(conn, protocol.MsgStreamConfig, protocol.StreamConfigPayload{
			Monitor:  &m,
			Keyframe: true,
		})
	}
}

func (c *RelayClient) currentConn() net.Conn {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.conn
}

func (c *RelayClient) isCurrentGen(gen uint64) bool {
	c.mu.RLock()
	ok := c.loopGen == gen
	c.mu.RUnlock()
	return ok
}

func (c *RelayClient) emitFileStatus(name string, ok bool, detail string) {
	c.mu.RLock()
	cb := c.events.OnFileStatus
	c.mu.RUnlock()
	if cb != nil {
		cb(name, ok, detail)
	}
}

func (c *RelayClient) runLoop(ctx context.Context, gen uint64, prof security.RelayProfile) {
	tlsCfg, err := security.MutualPinnedClientTLSConfig(prof.CertSHA256, prof.AdminCertDER, prof.AdminKeyDER)
	if err != nil {
		c.emitStatus(gen, false, "Certificado mTLS inválido")
		return
	}

	backoff := time.Second
	for ctx.Err() == nil && c.isCurrentGen(gen) {
		c.emitStatus(gen, false, "Conectando a "+prof.ServerAddr+"...")

		err := c.connectAndServe(ctx, gen, prof, tlsCfg)

		c.mu.Lock()
		isCur := c.loopGen == gen
		var hadSession bool
		var ev RelayEvents
		if isCur {
			hadSession = c.activeSession != ""
			c.activeSession = ""
			c.conn = nil
			ev = c.events
		}
		c.mu.Unlock()

		if !isCur || ctx.Err() != nil {
			return
		}

		if ev.OnClientsChanged != nil {
			ev.OnClientsChanged(nil)
		}
		if hadSession && ev.OnSessionEnded != nil {
			ev.OnSessionEnded("", "", "Se perdió la conexión con el servidor puente.")
		}
		if err != nil {
			c.emitStatus(gen, false, err.Error())
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 5*time.Second {
			backoff += time.Second
		}
	}
}

func (c *RelayClient) connectAndServe(ctx context.Context, gen uint64, prof security.RelayProfile, tlsCfg *tls.Config) error {
	tlsDialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 15 * time.Second},
		Config:    tlsCfg,
	}
	rawConn, err := tlsDialer.DialContext(ctx, "tcp", prof.ServerAddr)
	if err != nil {
		return fmt.Errorf("sin conexión (%s)", prof.ServerAddr)
	}
	conn := rawConn.(*tls.Conn)
	if err := security.VerifyServerTLSConn(conn, prof.CertSHA256); err != nil {
		_ = conn.Close()
		return errors.New("identidad TLS del servidor no verificada")
	}
	defer func() {
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		_ = conn.Close()
	}()

	if tc, ok := conn.NetConn().(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetReadBuffer(512 * 1024)
		_ = tc.SetWriteBuffer(256 * 1024)
	}

	stopWatch := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopWatch()

	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteJSON(conn, protocol.MsgAdminAuth, protocol.AdminAuthPayload{
		Token: prof.AdminToken,
	}); err != nil {
		return err
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	msgType, payload, err := protocol.ReadMessage(conn)
	if err != nil || msgType != protocol.MsgAuthResp {
		return errors.New("respuesta inválida del servidor")
	}
	var resp protocol.AuthRespPayload
	if err := json.Unmarshal(payload, &resp); err != nil || !resp.OK {
		return errors.New("token de administrador rechazado")
	}

	c.mu.Lock()
	if c.loopGen != gen {
		c.mu.Unlock()
		return nil
	}
	c.conn = conn
	c.mu.Unlock()
	c.emitStatus(gen, true, prof.ServerAddr)

	// Periodic heartbeat (every 10s when idle) and end-to-end RTT probe (every 2s during active session).
	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		ticks := 0
		var ts [8]byte
		for {
			select {
			case <-connCtx.Done():
				return
			case <-ticker.C:
				ticks++
				c.mu.RLock()
				inSess := c.activeSession != ""
				c.mu.RUnlock()
				if inSess {
					binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixNano()))
					if err := c.writeMessage(conn, protocol.MsgPing, ts[:]); err != nil {
						_ = conn.Close()
						return
					}
				} else if ticks%5 == 0 {
					if err := c.writeMessage(conn, protocol.MsgPing, nil); err != nil {
						_ = conn.Close()
						return
					}
				}
			}
		}
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(35 * time.Second))
		mType, data, err := protocol.ReadMessage(conn)
		if err != nil {
			return errors.New("conexión interrumpida")
		}

		switch mType {
		case protocol.MsgPing:
			_ = c.writeMessage(conn, protocol.MsgPong, data)

		case protocol.MsgPong:
			if len(data) == 8 {
				sent := int64(binary.BigEndian.Uint64(data))
				rttMs := int((time.Now().UnixNano() - sent) / int64(time.Millisecond))
				if rttMs < 0 {
					rttMs = 0
				}
				c.mu.RLock()
				cb := c.events.OnLatency
				c.mu.RUnlock()
				if cb != nil {
					cb(rttMs)
				}
			}

		case protocol.MsgClientList:
			var list []protocol.ClientSummary
			if json.Unmarshal(data, &list) == nil {
				c.mu.RLock()
				cb := c.events.OnClientsChanged
				c.mu.RUnlock()
				if cb != nil {
					cb(list)
				}
			}

		case protocol.MsgAdminProvisionResp:
			var pr protocol.AdminProvisionRespPayload
			if json.Unmarshal(data, &pr) == nil {
				c.provMu.Lock()
				ch := c.provCh
				c.provMu.Unlock()
				if ch != nil {
					select {
					case ch <- pr:
					default:
					}
				}
			}

		case protocol.MsgSessionEvent:
			var sev protocol.SessionEventPayload
			if json.Unmarshal(data, &sev) == nil {
				c.handleSessionEvent(sev)
			}

		case protocol.MsgFrame:
			c.mu.RLock()
			sid := c.activeSession
			cb := c.events.OnFrame
			c.mu.RUnlock()
			if sid != "" && cb != nil {
				cb(sid, data)
			}

		case protocol.MsgCursor:
			if pkt, err := protocol.DecodeCursor(data); err == nil {
				c.mu.RLock()
				sid := c.activeSession
				cb := c.events.OnCursor
				c.mu.RUnlock()
				if sid != "" && cb != nil {
					cb(pkt)
				}
			}

		case protocol.MsgP2POffer:
			var offer protocol.P2POfferPayload
			if json.Unmarshal(data, &offer) == nil && offer.SessionID != "" {
				go func(off protocol.P2POfferPayload) {
					p2pCtx, p2pCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
					defer p2pCancel()
					if directConn, err := p2p.DialP2POffer(p2pCtx, off, 450*time.Millisecond); err == nil {
						c.mu.RLock()
						sid := c.activeSession
						c.mu.RUnlock()
						if sid == off.SessionID {
							_ = c.writeJSON(c.currentConn(), protocol.MsgP2PAnswer, protocol.P2PAnswerPayload{
								SessionID: sid,
								OK:        true,
							})
							go c.readDirectP2PLoop(directConn, sid)
						} else {
							_ = directConn.Close()
						}
					}
				}(offer)
			}

		case protocol.MsgClipboard:
			var clip protocol.ClipboardPayload
			if json.Unmarshal(data, &clip) == nil && clip.Text != "" {
				c.mu.RLock()
				sid := c.activeSession
				cb := c.events.OnClipboard
				c.mu.RUnlock()
				if sid != "" && cb != nil {
					cb(clip.Text)
				}
			}

		case protocol.MsgFileAccept:
			var fa protocol.FileAcceptPayload
			if json.Unmarshal(data, &fa) == nil && fa.TransferID != "" {
				c.fileMu.Lock()
				ch := c.fileAcceptCh[fa.TransferID]
				c.fileMu.Unlock()
				if ch != nil {
					select {
					case ch <- fa:
					default:
					}
				}
			}

		case protocol.MsgFileDone:
			var fd protocol.FileDonePayload
			if json.Unmarshal(data, &fd) == nil && fd.TransferID != "" {
				c.fileMu.Lock()
				ch := c.fileDoneCh[fd.TransferID]
				c.fileMu.Unlock()
				if ch != nil {
					select {
					case ch <- fd:
					default:
					}
				}
			}
		}
	}
}

func (c *RelayClient) handleSessionEvent(sev protocol.SessionEventPayload) {
	c.mu.Lock()
	ev := c.events
	switch sev.Event {
	case "started":
		c.activeSession = sev.SessionID
	case "denied", "ended", "error":
		if sev.SessionID == "" || sev.SessionID == c.activeSession {
			c.activeSession = ""
		}
	}
	c.mu.Unlock()

	switch sev.Event {
	case "pending":
		if ev.OnSessionPending != nil {
			ev.OnSessionPending(sev.SessionID, sev.ClientID, sev.Hostname)
		}
	case "started":
		if ev.OnSessionStarted != nil {
			ev.OnSessionStarted(sev.SessionID, sev.ClientID, sev.Hostname, sev.Width, sev.Height, sev.MonitorCount)
		}
	case "denied":
		if ev.OnSessionDenied != nil {
			ev.OnSessionDenied(sev.SessionID, sev.ClientID, sev.Hostname, sev.Reason)
		}
	case "ended":
		if ev.OnSessionEnded != nil {
			ev.OnSessionEnded(sev.SessionID, sev.Hostname, sev.Reason)
		}
	case "error":
		if ev.OnSessionError != nil {
			ev.OnSessionError(sev.Reason)
		}
	}
}

func (c *RelayClient) emitStatus(gen uint64, connected bool, detail string) {
	c.mu.RLock()
	if c.loopGen != gen {
		c.mu.RUnlock()
		return
	}
	cb := c.events.OnRelayStatus
	c.mu.RUnlock()
	if cb != nil {
		cb(connected, detail)
	}
}

func (c *RelayClient) writeMessage(conn net.Conn, msgType uint8, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return protocol.WriteMessage(conn, msgType, payload)
}

func (c *RelayClient) writeJSON(conn net.Conn, msgType uint8, v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return protocol.WriteJSON(conn, msgType, v)
}

func (c *RelayClient) readDirectP2PLoop(conn net.Conn, sessionID string) {
	defer conn.Close()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		mType, data, err := protocol.ReadMessage(conn)
		if err != nil {
			return
		}

		c.mu.RLock()
		sid := c.activeSession
		onFrame := c.events.OnFrame
		onCursor := c.events.OnCursor
		c.mu.RUnlock()

		if sid != sessionID {
			return
		}

		switch mType {
		case protocol.MsgFrame:
			if onFrame != nil {
				onFrame(sid, data)
			}
		case protocol.MsgCursor:
			if pkt, err := protocol.DecodeCursor(data); err == nil && onCursor != nil {
				onCursor(pkt)
			}
		case protocol.MsgPing:
			_ = protocol.WriteMessage(conn, protocol.MsgPong, data)
		}
	}
}

func normalizeHostPort(addr, defaultPort string) string {
	s := strings.TrimSpace(addr)
	if s == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(s); err == nil {
		return s
	}
	if !strings.Contains(s, ":") {
		return net.JoinHostPort(s, defaultPort)
	}
	return s
}
