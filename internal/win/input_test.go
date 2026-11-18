//go:build windows

package win

import (
	"runtime"
	"testing"
	"unsafe"

	"remote-control/internal/protocol"
)

func TestWin32InputStructSizes(t *testing.T) {
	const want uintptr = 40
	if got := unsafe.Sizeof(mouseInputPacket{}); got != want {
		t.Fatalf("mouseInputPacket size = %d, want %d on %s", got, want, runtime.GOARCH)
	}
	if got := unsafe.Sizeof(keybdInputPacket{}); got != want {
		t.Fatalf("keybdInputPacket size = %d, want %d on %s", got, want, runtime.GOARCH)
	}
}

func TestInputInjectorDisabledByDefault(t *testing.T) {
	inj := NewInputInjector()
	if inj.Enabled() {
		t.Fatal("InputInjector must start disabled until server identity and user consent are verified")
	}

	// Must ignore key/mouse events completely while disabled (heldKeys must stay empty).
	inj.HandleKey(protocol.KeyEvent{Action: protocol.KeyDown, VK: 0x41})
	inj.HandleMouse(protocol.MouseEvent{Action: protocol.MouseMove, X: 100, Y: 100}, 0)

	inj.mu.Lock()
	held := len(inj.heldKeys)
	inj.mu.Unlock()
	if held != 0 {
		t.Fatalf("expected 0 held keys while disabled, got %d", held)
	}
}
