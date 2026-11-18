//go:build windows

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"remote-control/internal/config"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
	"remote-control/internal/win"
)

type clientAgent struct {
	cfg      config.ClientConfig
	tlsCfg   *tls.Config
	hostname string
	window   *win.ClientWindow
	injector *win.InputInjector

	mu           sync.Mutex
	conn         net.Conn
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
	win.EnableDPIAwareness()

	flagServer := flag.String("server", "", "Admin server address (dev override)")
	flagPin := flag.String("pin", "", "Admin TLS cert SHA-256 fingerprint (dev override)")
	flagToken := flag.String("token", "", "Admin shared auth token (dev override)")
	flag.Parse()

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "EQUIPO"
	}

	cfg, cfgErr := config.ReadFromExecutable()
	if cfgErr != nil && *flagServer != "" {
		cfg = config.ClientConfig{
			ServerAddr: *flagServer,
			CertSHA256: *flagPin,
			AuthToken:  *flagToken,
		}
		cfgErr = cfg.Validate()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agent := &clientAgent{
		cfg:       cfg,
		hostname:  hostname,
		injector:  win.NewInputInjector(),
		ackWakeCh: make(chan struct{}, 1),
	}
	agent.quality.Store(78)

	window := win.NewClientWindow(hostname, cfg.LogoPNG, win.ClientCallbacks{
		OnAllow:      agent.onUserAllow,
		OnDeny:       agent.onUserDeny,
		OnDisconnect: agent.onUserDisconnect,
		OnClose: func() {
			agent.stopAllSessions("Cliente cerrado")
			cancel()
		},
	})
	agent.window = window

	if cfgErr != nil {
		go func() {
			<-window.Ready()
			window.SetState(win.UIStateError, "Sin configuración. Genera este cliente desde el Admin.")
		}()
		_ = window.Run()
		return
	}

	tlsCfg, err := security.PinnedClientTLSConfig(cfg.CertSHA256)
	if err != nil {
		go func() {
			<-window.Ready()
			window.SetState(win.UIStateError, "Certificado inválido en la configuración.")
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
		a.window.SetState(win.UIStateConnecting, "Conectando con el administrador...")

		err := a.connectAndServe(ctx)
		a.cleanupAfterDisconnect()

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if errors.Is(err, errAuthRejected) {
				a.window.SetState(win.UIStateError, "Acceso rechazado: genera un nuevo cliente.")
				backoff = 5 * time.Second
			} else {
				a.window.SetState(win.UIStateConnecting, "Reconectando...")
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
	dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 15 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", a.cfg.ServerAddr, a.tlsCfg)
	if err != nil {
		return err
	}
	defer conn.Close()

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

	winMons := win.EnumMonitors()
	protoMons := make([]protocol.MonitorInfo, 0, len(winMons))
	for _, m := range winMons {
		protoMons = append(protoMons, protocol.MonitorInfo{
			Index:   m.Index,
			Width:   m.Width,
			Height:  m.Height,
			Primary: m.Primary,
		})
	}
	w, h := winMons[0].Width, winMons[0].Height
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteJSON(conn, protocol.MsgAuth, protocol.AuthPayload{
		Token:    a.cfg.AuthToken,
		Hostname: a.hostname,
		OS:       "windows",
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

	a.window.SetState(win.UIStateOnline, "En línea · Esperando conexión")

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
			if ack, err := protocol.DecodeFrameAck(data); err == nil {
				for {
					cur := a.lastAckedFrameID.Load()
					if int32(ack.FrameID-cur) <= 0 {
						break
					}
					if a.lastAckedFrameID.CompareAndSwap(cur, ack.FrameID) {
						break
					}
				}
				select {
				case a.ackWakeCh <- struct{}{}:
				default:
				}
			}

		case protocol.MsgMouse:
			if a.isSessionActive() {
				if ev, err := protocol.DecodeMouse(data); err == nil {
					a.injector.HandleMouse(ev, int(a.monitorIdx.Load()))
				}
			}

		case protocol.MsgKey:
			if a.isSessionActive() {
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
				Reason:    "El equipo ya está ocupado",
			})
		}
		return
	}

	timeout := time.Duration(req.TimeoutSec) * time.Second
	if timeout <= 0 || timeout > 120*time.Second {
		timeout = 60 * time.Second
	}

	a.pendingID = req.SessionID
	a.promptTimer = time.AfterFunc(timeout, func() {
		a.expirePendingPrompt(req.SessionID)
	})
	a.mu.Unlock()

	a.window.SetState(win.UIStatePrompt, "")
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
			Reason:    "Tiempo de espera agotado",
		})
	}
	a.window.SetState(win.UIStateOnline, "En línea · Esperando conexión")
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

	if conn == nil {
		streamCancel()
		return
	}

	if err := a.writeJSON(conn, protocol.MsgConsentResp, protocol.ConsentRespPayload{
		SessionID: sid,
		Allowed:   true,
	}); err != nil {
		a.handleRemoteSessionStop(sid)
		return
	}

	a.window.SetState(win.UIStateActive, "")
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
			Reason:    "Denegado por el usuario",
		})
	}
	a.window.SetState(win.UIStateOnline, "En línea · Esperando conexión")
}

