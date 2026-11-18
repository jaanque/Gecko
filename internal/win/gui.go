//go:build windows

package win

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"remote-control/internal/clientui"
	"remote-control/internal/i18n"
)

var (
	kernel32GUI = syscall.NewLazyDLL("kernel32.dll")
	user32GUI   = syscall.NewLazyDLL("user32.dll")
	gdi32GUI    = syscall.NewLazyDLL("gdi32.dll")

	procGetModuleHandleW = kernel32GUI.NewProc("GetModuleHandleW")

	procRegisterClassExW  = user32GUI.NewProc("RegisterClassExW")
	procCreateWindowExW   = user32GUI.NewProc("CreateWindowExW")
	procShowWindow        = user32GUI.NewProc("ShowWindow")
	procUpdateWindow      = user32GUI.NewProc("UpdateWindow")
	procGetMessageW       = user32GUI.NewProc("GetMessageW")
	procTranslateMessage  = user32GUI.NewProc("TranslateMessage")
	procDispatchMessageW  = user32GUI.NewProc("DispatchMessageW")
	procPostQuitMessage   = user32GUI.NewProc("PostQuitMessage")
	procDefWindowProcW    = user32GUI.NewProc("DefWindowProcW")
	procPostMessageW      = user32GUI.NewProc("PostMessageW")
	procInvalidateRect    = user32GUI.NewProc("InvalidateRect")
	procBeginPaint        = user32GUI.NewProc("BeginPaint")
	procEndPaint          = user32GUI.NewProc("EndPaint")
	procFillRect          = user32GUI.NewProc("FillRect")
	procDrawTextW         = user32GUI.NewProc("DrawTextW")
	procLoadCursorW       = user32GUI.NewProc("LoadCursorW")
	procGetClientRect     = user32GUI.NewProc("GetClientRect")
	procAdjustWindowRectE = user32GUI.NewProc("AdjustWindowRectEx")
	procSetWindowPos      = user32GUI.NewProc("SetWindowPos")
	procIsIconic          = user32GUI.NewProc("IsIconic")
	procFlashWindowEx     = user32GUI.NewProc("FlashWindowEx")
	procMessageBeep       = user32GUI.NewProc("MessageBeep")
	procGetDpiForWindow   = user32GUI.NewProc("GetDpiForWindow")
	procDestroyWindow     = user32GUI.NewProc("DestroyWindow")

	procCreateSolidBrush = gdi32GUI.NewProc("CreateSolidBrush")
	procCreatePen        = gdi32GUI.NewProc("CreatePen")
	procRoundRect        = gdi32GUI.NewProc("RoundRect")
	procEllipse          = gdi32GUI.NewProc("Ellipse")
	procMoveToEx         = gdi32GUI.NewProc("MoveToEx")
	procLineTo           = gdi32GUI.NewProc("LineTo")
	procSetBkMode        = gdi32GUI.NewProc("SetBkMode")
	procSetTextColor     = gdi32GUI.NewProc("SetTextColor")
	procCreateFontW      = gdi32GUI.NewProc("CreateFontW")
)

const (
	wsOverlapped  = 0x00000000
	wsCaption     = 0x00C00000
	wsSysMenu     = 0x00080000
	wsMinimizeBox = 0x00020000
	wsChild       = 0x40000000
	bsOwnerDraw   = 0x0000000B

	swHide           = 0
	swShowNormal     = 1
	swShowNoActivate = 4
	swRestore        = 9

	wmDestroy  = 0x0002
	wmPaint    = 0x000F
	wmClose    = 0x0010
	wmDrawItem = 0x002B
	wmCommand  = 0x0111
	wmAppState = 0x8001

	bnClicked  = 0
	odsSelect  = 0x0001
	dtSingleLn = 0x00000020
	dtCenter   = 0x00000001
	dtVCenter  = 0x00000004
	dtLeft     = 0x00000000
	dtEndEllip = 0x00008000

	transparentBk = 1
	psSolid       = 0
	idcArrow      = 32512
	smCXScreen    = 0
	smCYScreen    = 1

	hwndTopMost   = ^uintptr(0) // -1
	hwndNoTopMost = ^uintptr(1) // -2
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoActivate = 0x0010
	swpShowWindow = 0x0040

	flashwAll       = 0x00000003
	flashwTimerNoFG = 0x0000000C
	mbIconAsterisk  = 0x00000040

	idBtnAllow      = 1001
	idBtnDeny       = 1002
	idBtnDisconnect = 1003
)

type rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type msgStruct struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type paintStruct struct {
	Hdc         uintptr
	FErase      int32
	RcPaint     rect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

type drawItemStruct struct {
	CtlType    uint32
	CtlID      uint32
	ItemID     uint32
	ItemAction uint32
	ItemState  uint32
	HwndItem   uintptr
	HDC        uintptr
	RcItem     rect
	ItemData   uintptr
}

type flashWInfo struct {
	CbSize    uint32
	Hwnd      uintptr
	DwFlags   uint32
	UCount    uint32
	DwTimeout uint32
}

// ClientWindow manages the native Win32 client dialog (localized ca/es/en UI).
type ClientWindow struct {
	hostname  string
	logoPNG   []byte
	strings   i18n.ClientStrings
	callbacks clientui.Callbacks

	mu        sync.Mutex
	state     clientui.State
	detail    string
	hwnd      uintptr
	btnAllow  uintptr
	btnDeny   uintptr
	btnDisc   uintptr
	dpi       int32
	readyCh   chan struct{}
	readyOnce sync.Once

	logo     *logoGDI
	fontHost uintptr
	fontBody uintptr
	fontBtn  uintptr
	bgBrush  uintptr
}

var (
	activeWinMu sync.Mutex
	activeWin   *ClientWindow
)

// NewClientWindow constructs a ClientWindow instance. Call Run() to start the Win32 message loop.
func NewClientWindow(hostname string, logoPNG []byte, lang i18n.Lang, cb clientui.Callbacks) *ClientWindow {
	str := i18n.Client(lang)
	return &ClientWindow{
		hostname:  hostname,
		logoPNG:   logoPNG,
		strings:   str,
		callbacks: cb,
		state:     clientui.StateConnecting,
		detail:    str.Connecting,
		dpi:       96,
		readyCh:   make(chan struct{}),
	}
}

// Ready returns a channel closed once the Win32 window has been created.
func (w *ClientWindow) Ready() <-chan struct{} {
	return w.readyCh
}

// SetState safely updates the window state from any goroutine.
func (w *ClientWindow) SetState(st clientui.State, detail string) {
	w.mu.Lock()
	w.state = st
	w.detail = detail
	hwnd := w.hwnd
	w.mu.Unlock()

	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmAppState, 0, 0)
	}
}

