//go:build windows

package main

import (
	"remote-control/internal/clientui"
	"remote-control/internal/i18n"
	"remote-control/internal/protocol"
	"remote-control/internal/win"
)

func platformInit() {
	win.EnableDPIAwareness()
}

func platformSessionWarning(_ i18n.Lang) string {
	return ""
}

func newPlatformWindow(hostname string, logoPNG []byte, lang i18n.Lang, cb clientui.Callbacks) clientWindow {
	return win.NewClientWindow(hostname, logoPNG, lang, cb)
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
