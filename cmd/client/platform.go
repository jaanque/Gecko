package main

import "remote-control/internal/protocol"

type uiState int

const (
	uiConnecting uiState = iota
	uiOnline
	uiPrompt
	uiActive
	uiError
)

type clientWindowCallbacks struct {
	OnAllow      func()
	OnDeny       func()
	OnDisconnect func()
	OnClose      func()
}

type clientWindow interface {
	Ready() <-chan struct{}
	SetState(st uiState, detail string)
	Run() error
}

type screenCapturer interface {
	CaptureFrame(frameID uint32, monitorIndex, quality int, forceFull bool) ([]byte, int, int, error)
	Close()
}

type inputInjector interface {
	HandleMouse(ev protocol.MouseEvent, monitorIndex int)
	HandleKey(ev protocol.KeyEvent)
	ReleaseAllKeys()
}
