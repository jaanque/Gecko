package admin

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"remote-control/internal/protocol"
	"remote-control/internal/security"
	"remote-control/internal/ws"
)

// ClientInfo is the JSON snapshot sent to the Admin web UI.
type ClientInfo struct {
	ID          string `json:"id"`
	Hostname    string `json:"hostname"`
	IP          string `json:"ip"`
	OS          string `json:"os"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Status      string `json:"status"` // "online", "pending", "active"
	ConnectedAt int64  `json:"connected_at"`
}

type agentConn struct {
	id          string
	hostname    string
	ip          string
	os          string
	width       int
	height      int
	status      string
	sessionID   string
	connectedAt time.Time

	conn    net.Conn
	writeMu sync.Mutex
}

func (a *agentConn) writeMessage(msgType uint8, payload []byte) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_ = a.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return protocol.WriteMessage(a.conn, msgType, payload)
}

func (a *agentConn) writeJSON(msgType uint8, v any) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_ = a.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return protocol.WriteJSON(a.conn, msgType, v)
}

type viewerConn struct {
	ws        *ws.Conn
	sessionID string
	frameCh   chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

func newViewerConn(c *ws.Conn) *viewerConn {
	v := &viewerConn{
		ws:      c,
		frameCh: make(chan []byte, 4),
		done:    make(chan struct{}),
	}
	go v.framePump()
	return v
}

func (v *viewerConn) framePump() {
	for {
		select {
		case <-v.done:
			return
		case frame := <-v.frameCh:
			if err := v.ws.WriteBinary(frame); err != nil {
				v.Close()
				return
			}
		}
	}
}

// QueueFrame enqueues a binary frame for the browser, dropping an older queued frame if congested.
func (v *viewerConn) QueueFrame(frame []byte) {
	select {
	case v.frameCh <- frame:
	default:
		select {
		case <-v.frameCh:
		default:
		}
		select {
		case v.frameCh <- frame:
		default:
		}
	}
}

func (v *viewerConn) SendJSON(msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	if err := v.ws.WriteText(data); err != nil {
		v.Close()
	}
}

func (v *viewerConn) Close() {
	v.closeOnce.Do(func() {
		close(v.done)
		_ = v.ws.Close()
	})
}

type sessionState struct {
	id       string
	clientID string
	viewer   *viewerConn
	active   bool
}

// Hub coordinates TLS agent connections and local Admin UI viewers.
type Hub struct {
	identity *security.ServerIdentity

	mu       sync.RWMutex
	agents   map[string]*agentConn
	viewers  map[*viewerConn]struct{}
	sessions map[string]*sessionState
}

// NewHub initializes a new Hub.
func NewHub(identity *security.ServerIdentity) *Hub {
	return &Hub{
		identity: identity,
		agents:   make(map[string]*agentConn),
		viewers:  make(map[*viewerConn]struct{}),
		sessions: make(map[string]*sessionState),
	}
}

// ServeAgents accepts TLS 1.3 connections from remote clients until ln is closed.
func (h *Hub) ServeAgents(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go h.handleAgentConn(conn)
	}
}

func (h *Hub) handleAgentConn(rawConn net.Conn) {
	defer rawConn.Close()

	if tlsConn, ok := rawConn.(*tls.Conn); ok {
		_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
		if err := tlsConn.Handshake(); err != nil {
			return
		}
	}

	_ = rawConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	msgType, payload, err := protocol.ReadMessage(rawConn)
	if err != nil || msgType != protocol.MsgAuth {
		return
	}

	var auth protocol.AuthPayload
	if err := json.Unmarshal(payload, &auth); err != nil {
		return
	}

	if !security.ConstantTimeTokenEqual(auth.Token, h.identity.AuthToken) {
		_ = rawConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = protocol.WriteJSON(rawConn, protocol.MsgAuthResp, protocol.AuthRespPayload{
			OK:    false,
			Error: "token inválido",
		})
		return
	}

	clientID, err := security.RandomHex(8)
	if err != nil {
		return
	}

	hostIP, _, _ := net.SplitHostPort(rawConn.RemoteAddr().String())
	if hostIP == "" {
		hostIP = rawConn.RemoteAddr().String()
	}
	hostname := auth.Hostname
	if hostname == "" {
		hostname = "EQUIPO-" + clientID[:4]
	}

	agent := &agentConn{
		id:          clientID,
		hostname:    hostname,
		ip:          hostIP,
		os:          auth.OS,
		width:       auth.Width,
		height:      auth.Height,
		status:      "online",
		connectedAt: time.Now(),
		conn:        rawConn,
	}

	if err := agent.writeJSON(protocol.MsgAuthResp, protocol.AuthRespPayload{
		OK:       true,
		ClientID: clientID,
	}); err != nil {
		return
	}

	h.mu.Lock()
	h.agents[clientID] = agent
	h.mu.Unlock()
	log.Printf("[cliente] conectado: %s (%s) id=%s", agent.hostname, agent.ip, agent.id)
	h.broadcastClients()

	defer func() {
		h.unregisterAgent(agent)
		log.Printf("[cliente] desconectado: %s (%s)", agent.hostname, agent.ip)
	}()

	// Main client read loop.
	for {
		_ = rawConn.SetReadDeadline(time.Now().Add(35 * time.Second))
		mType, data, err := protocol.ReadMessage(rawConn)
		if err != nil {
			return
		}

		switch mType {
		case protocol.MsgPing:
			_ = agent.writeMessage(protocol.MsgPong, data)

		case protocol.MsgPong:
			// alive

		case protocol.MsgConsentResp:
			var resp protocol.ConsentRespPayload
			if err := json.Unmarshal(data, &resp); err == nil {
				h.handleConsentResp(agent, resp)
			}

		case protocol.MsgSessionStop:
			var stop protocol.SessionStopPayload
			_ = json.Unmarshal(data, &stop)
			h.handleAgentSessionStop(agent, stop)

		case protocol.MsgFrame:
			h.forwardFrame(agent, data)
		}
	}
}

func (h *Hub) unregisterAgent(agent *agentConn) {
	var notifyViewer *viewerConn
	var endedSID string
	var hostname string

	h.mu.Lock()
	delete(h.agents, agent.id)
	if agent.sessionID != "" {
		if sess, ok := h.sessions[agent.sessionID]; ok {
			notifyViewer = sess.viewer
			endedSID = sess.id
			hostname = agent.hostname
			if sess.viewer.sessionID == sess.id {
				sess.viewer.sessionID = ""
			}
			delete(h.sessions, agent.sessionID)
		}
	}
	h.mu.Unlock()

	if notifyViewer != nil {
		notifyViewer.SendJSON(map[string]any{
			"type":       "session_ended",
			"session_id": endedSID,
			"hostname":   hostname,
			"reason":     "El equipo se ha desconectado",
		})
	}
	h.broadcastClients()
}

func (h *Hub) handleConsentResp(agent *agentConn, resp protocol.ConsentRespPayload) {
	var viewer *viewerConn
	var hostname string
	var w, ht int

	h.mu.Lock()
	sess, ok := h.sessions[resp.SessionID]
	if !ok || sess.clientID != agent.id || agent.sessionID != resp.SessionID {
		h.mu.Unlock()
		return
	}
	viewer = sess.viewer
	hostname = agent.hostname
	w, ht = agent.width, agent.height

	if resp.Allowed {
		sess.active = true
		agent.status = "active"
	} else {
		agent.status = "online"
		agent.sessionID = ""
		if viewer.sessionID == sess.id {
			viewer.sessionID = ""
		}
		delete(h.sessions, sess.id)
	}
	h.mu.Unlock()

	if resp.Allowed {
		viewer.SendJSON(map[string]any{
			"type":       "session_started",
			"session_id": resp.SessionID,
			"client_id":  agent.id,
			"hostname":   hostname,
			"width":      w,
			"height":     ht,
		})
	} else {
		reason := resp.Reason
		if reason == "" {
			reason = "Solicitud denegada"
		}
		viewer.SendJSON(map[string]any{
			"type":       "session_denied",
			"session_id": resp.SessionID,
			"client_id":  agent.id,
			"hostname":   hostname,
			"reason":     reason,
		})
	}
	h.broadcastClients()
}

func (h *Hub) handleAgentSessionStop(agent *agentConn, stop protocol.SessionStopPayload) {
	var viewer *viewerConn
	var sid string
	var hostname string

	h.mu.Lock()
	sid = agent.sessionID
	if sid != "" && (stop.SessionID == "" || stop.SessionID == sid) {
		if sess, ok := h.sessions[sid]; ok {
			viewer = sess.viewer
			hostname = agent.hostname
			if viewer.sessionID == sid {
				viewer.sessionID = ""
			}
			delete(h.sessions, sid)
		}
		agent.sessionID = ""
		agent.status = "online"
	}
	h.mu.Unlock()

	if viewer != nil {
		reason := stop.Reason
		if reason == "" {
			reason = "Sesión finalizada por el equipo remoto"
		}
		viewer.SendJSON(map[string]any{
			"type":       "session_ended",
			"session_id": sid,
			"hostname":   hostname,
			"reason":     reason,
		})
		h.broadcastClients()
	}
}

func (h *Hub) forwardFrame(agent *agentConn, frame []byte) {
	h.mu.RLock()
	sid := agent.sessionID
	var viewer *viewerConn
	if sid != "" {
		if sess, ok := h.sessions[sid]; ok && sess.active {
			viewer = sess.viewer
		}
	}
	h.mu.RUnlock()

	if viewer != nil {
		viewer.QueueFrame(frame)
	}
}

// ServeViewer registers a browser WebSocket viewer and processes its commands until closed.
func (h *Hub) ServeViewer(c *ws.Conn) {
	viewer := newViewerConn(c)

	h.mu.Lock()
	h.viewers[viewer] = struct{}{}
	clients := h.snapshotClientsLocked()
	h.mu.Unlock()

	viewer.SendJSON(map[string]any{
		"type":    "clients_update",
		"clients": clients,
	})

	defer func() {
		h.endViewerSession(viewer, "Administrador desconectado")
		h.mu.Lock()
		delete(h.viewers, viewer)
		h.mu.Unlock()
		viewer.Close()
	}()

	for {
		op, data, err := c.ReadMessage()
		if err != nil {
			return
		}

		if op == ws.OpBinary {
			h.handleViewerBinaryInput(viewer, data)
			continue
		}
		if op == ws.OpText {
			h.handleViewerTextCommand(viewer, data)
		}
	}
}

func (h *Hub) handleViewerBinaryInput(viewer *viewerConn, data []byte) {
	if len(data) < 2 {
		return
	}
	msgType := data[0]
	payload := data[1:]
	if msgType != protocol.MsgMouse && msgType != protocol.MsgKey {
		return
	}

	h.mu.RLock()
	sid := viewer.sessionID
	var agent *agentConn
	if sid != "" {
		if sess, ok := h.sessions[sid]; ok && sess.active {
			agent = h.agents[sess.clientID]
		}
	}
	h.mu.RUnlock()

	if agent != nil {
		_ = agent.writeMessage(msgType, payload)
	}
}

type viewerCmd struct {
	Type     string `json:"type"`
	ClientID string `json:"client_id"`
	Quality  int    `json:"quality"`
	Keyframe bool   `json:"keyframe"`
}

func (h *Hub) handleViewerTextCommand(viewer *viewerConn, raw []byte) {
	var cmd viewerCmd
	if err := json.Unmarshal(raw, &cmd); err != nil {
		return
	}

	switch cmd.Type {
	case "connect":
		h.requestConnect(viewer, cmd.ClientID)
	case "disconnect", "cancel":
		h.endViewerSession(viewer, "Desconectado por el administrador")
		viewer.SendJSON(map[string]any{
			"type":   "session_ended",
			"reason": "Sesión cerrada",
		})
	case "stream_config":
		h.sendStreamConfig(viewer, cmd.Quality, cmd.Keyframe)
	}
}

func (h *Hub) requestConnect(viewer *viewerConn, clientID string) {
	// Stop any previous session this viewer had.
	h.endViewerSession(viewer, "Nueva conexión iniciada")

	sid, err := security.RandomHex(12)
	if err != nil {
		return
	}

	h.mu.Lock()
	agent, ok := h.agents[clientID]
	if !ok {
		h.mu.Unlock()
		viewer.SendJSON(map[string]any{
			"type":    "error",
			"message": "El equipo ya no está conectado.",
		})
		return
	}
	if agent.status != "online" {
		h.mu.Unlock()
		viewer.SendJSON(map[string]any{
			"type":    "error",
			"message": "El equipo ya tiene una solicitud o sesión activa.",
		})
		return
	}

	agent.status = "pending"
	agent.sessionID = sid
	viewer.sessionID = sid
	h.sessions[sid] = &sessionState{
		id:       sid,
		clientID: agent.id,
		viewer:   viewer,
		active:   false,
	}
	hostname := agent.hostname
	h.mu.Unlock()

	if err := agent.writeJSON(protocol.MsgConsentReq, protocol.ConsentReqPayload{
		SessionID:  sid,
		TimeoutSec: 60,
	}); err != nil {
		h.endViewerSession(viewer, "Error de comunicación")
		viewer.SendJSON(map[string]any{
			"type":    "error",
			"message": "No se pudo contactar con el equipo.",
		})
		return
	}

	viewer.SendJSON(map[string]any{
		"type":       "session_pending",
		"session_id": sid,
		"client_id":  clientID,
		"hostname":   hostname,
	})
	h.broadcastClients()
}

func (h *Hub) endViewerSession(viewer *viewerConn, reason string) {
	var agent *agentConn
	var sid string

	h.mu.Lock()
	sid = viewer.sessionID
	if sid != "" {
		viewer.sessionID = ""
		if sess, ok := h.sessions[sid]; ok {
			if a, exists := h.agents[sess.clientID]; exists && a.sessionID == sid {
				a.sessionID = ""
				a.status = "online"
				agent = a
			}
			delete(h.sessions, sid)
		}
	}
	h.mu.Unlock()

	if agent != nil {
		_ = agent.writeJSON(protocol.MsgSessionStop, protocol.SessionStopPayload{
			SessionID: sid,
			Reason:    reason,
		})
		h.broadcastClients()
	}
}

func (h *Hub) sendStreamConfig(viewer *viewerConn, quality int, keyframe bool) {
	h.mu.RLock()
	sid := viewer.sessionID
	var agent *agentConn
	if sid != "" {
		if sess, ok := h.sessions[sid]; ok && sess.active {
			agent = h.agents[sess.clientID]
		}
	}
	h.mu.RUnlock()

	if agent != nil {
		_ = agent.writeJSON(protocol.MsgStreamConfig, protocol.StreamConfigPayload{
			Quality:  quality,
			Keyframe: keyframe,
		})
	}
}

// ListClients returns a sorted snapshot of currently online clients.
func (h *Hub) ListClients() []ClientInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.snapshotClientsLocked()
}

func (h *Hub) snapshotClientsLocked() []ClientInfo {
	out := make([]ClientInfo, 0, len(h.agents))
	for _, a := range h.agents {
		out = append(out, ClientInfo{
			ID:          a.id,
			Hostname:    a.hostname,
			IP:          a.ip,
			OS:          a.os,
			Width:       a.width,
			Height:      a.height,
			Status:      a.status,
			ConnectedAt: a.connectedAt.Unix(),
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

func (h *Hub) broadcastClients() {
	h.mu.RLock()
	clients := h.snapshotClientsLocked()
	viewers := make([]*viewerConn, 0, len(h.viewers))
	for v := range h.viewers {
		viewers = append(viewers, v)
	}
	h.mu.RUnlock()

	msg := map[string]any{
		"type":    "clients_update",
		"clients": clients,
	}
	for _, v := range viewers {
		v.SendJSON(msg)
	}
}
