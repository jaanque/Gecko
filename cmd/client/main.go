package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"remote-control/internal/config"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
)

type clientAgent struct {
	cfg      config.ClientConfig
	tlsCfg   *tls.Config
	hostname string
	window   clientWindow
	injector inputInjector

	mu           sync.Mutex
	conn         *tls.Conn
	writeMu      sync.Mutex
	pendingID    string
	activeID     string
	promptTimer  *time.Timer
	streamCancel context.CancelFunc

	quality          atomic.Int32
	monitorIdx       atomic.Int32 // 0 = Monitor 1 (default), 1..N-1 = other monitors, -1 = all monitors
	forceKeyframe    atomic.Bool
	lastSentFrameID  atomic.Uint32
	lastAckedFrameID atomic.Uint32
	ackWakeCh        chan struct{}
}

func main() {
	platformInit()

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "EQUIP"
	}

	cfg, cfgErr := config.ReadFromExecutable()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agent := &clientAgent{
		cfg:       cfg,
		hostname:  hostname,
		injector:  newPlatformInjector(),
		ackWakeCh: make(chan struct{}, 1),
	}
	agent.quality.Store(78)

	window := newPlatformWindow(hostname, cfg.LogoPNG, clientWindowCallbacks{
		OnAllow:      agent.onUserAllow,
		OnDeny:       agent.onUserDeny,
		OnDisconnect: agent.onUserDisconnect,
		OnClose: func() {
			agent.stopAllSessions("Client tancat")
			cancel()
		},
	})
	agent.window = window

	if cfgErr != nil {
		go func() {
			<-window.Ready()
			window.SetState(uiError, "Sense configuració")
		}()
		_ = window.Run()
		return
	}

	tlsCfg, err := security.MutualPinnedClientTLSConfig(cfg.CertSHA256, cfg.ClientCertDER, cfg.ClientKeyDER)
	if err != nil {
		go func() {
			<-window.Ready()
			window.SetState(uiError, "Certificat no vàlid")
		}()
		_ = window.Run()
		return
	}
	agent.tlsCfg = tlsCfg

	go func() {
		<-window.Ready()
		agent.runLoop(ctx)
	}()

	_ = window.Run()
	cancel()
}

func (a *clientAgent) runLoop(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		a.window.SetState(uiConnecting, "Connectant...")

		err := a.connectAndServe(ctx)
		a.cleanupAfterDisconnect()

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if errors.Is(err, errAuthRejected) {
				a.window.SetState(uiError, "Accés rebutjat")
				backoff = 5 * time.Second
			} else {
				a.window.SetState(uiConnecting, "Reconnectant...")
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff += time.Second
		}
	}
}

var errAuthRejected = errors.New("authentication rejected")

