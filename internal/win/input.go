//go:build windows

package win

import (
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"remote-control/internal/protocol"
)

var (
	sasDLL = syscall.NewLazyDLL("sas.dll")

	procSendInput     = user32GUI.NewProc("SendInput")
	procMapVirtualKey = user32GUI.NewProc("MapVirtualKeyW")
	procSendSAS       = sasDLL.NewProc("SendSAS")
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
	mouseEventfXDown       = 0x0080
	mouseEventfXUp         = 0x0100
	mouseEventfWheel       = 0x0800
	mouseEventfHWheel      = 0x1000
	mouseEventfVirtualDesk = 0x4000
	mouseEventfAbsolute    = 0x8000

	xButton1 = 0x0001
	xButton2 = 0x0002

	keyEventfExtendedKey = 0x0001
	keyEventfKeyUp       = 0x0002

	mapvkVKToVSC = 0
)

// InputInjector wraps Win32 SendInput and tracks held keys so they can always be released cleanly.
// Injection is disabled by default and MUST be explicitly enabled via SetEnabled(true) only after
// strict mTLS server identity verification and explicit user consent.
type InputInjector struct {
	enabled  atomic.Bool
	mu       sync.Mutex
	heldKeys map[uint16]uint8 // vk -> flags
}

// NewInputInjector creates a new InputInjector (locked/disabled by default).
func NewInputInjector() *InputInjector {
	return &InputInjector{
		heldKeys: make(map[uint16]uint8),
	}
}

// SetEnabled opens or locks OS input injection. Locking immediately releases any held keys.
func (inj *InputInjector) SetEnabled(enabled bool) {
	inj.enabled.Store(enabled)
	if !enabled {
		inj.ReleaseAllKeys()
	}
}

// Enabled reports whether OS input injection is currently permitted.
func (inj *InputInjector) Enabled() bool {
	return inj.enabled.Load()
}

// HandleMouse injects a normalized mouse event mapped to the active monitor (0 = Monitor 1, -1 = all monitors).
func (inj *InputInjector) HandleMouse(ev protocol.MouseEvent, monitorIndex int) {
	if !inj.enabled.Load() {
		return
	}
	if ev.Action > protocol.MouseHWheel || ev.Button > protocol.ButtonX2 {
		return
	}
	pkt := mouseInputPacket{
		Type: inputMouse,
	}

	switch ev.Action {
	case protocol.MouseMove:
		pkt.DX, pkt.DY = mapNormalizedToVirtualDesk(ev.X, ev.Y, monitorIndex)
		pkt.DwFlags = mouseEventfMove | mouseEventfAbsolute | mouseEventfVirtualDesk
	case protocol.MouseDown:
		pkt.DX, pkt.DY = mapNormalizedToVirtualDesk(ev.X, ev.Y, monitorIndex)
		fl, md := buttonDownFlags(ev.Button)
		pkt.DwFlags = mouseEventfMove | mouseEventfAbsolute | mouseEventfVirtualDesk | fl
		pkt.MouseData = md
	case protocol.MouseUp:
		pkt.DX, pkt.DY = mapNormalizedToVirtualDesk(ev.X, ev.Y, monitorIndex)
		fl, md := buttonUpFlags(ev.Button)
		pkt.DwFlags = mouseEventfMove | mouseEventfAbsolute | mouseEventfVirtualDesk | fl
		pkt.MouseData = md
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

func mapNormalizedToVirtualDesk(normX, normY uint16, monitorIndex int) (int32, int32) {
	if monitorIndex < 0 {
		return int32(normX), int32(normY)
	}
	reg := ResolveMonitorBounds(monitorIndex)
	virt := VirtualDesktopBounds()
	if virt.Width <= 1 || virt.Height <= 1 || reg.Width <= 1 || reg.Height <= 1 {
		return int32(normX), int32(normY)
	}

	px := float64(reg.X) + (float64(normX)/65535.0)*float64(reg.Width-1)
	py := float64(reg.Y) + (float64(normY)/65535.0)*float64(reg.Height-1)

	vx := ((px - float64(virt.X)) / float64(virt.Width-1)) * 65535.0
	vy := ((py - float64(virt.Y)) / float64(virt.Height-1)) * 65535.0
	if vx < 0 {
		vx = 0
	} else if vx > 65535 {
		vx = 65535
	}
	if vy < 0 {
		vy = 0
	} else if vy > 65535 {
		vy = 65535
	}
	return int32(vx + 0.5), int32(vy + 0.5)
}

// HandleKey injects a keyboard event via Win32 SendInput.
func (inj *InputInjector) HandleKey(ev protocol.KeyEvent) {
	if !inj.enabled.Load() {
		return
	}
	if ev.Action > protocol.KeyUp || ev.VK == 0 || ev.VK > 0xFE || (ev.Flags&^protocol.KeyFlagExtended) != 0 {
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

// SendSystemAction triggers a supported Windows system action such as Secure Attention Sequence
// (Ctrl+Alt+Del) via sas.dll!SendSAS (effective when running as a Windows Service / with SAS policy).
func (inj *InputInjector) SendSystemAction(action string) {
	if !inj.enabled.Load() || action != protocol.ActionCtrlAltDel {
		return
	}
	if err := procSendSAS.Find(); err == nil {
		// FALSE (0) = service/system context, TRUE (1) = regular user context (requires policy).
		_, _, _ = procSendSAS.Call(1)
	}
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

func buttonDownFlags(btn uint8) (uint32, uint32) {
	switch btn {
	case protocol.ButtonRight:
		return mouseEventfRightDown, 0
	case protocol.ButtonMiddle:
		return mouseEventfMiddleDown, 0
	case protocol.ButtonX1:
		return mouseEventfXDown, xButton1
	case protocol.ButtonX2:
		return mouseEventfXDown, xButton2
	default:
		return mouseEventfLeftDown, 0
	}
}

func buttonUpFlags(btn uint8) (uint32, uint32) {
	switch btn {
	case protocol.ButtonRight:
		return mouseEventfRightUp, 0
	case protocol.ButtonMiddle:
		return mouseEventfMiddleUp, 0
	case protocol.ButtonX1:
		return mouseEventfXUp, xButton1
	case protocol.ButtonX2:
		return mouseEventfXUp, xButton2
	default:
		return mouseEventfLeftUp, 0
	}
}
