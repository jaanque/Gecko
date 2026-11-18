package relay

import (
	"crypto/x509"
	"encoding/json"
	"log"
	"net"
	"time"

	"remote-control/internal/protocol"
	"remote-control/internal/security"
)

type clientPeer struct {
	peerConn
	id           string
	credentialID string
	hostname     string
	username     string
	version      string
	ip           string
	width        int
	height       int
	monitorCount int
	status       string // "online", "pending", "active"
	sessionID    string
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
	username := sanitizeText(auth.Username, maxUsernameRunes)
	clientVer := sanitizeText(auth.Version, 32)

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
		username:     username,
		version:      clientVer,
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
	log.Printf("[cliente] conectado: %s user=%s (%s) id=%s cred=%s", cp.hostname, cp.username, cp.ip, cp.id, cp.credentialID)
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
				s.forwardToAdmin(cp, protocol.MsgPong, data)
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
			s.forwardToAdmin(cp, protocol.MsgFrame, data)

		case protocol.MsgClipboard:
			var clip protocol.ClipboardPayload
			if json.Unmarshal(data, &clip) == nil {
				s.forwardToAdmin(cp, protocol.MsgClipboard, data)
			}

		case protocol.MsgFileAccept:
			var fa protocol.FileAcceptPayload
			if json.Unmarshal(data, &fa) == nil {
				s.forwardToAdmin(cp, protocol.MsgFileAccept, data)
			}

		case protocol.MsgFileDone:
			var fd protocol.FileDonePayload
			if json.Unmarshal(data, &fd) == nil {
				s.forwardToAdmin(cp, protocol.MsgFileDone, data)
			}
		}
	}
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
