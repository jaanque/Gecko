//go:build windows

package main

import (
	"remote-control/internal/protocol"
	"remote-control/internal/win"
)

func platformInit() {
	win.EnableDPIAwareness()
}

type winWindowAdapter struct {
	w *win.ClientWindow
}

func (a *winWindowAdapter) Ready() <-chan struct{} { return a.w.Ready() }
func (a *winWindowAdapter) Run() error             { return a.w.Run() }
func (a *winWindowAdapter) SetState(st uiState, detail string) {
	a.w.SetState(mapWinState(st), detail)
}

func mapWinState(st uiState) win.ClientUIState {
	switch st {
	case uiOnline:
		return win.UIStateOnline
	case uiPrompt:
		return win.UIStatePrompt
	case uiActive:
		return win.UIStateActive
	case uiError:
		return win.UIStateError
	default:
		return win.UIStateConnecting
	}
}

func newPlatformWindow(hostname string, logoPNG []byte, cb clientWindowCallbacks) clientWindow {
	return &winWindowAdapter{
		w: win.NewClientWindow(hostname, logoPNG, win.ClientCallbacks{
			OnAllow:      cb.OnAllow,
			OnDeny:       cb.OnDeny,
			OnDisconnect: cb.OnDisconnect,
			OnClose:      cb.OnClose,
		}),
	}
}

func newPlatformCapturer() screenCapturer {
	return win.NewScreenCapturer()
}

func newPlatformInjector() inputInjector {
	return win.NewInputInjector()
}

func enumPlatformMonitors() []protocol.MonitorInfo {
	winMons := win.EnumMonitors()
	out := make([]protocol.MonitorInfo, 0, len(winMons))
	for _, m := range winMons {
		out = append(out, protocol.MonitorInfo{
			Index:   m.Index,
			Width:   m.Width,
			Height:  m.Height,
			Primary: m.Primary,
		})
	}
	return out
}
