package linux

import "testing"

func TestDetectSessionWarning(t *testing.T) {
	if got := detectSessionWarningFromEnv("x11", "", ":0"); got != "" {
		t.Fatalf("expected no warning on X11, got %q", got)
	}
	if got := detectSessionWarningFromEnv("wayland", "wayland-0", ""); got == "" {
		t.Fatal("expected warning on pure Wayland session")
	}
	if got := detectSessionWarningFromEnv("wayland", "wayland-0", ":0"); got == "" {
		t.Fatal("expected warning on Wayland with XWayland")
	}
	if got := detectSessionWarningFromEnv("", "", ""); got == "" {
		t.Fatal("expected warning when DISPLAY is empty")
	}
}
