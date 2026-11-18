package linux

import "testing"

func TestClientCanvasRenderAndHitTest(t *testing.T) {
	c := newClientCanvas(nil)

	for _, st := range []ClientUIState{
		UIStateConnecting,
		UIStateOnline,
		UIStatePrompt,
		UIStateActive,
		UIStateError,
	} {
		c.render("PC-LINUX-01", st, "En línia · Sessió", btnNone)
	}

	allowRC, denyRC, discRC := c.buttonRects()
	if got := c.hitTest(UIStatePrompt, (allowRC.x0+allowRC.x1)/2, (allowRC.y0+allowRC.y1)/2); got != btnAllow {
		t.Fatalf("expected btnAllow hit, got %v", got)
	}
	if got := c.hitTest(UIStatePrompt, (denyRC.x0+denyRC.x1)/2, (denyRC.y0+denyRC.y1)/2); got != btnDeny {
		t.Fatalf("expected btnDeny hit, got %v", got)
	}
	if got := c.hitTest(UIStateActive, (discRC.x0+discRC.x1)/2, (discRC.y0+discRC.y1)/2); got != btnDisconnect {
		t.Fatalf("expected btnDisconnect hit, got %v", got)
	}
	if got := c.hitTest(UIStateOnline, (allowRC.x0+allowRC.x1)/2, (allowRC.y0+allowRC.y1)/2); got != btnNone {
		t.Fatalf("expected no button hits while online, got %v", got)
	}
}
