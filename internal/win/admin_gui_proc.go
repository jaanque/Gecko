//go:build windows

package win

import (
	"fmt"
	"strings"
	"unsafe"

	"remote-control/internal/security"
)

func (a *AdminWindow) hitTest(x, y int32) int {
	for i, h := range a.hits {
		if x >= h.rc.Left && x <= h.rc.Right && y >= h.rc.Top && y <= h.rc.Bottom {
			return i
		}
	}
	return -1
}

func (a *AdminWindow) handleClick(hwnd uintptr, x, y int32) {
	idx := a.hitTest(x, y)
	if idx < 0 || idx >= len(a.hits) {
		return
	}
	hit := a.hits[idx]
	switch hit.kind {
	case "tab_devices":
		a.switchTab(hwnd, adminTabDevices)

	case "tab_builder":
		a.switchTab(hwnd, adminTabBuilder)

	case "tab_server":
		a.switchTab(hwnd, adminTabServer)

	case "paste_key":
		if clip := readTextFromClipboard(hwnd); clip != "" {
			clip = strings.TrimSpace(clip)
			setControlText(a.ctrlSrvKey, clip)
			if prof, err := security.DecodeProfileKey(clip); err == nil && prof.ServerAddr != "" {
				setControlText(a.ctrlSrvAddr, prof.ServerAddr)
			}
		}

	case "save_server":
		a.handleSaveServer(hwnd)

	case "pick_logo":
		a.handlePickLogo(hwnd)

	case "remove_logo":
		if a.logoPreview != nil {
			a.logoPreview.Close()
			a.logoPreview = nil
		}
		a.logoPNG = nil
		a.logoName = ""
		procInvalidateRect.Call(hwnd, 0, 0)

	case "lang_ca", "lang_es", "lang_en":
		a.clientLang = strings.TrimPrefix(hit.kind, "lang_")
		procInvalidateRect.Call(hwnd, 0, 0)

	case "save_client_win":
		a.handleSaveClient(hwnd)

	case "cancel_pending":
		a.mu.Lock()
		a.pendingHost = ""
		a.mu.Unlock()
		if a.callbacks.OnCancelPending != nil {
			go a.callbacks.OnCancelPending()
		}
		a.syncServerControls(hwnd)
		procInvalidateRect.Call(hwnd, 0, 0)

	case "close_notice":
		a.mu.Lock()
		a.noticeText = ""
		a.mu.Unlock()
		a.syncServerControls(hwnd)
		procInvalidateRect.Call(hwnd, 0, 0)

	case "revoke":
		if a.callbacks.OnRevokeClient != nil {
			msg := fmt.Sprintf("¿Revocar el acceso del equipo %s?\n\nSe desconectará inmediatamente y ese ejecutable no podrá volver a conectarse.", hit.hostname)
			if ShowConfirmBox(hwnd, "Revocar cliente", msg) {
				credID, cid := hit.credID, hit.clientID
				go func() {
					if err := a.callbacks.OnRevokeClient(credID, cid); err != nil {
						a.NotifyError(err.Error())
					}
				}()
			}
		}

	case "row":
		cid := hit.clientID
		if a.callbacks.OnConnect != nil {
			go func() {
				if err := a.callbacks.OnConnect(cid); err != nil {
					a.NotifyError(err.Error())
				}
			}()
		}
	}
}

func adminWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	activeAdminMu.Lock()
	a := activeAdmin
	activeAdminMu.Unlock()

	if a == nil {
		ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	}

	switch msg {
	case wmEraseBkgnd:
		return 1

	case wmGetMinMaxInf:
		if lParam != 0 {
			ptr := *(*unsafe.Pointer)(unsafe.Pointer(&lParam))
			mmi := (*minMaxInfo)(ptr)
			mmi.PtMinTrackSize.X = a.scale(620)
			mmi.PtMinTrackSize.Y = a.scale(420)
		}
		return 0

	case wmCommand:
		id := uint16(wParam & 0xFFFF)
		code := uint16((wParam >> 16) & 0xFFFF)
		if id == idSearchBox && code == enChange {
			a.searchQuery = getControlText(a.ctrlSearch)
			a.scrollY = 0
			procInvalidateRect.Call(hwnd, 0, 0)
			return 0
		}

	case wmDPIChanged:
		newDPI := int32(wParam & 0xFFFF)
		if newDPI >= 72 && newDPI <= 480 && newDPI != a.dpi {
			a.dpi = newDPI
			a.destroyFonts()
			a.initFonts()
			procSendMessageW.Call(a.ctrlSearch, wmSetFont, a.fontBody, 1)
			procSendMessageW.Call(a.ctrlSrvAddr, wmSetFont, a.fontMono, 1)
			procSendMessageW.Call(a.ctrlSrvKey, wmSetFont, a.fontMono, 1)
			if lParam != 0 {
				ptr := *(*unsafe.Pointer)(unsafe.Pointer(&lParam))
				sug := (*rect)(ptr)
				procSetWindowPos.Call(hwnd, 0,
					uintptr(sug.Left), uintptr(sug.Top),
					uintptr(sug.Right-sug.Left), uintptr(sug.Bottom-sug.Top),
					0x0004|0x0010,
				)
			}
			a.syncServerControls(hwnd)
			procInvalidateRect.Call(hwnd, 0, 0)
		}
		return 0

	case wmSize, wmAdminRefresh:
		a.syncServerControls(hwnd)
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0

	case wmAdminOpenViewer:
		a.mu.Lock()
		req := a.openReq
		a.openReq = nil
		v := a.viewer
		a.mu.Unlock()
		if req != nil && v != nil {
			v.Open(req.sessionID, req.hostname, req.width, req.height, req.monitorCount)
		}
		a.syncServerControls(hwnd)
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0

	case wmAdminCloseViewer:
		a.mu.Lock()
		v := a.viewer
		a.mu.Unlock()
		if v != nil {
			v.CloseUI()
		}
		a.syncServerControls(hwnd)
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0

	case wmPaint:
		a.paint(hwnd)
		return 0

	case wmSetCursor:
		if (lParam & 0xFFFF) == 1 {
			if a.hoverIdx >= 0 {
				procSetCursor.Call(a.cursorHnd)
			} else {
				procSetCursor.Call(a.cursorArr)
			}
			return 1
		}

	case wmMouseMove:
		x, y := unpackXY(lParam)
		hi := a.hitTest(x, y)
		if hi != a.hoverIdx {
			a.hoverIdx = hi
			procInvalidateRect.Call(hwnd, 0, 0)
		}
		return 0

	case wmLButtonDown:
		x, y := unpackXY(lParam)
		a.handleClick(hwnd, x, y)
		return 0

	case wmMouseWheel:
		if a.activeTab == adminTabDevices && a.maxScrollY > 0 {
			delta := int16(uint16((wParam >> 16) & 0xFFFF))
			a.scrollY -= int32(delta) / 3
			if a.scrollY < 0 {
				a.scrollY = 0
			}
			if a.scrollY > a.maxScrollY {
				a.scrollY = a.maxScrollY
			}
			procInvalidateRect.Call(hwnd, 0, 0)
		}
		return 0

	case wmClose:
		if a.logoPreview != nil {
			a.logoPreview.Close()
			a.logoPreview = nil
		}
		a.mu.Lock()
		v := a.viewer
		a.mu.Unlock()
		if v != nil {
			v.CloseUI()
		}
		if a.callbacks.OnClose != nil {
			go a.callbacks.OnClose()
		}
		procDestroyWindow.Call(hwnd)
		return 0

	case wmDestroy:
		a.destroyFonts()
		procPostQuitMessage.Call(0)
		return 0
	}

	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func getControlText(hwnd uintptr) string {
	n, _, _ := procGetWindowTextLenW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+2)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

func setControlText(hwnd uintptr, s string) {
	u16, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return
	}
	procSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(u16)))
}
