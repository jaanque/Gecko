package relay

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"remote-control/internal/protocol"
	"remote-control/internal/security"
)

const (
	maxConcurrentHandshakes = 64
	maxHostnameRunes        = 64
	maxReportedMonitors     = 16
)

type peerConn struct {
	conn    net.Conn
	writeMu sync.Mutex
}

func (p *peerConn) writeMessage(msgType uint8, payload []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return protocol.WriteMessage(p.conn, msgType, payload)
}

func (p *peerConn) writeJSON(msgType uint8, v any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return protocol.WriteJSON(p.conn, msgType, v)
}

type clientPeer struct {
	peerConn
	id           string
	hostname     string
	ip           string
	width        int
	height       int
	monitorCount int
	status       string // "online", "pending", "active"
	sessionID    string
}

type adminPeer struct {
	peerConn
	id        string
	ip        string
	sessionID string
}

type bridgeSession struct {
	id       string
	clientID string
	admin    *adminPeer
	active   bool
}

// Server acts as a secure mTLS 1.3 bridge between Admin apps and Client PCs across different networks.
type Server struct {
	identity     *security.ServerIdentity
	handshakeSem chan struct{}

	mu       sync.RWMutex
	clients  map[string]*clientPeer
	admins   map[string]*adminPeer
	sessions map[string]*bridgeSession
}

// NewServer creates a Relay bridge server.
func NewServer(identity *security.ServerIdentity) *Server {
	return &Server{
		identity:     identity,
		handshakeSem: make(chan struct{}, maxConcurrentHandshakes),
		clients:      make(map[string]*clientPeer),
		admins:       make(map[string]*adminPeer),
		sessions:     make(map[string]*bridgeSession),
	}
}

// Serve accepts incoming mTLS 1.3 connections from both Admins and Clients until ln is closed.
func (s *Server) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case s.handshakeSem <- struct{}{}:
			go s.handleConn(conn)
		default:
			_ = conn.Close()
		}
	}
}

func (s *Server) handleConn(rawConn net.Conn) {
	defer rawConn.Close()
	semReleased := false
	releaseSem := func() {
		if !semReleased {
			semReleased = true
			<-s.handshakeSem
		}
	}
	defer releaseSem()

	tlsConn, ok := rawConn.(*tls.Conn)
	if !ok {
		return
	}
	if tc, ok := tlsConn.NetConn().(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetReadBuffer(512 * 1024)
		_ = tc.SetWriteBuffer(512 * 1024)
	}
	_ = tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	// Verify Mutual TLS 1.3 client certificate and extract cryptographically signed role ("admin" or "client").
	peerRole, err := security.PeerRoleFromTLS(tlsConn)
	if err != nil {
		return
	}

	_ = rawConn.SetReadDeadline(time.Now().Add(8 * time.Second))
	msgType, payload, err := protocol.ReadMessage(rawConn)
	if err != nil {
		return
	}

	// Release handshake slot once mTLS + initial auth message arrived.
	releaseSem()

	switch {
	case msgType == protocol.MsgAuth && peerRole == security.RoleClient:
		s.serveClient(rawConn, payload)
	case msgType == protocol.MsgAdminAuth && peerRole == security.RoleAdmin:
		s.serveAdmin(rawConn, payload)
	default:
		_ = rawConn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		_ = protocol.WriteJSON(rawConn, protocol.MsgAuthResp, protocol.AuthRespPayload{
			OK:    false,
			Error: "rol mTLS no autorizado",
		})
	}
}

