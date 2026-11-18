//go:build windows

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/user"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"remote-control/internal/config"
	"remote-control/internal/i18n"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
	"remote-control/internal/transfer"
	"remote-control/internal/version"
	"remote-control/internal/win"
)

type clientAgent struct {
	cfg      config.ClientConfig
	tlsCfg   *tls.Config
	hostname string
	username string
	strings  i18n.ClientStrings
	window   *win.ClientWindow
	injector *win.InputInjector
	receiver *transfer.Receiver

	mu           sync.Mutex
	conn         *tls.Conn
	writeMu      sync.Mutex
	pendingID    string
	activeID     string
	promptTimer  *time.Timer
	streamCancel context.CancelFunc

	clipMu       sync.Mutex
	lastClipText string

	quality          atomic.Int32
	monitorIdx       atomic.Int32 // 0 = Monitor 1 (default), 1..N-1 = other monitors, -1 = all monitors
	forceKeyframe    atomic.Bool
	lastSentFrameID  atomic.Uint32
	lastAckedFrameID atomic.Uint32
	ackWakeCh        chan struct{}
}

func main() {
	showVersion := flag.Bool("version", false, "Mostrar versión y salir")
	langFlag := flag.String("lang", "", "Idioma de interfaz (ca, es, en)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Banner("ClientRemot"))
		return
	}

	win.EnableDPIAwareness()

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "EQUIP"
	}

	cfg, cfgErr := config.ReadFromExecutable()

	lang := i18n.LangCA
	if cfgErr == nil && cfg.Lang != "" {
		lang = i18n.Normalize(cfg.Lang, i18n.LangCA)
	} else {
		lang = i18n.Detect(i18n.LangCA)
	}
	if strings.TrimSpace(*langFlag) != "" {
		lang = i18n.Normalize(*langFlag, lang)
	}
	str := i18n.Client(lang)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agent := &clientAgent{
		cfg:       cfg,
		hostname:  hostname,
		username:  currentUsername(),
		strings:   str,
		injector:  win.NewInputInjector(),
		receiver:  transfer.NewReceiver(""),
		ackWakeCh: make(chan struct{}, 1),
	}
	agent.quality.Store(78)

	window := win.NewClientWindow(hostname, cfg.LogoPNG, lang, win.ClientCallbacks{
		OnAllow:      agent.onUserAllow,
		OnDeny:       agent.onUserDeny,
		OnDisconnect: agent.onUserDisconnect,
		OnClipboard:  agent.onLocalClipboard,
		OnClose: func() {
			agent.stopAllSessions(str.ClientClosedReason)
			cancel()
		},
	})
	agent.window = window

	if cfgErr != nil {
		go func() {
			<-window.Ready()
			window.SetState(win.UIStateError, str.NoConfig)
		}()
		_ = window.Run()
		return
	}

	tlsCfg, err := security.MutualPinnedClientTLSConfig(cfg.CertSHA256, cfg.ClientCertDER, cfg.ClientKeyDER)
	if err != nil {
		go func() {
			<-window.Ready()
			window.SetState(win.UIStateError, str.InvalidCert)
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
	for ctx.Err() == nil {
		a.window.SetState(win.UIStateConnecting, a.strings.Connecting)

		err := a.connectAndServe(ctx)
		a.cleanupAfterDisconnect()

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if errors.Is(err, errAuthRejected) {
				a.window.SetState(win.UIStateError, a.strings.AccessRejected)
				backoff = 5 * time.Second
			} else {
				a.window.SetState(win.UIStateConnecting, a.strings.Reconnecting)
			}
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
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

	stopWatch := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopWatch()

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
	w, h := 1920, 1080
	if len(protoMons) > 0 {
		w, h = protoMons[0].Width, protoMons[0].Height
	}

	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteJSON(conn, protocol.MsgAuth, protocol.AuthPayload{
		Token:    a.cfg.AuthToken,
		Hostname: a.hostname,
		Username: a.username,
		Version:  version.Version,
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

	a.window.SetState(win.UIStateOnline, a.strings.Online)

	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-ticker.C:
				if err := a.writeMessage(conn, protocol.MsgPing, nil); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()

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

		case protocol.MsgClipboard:
			if a.isSessionActive() {
				var clip protocol.ClipboardPayload
				if json.Unmarshal(data, &clip) == nil && clip.Text != "" {
					a.clipMu.Lock()
					a.lastClipText = clip.Text
					a.clipMu.Unlock()
					_ = win.WriteClipboardText(clip.Text)
				}
			}

		case protocol.MsgSystemAction:
			if a.isSessionActive() {
				var sa protocol.SystemActionPayload
				if json.Unmarshal(data, &sa) == nil && sa.Action != "" {
					a.injector.SendSystemAction(sa.Action)
				}
			}

		case protocol.MsgFileOffer:
			if a.isSessionActive() {
				var offer protocol.FileOfferPayload
				if json.Unmarshal(data, &offer) == nil && offer.TransferID != "" {
					if err := a.receiver.StartOffer(offer); err != nil {
						_ = a.writeJSON(conn, protocol.MsgFileAccept, protocol.FileAcceptPayload{
							TransferID: offer.TransferID,
							Accepted:   false,
							Reason:     err.Error(),
						})
					} else {
						_ = a.writeJSON(conn, protocol.MsgFileAccept, protocol.FileAcceptPayload{
							TransferID: offer.TransferID,
							Accepted:   true,
						})
					}
				}
			}

		case protocol.MsgFileChunk:
			if a.isSessionActive() {
				if ch, err := protocol.DecodeFileChunk(data); err == nil {
					if err := a.receiver.WriteChunk(ch); err != nil {
						_ = a.writeJSON(conn, protocol.MsgFileDone, protocol.FileDonePayload{
							TransferID: ch.TransferID,
							OK:         false,
							Error:      err.Error(),
						})
					}
				}
			}

		case protocol.MsgFileDone:
			if a.isSessionActive() {
				var fd protocol.FileDonePayload
				if json.Unmarshal(data, &fd) == nil && fd.TransferID != "" {
					savedPath, err := a.receiver.Finish(fd.TransferID)
					if err != nil {
						_ = a.writeJSON(conn, protocol.MsgFileDone, protocol.FileDonePayload{
							TransferID: fd.TransferID,
							OK:         false,
							Error:      err.Error(),
						})
					} else {
						_ = a.writeJSON(conn, protocol.MsgFileDone, protocol.FileDonePayload{
							TransferID: fd.TransferID,
							OK:         true,
							SavedPath:  savedPath,
						})
					}
				}
			}
		}
	}
}

func (a *clientAgent) handleConsentRequest(req protocol.ConsentReqPayload) {
	a.mu.Lock()
	if a.activeID != "" || a.pendingID != "" {
		conn := a.conn
		a.mu.Unlock()
		if conn != nil {
			_ = a.writeJSON(conn, protocol.MsgConsentResp, protocol.ConsentRespPayload{
				SessionID: req.SessionID,
				Allowed:   false,
				Reason:    a.strings.BusyReason,
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
			Reason:    a.strings.TimeoutReason,
		})
	}
	a.window.SetState(win.UIStateOnline, a.strings.Online)
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

	a.clipMu.Lock()
	a.lastClipText = win.ReadClipboardText()
	a.clipMu.Unlock()

	a.injector.SetEnabled(true)
	a.window.SetState(win.UIStateActive, "")
	a.monitorIdx.Store(0)
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
			Reason:    a.strings.DeniedByUserReason,
		})
	}
	a.window.SetState(win.UIStateOnline, a.strings.Online)
}

