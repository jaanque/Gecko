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
