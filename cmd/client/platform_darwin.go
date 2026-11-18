//go:build darwin

package main

import (
	"remote-control/internal/clientui"
	"remote-control/internal/darwin"
	"remote-control/internal/i18n"
	"remote-control/internal/protocol"
)

func platformInit() {}

func platformSessionWarning(_ i18n.Lang) string {
	return ""
}

func newPlatformWindow(hostname string, logoPNG []byte, lang i18n.Lang, cb clientui.Callbacks) clientWindow {
	return darwin.NewClientWindow(hostname, logoPNG, lang, cb)
}

func newPlatformCapturer() screenCapturer {
	return darwin.NewScreenCapturer()
}

func newPlatformInjector() inputInjector {
	return darwin.NewInputInjector()
}

func enumPlatformMonitors() []protocol.MonitorInfo {
	return monitorBoundsToProto(darwin.EnumMonitors())
}

func readPlatformClipboard() string {
	return darwin.ReadClipboardText()
}

func writePlatformClipboard(text string) error {
	return darwin.WriteClipboardText(text)
}
