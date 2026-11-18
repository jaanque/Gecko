package admin

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"remote-control/internal/config"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
)

// ClientInfo is a snapshot of a connected remote PC.
type ClientInfo struct {
	ID           string
	Hostname     string
	IP           string
	OS           string
	Width        int
	Height       int
	MonitorCount int
	Status       string // "online", "pending", "active"
	ConnectedAt  time.Time
}

// HubEvents receives asynchronous notifications from the Hub for the desktop UI.
type HubEvents struct {
	OnClientsChanged func(clients []ClientInfo)
	OnSessionPending func(sessionID, clientID, hostname string)
	OnSessionStarted func(sessionID, clientID, hostname string, width, height, monitorCount int)
	OnSessionDenied  func(sessionID, clientID, hostname, reason string)
	OnSessionEnded   func(sessionID, hostname, reason string)
	OnFrame          func(sessionID string, frameData []byte)
	OnLatency        func(rttMs int)
}

type agentConn struct {
	id           string
	hostname     string
	ip           string
	os           string
	width        int
	height       int
	monitorCount int
	status       string
	sessionID    string
	connectedAt  time.Time

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

type activeSession struct {
	id       string
	clientID string
	hostname string
	active   bool
}

// Hub manages TLS 1.3 client connections and routes the active remote session directly to the desktop UI.
type Hub struct {
	identity  *security.ServerIdentity
	agentPort int

	mu      sync.RWMutex
	events  HubEvents
	agents  map[string]*agentConn
	session *activeSession
}

// NewHub constructs a Hub.
func NewHub(identity *security.ServerIdentity, agentPort int) *Hub {
	return &Hub{
		identity:  identity,
		agentPort: agentPort,
		agents:    make(map[string]*agentConn),
	}
}

// SetEvents registers the desktop UI callbacks.
func (h *Hub) SetEvents(ev HubEvents) {
	h.mu.Lock()
	h.events = ev
	h.mu.Unlock()
}

// AgentPort returns the TCP port clients connect to.
func (h *Hub) AgentPort() int {
	return h.agentPort
}

// CertFingerprintShort returns a human-readable prefix of the server TLS SHA-256 fingerprint.
func (h *Hub) CertFingerprintShort() string {
	fp := h.identity.CertSHA256
	if len(fp) < 16 {
		return fp
	}
	upper := strings.ToUpper(fp[:16])
	var parts []string
	for i := 0; i < len(upper); i += 4 {
		parts = append(parts, upper[i:i+4])
	}
	return strings.Join(parts, ":") + "…"
}

// GenerateClientBinary creates a ready-to-run ClienteRemoto.exe embedding the server address,
// TLS certificate SHA-256 pin, shared auth token, and optional PNG logo.
func (h *Hub) GenerateClientBinary(template []byte, serverAddr string, logoPNG []byte) ([]byte, error) {
	if len(template) == 0 {
		return nil, errors.New("plantilla de cliente no disponible; compila el proyecto con build.bat")
	}
	addr := strings.TrimSpace(serverAddr)
	if addr == "" {
		return nil, errors.New("la dirección del servidor es obligatoria")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		if !strings.Contains(addr, ":") {
			addr = net.JoinHostPort(addr, strconv.Itoa(h.agentPort))
		} else {
			return nil, fmt.Errorf("formato inválido (usa IP:PUERTO, ej. 192.168.1.50:%d)", h.agentPort)
		}
	}
	return config.AppendToBinary(template, config.ClientConfig{
		ServerAddr: addr,
		CertSHA256: h.identity.CertSHA256,
		AuthToken:  h.identity.AuthToken,
		LogoPNG:    logoPNG,
	})
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
		if tc, ok := tlsConn.NetConn().(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			_ = tc.SetReadBuffer(512 * 1024)
			_ = tc.SetWriteBuffer(256 * 1024)
		}
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

	monCount := len(auth.Monitors)
	if monCount <= 0 {
		monCount = 1
	}

	agent := &agentConn{
		id:           clientID,
		hostname:     hostname,
		ip:           hostIP,
		os:           auth.OS,
		width:        auth.Width,
		height:       auth.Height,
		monitorCount: monCount,
		status:       "online",
		connectedAt:  time.Now(),
		conn:         rawConn,
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
	log.Printf("[cliente] conectado: %s (%s)", agent.hostname, agent.ip)
	h.notifyClients()

	defer func() {
		h.unregisterAgent(agent)
		log.Printf("[cliente] desconectado: %s (%s)", agent.hostname, agent.ip)
	}()

	// Periodic RTT probe while this agent is in an active session.
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var ts [8]byte
		for {
			select {
			case <-pingDone:
				return
			case <-ticker.C:
				if h.activeAgent() == agent {
					binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixNano()))
					_ = agent.writeMessage(protocol.MsgPing, ts[:])
				}
			}
		}
	}()
	defer close(pingDone)

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
			if len(data) == 8 && h.activeAgent() == agent {
				sent := int64(binary.BigEndian.Uint64(data))
				rttMs := int((time.Now().UnixNano() - sent) / int64(time.Millisecond))
				if rttMs < 0 {
					rttMs = 0
				}
				h.mu.RLock()
				cb := h.events.OnLatency
				h.mu.RUnlock()
				if cb != nil {
					cb(rttMs)
				}
			}

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
			h.handleAgentFrame(agent, data)
		}
	}
}

