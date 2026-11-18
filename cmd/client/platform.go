//go:build windows || linux

package main

import (
	"remote-control/internal/clientui"
	"remote-control/internal/protocol"
)

type clientWindow interface {
	Ready() <-chan struct{}
	SetState(st clientui.State, detail string)
	Run() error
}

type screenCapturer interface {
	CaptureFrame(frameID uint32, monitorIndex, quality int, forceFull bool) ([]byte, int, int, error)
	Close()
}

type inputInjector interface {
	SetEnabled(enabled bool)
	HandleMouse(ev protocol.MouseEvent, monitorIndex int)
	HandleKey(ev protocol.KeyEvent)
	SendSystemAction(action string)
	ReleaseAllKeys()
}

func monitorBoundsToProto(mons []clientui.MonitorBounds) []protocol.MonitorInfo {
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