func (a *clientAgent) onUserDisconnect() {
	a.stopAllSessions(a.strings.DisconnectedByUser)
	a.window.SetState(win.UIStateOnline, a.strings.Online)
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
		a.receiver.AbortAll()
		if connected {
			a.window.SetState(win.UIStateOnline, a.strings.Online)
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
	a.receiver.AbortAll()
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
	a.receiver.AbortAll()
}

func (a *clientAgent) isSessionActive() bool {
	a.mu.Lock()
	active := a.activeID != ""
	a.mu.Unlock()
	return active
}

func (a *clientAgent) onLocalClipboard(text string) {
	if text == "" || len(text) > protocol.MaxClipboardPayloadSize-64 {
		return
	}
	a.mu.Lock()
	conn := a.conn
	active := a.activeID != ""
	a.mu.Unlock()
	if !active || conn == nil {
		return
	}

	a.clipMu.Lock()
	changed := text != a.lastClipText
	if changed {
		a.lastClipText = text
	}
	a.clipMu.Unlock()

	if changed {
		_ = a.writeJSON(conn, protocol.MsgClipboard, protocol.ClipboardPayload{Text: text})
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func (a *clientAgent) streamScreen(ctx context.Context, conn net.Conn, sid string) {
	capturer := win.NewScreenCapturer()
	defer capturer.Close()

	a.lastSentFrameID.Store(0)
	a.lastAckedFrameID.Store(0)

	const (
		frameInterval = 16 * time.Millisecond
		maxInFlight   = 2
	)

	timer := time.NewTimer(frameInterval)
	defer timer.Stop()

	sleep := func(d time.Duration) bool {
		resetTimer(timer, d)
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}

	var frameSeq uint32

	for ctx.Err() == nil {
		if int32(a.lastSentFrameID.Load()-a.lastAckedFrameID.Load()) >= maxInFlight {
			resetTimer(timer, 300*time.Millisecond)
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
			if !sleep(200 * time.Millisecond) {
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

		wait := max(frameInterval-time.Since(start), 2*time.Millisecond)
		if !sleep(wait) {
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

func currentUsername() string {
	if u, err := user.Current(); err == nil && u != nil && u.Username != "" {
		name := u.Username
		if idx := strings.LastIndex(name, `\`); idx >= 0 && idx+1 < len(name) {
			name = name[idx+1:]
		}
		return name
	}
	if v := strings.TrimSpace(os.Getenv("USERNAME")); v != "" {
		return v
	}
	return ""
}
