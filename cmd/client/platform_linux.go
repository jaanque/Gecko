//go:build linux

package main

import (
	"remote-control/internal/linux"
	"remote-control/internal/protocol"
)

func platformInit() {}

type linuxWindowAdapter struct {
	w *linux.ClientWindow
}

func (a *linuxWindowAdapter) Ready() <-chan struct{} { return a.w.Ready() }
func (a *linuxWindowAdapter) Run() error             { return a.w.Run() }
func (a *linuxWindowAdapter) SetState(st uiState, detail string) {
	a.w.SetState(mapLinuxState(st), detail)
}

func mapLinuxState(st uiState) linux.ClientUIState {
	switch st {
	case uiOnline:
		return linux.UIStateOnline
	case uiPrompt:
		return linux.UIStatePrompt
	case uiActive:
		return linux.UIStateActive
	case uiError:
		return linux.UIStateError
	default:
		return linux.UIStateConnecting
	}
}

func newPlatformWindow(hostname string, logoPNG []byte, cb clientWindowCallbacks) clientWindow {
	return &linuxWindowAdapter{
		w: linux.NewClientWindow(hostname, logoPNG, linux.ClientCallbacks{
			OnAllow:      cb.OnAllow,
			OnDeny:       cb.OnDeny,
			OnDisconnect: cb.OnDisconnect,
			OnClose:      cb.OnClose,
		}),
	}
}

func newPlatformCapturer() screenCapturer {
	return linux.NewScreenCapturer()
}

func newPlatformInjector() inputInjector {
	return linux.NewInputInjector()
}

func enumPlatformMonitors() []protocol.MonitorInfo {
	mons := linux.EnumMonitors()
	out := make([]protocol.MonitorInfo, 0, len(mons))
	for _, m := range mons {
		out = append(out, protocol.MonitorInfo{
			Index:   m.Index,
			Width:   m.Width,
			Height:  m.Height,
			Primary: m.Primary,
		})
	}
	return out
}
