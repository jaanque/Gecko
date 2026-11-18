package relay

import (
	"encoding/json"
	"log"
	"net"
	"strings"
	"time"

	"remote-control/internal/protocol"
	"remote-control/internal/security"
)

type adminPeer struct {
	peerConn
	id        string
	ip        string
	sessionID string
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
				s.forwardToClient(ap, mType, data)
			}

		case protocol.MsgMouse:
			if _, err := protocol.DecodeMouse(data); err == nil {
				s.forwardToClient(ap, mType, data)
			}

		case protocol.MsgKey:
			if _, err := protocol.DecodeKey(data); err == nil {
				s.forwardToClient(ap, mType, data)
			}

		case protocol.MsgStreamConfig:
			var sc protocol.StreamConfigPayload
			if json.Unmarshal(data, &sc) == nil {
				s.forwardToClient(ap, mType, data)
			}

		case protocol.MsgClipboard:
			var clip protocol.ClipboardPayload
			if json.Unmarshal(data, &clip) == nil {
				s.forwardToClient(ap, mType, data)
			}

		case protocol.MsgSystemAction:
			var sa protocol.SystemActionPayload
			if json.Unmarshal(data, &sa) == nil && sa.Action != "" {
				s.forwardToClient(ap, mType, data)
			}

		case protocol.MsgFileOffer:
			var fo protocol.FileOfferPayload
			if json.Unmarshal(data, &fo) == nil && fo.TransferID != "" && fo.Size >= 0 && fo.Size <= protocol.MaxFileTransferSize {
				s.forwardToClient(ap, mType, data)
			}

		case protocol.MsgFileChunk:
			if _, err := protocol.DecodeFileChunk(data); err == nil {
				s.forwardToClient(ap, mType, data)
			}

		case protocol.MsgFileDone:
			var fd protocol.FileDonePayload
			if json.Unmarshal(data, &fd) == nil && fd.TransferID != "" {
				s.forwardToClient(ap, mType, data)
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
