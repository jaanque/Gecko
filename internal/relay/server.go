package relay

import (
	"crypto/tls"
	"crypto/x509"
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
	maxHandshakesPerIP      = 4
	ipRateWindow            = 10 * time.Second
	ipMaxConnsPerWindow     = 30
	maxHostnameRunes        = 64
	maxUsernameRunes        = 64
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
	credentialID string
	hostname     string
	os           string
	username     string
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
	id        string
	clientID  string
	hostname  string
	admin     *adminPeer
	active    bool
	startedAt time.Time
}

type ipBucket struct {
	inflight int
	stamps   []time.Time
}

type ipLimiter struct {
	mu          sync.Mutex
	maxInflight int
	maxPerWin   int
	window      time.Duration
	buckets     map[string]*ipBucket
	lastPrune   time.Time
}

func newIPLimiter(maxInflight, maxPerWin int, window time.Duration) *ipLimiter {
	return &ipLimiter{
		maxInflight: maxInflight,
		maxPerWin:   maxPerWin,
		window:      window,
		buckets:     make(map[string]*ipBucket),
		lastPrune:   time.Now(),
	}
}

func (l *ipLimiter) acquire(ip string) bool {
	if ip == "" {
		return true
	}
	now := time.Now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastPrune) > l.window*3 && len(l.buckets) > 256 {
		for k, b := range l.buckets {
			if b.inflight == 0 && (len(b.stamps) == 0 || b.stamps[len(b.stamps)-1].Before(cutoff)) {
				delete(l.buckets, k)
			}
		}
		l.lastPrune = now
	}

	b := l.buckets[ip]
	if b == nil {
		b = &ipBucket{}
		l.buckets[ip] = b
	}

	// Prune timestamps outside the sliding window.
	keep := b.stamps[:0]
	for _, ts := range b.stamps {
		if ts.After(cutoff) {
			keep = append(keep, ts)
		}
	}
	b.stamps = keep

	if b.inflight >= l.maxInflight || len(b.stamps) >= l.maxPerWin {
		return false
	}
	b.inflight++
	b.stamps = append(b.stamps, now)
	return true
}

func (l *ipLimiter) release(ip string) {
	if ip == "" {
		return
	}
	l.mu.Lock()
	if b := l.buckets[ip]; b != nil && b.inflight > 0 {
		b.inflight--
	}
	l.mu.Unlock()
}

// Server acts as a secure mTLS 1.3 bridge between Admin apps and Client PCs across different networks.
type Server struct {
	identity     *security.ServerIdentity
	handshakeSem chan struct{}
	ipLimit      *ipLimiter

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
		ipLimit:      newIPLimiter(maxHandshakesPerIP, ipMaxConnsPerWindow, ipRateWindow),
		clients:      make(map[string]*clientPeer),
		admins:       make(map[string]*adminPeer),
		sessions:     make(map[string]*bridgeSession),
	}
}

// SetRateLimits overrides per-IP connection rate limits (useful for tests or custom deployments).
func (s *Server) SetRateLimits(maxInflight, maxPerWindow int, window time.Duration) {
	s.ipLimit = newIPLimiter(maxInflight, maxPerWindow, window)
}

// Close gracefully notifies active peers and closes all live client and admin connections.
func (s *Server) Close() {
	s.mu.Lock()
	clients := make([]*clientPeer, 0, len(s.clients))
	for _, c := range s.clients {
		clients = append(clients, c)
	}
	admins := make([]*adminPeer, 0, len(s.admins))
	for _, a := range s.admins {
		admins = append(admins, a)
	}
	s.mu.Unlock()

	for _, c := range clients {
		if c.sessionID != "" {
			_ = c.writeJSON(protocol.MsgSessionStop, protocol.SessionStopPayload{
				SessionID: c.sessionID,
				Reason:    "Servidor puente detenido",
			})
		}
		_ = c.conn.Close()
	}
	for _, a := range admins {
		if a.sessionID != "" {
			_ = a.writeJSON(protocol.MsgSessionEvent, protocol.SessionEventPayload{
				Event:     "ended",
				SessionID: a.sessionID,
				Reason:    "Servidor puente detenido",
			})
		}
		_ = a.conn.Close()
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
		ip := remoteIP(conn)
		if !s.ipLimit.acquire(ip) {
			log.Printf("[seguridad] límite de conexiones por IP alcanzado: %s", ip)
			_ = conn.Close()
			continue
		}
		select {
		case s.handshakeSem <- struct{}{}:
			go s.handleConn(conn, ip)
		default:
			s.ipLimit.release(ip)
			_ = conn.Close()
		}
	}
}

