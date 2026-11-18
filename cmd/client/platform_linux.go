//go:build linux

package main

import (
	"remote-control/internal/clientui"
	"remote-control/internal/linux"
	"remote-control/internal/protocol"
)

func platformInit() {}

func platformSessionWarning() string {
	return linux.DetectSessionWarning()
}

func newPlatformWindow(hostname string, logoPNG []byte, cb clientui.Callbacks) clientWindow {
	return linux.NewClientWindow(hostname, logoPNG, cb)
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
