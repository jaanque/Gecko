//go:build windows

package main

import (
	"context"
	"time"

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
