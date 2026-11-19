//go:build windows

package win

import (
	"unsafe"

	"remote-control/internal/protocol"
)

var (
	procSetCapture        = user32GUI.NewProc("SetCapture")
	procReleaseCapture    = user32GUI.NewProc("ReleaseCapture")
	procSetFocus          = user32GUI.NewProc("SetFocus")
	procSetCursor         = user32GUI.NewProc("SetCursor")
	procGetForegroundWin  = user32GUI.NewProc("GetForegroundWindow")
	procGetAsyncKeyState  = user32GUI.NewProc("GetAsyncKeyState")
	procSetWindowsHookExW = user32GUI.NewProc("SetWindowsHookExW")
	procUnhookWindowsHook = user32GUI.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx    = user32GUI.NewProc("CallNextHookEx")
)

const (
	whKeyboardLL  = 13
	llkhfExtended = 0x01
	llkhfInjected = 0x10

	vkShift    = 0x10
	vkControl  = 0x11
	vkMenu     = 0x12
	vkM        = 0x4D
	vkQ        = 0x51
	vkLControl = 0xA2
	vkRControl = 0xA3
	vkLMenu    = 0xA4
	vkRMenu    = 0xA5
	vkF11      = 0x7A

	mapvkVscToVkEx = 3
)

type queuedInput struct {
	isKey bool
	mouse protocol.MouseEvent
	key   protocol.KeyEvent
}

type kbdLLHookStruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

func (v *remoteViewer) inputLoop(inCh <-chan queuedInput, mvCh <-chan protocol.MouseEvent, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case item, ok := <-inCh:
			if !ok {
				return
			}
			if item.isKey {
				if v.cb.OnKey != nil {
					v.cb.OnKey(item.key)
				}
			} else if v.cb.OnMouse != nil {
				v.cb.OnMouse(item.mouse)
			}
		case mv, ok := <-mvCh:
			if !ok {
				return
			}
			if v.cb.OnMouse != nil {
				v.cb.OnMouse(mv)
			}
		}
	}
}

func (v *remoteViewer) enqueueMouseMove(ev protocol.MouseEvent) {
	v.mu.Lock()
	ch := v.moveCh
	v.mu.Unlock()
	if ch == nil {
		return
	}
	// Replace any unread move with the latest cursor coordinates.
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- ev:
	default:
	}
}

func (v *remoteViewer) enqueueOrderedInput(item queuedInput) {
	v.mu.Lock()
	ch := v.inputCh
	v.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- item:
	default:
	}
}

func (v *remoteViewer) installKeyboardHook() {
	if v.kbHook != 0 {
		return
	}
	h, _, _ := procSetWindowsHookExW.Call(whKeyboardLL, v.llHookCB, v.hInst, 0)
	v.kbHook = h
}

func (v *remoteViewer) uninstallKeyboardHook() {
	if v.kbHook != 0 {
		procUnhookWindowsHook.Call(v.kbHook)
		v.kbHook = 0
	}
}

func (v *remoteViewer) mapMouseToNormalized(x, y int32) (uint16, uint16, bool) {
	rc := v.imgRect
	w := rc.Right - rc.Left
	h := rc.Bottom - rc.Top
	if w <= 1 || h <= 1 {
		return 0, 0, false
	}
	if x < rc.Left || x >= rc.Right || y < rc.Top || y >= rc.Bottom {
		if v.mouseBtns == 0 {
			return 0, 0, false
		}
	}
	// Map [0 .. w-1] -> [0 .. 65535] so both far edges align pixel-exact.
	nx := float64(x-rc.Left) / float64(w-1)
	ny := float64(y-rc.Top) / float64(h-1)
	if nx < 0 {
		nx = 0
	} else if nx > 1 {
		nx = 1
	}
	if ny < 0 {
		ny = 0
	} else if ny > 1 {
		ny = 1
	}
	return uint16(nx*65535.0 + 0.5), uint16(ny*65535.0 + 0.5), true
}

func (v *remoteViewer) sendKeyInternal(down bool, vk, scan uint16, extended bool) {
	var flags uint8
	if extended {
		flags |= protocol.KeyFlagExtended
	}
	action := uint8(protocol.KeyUp)
	if down {
		action = protocol.KeyDown
		v.heldKeys[vk] = flags
	} else {
		delete(v.heldKeys, vk)
	}
	v.enqueueOrderedInput(queuedInput{
		isKey: true,
		key: protocol.KeyEvent{
			Action: action,
			VK:     vk,
			Scan:   scan,
			Flags:  flags,
		},
	})
}

