package relay

import (
	"crypto/tls"
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

// SetRateLimits overrides default per-IP concurrency and rate-limiting parameters (primarily for tests).
func (s *Server) SetRateLimits(maxInflight, maxPerWin int, window time.Duration) {
	s.ipLimit = newIPLimiter(maxInflight, maxPerWin, window)
}

// Start binds a TLS 1.3 listener on addr and serves incoming connections in the background.
func (s *Server) Start(addr string) (net.Listener, error) {
	tlsCfg := security.ServerTLSConfig(s.identity.Certificate, s.identity.ClientCAs)
	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return nil, err
	}
	go func() {
		_ = s.Serve(ln)
	}()
	return ln, nil
}

// Serve accepts incoming connections on ln until closed.
func (s *Server) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
	}
}

// Close disconnects all active sessions, clients, and admins cleanly.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, cp := range s.clients {
		_ = cp.conn.Close()
	}
	for _, ap := range s.admins {
		_ = ap.conn.Close()
	}
	s.clients = make(map[string]*clientPeer)
	s.admins = make(map[string]*adminPeer)
	s.sessions = make(map[string]*bridgeSession)
}

func (s *Server) handleConn(rawConn net.Conn) {
	ip := remoteIP(rawConn)
	if !s.ipLimit.acquire(ip) {
		log.Printf("[seguridad] tasa de conexiones excedida desde %s", ip)
		_ = rawConn.Close()
		return
	}
	defer s.ipLimit.release(ip)

	select {
	case s.handshakeSem <- struct{}{}:
	default:
		log.Printf("[seguridad] límite de handshakes TLS concurrentes alcanzado (%s)", ip)
		_ = rawConn.Close()
		return
	}

	semReleased := false
	releaseSem := func() {
		if !semReleased {
			<-s.handshakeSem
			semReleased = true
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

func (s *Server) snapshotClientsLocked() []protocol.ClientSummary {
	out := make([]protocol.ClientSummary, 0, len(s.clients))
	for _, c := range s.clients {
		out = append(out, protocol.ClientSummary{
			ID:           c.id,
			CredentialID: c.credentialID,
			Hostname:     c.hostname,
			Username:     c.username,
			Version:      c.version,
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
