//go:build windows

package win

import (
	"runtime"
	"testing"
	"unsafe"
)

func TestWin32InputStructSizes(t *testing.T) {
	var want uintptr = 28
	if runtime.GOARCH == "amd64" {
		want = 40
	}
	if got := unsafe.Sizeof(mouseInputPacket{}); got != want {
		t.Fatalf("mouseInputPacket size = %d, want %d on %s", got, want, runtime.GOARCH)
	}
	if got := unsafe.Sizeof(keybdInputPacket{}); got != want {
		t.Fatalf("keybdInputPacket size = %d, want %d on %s", got, want, runtime.GOARCH)
	}
}