// Run creates the native Win32 window and runs the message loop until closed.
// It locks itself to an OS thread as required by Win32.
func (w *ClientWindow) Run() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	activeWinMu.Lock()
	activeWin = w
	activeWinMu.Unlock()
	defer func() {
		activeWinMu.Lock()
		if activeWin == w {
			activeWin = nil
		}
		activeWinMu.Unlock()
	}()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	w.bgBrush, _, _ = procCreateSolidBrush.Call(rgb(255, 255, 255))
	defer procDeleteObject.Call(w.bgBrush)

	className, _ := syscall.UTF16PtrFromString("RemoteClientWindowClass")
	title, _ := syscall.UTF16PtrFromString(w.strings.WindowTitle)

	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   syscall.NewCallback(clientWndProc),
		HInstance:     hInst,
		HCursor:       cursor,
		HbrBackground: w.bgBrush,
		LpszClassName: className,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	style := uintptr(wsOverlapped | wsCaption | wsSysMenu | wsMinimizeBox)

	// Create initially at logical size, then adjust for actual monitor DPI.
	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		style,
		100, 100, 420, 220,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return err
	}

	w.dpi = getWindowDPI(hwnd)
	w.initFonts()
	defer w.destroyFonts()

	if len(w.logoPNG) > 0 {
		w.logo = newLogoGDI(w.logoPNG)
		if w.logo != nil {
			defer w.logo.Close()
			if w.logo.hIcon != 0 {
				procSendMessageW.Call(hwnd, wmSetIcon, iconSmall, w.logo.hIcon)
				procSendMessageW.Call(hwnd, wmSetIcon, iconBig, w.logo.hIcon)
			}
		}
	}

	clientW := w.scale(420)
	clientH := w.scale(215)
	rc := rect{0, 0, clientW, clientH}
	procAdjustWindowRectE.Call(uintptr(unsafe.Pointer(&rc)), style, 0, 0)
	winW := rc.Right - rc.Left
	winH := rc.Bottom - rc.Top

	screenW, _, _ := procGetSystemMetrics.Call(smCXScreen)
	screenH, _, _ := procGetSystemMetrics.Call(smCYScreen)
	posX := (int32(screenW) - winW) / 2
	posY := (int32(screenH) - winH) / 2
	if posX < 0 {
		posX = 40
	}
	if posY < 0 {
		posY = 40
	}
	procSetWindowPos.Call(hwnd, 0, uintptr(posX), uintptr(posY), uintptr(winW), uintptr(winH), 0)

	btnClass, _ := syscall.UTF16PtrFromString("BUTTON")
	allowTxt, _ := syscall.UTF16PtrFromString(w.strings.Allow)
	denyTxt, _ := syscall.UTF16PtrFromString(w.strings.Deny)
	discTxt, _ := syscall.UTF16PtrFromString(w.strings.Disconnect)

	btnStyle := uintptr(wsChild | bsOwnerDraw)
	btnY := w.scale(150)
	btnH := w.scale(38)
	pad := w.scale(24)
	gap := w.scale(12)
	halfW := (clientW - pad*2 - gap) / 2

	btnAllow, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(btnClass)), uintptr(unsafe.Pointer(allowTxt)),
		btnStyle, uintptr(pad), uintptr(btnY), uintptr(halfW), uintptr(btnH),
		hwnd, idBtnAllow, hInst, 0,
	)
	btnDeny, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(btnClass)), uintptr(unsafe.Pointer(denyTxt)),
		btnStyle, uintptr(pad+halfW+gap), uintptr(btnY), uintptr(halfW), uintptr(btnH),
		hwnd, idBtnDeny, hInst, 0,
	)
	discW := w.scale(128)
	btnDisc, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(btnClass)), uintptr(unsafe.Pointer(discTxt)),
		btnStyle, uintptr(clientW-pad-discW), uintptr(w.scale(146)), uintptr(discW), uintptr(w.scale(36)),
		hwnd, idBtnDisconnect, hInst, 0,
	)

	w.mu.Lock()
	w.hwnd = hwnd
	w.btnAllow = btnAllow
	w.btnDeny = btnDeny
	w.btnDisc = btnDisc
	w.mu.Unlock()

	w.applyStateToControls()
	procShowWindow.Call(hwnd, swShowNormal)
	procUpdateWindow.Call(hwnd)
	w.readyOnce.Do(func() { close(w.readyCh) })

	var m msgStruct
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	return nil
}

func (w *ClientWindow) scale(px int32) int32 {
	return (px * w.dpi) / 96
}

func (w *ClientWindow) initFonts() {
	face, _ := syscall.UTF16PtrFromString("Segoe UI")
	w.fontHost = createFont(w.scale(26), 600, face)
	w.fontBody = createFont(w.scale(15), 400, face)
	w.fontBtn = createFont(w.scale(14), 600, face)
}

func (w *ClientWindow) destroyFonts() {
	for _, f := range []uintptr{w.fontHost, w.fontBody, w.fontBtn} {
		if f != 0 {
			procDeleteObject.Call(f)
		}
	}
}

func (w *ClientWindow) applyStateToControls() {
	w.mu.Lock()
	st := w.state
	hwnd := w.hwnd
	btnAllow := w.btnAllow
	btnDeny := w.btnDeny
	btnDisc := w.btnDisc
	w.mu.Unlock()

	if hwnd == 0 {
		return
	}

	if st == clientui.StatePrompt {
		procShowWindow.Call(btnAllow, swShowNormal)
		procShowWindow.Call(btnDeny, swShowNormal)
		procShowWindow.Call(btnDisc, swHide)

		// Pop window visibly to front without hijacking active keyboard input.
		if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
			procShowWindow.Call(hwnd, swRestore)
		} else {
			procShowWindow.Call(hwnd, swShowNoActivate)
		}
		procSetWindowPos.Call(hwnd, hwndTopMost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate|swpShowWindow)
		procMessageBeep.Call(mbIconAsterisk)
		fi := flashWInfo{
			CbSize:  uint32(unsafe.Sizeof(flashWInfo{})),
			Hwnd:    hwnd,
			DwFlags: flashwAll | flashwTimerNoFG,
			UCount:  3,
		}
		procFlashWindowEx.Call(uintptr(unsafe.Pointer(&fi)))
	} else {
		procShowWindow.Call(btnAllow, swHide)
		procShowWindow.Call(btnDeny, swHide)
		if st == clientui.StateActive {
			procShowWindow.Call(btnDisc, swShowNormal)
		} else {
			procShowWindow.Call(btnDisc, swHide)
		}
		procSetWindowPos.Call(hwnd, hwndNoTopMost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
	}

	procInvalidateRect.Call(hwnd, 0, 1)
}