func (h *Hub) unregisterAgent(agent *agentConn) {
	var endedSID, hostname string
	var onEnded func(string, string, string)

	h.mu.Lock()
	delete(h.agents, agent.id)
	if h.session != nil && h.session.clientID == agent.id {
		endedSID = h.session.id
		hostname = agent.hostname
		onEnded = h.events.OnSessionEnded
		h.session = nil
	}
	h.mu.Unlock()

	if onEnded != nil && endedSID != "" {
		onEnded(endedSID, hostname, "El equipo se ha desconectado.")
	}
	h.notifyClients()
}

func (h *Hub) handleConsentResp(agent *agentConn, resp protocol.ConsentRespPayload) {
	h.mu.Lock()
	if h.session == nil || h.session.id != resp.SessionID || h.session.clientID != agent.id {
		h.mu.Unlock()
		return
	}
	hostname := agent.hostname
	w, ht, monCount := agent.width, agent.height, agent.monitorCount
	onStarted := h.events.OnSessionStarted
	onDenied := h.events.OnSessionDenied

	if resp.Allowed {
		h.session.active = true
		agent.status = "active"
	} else {
		agent.status = "online"
		agent.sessionID = ""
		h.session = nil
	}
	h.mu.Unlock()

	if resp.Allowed {
		if onStarted != nil {
			onStarted(resp.SessionID, agent.id, hostname, w, ht, monCount)
		}
	} else {
		reason := resp.Reason
		if reason == "" {
			reason = "Solicitud denegada por el usuario."
		}
		if onDenied != nil {
			onDenied(resp.SessionID, agent.id, hostname, reason)
		}
	}
	h.notifyClients()
}

func (h *Hub) handleAgentSessionStop(agent *agentConn, stop protocol.SessionStopPayload) {
	h.mu.Lock()
	if h.session == nil || h.session.clientID != agent.id {
		h.mu.Unlock()
		return
	}
	if stop.SessionID != "" && stop.SessionID != h.session.id {
		h.mu.Unlock()
		return
	}
	sid := h.session.id
	hostname := agent.hostname
	onEnded := h.events.OnSessionEnded
	h.session = nil
	agent.status = "online"
	agent.sessionID = ""
	h.mu.Unlock()

	if onEnded != nil {
		reason := stop.Reason
		if reason == "" {
			reason = "Sesión finalizada por el equipo remoto."
		}
		onEnded(sid, hostname, reason)
	}
	h.notifyClients()
}

func (h *Hub) handleAgentFrame(agent *agentConn, frame []byte) {
	h.mu.RLock()
	var sid string
	var onFrame func(string, []byte)
	if h.session != nil && h.session.active && h.session.clientID == agent.id {
		sid = h.session.id
		onFrame = h.events.OnFrame
	}
	h.mu.RUnlock()

	if onFrame != nil && sid != "" {
		onFrame(sid, frame)
	}
}

// RequestConnect sends a consent request to the selected client PC.
func (h *Hub) RequestConnect(clientID string) error {
	h.DisconnectSession("Nueva solicitud iniciada")

	sid, err := security.RandomHex(12)
	if err != nil {
		return err
	}

	h.mu.Lock()
	agent, ok := h.agents[clientID]
	if !ok {
		h.mu.Unlock()
		return errors.New("el equipo ya no está conectado")
	}
	if agent.status != "online" {
		h.mu.Unlock()
		return errors.New("el equipo ya tiene una solicitud o sesión activa")
	}
	agent.status = "pending"
	agent.sessionID = sid
	h.session = &activeSession{
		id:       sid,
		clientID: agent.id,
		hostname: agent.hostname,
		active:   false,
	}
	hostname := agent.hostname
	onPending := h.events.OnSessionPending
	h.mu.Unlock()

	if err := agent.writeJSON(protocol.MsgConsentReq, protocol.ConsentReqPayload{
		SessionID:  sid,
		TimeoutSec: 60,
	}); err != nil {
		h.DisconnectSession("Error de red")
		return errors.New("no se pudo contactar con el equipo")
	}

	if onPending != nil {
		onPending(sid, clientID, hostname)
	}
	h.notifyClients()
	return nil
}

