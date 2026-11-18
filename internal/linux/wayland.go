package linux

import (
	"os"
	"strings"
)

// DetectSessionWarning returns a Catalan user-facing warning if the current Linux desktop
// session is running under Wayland (where X11 GetImage/XTEST capture & control are restricted).
// Returns "" on standard X11 sessions.
func DetectSessionWarning() string {
	return detectSessionWarningFromEnv(
		os.Getenv("XDG_SESSION_TYPE"),
		os.Getenv("WAYLAND_DISPLAY"),
		os.Getenv("DISPLAY"),
	)
}

func detectSessionWarningFromEnv(sessionType, waylandDisp, x11Disp string) string {
	isWayland := strings.EqualFold(strings.TrimSpace(sessionType), "wayland") || strings.TrimSpace(waylandDisp) != ""
	if isWayland {
		if strings.TrimSpace(x11Disp) == "" {
			return "Sessió Wayland sense X11: inicia sessió en Xorg (X11)"
		}
		return "Wayland detectat (XWayland): canvia a X11 per captura completa"
	}
	if strings.TrimSpace(x11Disp) == "" {
		return "No s'ha trobat cap sessió gràfica X11 (DISPLAY buit)"
	}
	return ""
}
