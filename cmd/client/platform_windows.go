//go:build windows

package main

import (
	"remote-control/internal/clientui"
	"remote-control/internal/protocol"
	"remote-control/internal/win"
)

func platformInit() {
	win.EnableDPIAwareness()
}

func platformSessionWarning() string {
	return ""
}

func newPlatformWindow(hostname string, logoPNG []byte, cb clientui.Callbacks) clientWindow {
	return win.NewClientWindow(hostname, logoPNG, cb)
}

func newPlatformCapturer() screenCapturer {
	return win.NewScreenCapturer()
}

func newPlatformInjector() inputInjector {
	return win.NewInputInjector()
}

func enumPlatformMonitors() []protocol.MonitorInfo {
	return monitorBoundsToProto(win.EnumMonitors())
}

func readPlatformClipboard() string {
	return win.ReadClipboardText()
}

func writePlatformClipboard(text string) error {
	return win.WriteClipboardText(text)
}
