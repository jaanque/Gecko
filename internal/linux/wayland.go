package linux

import (
	"os"
	"strings"

	"remote-control/internal/i18n"
)

// IsWaylandSession reports whether the process is running inside a Wayland desktop session.
func IsWaylandSession() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("XDG_SESSION_TYPE")), "wayland") ||
		strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")) != ""
}

// DetectSessionWarning returns a localized informational warning when running under Wayland
// or when no graphical display is available. Returns "" on native X11 sessions.
func DetectSessionWarning(lang i18n.Lang) string {
	return detectSessionWarningFromEnv(
		os.Getenv("XDG_SESSION_TYPE"),
		os.Getenv("WAYLAND_DISPLAY"),
		os.Getenv("DISPLAY"),
		lang,
	)
}

func detectSessionWarningFromEnv(sessionType, waylandDisp, x11Disp string, lang i18n.Lang) string {
	str := i18n.Client(lang)
	isWayland := strings.EqualFold(strings.TrimSpace(sessionType), "wayland") || strings.TrimSpace(waylandDisp) != ""
	if isWayland {
		if strings.TrimSpace(x11Disp) == "" {
			return str.WaylandNoX11Warning
		}
		return str.WaylandXWaylandWarn
	}
	if strings.TrimSpace(x11Disp) == "" {
		return str.NoX11DisplayWarning
	}
	return ""
}