// DisconnectSession terminates any active or pending session cleanly.
func (h *Hub) DisconnectSession(reason string) {
	h.mu.Lock()
	if h.session == nil {
		h.mu.Unlock()
		return
	}
	sess := h.session
	h.session = nil
	var agent *agentConn
	if a, ok := h.agents[sess.clientID]; ok && a.sessionID == sess.id {
		a.sessionID = ""
		a.status = "online"
		agent = a
	}
	h.mu.Unlock()

	if agent != nil {
		_ = agent.writeJSON(protocol.MsgSessionStop, protocol.SessionStopPayload{
			SessionID: sess.id,
			Reason:    reason,
		})
	}
	h.notifyClients()
}

// SendFrameAck acknowledges a decoded frame so the client's sliding window advances smoothly.
func (h *Hub) SendFrameAck(frameID uint32, sentUnixNano int64) {
	agent := h.activeAgent()
	if agent == nil {
		return
	}
	_ = agent.writeMessage(protocol.MsgFrameAck, protocol.EncodeFrameAck(protocol.FrameAck{
		FrameID:      frameID,
		SentUnixNano: sentUnixNano,
	}))
}

// SendMouse forwards a mouse event to the active remote client.
func (h *Hub) SendMouse(ev protocol.MouseEvent) {
	agent := h.activeAgent()
	if agent == nil {
		return
	}
	_ = agent.writeMessage(protocol.MsgMouse, protocol.EncodeMouse(ev))
}

// SendKey forwards a keyboard event to the active remote client.
func (h *Hub) SendKey(ev protocol.KeyEvent) {
	agent := h.activeAgent()
	if agent == nil {
		return
	}
	_ = agent.writeMessage(protocol.MsgKey, protocol.EncodeKey(ev))
}

// SendStreamConfig updates JPEG quality or requests a full keyframe on the active client.
func (h *Hub) SendStreamConfig(quality int, keyframe bool) {
	agent := h.activeAgent()
	if agent == nil {
		return
	}
	_ = agent.writeJSON(protocol.MsgStreamConfig, protocol.StreamConfigPayload{
		Quality:  quality,
		Keyframe: keyframe,
	})
}

// SendSelectMonitor switches the active client stream to a specific monitor (0..N-1) or all monitors (-1).
func (h *Hub) SendSelectMonitor(monitorIndex int) {
	agent := h.activeAgent()
	if agent == nil {
		return
	}
	m := monitorIndex
	_ = agent.writeJSON(protocol.MsgStreamConfig, protocol.StreamConfigPayload{
		Monitor:  &m,
		Keyframe: true,
	})
}

func (h *Hub) activeAgent() *agentConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.session == nil || !h.session.active {
		return nil
	}
	return h.agents[h.session.clientID]
}

// ListClients returns a sorted snapshot of all currently connected clients.
func (h *Hub) ListClients() []ClientInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.snapshotClientsLocked()
}

func (h *Hub) snapshotClientsLocked() []ClientInfo {
	out := make([]ClientInfo, 0, len(h.agents))
	for _, a := range h.agents {
		out = append(out, ClientInfo{
			ID:           a.id,
			Hostname:     a.hostname,
			IP:           a.ip,
			OS:           a.os,
			Width:        a.width,
			Height:       a.height,
			MonitorCount: a.monitorCount,
			Status:       a.status,
			ConnectedAt:  a.connectedAt,
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

func (h *Hub) notifyClients() {
	h.mu.RLock()
	cb := h.events.OnClientsChanged
	clients := h.snapshotClientsLocked()
	h.mu.RUnlock()
	if cb != nil {
		cb(clients)
	}
}

// DetectLocalIPs returns non-loopback IPv4 addresses sorted with private LAN addresses first.
func DetectLocalIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	type scoredIP struct {
		ip    string
		score int
	}
	var list []scoredIP

	for _, iface := range ifaces {
		if (iface.Flags&net.FlagUp) == 0 || (iface.Flags&net.FlagLoopback) != 0 {
			continue
		}
		nameLow := strings.ToLower(iface.Name)
		isVirtual := strings.Contains(nameLow, "vethernet") ||
			strings.Contains(nameLow, "vmware") ||
			strings.Contains(nameLow, "virtualbox") ||
			strings.Contains(nameLow, "wsl") ||
			strings.Contains(nameLow, "docker")

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			score := 10
			if ip4[0] == 192 && ip4[1] == 168 {
				score = 1
			} else if ip4[0] == 10 {
				score = 2
			} else if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
				score = 3
			}
			if isVirtual {
				score += 20
			}
			list = append(list, scoredIP{ip: ip4.String(), score: score})
		}
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score < list[j].score
		}
		return list[i].ip < list[j].ip
	})

	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, item := range list {
		if _, dup := seen[item.ip]; dup {
			continue
		}
		seen[item.ip] = struct{}{}
		out = append(out, item.ip)
	}
	return out
}
