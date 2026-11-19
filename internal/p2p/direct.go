package p2p

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"

	"remote-control/internal/protocol"
)

// P2PListener hosts an ephemeral TLS 1.3 direct listener on the client PC
// for direct peer-to-peer / LAN streaming, matching RustDesk's direct connection fallback.
type P2PListener struct {
	listener net.Listener
	token    string
	port     int
	ips      []string
	connCh   chan net.Conn
	stopOnce sync.Once
	closed   chan struct{}
}

// StartP2PListener initializes an ephemeral TLS 1.3 server on port 0 with a random 256-bit secret.
func StartP2PListener() (*P2PListener, error) {
	tlsCert, err := generateEphemeralCert()
	if err != nil {
		return nil, fmt.Errorf("failed to generate ephemeral TLS cert: %w", err)
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(tokenBytes)

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS13,
	}

	ln, err := tls.Listen("tcp", "0.0.0.0:0", tlsCfg)
	if err != nil {
		return nil, err
	}

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, errors.New("invalid listener address type")
	}

	ips := collectLocalIPs()

	p := &P2PListener{
		listener: ln,
		token:    token,
		port:     tcpAddr.Port,
		ips:      ips,
		connCh:   make(chan net.Conn, 1),
		closed:   make(chan struct{}),
	}

	go p.acceptLoop()
	return p, nil
}

// Endpoints returns candidate dial addresses for the viewer (e.g. ["192.168.1.100:54321"]).
func (p *P2PListener) Endpoints() []string {
	res := make([]string, 0, len(p.ips))
	for _, ip := range p.ips {
		res = append(res, net.JoinHostPort(ip, fmt.Sprintf("%d", p.port)))
	}
	return res
}

// Token returns the 256-bit secret token required to authenticate direct connection attempts.
func (p *P2PListener) Token() string {
	return p.token
}

// Conn returns the channel delivering the direct peer connection if established.
func (p *P2PListener) Conn() <-chan net.Conn {
	return p.connCh
}

// Close shuts down the listener and cleans up resources.
func (p *P2PListener) Close() {
	p.stopOnce.Do(func() {
		close(p.closed)
		_ = p.listener.Close()
	})
}

func (p *P2PListener) acceptLoop() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}

		go func(c net.Conn) {
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			msgType, data, err := protocol.ReadMessage(c)
			if err != nil || msgType != protocol.MsgAuth {
				_ = c.Close()
				return
			}

			if strings.TrimSpace(string(data)) != p.token {
				_ = protocol.WriteMessage(c, protocol.MsgAuthResp, []byte("FAIL"))
				_ = c.Close()
				return
			}

			_ = protocol.WriteMessage(c, protocol.MsgAuthResp, []byte("OK"))
			_ = c.SetDeadline(time.Time{})

			select {
			case p.connCh <- c:
			default:
				_ = c.Close()
			}
		}(conn)
	}
}

// DialP2POffer concurrently races candidate endpoints from offer within the given timeout.
// If direct LAN connection succeeds, returns the authenticated TLS 1.3 connection.
// If all fail or timeout expires, returns an error allowing seamless Relay fallback.
func DialP2POffer(ctx context.Context, offer protocol.P2POfferPayload, timeout time.Duration) (net.Conn, error) {
	if len(offer.Endpoints) == 0 || offer.Token == "" {
		return nil, errors.New("empty p2p offer")
	}

	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type dialResult struct {
		conn net.Conn
		err  error
	}

	resCh := make(chan dialResult, len(offer.Endpoints))

	tlsCfg := &tls.Config{
		InsecureSkipVerify: true, // Ephemeral self-signed; auth verified by offer.Token
		MinVersion:         tls.VersionTLS13,
	}

	for _, ep := range offer.Endpoints {
		go func(target string) {
			dialer := &tls.Dialer{
				NetDialer: &net.Dialer{Timeout: timeout},
				Config:    tlsCfg,
			}
			conn, err := dialer.DialContext(dialCtx, "tcp", target)
			if err != nil {
				resCh <- dialResult{nil, err}
				return
			}

			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			if err := protocol.WriteMessage(conn, protocol.MsgAuth, []byte(offer.Token)); err != nil {
				_ = conn.Close()
				resCh <- dialResult{nil, err}
				return
			}

			msgType, data, err := protocol.ReadMessage(conn)
			if err != nil || msgType != protocol.MsgAuthResp || string(data) != "OK" {
				_ = conn.Close()
				resCh <- dialResult{nil, errors.New("p2p auth failed")}
				return
			}

			_ = conn.SetDeadline(time.Time{})
			resCh <- dialResult{conn, nil}
		}(ep)
	}

	var lastErr error
	for i := 0; i < len(offer.Endpoints); i++ {
		select {
		case <-dialCtx.Done():
			return nil, dialCtx.Err()
		case res := <-resCh:
			if res.err == nil && res.conn != nil {
				cancel() // cancel outstanding racing dials
				return res.conn, nil
			}
			lastErr = res.err
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("all p2p endpoints unreachable")
}

func collectLocalIPs() []string {
	var ips []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return []string{"127.0.0.1"}
	}
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ip4 := ipNet.IP.To4(); ip4 != nil {
				ips = append(ips, ip4.String())
			}
		}
	}
	if len(ips) == 0 {
		ips = append(ips, "127.0.0.1")
	}
	return ips
}

func generateEphemeralCert() (tls.Certificate, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "remote-p2p",
		},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}

	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
	}, nil
}
