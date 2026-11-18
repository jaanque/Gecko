//go:build windows

package win

import "syscall"

var (
	user32DPI = syscall.NewLazyDLL("user32.dll")

	procSetProcessDpiAwarenessContext = user32DPI.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = user32DPI.NewProc("SetProcessDPIAware")
)

// EnableDPIAwareness configures the process as Per-Monitor DPI Aware v2
// so screen capture and mouse coordinates map 1:1 with physical pixels.
func EnableDPIAwareness() {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (DPI_AWARENESS_CONTEXT)-4
	const perMonitorAwareV2 = ^uintptr(3)
	if err := procSetProcessDpiAwarenessContext.Find(); err == nil {
		r, _, _ := procSetProcessDpiAwarenessContext.Call(perMonitorAwareV2)
		if r != 0 {
			return
		}
	}
	if err := procSetProcessDPIAware.Find(); err == nil {
		_, _, _ = procSetProcessDPIAware.Call()
	}
}
