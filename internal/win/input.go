//go:build windows

package win

import (
	"sync"
	"syscall"
	"unsafe"

	"remote-control/internal/protocol"
)

var (
	user32Input = syscall.NewLazyDLL("user32.dll")

	procSendInput     = user32Input.NewProc("SendInput")
	procMapVirtualKey = user32Input.NewProc("MapVirtualKeyW")
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseEventfMove        = 0x0001
	mouseEventfLeftDown    = 0x0002
	mouseEventfLeftUp      = 0x0004
	mouseEventfRightDown   = 0x0008
	mouseEventfRightUp     = 0x0010
	mouseEventfMiddleDown  = 0x0020
	mouseEventfMiddleUp    = 0x0040
	mouseEventfWheel       = 0x0800
	mouseEventfHWheel      = 0x1000
	mouseEventfVirtualDesk = 0x4000
	mouseEventfAbsolute    = 0x8000

	keyEventfExtendedKey = 0x0001
	keyEventfKeyUp       = 0x0002
	keyEventfUnicode     = 0x0004

	mapvkVKToVSC = 0
)

// InputInjector wraps Win32 SendInput and tracks held keys so they can always be released cleanly.
type InputInjector struct {
	mu       sync.Mutex
	heldKeys map[uint16]uint8 // vk -> flags
}

// NewInputInjector creates a new InputInjector.
func NewInputInjector() *InputInjector {
	return &InputInjector{
		heldKeys: make(map[uint16]uint8),
	}
}

// HandleMouse injects a normalized mouse event across the virtual desktop.
func (inj *InputInjector) HandleMouse(ev protocol.MouseEvent) {
	pkt := mouseInputPacket{
		Type: inputMouse,
		DX:   int32(ev.X),
		DY:   int32(ev.Y),
	}

	switch ev.Action {
	case protocol.MouseMove:
		pkt.DwFlags = mouseEventfMove | mouseEventfAbsolute | mouseEventfVirtualDesk
	case protocol.MouseDown:
		pkt.DwFlags = mouseEventfMove | mouseEventfAbsolute | mouseEventfVirtualDesk | buttonDownFlag(ev.Button)
	case protocol.MouseUp:
		pkt.DwFlags = mouseEventfMove | mouseEventfAbsolute | mouseEventfVirtualDesk | buttonUpFlag(ev.Button)
	case protocol.MouseWheel:
		pkt.DwFlags = mouseEventfWheel
		pkt.MouseData = uint32(int32(ev.Delta))
	case protocol.MouseHWheel:
		pkt.DwFlags = mouseEventfHWheel
		pkt.MouseData = uint32(int32(ev.Delta))
	default:
		return
	}

	procSendInput.Call(1, uintptr(unsafe.Pointer(&pkt)), unsafe.Sizeof(pkt))
}

// HandleKey injects a keyboard event (either virtual key or Unicode code unit).
func (inj *InputInjector) HandleKey(ev protocol.KeyEvent) {
	if ev.Action == protocol.KeyUnicode {
		if ev.Scan == 0 {
			return
		}
		down := keybdInputPacket{
			Type:    inputKeyboard,
			WScan:   ev.Scan,
			DwFlags: keyEventfUnicode,
		}
		up := keybdInputPacket{
			Type:    inputKeyboard,
			WScan:   ev.Scan,
			DwFlags: keyEventfUnicode | keyEventfKeyUp,
		}
		procSendInput.Call(1, uintptr(unsafe.Pointer(&down)), unsafe.Sizeof(down))
		procSendInput.Call(1, uintptr(unsafe.Pointer(&up)), unsafe.Sizeof(up))
		return
	}

	if ev.VK == 0 || ev.VK > 0xFE {
		return
	}

	scan := ev.Scan
	if scan == 0 {
		r, _, _ := procMapVirtualKey.Call(uintptr(ev.VK), mapvkVKToVSC)
		scan = uint16(r)
	}

	var flags uint32
	if (ev.Flags & protocol.KeyFlagExtended) != 0 {
		flags |= keyEventfExtendedKey
	}

	inj.mu.Lock()
	switch ev.Action {
	case protocol.KeyDown:
		inj.heldKeys[ev.VK] = ev.Flags
	case protocol.KeyUp:
		flags |= keyEventfKeyUp
		delete(inj.heldKeys, ev.VK)
	default:
		inj.mu.Unlock()
		return
	}
	inj.mu.Unlock()

	pkt := keybdInputPacket{
		Type:    inputKeyboard,
		WVk:     ev.VK,
		WScan:   scan,
		DwFlags: flags,
	}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&pkt)), unsafe.Sizeof(pkt))
}

// ReleaseAllKeys releases any keys currently held down (called on session end).
func (inj *InputInjector) ReleaseAllKeys() {
	inj.mu.Lock()
	snapshot := make(map[uint16]uint8, len(inj.heldKeys))
	for k, v := range inj.heldKeys {
		snapshot[k] = v
	}
	clear(inj.heldKeys)
	inj.mu.Unlock()

	for vk, fl := range snapshot {
		r, _, _ := procMapVirtualKey.Call(uintptr(vk), mapvkVKToVSC)
		flags := uint32(keyEventfKeyUp)
		if (fl & protocol.KeyFlagExtended) != 0 {
			flags |= keyEventfExtendedKey
		}
		pkt := keybdInputPacket{
			Type:    inputKeyboard,
			WVk:     vk,
			WScan:   uint16(r),
			DwFlags: flags,
		}
		procSendInput.Call(1, uintptr(unsafe.Pointer(&pkt)), unsafe.Sizeof(pkt))
	}
}

func buttonDownFlag(btn uint8) uint32 {
	switch btn {
	case protocol.ButtonRight:
		return mouseEventfRightDown
	case protocol.ButtonMiddle:
		return mouseEventfMiddleDown
	default:
		return mouseEventfLeftDown
	}
}

func buttonUpFlag(btn uint8) uint32 {
	switch btn {
	case protocol.ButtonRight:
		return mouseEventfRightUp
	case protocol.ButtonMiddle:
		return mouseEventfMiddleUp
	default:
		return mouseEventfLeftUp
	}
}