func (a *clientAgent) connectAndServe(ctx context.Context) error {
	tlsDialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 15 * time.Second},
		Config:    a.tlsCfg,
	}
	rawConn, err := tlsDialer.DialContext(ctx, "tcp", a.cfg.ServerAddr)
	if err != nil {
		return err
	}
	conn := rawConn.(*tls.Conn)
	defer conn.Close()
	if err := security.VerifyServerTLSConn(conn, a.cfg.CertSHA256); err != nil {
		return errAuthRejected
	}

	if tc, ok := conn.NetConn().(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetWriteBuffer(512 * 1024)
		_ = tc.SetReadBuffer(256 * 1024)
	}

	// Close connection immediately if context is canceled.
	doneConn := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-doneConn:
		}
	}()
	defer close(doneConn)

	protoMons := enumPlatformMonitors()
	w, h := 1280, 720
	if len(protoMons) > 0 {
		w, h = protoMons[0].Width, protoMons[0].Height
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteJSON(conn, protocol.MsgAuth, protocol.AuthPayload{
		Token:    a.cfg.AuthToken,
		Hostname: a.hostname,
		Width:    w,
		Height:   h,
		Monitors: protoMons,
	}); err != nil {
		return err
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	msgType, payload, err := protocol.ReadMessage(conn)
	if err != nil {
		return err
	}
	if msgType != protocol.MsgAuthResp {
		return errors.New("unexpected auth response")
	}
	var resp protocol.AuthRespPayload
	if err := json.Unmarshal(payload, &resp); err != nil || !resp.OK {
		return errAuthRejected
	}

	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()

	a.window.SetState(uiOnline, "En línia")

	// Start periodic heartbeat ping.
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := a.writeMessage(conn, protocol.MsgPing, nil); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	defer close(pingDone)

	// Main read loop.
	for {
		_ = conn.SetReadDeadline(time.Now().Add(35 * time.Second))
		mType, data, err := protocol.ReadMessage(conn)
		if err != nil {
			return err
		}

		switch mType {
		case protocol.MsgPing:
			_ = a.writeMessage(conn, protocol.MsgPong, data)

		case protocol.MsgPong:
			// Keepalive confirmed.

		case protocol.MsgConsentReq:
			var req protocol.ConsentReqPayload
			if err := json.Unmarshal(data, &req); err != nil || req.SessionID == "" {
				continue
			}
			a.handleConsentRequest(req)

		case protocol.MsgSessionStop:
			var stop protocol.SessionStopPayload
			_ = json.Unmarshal(data, &stop)
			a.handleRemoteSessionStop(stop.SessionID)

		case protocol.MsgStreamConfig:
			var sc protocol.StreamConfigPayload
			if err := json.Unmarshal(data, &sc); err == nil {
				if sc.Quality >= 30 && sc.Quality <= 95 {
					a.quality.Store(int32(sc.Quality))
				}
				if sc.Monitor != nil {
					a.monitorIdx.Store(int32(*sc.Monitor))
					a.forceKeyframe.Store(true)
				}
				if sc.Keyframe {
					a.forceKeyframe.Store(true)
				}
			}

		case protocol.MsgFrameAck:
			if frameID, err := protocol.DecodeFrameAck(data); err == nil {
				a.lastAckedFrameID.Store(frameID)
				select {
				case a.ackWakeCh <- struct{}{}:
				default:
				}
			}

		case protocol.MsgMouse:
			if a.isSessionActive() && a.verifyActiveServerConn(conn) {
				if ev, err := protocol.DecodeMouse(data); err == nil {
					a.injector.HandleMouse(ev, int(a.monitorIdx.Load()))
				}
			}

		case protocol.MsgKey:
			if a.isSessionActive() && a.verifyActiveServerConn(conn) {
				if ev, err := protocol.DecodeKey(data); err == nil {
					a.injector.HandleKey(ev)
				}
			}
		}
	}
}

func (a *clientAgent) handleConsentRequest(req protocol.ConsentReqPayload) {
	a.mu.Lock()
	// If already in an active session or already prompting, reject duplicate request.
	if a.activeID != "" || a.pendingID != "" {
		conn := a.conn
		a.mu.Unlock()
		if conn != nil {
			_ = a.writeJSON(conn, protocol.MsgConsentResp, protocol.ConsentRespPayload{
				SessionID: req.SessionID,
				Allowed:   false,
				Reason:    "L'equip ja està ocupat",
			})
		}
		return
	}

	timeout := time.Duration(req.TimeoutSec) * time.Second
	if timeout < 10*time.Second || timeout > 300*time.Second {
		timeout = 60 * time.Second
	}

	a.pendingID = req.SessionID
	a.promptTimer = time.AfterFunc(timeout, func() {
		a.expirePendingPrompt(req.SessionID)
	})
	a.mu.Unlock()

	a.window.SetState(uiPrompt, "")
}

func (a *clientAgent) expirePendingPrompt(sessionID string) {
	a.mu.Lock()
	if a.pendingID != sessionID {
		a.mu.Unlock()
		return
	}
	a.pendingID = ""
	a.promptTimer = nil
	conn := a.conn
	a.mu.Unlock()

	if conn != nil {
		_ = a.writeJSON(conn, protocol.MsgConsentResp, protocol.ConsentRespPayload{
			SessionID: sessionID,
			Allowed:   false,
			Reason:    "Temps d'espera esgotat",
		})
	}
	a.window.SetState(uiOnline, "En línia")
}

func (a *clientAgent) onUserAllow() {
	a.mu.Lock()
	sid := a.pendingID
	if sid == "" {
		a.mu.Unlock()
		return
	}
	if a.promptTimer != nil {
		a.promptTimer.Stop()
		a.promptTimer = nil
	}
	a.pendingID = ""
	a.activeID = sid
	conn := a.conn
	streamCtx, streamCancel := context.WithCancel(context.Background())
	a.streamCancel = streamCancel
	a.mu.Unlock()

	if conn == nil || security.VerifyServerTLSConn(conn, a.cfg.CertSHA256) != nil {
		streamCancel()
		a.handleRemoteSessionStop(sid)
		return
	}

	if err := a.writeJSON(conn, protocol.MsgConsentResp, protocol.ConsentRespPayload{
		SessionID: sid,
		Allowed:   true,
	}); err != nil {
		a.handleRemoteSessionStop(sid)
		return
	}

	// Only unlock OS input injection after strict mTLS 1.3 + pinned server cert + explicit user "Permetre".
	a.injector.SetEnabled(true)
	a.window.SetState(uiActive, "")
	a.monitorIdx.Store(0) // Always start on Monitor 1
	a.forceKeyframe.Store(true)
	go a.streamScreen(streamCtx, conn, sid)
}

func (a *clientAgent) onUserDeny() {
	a.mu.Lock()
	sid := a.pendingID
	if sid == "" {
		a.mu.Unlock()
		return
	}
	if a.promptTimer != nil {
		a.promptTimer.Stop()
		a.promptTimer = nil
	}
	a.pendingID = ""
	conn := a.conn
	a.mu.Unlock()

	if conn != nil {
		_ = a.writeJSON(conn, protocol.MsgConsentResp, protocol.ConsentRespPayload{
			SessionID: sid,
			Allowed:   false,
			Reason:    "Denegat per l'usuari",
		})
	}
	a.window.SetState(uiOnline, "En línia")
}

func (a *clientAgent) onUserDisconnect() {
	a.stopAllSessions("Desconnectat per l'usuari")
	a.window.SetState(uiOnline, "En línia")
}

func (a *clientAgent) handleRemoteSessionStop(sessionID string) {
	a.mu.Lock()
	changed := false
	if a.pendingID != "" && (sessionID == "" || a.pendingID == sessionID) {
		if a.promptTimer != nil {
			a.promptTimer.Stop()
			a.promptTimer = nil
		}
		a.pendingID = ""
		changed = true
	}
	if a.activeID != "" && (sessionID == "" || a.activeID == sessionID) {
		if a.streamCancel != nil {
			a.streamCancel()
			a.streamCancel = nil
		}
		a.activeID = ""
		changed = true
	}
	connected := a.conn != nil
	a.mu.Unlock()

	if changed {
		a.injector.SetEnabled(false)
		if connected {
			a.window.SetState(uiOnline, "En línia")
		}
	}
}

func (a *clientAgent) clearSessionLocked() {
	if a.promptTimer != nil {
		a.promptTimer.Stop()
		a.promptTimer = nil
	}
	if a.streamCancel != nil {
		a.streamCancel()
		a.streamCancel = nil
	}
	a.pendingID = ""
	a.activeID = ""
}

func (a *clientAgent) stopAllSessions(reason string) {
	a.mu.Lock()
	sid := a.activeID
	if sid == "" {
		sid = a.pendingID
	}
	a.clearSessionLocked()
	conn := a.conn
	a.mu.Unlock()

	a.injector.SetEnabled(false)
	if conn != nil && sid != "" {
		_ = a.writeJSON(conn, protocol.MsgSessionStop, protocol.SessionStopPayload{
			SessionID: sid,
			Reason:    reason,
		})
	}
}

func (a *clientAgent) cleanupAfterDisconnect() {
	a.mu.Lock()
	a.clearSessionLocked()
	a.conn = nil
	a.mu.Unlock()

	a.injector.SetEnabled(false)
}

func (a *clientAgent) isSessionActive() bool {
	a.mu.Lock()
	active := a.activeID != ""
	a.mu.Unlock()
	return active
}

func (a *clientAgent) verifyActiveServerConn(conn *tls.Conn) bool {
	return conn != nil && security.VerifyServerTLSConn(conn, a.cfg.CertSHA256) == nil
}

func (a *clientAgent) streamScreen(ctx context.Context, conn net.Conn, sid string) {
	capturer := newPlatformCapturer()
	defer capturer.Close()

	a.lastSentFrameID.Store(0)
	a.lastAckedFrameID.Store(0)

	// Target up to ~60 FPS on fast links, bounded by 2-frame sliding-window ACK backpressure.
	const (
		frameInterval = 16 * time.Millisecond
		maxInFlight   = 2
	)

	timer := time.NewTimer(frameInterval)
	defer timer.Stop()

	sleepTimer := func(d time.Duration) bool {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}

	var frameSeq uint32

	for ctx.Err() == nil {
		// Sliding-window pacing: never let more than 2 frames queue in TCP buffers.
		if int32(a.lastSentFrameID.Load()-a.lastAckedFrameID.Load()) >= maxInFlight {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(300 * time.Millisecond)
			select {
			case <-ctx.Done():
				return
			case <-a.ackWakeCh:
			case <-timer.C:
				a.lastAckedFrameID.Store(a.lastSentFrameID.Load())
			}
		}

		start := time.Now()
		q := int(a.quality.Load())
		monIdx := int(a.monitorIdx.Load())
		force := a.forceKeyframe.Swap(false)

		nextID := frameSeq + 1
		frame, _, _, err := capturer.CaptureFrame(nextID, monIdx, q, force)
		if err != nil {
			if !sleepTimer(200 * time.Millisecond) {
				return
			}
			continue
		}

		if len(frame) > 0 {
			frameSeq = nextID
			a.lastSentFrameID.Store(frameSeq)
			if err := a.writeMessage(conn, protocol.MsgFrame, frame); err != nil {
				a.handleRemoteSessionStop(sid)
				return
			}
		}

		wait := frameInterval - time.Since(start)
		if wait < 2*time.Millisecond {
			wait = 2 * time.Millisecond
		}
		if !sleepTimer(wait) {
			return
		}
	}
}

func (a *clientAgent) writeMessage(conn net.Conn, msgType uint8, payload []byte) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return protocol.WriteMessage(conn, msgType, payload)
}

func (a *clientAgent) writeJSON(conn net.Conn, msgType uint8, v any) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return protocol.WriteJSON(conn, msgType, v)
}
