//go:build linux

package linux

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"remote-control/internal/clientui"
	"remote-control/internal/protocol"
)

const (
	xEventKeyPress      = 2
	xEventKeyRelease    = 3
	xEventButtonPress   = 4
	xEventButtonRelease = 5
	xEventMotionNotify  = 6
)

// InputInjector injects remote mouse and keyboard events into X11 via the XTEST extension.
// Injection is disabled by default and MUST be explicitly enabled via SetEnabled(true) only after
// strict mTLS server identity verification and explicit user consent.
type InputInjector struct {
	enabled      atomic.Bool
	mu           sync.Mutex
	xc           *x11Conn
	xtestMajor   byte
	xtestOK      bool
	symToCode    map[uint32]byte
	heldCodes    map[byte]bool
	lastMonIdx   int
	lastMonCheck time.Time
	cachedBounds clientui.MonitorBounds
}

// NewInputInjector constructs a Linux XTEST InputInjector (locked/disabled by default).
func NewInputInjector() *InputInjector {
	return &InputInjector{
		symToCode:  make(map[uint32]byte),
		heldCodes:  make(map[byte]bool),
		lastMonIdx: -999,
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

func (inj *InputInjector) ensureConnLocked() bool {
	if inj.xc != nil && inj.xtestOK {
		return true
	}
	if inj.xc != nil {
		_ = inj.xc.Close()
		inj.xc = nil
		inj.xtestOK = false
	}

	xc, err := dialX11()
	if err != nil {
		return false
	}
	present, major, _, _, err := xc.QueryExtension("XTEST")
	if err != nil || !present {
		_ = xc.Close()
		return false
	}

	symMap := make(map[uint32]byte, 256)
	if per, syms, err := xc.GetKeyboardMapping(); err == nil && per > 0 {
		count := len(syms) / per
		for i := 0; i < count; i++ {
			kc := byte(int(xc.minKeycode) + i)
			// Prefer column 0 (unshifted base keysym) first.
			base := syms[i*per]
			if base != 0 {
				if _, exists := symMap[base]; !exists {
					symMap[base] = kc
				}
			}
		}
		// Fill remaining symbols from secondary columns if not yet mapped.
		for i := 0; i < count; i++ {
			kc := byte(int(xc.minKeycode) + i)
			for col := 1; col < per; col++ {
				sym := syms[i*per+col]
				if sym != 0 {
					if _, exists := symMap[sym]; !exists {
						symMap[sym] = kc
					}
				}
			}
		}
	}

	inj.xc = xc
	inj.xtestMajor = major
	inj.xtestOK = true
	inj.symToCode = symMap
	inj.lastMonIdx = -999
	return true
}

func (inj *InputInjector) fakeInputLocked(evType, detail byte, rootX, rootY int16) {
	if !inj.ensureConnLocked() {
		return
	}
	var req [36]byte
	req[0] = inj.xtestMajor
	req[1] = 2 // XTestFakeInput
	binary.LittleEndian.PutUint16(req[2:4], 9)
	req[4] = evType
	req[5] = detail
	binary.LittleEndian.PutUint32(req[12:16], inj.xc.root)
	binary.LittleEndian.PutUint16(req[24:26], uint16(rootX))
	binary.LittleEndian.PutUint16(req[26:28], uint16(rootY))

	if err := inj.xc.writeRaw(req[:]); err != nil {
		_ = inj.xc.Close()
		inj.xc = nil
		inj.xtestOK = false
	}
}

func (inj *InputInjector) mapCoordsLocked(normX, normY uint16, monitorIndex int) (int16, int16) {
	now := time.Now()
	if monitorIndex != inj.lastMonIdx || now.Sub(inj.lastMonCheck) > 2*time.Second || inj.cachedBounds.Width <= 0 {
		inj.cachedBounds = resolveMonitorBoundsOnConn(inj.xc, monitorIndex)
		inj.lastMonIdx = monitorIndex
		inj.lastMonCheck = now
	}
	b := inj.cachedBounds
	if b.Width <= 1 || b.Height <= 1 {
		return 0, 0
	}
	px := b.X + int((float64(normX)/65535.0)*float64(b.Width-1)+0.5)
	py := b.Y + int((float64(normY)/65535.0)*float64(b.Height-1)+0.5)
	return int16(px), int16(py)
}

// HandleMouse injects a normalized mouse event mapped to the active monitor (0 = Monitor 1, -1 = all monitors).
func (inj *InputInjector) HandleMouse(ev protocol.MouseEvent, monitorIndex int) {
	if !inj.enabled.Load() {
		return
	}
	if ev.Action > protocol.MouseHWheel || ev.Button > protocol.ButtonX2 {
		return
	}
	inj.mu.Lock()
	defer inj.mu.Unlock()

	if !inj.ensureConnLocked() {
		return
	}

	switch ev.Action {
	case protocol.MouseMove:
		x, y := inj.mapCoordsLocked(ev.X, ev.Y, monitorIndex)
		inj.fakeInputLocked(xEventMotionNotify, 0, x, y)

	case protocol.MouseDown:
		x, y := inj.mapCoordsLocked(ev.X, ev.Y, monitorIndex)
		inj.fakeInputLocked(xEventMotionNotify, 0, x, y)
		inj.fakeInputLocked(xEventButtonPress, mapMouseButton(ev.Button), 0, 0)

	case protocol.MouseUp:
		x, y := inj.mapCoordsLocked(ev.X, ev.Y, monitorIndex)
		inj.fakeInputLocked(xEventMotionNotify, 0, x, y)
		inj.fakeInputLocked(xEventButtonRelease, mapMouseButton(ev.Button), 0, 0)

	case protocol.MouseWheel:
		steps := int(ev.Delta) / 120
		if steps == 0 {
			if ev.Delta > 0 {
				steps = 1
			} else if ev.Delta < 0 {
				steps = -1
			}
		}
		btn := byte(4) // Scroll Up
		if steps < 0 {
			btn = 5 // Scroll Down
			steps = -steps
		}
		if steps > 10 {
			steps = 10
		}
		for i := 0; i < steps; i++ {
			inj.fakeInputLocked(xEventButtonPress, btn, 0, 0)
			inj.fakeInputLocked(xEventButtonRelease, btn, 0, 0)
		}

	case protocol.MouseHWheel:
		steps := int(ev.Delta) / 120
		if steps == 0 {
			if ev.Delta > 0 {
				steps = 1
			} else if ev.Delta < 0 {
				steps = -1
			}
		}
		btn := byte(7) // Scroll Right
		if steps < 0 {
			btn = 6 // Scroll Left
			steps = -steps
		}
		if steps > 10 {
			steps = 10
		}
		for i := 0; i < steps; i++ {
			inj.fakeInputLocked(xEventButtonPress, btn, 0, 0)
			inj.fakeInputLocked(xEventButtonRelease, btn, 0, 0)
		}
	}
}

func mapMouseButton(btn uint8) byte {
	switch btn {
	case protocol.ButtonRight:
		return 3
	case protocol.ButtonMiddle:
		return 2
	case protocol.ButtonX1:
		return 8
	case protocol.ButtonX2:
		return 9
	default:
		return 1
	}
}

// HandleKey injects a keyboard event via XTEST.
func (inj *InputInjector) HandleKey(ev protocol.KeyEvent) {
	if !inj.enabled.Load() {
		return
	}
	if ev.Action > protocol.KeyUp || ev.VK == 0 || ev.VK > 0xFE || (ev.Flags&^protocol.KeyFlagExtended) != 0 {
		return
	}

	inj.mu.Lock()
	defer inj.mu.Unlock()

	if !inj.ensureConnLocked() {
		return
	}

	kc := inj.resolveKeycodeLocked(ev)
	if kc == 0 {
		return
	}

	switch ev.Action {
	case protocol.KeyDown:
		inj.heldCodes[kc] = true
		inj.fakeInputLocked(xEventKeyPress, kc, 0, 0)
	case protocol.KeyUp:
		delete(inj.heldCodes, kc)
		inj.fakeInputLocked(xEventKeyRelease, kc, 0, 0)
	}
}

func (inj *InputInjector) resolveKeycodeLocked(ev protocol.KeyEvent) byte {
	// For OEM keys, prefer physical scancode mapping so Spanish/Catalan/European keyboard layouts match naturally.
	isOEM := (ev.VK >= 0xBA && ev.VK <= 0xC0) || (ev.VK >= 0xDB && ev.VK <= 0xE2)
	ext := (ev.Flags & protocol.KeyFlagExtended) != 0
	if isOEM && ev.Scan != 0 {
		if kc := ScanToEvdevKeycode(ev.Scan, ext); kc >= inj.xc.minKeycode && kc <= inj.xc.maxKeycode {
			return kc
		}
	}
	if sym := VKToKeySym(ev.VK); sym != 0 {
		if kc, ok := inj.symToCode[sym]; ok && kc != 0 {
			return kc
		}
	}
	if ev.Scan != 0 {
		if kc := ScanToEvdevKeycode(ev.Scan, ext); kc >= inj.xc.minKeycode && kc <= inj.xc.maxKeycode {
			return kc
		}
	}
	return 0
}

// SendSystemAction triggers a supported system key sequence (e.g. Ctrl+Alt+Del) via XTEST.
func (inj *InputInjector) SendSystemAction(action string) {
	if !inj.enabled.Load() || action != protocol.ActionCtrlAltDel {
		return
	}
	inj.mu.Lock()
	defer inj.mu.Unlock()
	if !inj.ensureConnLocked() {
		return
	}
	ctrl := inj.resolveKeycodeLocked(protocol.KeyEvent{VK: 0xA2})
	alt := inj.resolveKeycodeLocked(protocol.KeyEvent{VK: 0xA4})
	del := inj.resolveKeycodeLocked(protocol.KeyEvent{VK: 0x2E, Flags: protocol.KeyFlagExtended})
	if ctrl == 0 || alt == 0 || del == 0 {
		return
	}
	inj.fakeInputLocked(xEventKeyPress, ctrl, 0, 0)
	inj.fakeInputLocked(xEventKeyPress, alt, 0, 0)
	inj.fakeInputLocked(xEventKeyPress, del, 0, 0)
	inj.fakeInputLocked(xEventKeyRelease, del, 0, 0)
	inj.fakeInputLocked(xEventKeyRelease, alt, 0, 0)
	inj.fakeInputLocked(xEventKeyRelease, ctrl, 0, 0)
}

// ReleaseAllKeys releases any keys currently held down (called on session end).
func (inj *InputInjector) ReleaseAllKeys() {
	inj.mu.Lock()
	defer inj.mu.Unlock()

	if len(inj.heldCodes) == 0 {
		return
	}
	for kc := range inj.heldCodes {
		inj.fakeInputLocked(xEventKeyRelease, kc, 0, 0)
	}
	clear(inj.heldCodes)
}