func (s *Server) serveClient(rawConn net.Conn, payload []byte) {
	var auth protocol.AuthPayload
	if err := json.Unmarshal(payload, &auth); err != nil {
		return
	}
	if !security.ConstantTimeTokenEqual(auth.Token, s.identity.ClientToken) {
		_ = rawConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = protocol.WriteJSON(rawConn, protocol.MsgAuthResp, protocol.AuthRespPayload{
			OK:    false,
			Error: "token de cliente inválido",
		})
		return
	}

	clientID, err := security.RandomHex(8)
	if err != nil {
		return
	}

	ip := remoteIP(rawConn)
	hostname := sanitizeHostname(auth.Hostname)
	if hostname == "" {
		hostname = "EQUIPO-" + clientID[:4]
	}
	monCount := len(auth.Monitors)
	if monCount < 1 {
		monCount = 1
	} else if monCount > maxReportedMonitors {
		monCount = maxReportedMonitors
	}
	width := auth.Width
	height := auth.Height
	if width < 0 || width > 8192 {
		width = 0
	}
	if height < 0 || height > 8192 {
		height = 0
	}

	cp := &clientPeer{
		peerConn:     peerConn{conn: rawConn},
		id:           clientID,
		hostname:     hostname,
		ip:           ip,
		width:        width,
		height:       height,
		monitorCount: monCount,
		status:       "online",
	}

	if err := cp.writeJSON(protocol.MsgAuthResp, protocol.AuthRespPayload{
		OK:       true,
		ClientID: clientID,
	}); err != nil {
		return
	}

	s.mu.Lock()
	s.clients[clientID] = cp
	s.mu.Unlock()
	log.Printf("[cliente] conectado: %s (%s) id=%s", cp.hostname, cp.ip, cp.id)
	s.broadcastClientList()

	defer func() {
		s.unregisterClient(cp)
		log.Printf("[cliente] desconectado: %s (%s)", cp.hostname, cp.ip)
	}()

	for {
		_ = rawConn.SetReadDeadline(time.Now().Add(35 * time.Second))
		mType, data, err := protocol.ReadMessage(rawConn)
		if err != nil {
			return
		}

		switch mType {
		case protocol.MsgPing:
			_ = cp.writeMessage(protocol.MsgPong, data)

		case protocol.MsgPong:
			if len(data) == 8 {
				if adm := s.activeAdminForClient(cp); adm != nil {
					_ = adm.writeMessage(protocol.MsgPong, data)
				}
			}

		case protocol.MsgConsentResp:
			var resp protocol.ConsentRespPayload
			if json.Unmarshal(data, &resp) == nil {
				s.handleConsentResp(cp, resp)
			}

		case protocol.MsgSessionStop:
			var stop protocol.SessionStopPayload
			_ = json.Unmarshal(data, &stop)
			s.handleClientSessionStop(cp, stop)

		case protocol.MsgFrame:
			if adm := s.activeAdminForClient(cp); adm != nil {
				_ = adm.writeMessage(protocol.MsgFrame, data)
			}
		}
	}
}

func (s *Server) serveAdmin(rawConn net.Conn, payload []byte) {
	var auth protocol.AdminAuthPayload
	if err := json.Unmarshal(payload, &auth); err != nil {
		return
	}
	if !security.ConstantTimeTokenEqual(auth.Token, s.identity.AdminToken) {
		_ = rawConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = protocol.WriteJSON(rawConn, protocol.MsgAuthResp, protocol.AuthRespPayload{
			OK:    false,
			Error: "token de administrador inválido",
		})
		return
	}

	adminID, err := security.RandomHex(8)
	if err != nil {
		return
	}

	ap := &adminPeer{
		peerConn: peerConn{conn: rawConn},
		id:       adminID,
		ip:       remoteIP(rawConn),
	}

	if err := ap.writeJSON(protocol.MsgAuthResp, protocol.AuthRespPayload{
		OK:       true,
		ClientID: adminID,
	}); err != nil {
		return
	}

	s.mu.Lock()
	s.admins[adminID] = ap
	list := s.snapshotClientsLocked()
	s.mu.Unlock()

	log.Printf("[admin] conectado (%s) id=%s", ap.ip, ap.id)
	_ = ap.writeJSON(protocol.MsgClientList, list)

	defer func() {
		s.endAdminSession(ap, "Administrador desconectado")
		s.mu.Lock()
		delete(s.admins, adminID)
		s.mu.Unlock()
		log.Printf("[admin] desconectado (%s)", ap.ip)
	}()

	for {
		_ = rawConn.SetReadDeadline(time.Now().Add(35 * time.Second))
		mType, data, err := protocol.ReadMessage(rawConn)
		if err != nil {
			return
		}

		switch mType {
		case protocol.MsgPing:
			if len(data) == 8 {
				if cp := s.activeClientForAdmin(ap); cp != nil {
					_ = cp.writeMessage(protocol.MsgPing, data)
					continue
				}
			}
			_ = ap.writeMessage(protocol.MsgPong, data)

		case protocol.MsgPong:
			// keepalive ok

		case protocol.MsgAdminConnect:
			var req protocol.AdminConnectPayload
			if json.Unmarshal(data, &req) == nil && req.ClientID != "" {
				s.handleAdminConnect(ap, req.ClientID)
			}

		case protocol.MsgSessionStop:
			var stop protocol.SessionStopPayload
			_ = json.Unmarshal(data, &stop)
			reason := stop.Reason
			if reason == "" {
				reason = "Desconectado por el administrador"
			}
			s.endAdminSession(ap, reason)

		case protocol.MsgFrameAck:
			if _, err := protocol.DecodeFrameAck(data); err == nil {
				if cp := s.activeClientForAdmin(ap); cp != nil {
					_ = cp.writeMessage(mType, data)
				}
			}

		case protocol.MsgMouse:
			if _, err := protocol.DecodeMouse(data); err == nil {
				if cp := s.activeClientForAdmin(ap); cp != nil {
					_ = cp.writeMessage(mType, data)
				}
			}

		case protocol.MsgKey:
			if _, err := protocol.DecodeKey(data); err == nil {
				if cp := s.activeClientForAdmin(ap); cp != nil {
					_ = cp.writeMessage(mType, data)
				}
			}

		case protocol.MsgStreamConfig:
			var sc protocol.StreamConfigPayload
			if json.Unmarshal(data, &sc) == nil {
				if cp := s.activeClientForAdmin(ap); cp != nil {
					_ = cp.writeMessage(mType, data)
				}
			}
		}
	}
}

