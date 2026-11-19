//go:build windows

package main

import (
	"context"
	"net"
	"time"

	"remote-control/internal/p2p"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
	"remote-control/internal/win"
)

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

	if p2pLn, err := p2p.StartP2PListener(); err == nil {
		a.mu.Lock()
		a.p2pLn = p2pLn
		a.mu.Unlock()
		_ = a.writeJSON(conn, protocol.MsgP2POffer, protocol.P2POfferPayload{
			SessionID: sid,
			Endpoints: p2pLn.Endpoints(),
			Token:     p2pLn.Token(),
		})
		go a.watchP2PAccept(streamCtx, p2pLn, sid)
	}

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
	if a.p2pLn != nil {
		a.p2pLn.Close()
		a.p2pLn = nil
	}
	if a.p2pConn != nil {
		_ = a.p2pConn.Close()
		a.p2pConn = nil
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

func (a *clientAgent) getActiveConn(fallback net.Conn) net.Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.p2pConn != nil {
		return a.p2pConn
	}
	return fallback
}

func (a *clientAgent) watchP2PAccept(ctx context.Context, ln *p2p.P2PListener, sid string) {
	select {
	case <-ctx.Done():
		return
	case directConn := <-ln.Conn():
		a.mu.Lock()
		if a.activeID == sid {
			a.p2pConn = directConn
		} else {
			_ = directConn.Close()
		}
		a.mu.Unlock()
	}
}
