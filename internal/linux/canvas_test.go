package linux

import (
	"testing"

	"remote-control/internal/clientui"
)

func TestClientCanvasRenderAndHitTest(t *testing.T) {
	c := newClientCanvas(nil)

	for _, st := range []clientui.State{
		clientui.StateConnecting,
		clientui.StateOnline,
		clientui.StatePrompt,
		clientui.StateActive,
		clientui.StateError,
	} {
		c.render("PC-LINUX-01", st, "En línia · Sessió", btnNone)
	}

	allowRC, denyRC, discRC := c.buttonRects()
	if got := c.hitTest(clientui.StatePrompt, (allowRC.x0+allowRC.x1)/2, (allowRC.y0+allowRC.y1)/2); got != btnAllow {
		t.Fatalf("expected btnAllow hit, got %v", got)
	}
	if got := c.hitTest(clientui.StatePrompt, (denyRC.x0+denyRC.x1)/2, (denyRC.y0+denyRC.y1)/2); got != btnDeny {
		t.Fatalf("expected btnDeny hit, got %v", got)
	}
	if got := c.hitTest(clientui.StateActive, (discRC.x0+discRC.x1)/2, (discRC.y0+discRC.y1)/2); got != btnDisconnect {
		t.Fatalf("expected btnDisconnect hit, got %v", got)
	}
	if got := c.hitTest(clientui.StateOnline, (allowRC.x0+allowRC.x1)/2, (allowRC.y0+allowRC.y1)/2); got != btnNone {
		t.Fatalf("expected no button hits while online, got %v", got)
	}
}
