//go:build linux

package main

import (
	"remote-control/internal/clientui"
	"remote-control/internal/i18n"
	"remote-control/internal/linux"
	"remote-control/internal/protocol"
)

func platformInit() {}

func platformSessionWarning(lang i18n.Lang) string {
	return linux.DetectSessionWarning(lang)
}

func newPlatformWindow(hostname string, logoPNG []byte, lang i18n.Lang, cb clientui.Callbacks) clientWindow {
	return linux.NewClientWindow(hostname, logoPNG, lang, cb)
}

func newPlatformCapturer() screenCapturer {
	return linux.NewScreenCapturer()
}

func newPlatformInjector() inputInjector {
	return linux.NewInputInjector()
}

func enumPlatformMonitors() []protocol.MonitorInfo {
	return monitorBoundsToProto(linux.EnumMonitors())
}

func readPlatformClipboard() string {
	return linux.ReadClipboardText()
}

func writePlatformClipboard(text string) error {
	return linux.WriteClipboardText(text)
}