func (w *ClientWindow) paint(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	var cr rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr)))
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&cr)), w.bgBrush)
	procSetBkMode.Call(hdc, transparentBk)

	pad := w.scale(24)
	textLeft := pad

	if w.logo != nil {
		logoSz := w.scale(48)
		logoRC := rect{pad, w.scale(24), pad + logoSz, w.scale(24) + logoSz}
		w.logo.DrawFit(hdc, logoRC)
		textLeft = pad + logoSz + w.scale(14)
	}

	// Large clear Hostname
	oldFont, _, _ := procSelectObject.Call(hdc, w.fontHost)
	procSetTextColor.Call(hdc, rgb(24, 24, 27))
	rcHost := rect{textLeft, w.scale(26), cr.Right - pad, w.scale(72)}
	drawText(hdc, w.hostname, &rcHost, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

	// Hairline divider
	divY := w.scale(94)
	pen, _, _ := procCreatePen.Call(psSolid, 1, rgb(228, 228, 231))
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	procMoveToEx.Call(hdc, uintptr(pad), uintptr(divY), 0)
	procLineTo.Call(hdc, uintptr(cr.Right-pad), uintptr(divY))
	procSelectObject.Call(hdc, oldPen)
	procDeleteObject.Call(pen)

	w.mu.Lock()
	st := w.state
	detail := w.detail
	w.mu.Unlock()

	procSelectObject.Call(hdc, w.fontBody)

	switch st {
	case clientui.StatePrompt:
		procSetTextColor.Call(hdc, rgb(24, 24, 27))
		rcMsg := rect{pad, w.scale(108), cr.Right - pad, w.scale(136)}
		drawText(hdc, w.strings.PromptMessage, &rcMsg, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

	case clientui.StateActive:
		drawStatusDot(hdc, pad, w.scale(158), w.scale(10), rgb(22, 163, 74))
		procSetTextColor.Call(hdc, rgb(21, 128, 61))
		rcTxt := rect{pad + w.scale(18), w.scale(148), cr.Right - w.scale(164), w.scale(178)}
		drawText(hdc, w.strings.ActiveSession, &rcTxt, dtLeft|dtSingleLn|dtVCenter)

	case clientui.StateOnline:
		drawStatusDot(hdc, pad, w.scale(138), w.scale(10), rgb(22, 163, 74))
		procSetTextColor.Call(hdc, rgb(63, 63, 70))
		txt := detail
		if txt == "" {
			txt = w.strings.Online
		}
		rcTxt := rect{pad + w.scale(18), w.scale(128), cr.Right - pad, w.scale(158)}
		drawText(hdc, txt, &rcTxt, dtLeft|dtSingleLn|dtVCenter)

	case clientui.StateError:
		drawStatusDot(hdc, pad, w.scale(138), w.scale(10), rgb(220, 38, 38))
		procSetTextColor.Call(hdc, rgb(185, 28, 28))
		rcTxt := rect{pad + w.scale(18), w.scale(128), cr.Right - pad, w.scale(158)}
		drawText(hdc, detail, &rcTxt, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

	default: // clientui.StateConnecting
		drawStatusDot(hdc, pad, w.scale(138), w.scale(10), rgb(217, 119, 6))
		procSetTextColor.Call(hdc, rgb(82, 82, 91))
		txt := detail
		if txt == "" {
			txt = w.strings.Connecting
		}
		rcTxt := rect{pad + w.scale(18), w.scale(128), cr.Right - pad, w.scale(158)}
		drawText(hdc, txt, &rcTxt, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)
	}

	procSelectObject.Call(hdc, oldFont)
}

func (w *ClientWindow) drawButton(dis *drawItemStruct) {
	hdc := dis.HDC
	rc := dis.RcItem
	pressed := (dis.ItemState & odsSelect) != 0

	var bgColor, borderColor, textColor uintptr
	var label string

	switch dis.CtlID {
	case idBtnAllow:
		label = w.strings.Allow
		if pressed {
			bgColor = rgb(20, 83, 45)
			borderColor = rgb(20, 83, 45)
		} else {
			bgColor = rgb(22, 101, 52)
			borderColor = rgb(22, 101, 52)
		}
		textColor = rgb(255, 255, 255)

	case idBtnDeny:
		label = w.strings.Deny
		if pressed {
			bgColor = rgb(254, 242, 242)
		} else {
			bgColor = rgb(255, 255, 255)
		}
		borderColor = rgb(212, 212, 216)
		textColor = rgb(185, 28, 28)

	case idBtnDisconnect:
		label = w.strings.Disconnect
		if pressed {
			bgColor = rgb(244, 244, 245)
		} else {
			bgColor = rgb(255, 255, 255)
		}
		borderColor = rgb(212, 212, 216)
		textColor = rgb(24, 24, 27)
	default:
		return
	}

	// Clear corners with parent white background first.
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), w.bgBrush)

	brush, _, _ := procCreateSolidBrush.Call(bgColor)
	pen, _, _ := procCreatePen.Call(psSolid, 1, borderColor)
	oldBrush, _, _ := procSelectObject.Call(hdc, brush)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)

	radius := w.scale(6)
	procRoundRect.Call(
		hdc,
		uintptr(rc.Left), uintptr(rc.Top), uintptr(rc.Right), uintptr(rc.Bottom),
		uintptr(radius), uintptr(radius),
	)

	procSelectObject.Call(hdc, oldBrush)
	procSelectObject.Call(hdc, oldPen)
	procDeleteObject.Call(brush)
	procDeleteObject.Call(pen)

	oldFont, _, _ := procSelectObject.Call(hdc, w.fontBtn)
	procSetBkMode.Call(hdc, transparentBk)
	procSetTextColor.Call(hdc, textColor)
	drawText(hdc, label, &rc, dtCenter|dtVCenter|dtSingleLn)
	procSelectObject.Call(hdc, oldFont)
}

func clientWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	activeWinMu.Lock()
	w := activeWin
	activeWinMu.Unlock()

	if w == nil {
		ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	}

	switch msg {
	case wmPaint:
		w.paint(hwnd)
		return 0

	case wmDrawItem:
		if lParam != 0 {
			ptr := *(*unsafe.Pointer)(unsafe.Pointer(&lParam))
			dis := (*drawItemStruct)(ptr)
			w.drawButton(dis)
			return 1
		}

	case wmAppState:
		w.applyStateToControls()
		return 0

	case wmCommand:
		id := uint16(wParam & 0xFFFF)
		code := uint16((wParam >> 16) & 0xFFFF)
		if code == bnClicked {
			switch id {
			case idBtnAllow:
				if w.callbacks.OnAllow != nil {
					go w.callbacks.OnAllow()
				}
			case idBtnDeny:
				if w.callbacks.OnDeny != nil {
					go w.callbacks.OnDeny()
				}
			case idBtnDisconnect:
				if w.callbacks.OnDisconnect != nil {
					go w.callbacks.OnDisconnect()
				}
			}
		}
		return 0

	case wmClose:
		if w.callbacks.OnClose != nil {
			go w.callbacks.OnClose()
		}
		procDestroyWindow.Call(hwnd)
		return 0

	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}

	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func drawStatusDot(hdc uintptr, x, y, size int32, color uintptr) {
	brush, _, _ := procCreateSolidBrush.Call(color)
	pen, _, _ := procCreatePen.Call(psSolid, 1, color)
	oldBrush, _, _ := procSelectObject.Call(hdc, brush)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	procEllipse.Call(hdc, uintptr(x), uintptr(y), uintptr(x+size), uintptr(y+size))
	procSelectObject.Call(hdc, oldBrush)
	procSelectObject.Call(hdc, oldPen)
	procDeleteObject.Call(brush)
	procDeleteObject.Call(pen)
}

func drawText(hdc uintptr, s string, rc *rect, flags uintptr) {
	u16, _ := syscall.UTF16FromString(s)
	if len(u16) <= 1 {
		return
	}
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)-1), uintptr(unsafe.Pointer(rc)), flags)
}

func createFont(height, weight int32, face *uint16) uintptr {
	f, _, _ := procCreateFontW.Call(
		uintptr(-height), 0, 0, 0,
		uintptr(weight),
		0, 0, 0,
		1, // DEFAULT_CHARSET
		0, 0,
		5, // CLEARTYPE_QUALITY
		0,
		uintptr(unsafe.Pointer(face)),
	)
	return f
}

func getWindowDPI(hwnd uintptr) int32 {
	if err := procGetDpiForWindow.Find(); err == nil {
		if d, _, _ := procGetDpiForWindow.Call(hwnd); d >= 72 && d <= 480 {
			return int32(d)
		}
	}
	return 96
}

func rgb(r, g, b byte) uintptr {
	return uintptr(r) | (uintptr(g) << 8) | (uintptr(b) << 16)
}