func (a *clientAgent) onUserDisconnect() {
	a.stopAllSessions("Desconectado por el usuario")
	a.window.SetState(win.UIStateOnline, "En línea · Esperando conexión")
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
		a.injector.ReleaseAllKeys()
		if connected {
			a.window.SetState(win.UIStateOnline, "En línea · Esperando conexión")
		}
	}
}

func (a *clientAgent) stopAllSessions(reason string) {
	a.mu.Lock()
	sid := a.activeID
	if sid == "" {
		sid = a.pendingID
	}
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
	conn := a.conn
	a.mu.Unlock()

	a.injector.ReleaseAllKeys()
	if conn != nil && sid != "" {
		_ = a.writeJSON(conn, protocol.MsgSessionStop, protocol.SessionStopPayload{
			SessionID: sid,
			Reason:    reason,
		})
	}
}

func (a *clientAgent) cleanupAfterDisconnect() {
	a.mu.Lock()
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
	a.conn = nil
	a.mu.Unlock()

	a.injector.ReleaseAllKeys()
}

func (a *clientAgent) isSessionActive() bool {
	a.mu.Lock()
	active := a.activeID != ""
	a.mu.Unlock()
	return active
}

func (a *clientAgent) streamScreen(ctx context.Context, conn net.Conn, sid string) {
	capturer := win.NewScreenCapturer()
	defer capturer.Close()

	a.lastSentFrameID.Store(0)
	a.lastAckedFrameID.Store(0)

	// Target up to ~60 FPS on fast links, bounded by 2-frame sliding-window ACK backpressure.
	const (
		frameInterval = 16 * time.Millisecond
		maxInFlight   = 2
	)

	var frameSeq uint32

	for {
		if ctx.Err() != nil {
			return
		}

		// Sliding-window pacing: never let more than 2 frames queue in TCP buffers.
		if int32(a.lastSentFrameID.Load()-a.lastAckedFrameID.Load()) >= maxInFlight {
			select {
			case <-ctx.Done():
				return
			case <-a.ackWakeCh:
			case <-time.After(300 * time.Millisecond):
				// Safety fallback in case an ACK was delayed.
				a.lastAckedFrameID.Store(a.lastSentFrameID.Load())
			}
		}

		start := time.Now()
		q := int(a.quality.Load())
		monIdx := int(a.monitorIdx.Load())
		force := a.forceKeyframe.Swap(false)

		nextID := frameSeq + 1
		frame, _, _, err := capturer.CaptureFrame(nextID, start.UnixNano(), monIdx, q, force)
		if err != nil {
			// E.g. temporary UAC/lock screen; wait briefly and retry.
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
				continue
			}
		}

		if len(frame) > 0 {
			frameSeq = nextID
			a.lastSentFrameID.Store(frameSeq)
			if err := a.writeMessage(conn, protocol.MsgFrame, frame); err != nil {
				a.handleRemoteSessionStop(sid)
				return
			}
		}

		elapsed := time.Since(start)
		wait := frameInterval - elapsed
		if wait < 2*time.Millisecond {
			wait = 2 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
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
