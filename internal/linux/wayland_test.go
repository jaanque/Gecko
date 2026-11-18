package linux

import (
	"testing"

	"remote-control/internal/i18n"
)

func TestDetectSessionWarning(t *testing.T) {
	if got := detectSessionWarningFromEnv("x11", "", ":0", i18n.LangCA); got != "" {
		t.Fatalf("expected no warning on X11, got %q", got)
	}
	if got := detectSessionWarningFromEnv("wayland", "wayland-0", "", i18n.LangCA); got == "" {
		t.Fatal("expected warning on pure Wayland session")
	}
	if got := detectSessionWarningFromEnv("wayland", "wayland-0", ":0", i18n.LangEN); got == "" {
		t.Fatal("expected warning on Wayland with XWayland")
	}
	if got := detectSessionWarningFromEnv("", "", "", i18n.LangES); got == "" {
		t.Fatal("expected warning when DISPLAY is empty")
	}
}