func (s *Server) handleAdminConnect(ap *adminPeer, clientID string) {
	s.endAdminSession(ap, "Nueva solicitud iniciada")

	sid, err := security.RandomHex(12)
	if err != nil {
		return
	}

	s.mu.Lock()
	cp, ok := s.clients[clientID]
	if !ok {
		s.mu.Unlock()
		_ = ap.writeJSON(protocol.MsgSessionEvent, protocol.SessionEventPayload{
			Event:  "error",
			Reason: "El equipo ya no está conectado.",
		})
		return
	}
	if cp.status != "online" {
		s.mu.Unlock()
		_ = ap.writeJSON(protocol.MsgSessionEvent, protocol.SessionEventPayload{
			Event:  "error",
			Reason: "El equipo ya tiene una solicitud o sesión activa.",
		})
		return
	}

	cp.status = "pending"
	cp.sessionID = sid
	ap.sessionID = sid
	s.sessions[sid] = &bridgeSession{
		id:       sid,
		clientID: cp.id,
		admin:    ap,
		active:   false,
	}
	hostname := cp.hostname
	s.mu.Unlock()

	if err := cp.writeJSON(protocol.MsgConsentReq, protocol.ConsentReqPayload{
		SessionID:  sid,
		TimeoutSec: 60,
	}); err != nil {
		s.endAdminSession(ap, "Error de comunicación con el equipo")
		_ = ap.writeJSON(protocol.MsgSessionEvent, protocol.SessionEventPayload{
			Event:  "error",
			Reason: "No se pudo contactar con el equipo.",
		})
		return
	}

	_ = ap.writeJSON(protocol.MsgSessionEvent, protocol.SessionEventPayload{
		Event:     "pending",
		SessionID: sid,
		ClientID:  clientID,
		Hostname:  hostname,
	})
	s.broadcastClientList()
}

func (s *Server) handleConsentResp(cp *clientPeer, resp protocol.ConsentRespPayload) {
	s.mu.Lock()
	sess, ok := s.sessions[resp.SessionID]
	if !ok || sess.clientID != cp.id || cp.sessionID != resp.SessionID {
		s.mu.Unlock()
		return
	}
	adm := sess.admin
	hostname := cp.hostname
	w, h, monCount := cp.width, cp.height, cp.monitorCount

	if resp.Allowed {
		sess.active = true
		cp.status = "active"
	} else {
		cp.status = "online"
		cp.sessionID = ""
		if adm.sessionID == sess.id {
			adm.sessionID = ""
		}
		delete(s.sessions, sess.id)
	}
	s.mu.Unlock()

	if resp.Allowed {
		_ = adm.writeJSON(protocol.MsgSessionEvent, protocol.SessionEventPayload{
			Event:        "started",
			SessionID:    resp.SessionID,
			ClientID:     cp.id,
			Hostname:     hostname,
			Width:        w,
			Height:       h,
			MonitorCount: monCount,
		})
	} else {
		reason := resp.Reason
		if reason == "" {
			reason = "Solicitud denegada por el usuario."
		}
		_ = adm.writeJSON(protocol.MsgSessionEvent, protocol.SessionEventPayload{
			Event:     "denied",
			SessionID: resp.SessionID,
			ClientID:  cp.id,
			Hostname:  hostname,
			Reason:    reason,
		})
	}
	s.broadcastClientList()
}