func (s *Server) handleConn(rawConn net.Conn, ip string) {
	defer rawConn.Close()
	semReleased := false
	releaseSem := func() {
		if !semReleased {
			semReleased = true
			<-s.handshakeSem
			s.ipLimit.release(ip)
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
	peerRole, leafCert, err := security.PeerIdentityFromTLS(tlsConn)
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
		s.serveClient(rawConn, leafCert, payload)
	case msgType == protocol.MsgAdminAuth && peerRole == security.RoleAdmin:
		s.serveAdmin(rawConn, payload)
	default:
		log.Printf("[seguridad] rol mTLS no autorizado desde %s (role=%s msg=0x%02X)", ip, peerRole, msgType)
		_ = rawConn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		_ = protocol.WriteJSON(rawConn, protocol.MsgAuthResp, protocol.AuthRespPayload{
			OK:    false,
			Error: "rol mTLS no autorizado",
		})
	}
}

func (s *Server) serveClient(rawConn net.Conn, leafCert *x509.Certificate, payload []byte) {
	var auth protocol.AuthPayload
	if err := json.Unmarshal(payload, &auth); err != nil {
		return
	}
	credID, ok := s.identity.VerifyClientCredential(leafCert, auth.Token)
	if !ok {
		log.Printf("[seguridad] cliente rechazado desde %s (cert CN=%q)", remoteIP(rawConn), leafCert.Subject.CommonName)
		_ = rawConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = protocol.WriteJSON(rawConn, protocol.MsgAuthResp, protocol.AuthRespPayload{
			OK:    false,
			Error: "credencial de cliente inválida o revocada",
		})
		return
	}

	clientID, err := security.RandomHex(8)
	if err != nil {
		return
	}

	ip := remoteIP(rawConn)
	hostname := sanitizeText(auth.Hostname, maxHostnameRunes)
	if hostname == "" {
		hostname = "EQUIPO-" + clientID[:4]
	}
	osName := sanitizeOS(auth.OS)
	username := sanitizeText(auth.Username, maxUsernameRunes)

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
		credentialID: credID,
		hostname:     hostname,
		os:           osName,
		username:     username,
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
	log.Printf("[cliente] conectado: %s os=%s user=%s (%s) id=%s cred=%s", cp.hostname, cp.os, cp.username, cp.ip, cp.id, cp.credentialID)
	s.broadcastClientList()

	defer func() {
		s.unregisterClient(cp)
		log.Printf("[cliente] desconectado: %s (%s) id=%s", cp.hostname, cp.ip, cp.id)
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

		case protocol.MsgClipboard:
			var clip protocol.ClipboardPayload
			if json.Unmarshal(data, &clip) == nil {
				if adm := s.activeAdminForClient(cp); adm != nil {
					_ = adm.writeMessage(protocol.MsgClipboard, data)
				}
			}

		case protocol.MsgFileAccept:
			var fa protocol.FileAcceptPayload
			if json.Unmarshal(data, &fa) == nil {
				if adm := s.activeAdminForClient(cp); adm != nil {
					_ = adm.writeMessage(protocol.MsgFileAccept, data)
				}
			}

		case protocol.MsgFileDone:
			var fd protocol.FileDonePayload
			if json.Unmarshal(data, &fd) == nil {
				if adm := s.activeAdminForClient(cp); adm != nil {
					_ = adm.writeMessage(protocol.MsgFileDone, data)
				}
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
		log.Printf("[seguridad] token de administrador rechazado desde %s", remoteIP(rawConn))
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
		log.Printf("[admin] desconectado (%s) id=%s", ap.ip, ap.id)
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

		case protocol.MsgAdminProvisionClient:
			var req protocol.AdminProvisionReqPayload
			_ = json.Unmarshal(data, &req)
			s.handleAdminProvisionClient(ap, req)

		case protocol.MsgAdminRevokeClient:
			var req protocol.AdminRevokeClientPayload
			if json.Unmarshal(data, &req) == nil {
				s.handleAdminRevokeClient(ap, req)
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

		case protocol.MsgClipboard:
			var clip protocol.ClipboardPayload
			if json.Unmarshal(data, &clip) == nil {
				if cp := s.activeClientForAdmin(ap); cp != nil {
					_ = cp.writeMessage(mType, data)
				}
			}

		case protocol.MsgSystemAction:
			var sa protocol.SystemActionPayload
			if json.Unmarshal(data, &sa) == nil && sa.Action != "" {
				if cp := s.activeClientForAdmin(ap); cp != nil {
					_ = cp.writeMessage(mType, data)
				}
			}

		case protocol.MsgFileOffer:
			var fo protocol.FileOfferPayload
			if json.Unmarshal(data, &fo) == nil && fo.TransferID != "" && fo.Size >= 0 && fo.Size <= protocol.MaxFileTransferSize {
				if cp := s.activeClientForAdmin(ap); cp != nil {
					_ = cp.writeMessage(mType, data)
				}
			}

		case protocol.MsgFileChunk:
			if _, err := protocol.DecodeFileChunk(data); err == nil {
				if cp := s.activeClientForAdmin(ap); cp != nil {
					_ = cp.writeMessage(mType, data)
				}
			}

		case protocol.MsgFileDone:
			var fd protocol.FileDonePayload
			if json.Unmarshal(data, &fd) == nil && fd.TransferID != "" {
				if cp := s.activeClientForAdmin(ap); cp != nil {
					_ = cp.writeMessage(mType, data)
				}
			}
		}
	}
}

func (s *Server) handleAdminProvisionClient(ap *adminPeer, req protocol.AdminProvisionReqPayload) {
	cred, err := s.identity.IssueClientCredential(req.Label)
	if err != nil {
		_ = ap.writeJSON(protocol.MsgAdminProvisionResp, protocol.AdminProvisionRespPayload{
			OK:    false,
			Error: err.Error(),
		})
		return
	}
	log.Printf("[auditoria] admin=%s (%s) emitió credencial de cliente id=%s label=%q", ap.id, ap.ip, cred.ID, cred.Label)
	_ = ap.writeJSON(protocol.MsgAdminProvisionResp, protocol.AdminProvisionRespPayload{
		OK:            true,
		CredentialID:  cred.ID,
		ClientToken:   cred.ClientToken,
		ClientCertDER: cred.ClientCertDER,
		ClientKeyDER:  cred.ClientKeyDER,
	})
}

func (s *Server) handleAdminRevokeClient(ap *adminPeer, req protocol.AdminRevokeClientPayload) {
	credID := strings.TrimSpace(req.CredentialID)
	if credID == "" && req.ClientID != "" {
		s.mu.RLock()
		if cp, ok := s.clients[req.ClientID]; ok {
			credID = cp.credentialID
		}
		s.mu.RUnlock()
	}
	if credID == "" {
		_ = ap.writeJSON(protocol.MsgSessionEvent, protocol.SessionEventPayload{
			Event:  "error",
			Reason: "No se encontró la credencial del equipo a revocar.",
		})
		return
	}
	if err := s.identity.RevokeClient(credID); err != nil {
		_ = ap.writeJSON(protocol.MsgSessionEvent, protocol.SessionEventPayload{
			Event:  "error",
			Reason: "No se pudo revocar la credencial: " + err.Error(),
		})
		return
	}

	log.Printf("[auditoria] admin=%s (%s) revocó credencial=%s", ap.id, ap.ip, credID)

	// Immediately disconnect all live clients using the revoked credential.
	s.mu.RLock()
	var victims []*clientPeer
	for _, cp := range s.clients {
		if cp.credentialID == credID {
			victims = append(victims, cp)
		}
	}
	s.mu.RUnlock()

	for _, cp := range victims {
		_ = cp.writeJSON(protocol.MsgSessionStop, protocol.SessionStopPayload{
			SessionID: cp.sessionID,
			Reason:    "Credencial revocada por el administrador",
		})
		_ = cp.conn.Close()
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
		hostname: cp.hostname,
		admin:    ap,
		active:   false,
	}
	hostname := cp.hostname
	s.mu.Unlock()

	log.Printf("[auditoria] sesión solicitada sid=%s admin=%s (%s) -> cliente=%s (%s)", sid, ap.id, ap.ip, hostname, cp.ip)

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
		sess.startedAt = time.Now()
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
		log.Printf("[auditoria] sesión iniciada sid=%s admin=%s cliente=%s", resp.SessionID, adm.id, hostname)
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
		log.Printf("[auditoria] sesión denegada sid=%s cliente=%s motivo=%q", resp.SessionID, hostname, reason)
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
	var startedAt time.Time

	s.mu.Lock()
	sid = cp.sessionID
	if sid != "" && (stop.SessionID == "" || stop.SessionID == sid) {
		if sess, ok := s.sessions[sid]; ok {
			adm = sess.admin
			hostname = cp.hostname
			startedAt = sess.startedAt
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
		logSessionEnded(sid, hostname, startedAt, reason)
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
	var startedAt time.Time

	s.mu.Lock()
	delete(s.clients, cp.id)
	if cp.sessionID != "" {
		if sess, ok := s.sessions[cp.sessionID]; ok {
			adm = sess.admin
			sid = sess.id
			hostname = cp.hostname
			startedAt = sess.startedAt
			if adm.sessionID == sid {
				adm.sessionID = ""
			}
			delete(s.sessions, sid)
		}
	}
	s.mu.Unlock()

	if adm != nil {
		logSessionEnded(sid, hostname, startedAt, "El equipo se ha desconectado.")
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
	var sid, hostname string
	var startedAt time.Time

	s.mu.Lock()
	sid = ap.sessionID
	if sid != "" {
		ap.sessionID = ""
		if sess, ok := s.sessions[sid]; ok {
			hostname = sess.hostname
			startedAt = sess.startedAt
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
		logSessionEnded(sid, hostname, startedAt, reason)
		_ = cp.writeJSON(protocol.MsgSessionStop, protocol.SessionStopPayload{
			SessionID: sid,
			Reason:    reason,
		})
		s.broadcastClientList()
	}
}

func logSessionEnded(sid, hostname string, startedAt time.Time, reason string) {
	if !startedAt.IsZero() {
		dur := time.Since(startedAt).Round(time.Second)
		log.Printf("[auditoria] sesión finalizada sid=%s cliente=%s duracion=%s motivo=%q", sid, hostname, dur, reason)
	} else {
		log.Printf("[auditoria] solicitud cancelada sid=%s cliente=%s motivo=%q", sid, hostname, reason)
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
			CredentialID: c.credentialID,
			Hostname:     c.hostname,
			OS:           c.os,
			Username:     c.username,
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

func sanitizeText(raw string, maxRunes int) string {
	var b strings.Builder
	count := 0
	for _, r := range strings.TrimSpace(raw) {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		count++
		if count >= maxRunes {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func sanitizeOS(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "windows":
		return "windows"
	case "linux":
		return "linux"
	case "darwin", "macos":
		return "macos"
	default:
		return ""
	}
}
