package relay

import (
	"log"
	"time"

	"remote-control/internal/protocol"
)

type bridgeSession struct {
	id        string
	clientID  string
	hostname  string
	admin     *adminPeer
	active    bool
	startedAt time.Time
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

func (s *Server) forwardToAdmin(cp *clientPeer, msgType uint8, data []byte) {
	if adm := s.activeAdminForClient(cp); adm != nil {
		_ = adm.writeMessage(msgType, data)
	}
}

func (s *Server) forwardToClient(ap *adminPeer, msgType uint8, data []byte) {
	if cp := s.activeClientForAdmin(ap); cp != nil {
		_ = cp.writeMessage(msgType, data)
	}
}

func (s *Server) activeAdminForClient(cp *clientPeer) *adminPeer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cp.sessionID == "" {
		return nil
	}
	sess := s.sessions[cp.sessionID]
	if sess == nil || sess.clientID != cp.id || !sess.active {
		return nil
	}
	return sess.admin
}

func (s *Server) activeClientForAdmin(ap *adminPeer) *clientPeer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ap.sessionID == "" {
		return nil
	}
	sess := s.sessions[ap.sessionID]
	if sess == nil || sess.admin != ap || !sess.active {
		return nil
	}
	return s.clients[sess.clientID]
}