func (s *Server) handleClientSessionStop(cp *clientPeer, stop protocol.SessionStopPayload) {
	var adm *adminPeer
	var sid, hostname string

	s.mu.Lock()
	sid = cp.sessionID
	if sid != "" && (stop.SessionID == "" || stop.SessionID == sid) {
		if sess, ok := s.sessions[sid]; ok {
			adm = sess.admin
			hostname = cp.hostname
			if adm.sessionID == sid {
				adm.sessionID = ""
			}
			delete(s.sessions, sid)
		}
		cp.sessionID = ""
		cp.status = "online"
	}
	s.mu.Unlock()

	if adm != nil {
		reason := stop.Reason
		if reason == "" {
			reason = "Sesión finalizada por el equipo remoto."
		}
		_ = adm.writeJSON(protocol.MsgSessionEvent, protocol.SessionEventPayload{
			Event:     "ended",
			SessionID: sid,
			Hostname:  hostname,
			Reason:    reason,
		})
		s.broadcastClientList()
	}
}

func (s *Server) unregisterClient(cp *clientPeer) {
	var adm *adminPeer
	var sid, hostname string

	s.mu.Lock()
	delete(s.clients, cp.id)
	if cp.sessionID != "" {
		if sess, ok := s.sessions[cp.sessionID]; ok {
			adm = sess.admin
			sid = sess.id
			hostname = cp.hostname
			if adm.sessionID == sid {
				adm.sessionID = ""
			}
			delete(s.sessions, sid)
		}
	}
	s.mu.Unlock()

	if adm != nil {
		_ = adm.writeJSON(protocol.MsgSessionEvent, protocol.SessionEventPayload{
			Event:     "ended",
			SessionID: sid,
			Hostname:  hostname,
			Reason:    "El equipo se ha desconectado.",
		})
	}
	s.broadcastClientList()
}

func (s *Server) endAdminSession(ap *adminPeer, reason string) {
	var cp *clientPeer
	var sid string

	s.mu.Lock()
	sid = ap.sessionID
	if sid != "" {
		ap.sessionID = ""
		if sess, ok := s.sessions[sid]; ok {
			if c, exists := s.clients[sess.clientID]; exists && c.sessionID == sid {
				c.sessionID = ""
				c.status = "online"
				cp = c
			}
			delete(s.sessions, sid)
		}
	}
	s.mu.Unlock()

	if cp != nil {
		_ = cp.writeJSON(protocol.MsgSessionStop, protocol.SessionStopPayload{
			SessionID: sid,
			Reason:    reason,
		})
		s.broadcastClientList()
	}
}

func (s *Server) activeAdminForClient(cp *clientPeer) *adminPeer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cp.sessionID == "" {
		return nil
	}
	if sess, ok := s.sessions[cp.sessionID]; ok && sess.active {
		return sess.admin
	}
	return nil
}

func (s *Server) activeClientForAdmin(ap *adminPeer) *clientPeer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ap.sessionID == "" {
		return nil
	}
	if sess, ok := s.sessions[ap.sessionID]; ok && sess.active {
		return s.clients[sess.clientID]
	}
	return nil
}

func (s *Server) snapshotClientsLocked() []protocol.ClientSummary {
	out := make([]protocol.ClientSummary, 0, len(s.clients))
	for _, c := range s.clients {
		out = append(out, protocol.ClientSummary{
			ID:           c.id,
			Hostname:     c.hostname,
			IP:           c.ip,
			Width:        c.width,
			Height:       c.height,
			MonitorCount: c.monitorCount,
			Status:       c.status,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hostname == out[j].Hostname {
			return out[i].ID < out[j].ID
		}
		return out[i].Hostname < out[j].Hostname
	})
	return out
}

func (s *Server) broadcastClientList() {
	s.mu.RLock()
	list := s.snapshotClientsLocked()
	admins := make([]*adminPeer, 0, len(s.admins))
	for _, a := range s.admins {
		admins = append(admins, a)
	}
	s.mu.RUnlock()

	for _, a := range admins {
		_ = a.writeJSON(protocol.MsgClientList, list)
	}
}

func remoteIP(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil || host == "" {
		return conn.RemoteAddr().String()
	}
	return host
}

func sanitizeHostname(raw string) string {
	var b strings.Builder
	count := 0
	for _, r := range strings.TrimSpace(raw) {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		count++
		if count >= maxHostnameRunes {
			break
		}
	}
	return strings.TrimSpace(b.String())
}