func (v *remoteViewer) handleKeyMessage(down bool, wParam, lParam uintptr) {
	vk := uint16(wParam & 0xFF)
	scan := uint16((lParam >> 16) & 0xFF)
	extended := (lParam & (1 << 24)) != 0

	if vk == vkF11 {
		if down {
			v.mu.Lock()
			hwnd := v.hwnd
			v.mu.Unlock()
			if hwnd != 0 {
				v.toggleFullscreen(hwnd)
			}
		}
		return
	}
	if (vk == vkM || vk == vkQ) && isAsyncKeyDown(vkControl) && isAsyncKeyDown(vkMenu) {
		if down {
			v.mu.Lock()
			hwnd := v.hwnd
			v.mu.Unlock()
			if hwnd != 0 {
				if vk == vkM {
					procPostMessageW.Call(hwnd, wmViewerToggleMenu, 0, 0)
				} else {
					procPostMessageW.Call(hwnd, wmViewerDisconnect, 0, 0)
				}
			}
		}
		return
	}

	switch vk {
	case vkShift:
		if r, _, _ := procMapVirtualKey.Call(uintptr(scan), mapvkVscToVkEx); r != 0 {
			vk = uint16(r)
		}
	case vkControl:
		if extended {
			vk = vkRControl
		} else {
			vk = vkLControl
		}
	case vkMenu:
		if extended {
			vk = vkRMenu
		} else {
			vk = vkLMenu
		}
	}

	v.sendKeyInternal(down, vk, scan, extended)
}

func (v *remoteViewer) releaseAllHeldKeys() {
	if len(v.heldKeys) == 0 {
		return
	}
	for vk, fl := range v.heldKeys {
		v.enqueueOrderedInput(queuedInput{
			isKey: true,
			key: protocol.KeyEvent{
				Action: protocol.KeyUp,
				VK:     vk,
				Flags:  fl,
			},
		})
	}
	clear(v.heldKeys)
}

func (v *remoteViewer) handleMouseDown(hwnd uintptr, btn uint8, x, y int32) {
	if nx, ny, ok := v.mapMouseToNormalized(x, y); ok {
		v.mouseBtns++
		procSetCapture.Call(hwnd)
		v.enqueueOrderedInput(queuedInput{mouse: protocol.MouseEvent{Action: protocol.MouseDown, Button: btn, X: nx, Y: ny}})
	}
}

func (v *remoteViewer) handleMouseUp(btn uint8, lParam uintptr) {
	x, y := unpackXY(lParam)
	if v.mouseBtns > 0 {
		v.mouseBtns--
		if v.mouseBtns == 0 {
			procReleaseCapture.Call()
		}
	}
	if nx, ny, ok := v.mapMouseToNormalized(x, y); ok {
		v.enqueueOrderedInput(queuedInput{mouse: protocol.MouseEvent{Action: protocol.MouseUp, Button: btn, X: nx, Y: ny}})
	}
}

func xButtonFromWParam(wParam uintptr) uint8 {
	if ((wParam >> 16) & 0xFFFF) == xButton2 {
		return protocol.ButtonX2
	}
	return protocol.ButtonX1
}

func unpackXY(lParam uintptr) (int32, int32) {
	x := int32(int16(lParam & 0xFFFF))
	y := int32(int16((lParam >> 16) & 0xFFFF))
	return x, y
}

func isAsyncKeyDown(vk uintptr) bool {
	r, _, _ := procGetAsyncKeyState.Call(vk)
	return (r & 0x8000) != 0
}

func viewerKeyboardLLProc(nCode int32, wParam, lParam uintptr) uintptr {
	activeViewerMu.Lock()
	v := activeViewer
	activeViewerMu.Unlock()

	if nCode >= 0 && v != nil && lParam != 0 {
		v.mu.Lock()
		hwnd := v.hwnd
		v.mu.Unlock()

		if hwnd != 0 {
			fg, _, _ := procGetForegroundWin.Call()
			if fg == hwnd {
				ptr := *(*unsafe.Pointer)(unsafe.Pointer(&lParam))
				kb := (*kbdLLHookStruct)(ptr)

				// Never re-intercept injected events (prevents loopback if testing on same PC).
				if (kb.Flags & llkhfInjected) == 0 {
					vk := uint16(kb.VkCode & 0xFF)
					down := wParam == wmKeyDown || wParam == wmSysKeyDown
					up := wParam == wmKeyUp || wParam == wmSysKeyUp

					// Local viewer hotkeys (dispatched asynchronously via PostMessageW):
					// - F11: toggle fullscreen
					// - Ctrl+Alt+M: toggle menu bar
					// - Ctrl+Alt+Q: disconnect session
					if vk == vkF11 {
						if down {
							procPostMessageW.Call(hwnd, wmViewerToggleFull, 0, 0)
						}
						return 1
					}
					if (vk == vkM || vk == vkQ) && isAsyncKeyDown(vkControl) && isAsyncKeyDown(vkMenu) {
						if down {
							if vk == vkM {
								procPostMessageW.Call(hwnd, wmViewerToggleMenu, 0, 0)
							} else {
								procPostMessageW.Call(hwnd, wmViewerDisconnect, 0, 0)
							}
						}
						return 1
					}

					if down || up {
						extended := (kb.Flags & llkhfExtended) != 0
						v.sendKeyInternal(down, vk, uint16(kb.ScanCode&0xFF), extended)
						return 1
					}
				}
			}
		}
	}

	r, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return r
}
