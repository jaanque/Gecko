//go:build windows

package win

import (
	"sync"
	"syscall"
	"time"
	"unsafe"

	"remote-control/internal/protocol"
	"remote-control/internal/video"
)

var (
	procGetWindowLongPtrW  = user32GUI.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW  = user32GUI.NewProc("SetWindowLongPtrW")
	procGetWindowPlacement = user32GUI.NewProc("GetWindowPlacement")
	procSetWindowPlacement = user32GUI.NewProc("SetWindowPlacement")
	procMonitorFromWindow  = user32GUI.NewProc("MonitorFromWindow")
	procCreateIconIndirect = user32GUI.NewProc("CreateIconIndirect")
	procDestroyIcon        = user32GUI.NewProc("DestroyIcon")
	procCreateDIBSection   = gdi32GUI.NewProc("CreateDIBSection")
	procCreateBitmap       = gdi32GUI.NewProc("CreateBitmap")
	procDrawIconEx         = user32GUI.NewProc("DrawIconEx")
	procLoadCursorW        = user32GUI.NewProc("LoadCursorW")
)

const (
	wsOverlappedWindow = 0x00CF0000
	wmDropFiles        = 0x0233
	diNormal           = 0x0003
	idcArrow           = 32512

	wmSize         = 0x0005
	wmKillFocus    = 0x0008
	wmEraseBkgnd   = 0x0014
	wmGetMinMaxInf = 0x0024
	wmKeyDown      = 0x0100
	wmKeyUp        = 0x0101
	wmSysKeyDown   = 0x0104
	wmSysKeyUp     = 0x0105
	wmMouseMove    = 0x0200
	wmLButtonDown  = 0x0201
	wmLButtonUp    = 0x0202
	wmRButtonDown  = 0x0204
	wmRButtonUp    = 0x0205
	wmMButtonDown  = 0x0207
	wmMButtonUp    = 0x0208
	wmMouseWheel   = 0x020A
	wmXButtonDown  = 0x020B
	wmXButtonUp    = 0x020C
	wmMouseHWheel  = 0x020E
	wmSetCursor    = 0x0020

	wmViewerFrame      = 0x8020
	wmViewerToggleFull = 0x8021
	wmViewerToggleMenu = 0x8022
	wmViewerDisconnect = 0x8023

	monitorDefaultNear = 2
	gwlStyle           = ^uintptr(15) // -16
	swpFrameChanged    = 0x0020
	swpNoOwnerZOrder   = 0x0200

	idcHand = 32649
)

type windowPlacement struct {
	Length           uint32
	Flags            uint32
	ShowCmd          uint32
	PtMinPosition    point
	PtMaxPosition    point
	RcNormalPosition rect
}

type monitorInfo struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
}

type minMaxInfo struct {
	PtReserved     point
	PtMaxSize      point
	PtMaxPosition  point
	PtMinTrackSize point
	PtMaxTrackSize point
}

// ViewerCallbacks wires the native remote desktop viewer back to the Hub.
type ViewerCallbacks struct {
	OnMouse         func(ev protocol.MouseEvent)
	OnKey           func(ev protocol.KeyEvent)
	OnFrameAck      func(frameID uint32)
	OnQuality       func(quality int)
	OnSelectMonitor func(monitorIndex int)
	OnClipboard     func(text string)
	OnSystemAction  func(action string)
	OnSendFile      func(path string) error
	OnDisconnect    func()
}

type remoteViewer struct {
	cb        ViewerCallbacks
	fb        *video.FrameBuffer
	hInst     uintptr
	className *uint16
	cursorArr uintptr
	cursorHnd uintptr
	llHookCB  uintptr

	mu            sync.Mutex
	hwnd          uintptr
	sessionID     string
	hostname      string
	remoteW       int
	remoteH       int
	monitorCount  int
	activeMonitor int // 0 = Monitor 1 (default), 1..N-1 = other, -1 = all monitors
	quality       int
	dpi           int32

	fpsCount int
	fpsTick  time.Time
	fps      int
	rttMs    int // -1 until first RTT measurement

	clipMu       sync.Mutex
	lastClipText string

	statusMsg   string
	statusUntil time.Time

	frameCh    chan []byte
	decoderEnd chan struct{}

	// Non-blocking input pipeline: ordered clicks/keys + coalesced mouse moves.
	inputCh   chan queuedInput
	moveCh    chan protocol.MouseEvent
	inputDone chan struct{}

	// Cached offscreen double buffer (UI thread only)
	backDC  uintptr
	backBmp uintptr
	backOld uintptr
	backW   int32
	backH   int32

	// UI thread-only state
	kbHook          uintptr
	fontBold        uintptr
	fontBody        uintptr
	fontMono        uintptr
	fontBtn         uintptr
	toolbarBtns     []viewerToolbarBtn
	toolbarDividers []int32
	toolbarHidden   bool
	pillBox         rect
	hoverBtn        int
	imgRect         rect
	fullscreen      bool
	prevStyle       uintptr
	prevPlace       windowPlacement
	lastMoveAt      time.Time
	heldKeys        map[uint16]uint8
	mouseBtns       int

	cursorMu      sync.Mutex
	cursorX       uint16
	cursorY       uint16
	cursorHotX    uint16
	cursorHotY    uint16
	cursorWidth   uint16
	cursorHeight  uint16
	cursorVisible bool
	cursorHIcon   uintptr
}

var (
	activeViewerMu sync.Mutex
	activeViewer   *remoteViewer
)

func newRemoteViewer(hInst uintptr, cb ViewerCallbacks) *remoteViewer {
	className, _ := syscall.UTF16PtrFromString("RemoteAdminViewerClass")
	cursorArr, _, _ := procLoadCursorW.Call(0, idcArrow)
	cursorHnd, _, _ := procLoadCursorW.Call(0, idcHand)

	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   syscall.NewCallback(viewerWndProc),
		HInstance:     hInst,
		HCursor:       cursorArr,
		LpszClassName: className,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	return &remoteViewer{
		cb:        cb,
		fb:        video.NewFrameBuffer(),
		hInst:     hInst,
		className: className,
		cursorArr: cursorArr,
		cursorHnd: cursorHnd,
		llHookCB:  syscall.NewCallback(viewerKeyboardLLProc),
		quality:   qualMedVal,
		dpi:       96,
		rttMs:     -1,
		heldKeys:  make(map[uint16]uint8),
	}
}

// Open must be called on the Win32 UI thread.
func (v *remoteViewer) Open(sessionID, hostname string, remoteW, remoteH, monitorCount int) {
	v.CloseUI()

	if monitorCount < 1 {
		monitorCount = 1
	}

	v.mu.Lock()
	v.sessionID = sessionID
	v.hostname = hostname
	v.remoteW = remoteW
	v.remoteH = remoteH
	v.monitorCount = monitorCount
	v.activeMonitor = 0 // Always start on Monitor 1
	v.quality = qualMedVal
	v.fps = 0
	v.fpsCount = 0
	v.fpsTick = time.Now()
	v.rttMs = -1
	v.frameCh = make(chan []byte, 2)
	v.decoderEnd = make(chan struct{})
	v.inputCh = make(chan queuedInput, 256)
	v.moveCh = make(chan protocol.MouseEvent, 1)
	v.inputDone = make(chan struct{})
	fCh := v.frameCh
	fEnd := v.decoderEnd
	inCh := v.inputCh
	mvCh := v.moveCh
	inDone := v.inputDone
	v.mu.Unlock()

	v.fb.Reset(remoteW, remoteH)
	v.fullscreen = false
	v.toolbarHidden = false
	v.hoverBtn = 0
	clear(v.heldKeys)
	v.mouseBtns = 0

	activeViewerMu.Lock()
	activeViewer = v
	activeViewerMu.Unlock()

	screenW, _, _ := procGetSystemMetrics.Call(smCXScreen)
	screenH, _, _ := procGetSystemMetrics.Call(smCYScreen)
	winW := int32(screenW) * 84 / 100
	winH := int32(screenH) * 84 / 100
	if winW > 1360 {
		winW = 1360
	}
	if winH > 860 {
		winH = 860
	}
	if winW < 760 {
		winW = 760
	}
	if winH < 520 {
		winH = 520
	}
	posX := (int32(screenW) - winW) / 2
	posY := (int32(screenH) - winH) / 2

	title, _ := syscall.UTF16PtrFromString(hostname)
	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(v.className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow,
		uintptr(posX), uintptr(posY), uintptr(winW), uintptr(winH),
		0, 0, v.hInst, 0,
	)
	if hwnd == 0 {
		close(fEnd)
		close(inDone)
		return
	}

	v.dpi = getWindowDPI(hwnd)
	v.initFonts()

	v.mu.Lock()
	v.hwnd = hwnd
	v.mu.Unlock()

	// Install scoped low-level keyboard hook so Win, Alt+Tab, and system shortcuts
	// go cleanly to the remote PC whenever this viewer window has foreground focus.
	v.installKeyboardHook()

	// Snapshot current clipboard and subscribe to native WM_CLIPBOARDUPDATE events (zero polling).
	v.clipMu.Lock()
	v.lastClipText = ReadClipboardText()
	v.clipMu.Unlock()
	addClipboardListener(hwnd)
	procDragAcceptFiles.Call(hwnd, 1)

	go v.decodeLoop(sessionID, fCh, fEnd)
	go v.inputLoop(inCh, mvCh, inDone)

	procShowWindow.Call(hwnd, swShowNormal)
	procUpdateWindow.Call(hwnd)
	procSetFocus.Call(hwnd)
}

// ApplyRemoteClipboard writes clipboard text received from the remote client without echoing it back.
func (v *remoteViewer) ApplyRemoteClipboard(text string) {
	if text == "" {
		return
	}
	v.clipMu.Lock()
	v.lastClipText = text
	v.clipMu.Unlock()
	_ = WriteClipboardText(text)
}

func (v *remoteViewer) onLocalClipboardChanged() {
	cur := ReadClipboardText()
	if cur == "" {
		return
	}
	v.clipMu.Lock()
	changed := cur != v.lastClipText
	if changed {
		v.lastClipText = cur
	}
	v.clipMu.Unlock()

	if changed && v.cb.OnClipboard != nil {
		v.cb.OnClipboard(cur)
	}
}

// ApplyRemoteCursor updates decoupled mouse cursor position and shape.
func (v *remoteViewer) ApplyRemoteCursor(pkt protocol.CursorPacket) {
	v.cursorMu.Lock()
	v.cursorX = pkt.X
	v.cursorY = pkt.Y
	v.cursorVisible = pkt.Visible
	v.cursorHotX = pkt.HotspotX
	v.cursorHotY = pkt.HotspotY
	v.cursorWidth = pkt.Width
	v.cursorHeight = pkt.Height

	if len(pkt.ShapeBGRA) > 0 {
		if v.cursorHIcon != 0 {
			procDestroyIcon.Call(v.cursorHIcon)
			v.cursorHIcon = 0
		}
		v.cursorHIcon = createHIconFromBGRA(pkt.Width, pkt.Height, pkt.HotspotX, pkt.HotspotY, pkt.ShapeBGRA)
	}
	hwnd := v.hwnd
	v.cursorMu.Unlock()

	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmViewerFrame, 0, 0)
	}
}

func createHIconFromBGRA(w, h, hotX, hotY uint16, bgra []byte) uintptr {
	if int(w) <= 0 || int(h) <= 0 || len(bgra) < int(w)*int(h)*4 {
		return 0
	}
	var bi bitmapInfo
	bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
	bi.Header.Width = int32(w)
	bi.Header.Height = -int32(h)
	bi.Header.Planes = 1
	bi.Header.BitCount = 32
	bi.Header.Compression = biRGB

	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return 0
	}
	defer procReleaseDC.Call(0, hdcScreen)

	var pBits uintptr
	hBmpColor, _, _ := procCreateDIBSection.Call(
		hdcScreen,
		uintptr(unsafe.Pointer(&bi)),
		dibRGBColors,
		uintptr(unsafe.Pointer(&pBits)),
		0, 0,
	)
	if hBmpColor == 0 || pBits == 0 {
		return 0
	}
	copy(unsafe.Slice((*byte)(ptrFromUintptr(pBits)), len(bgra)), bgra)

	hBmpMask, _, _ := procCreateBitmap.Call(uintptr(w), uintptr(h), 1, 1, 0)

	var ii iconInfo
	ii.FIcon = 0
	ii.XHotspot = uint32(hotX)
	ii.YHotspot = uint32(hotY)
	ii.HbmMask = hBmpMask
	ii.HbmColor = hBmpColor

	hIcon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	procDeleteObject.Call(hBmpColor)
	if hBmpMask != 0 {
		procDeleteObject.Call(hBmpMask)
	}
	return hIcon
}

// SetFileStatus displays a brief transfer status notice in the viewer toolbar.
func (v *remoteViewer) SetFileStatus(_ string, _ bool, detail string) {
	v.mu.Lock()
	v.statusMsg = detail
	v.statusUntil = time.Now().Add(6 * time.Second)
	hwnd := v.hwnd
	v.mu.Unlock()
	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmViewerFrame, 0, 0)
	}
}

// SetLatency updates the measured round-trip latency (ms) shown in the toolbar.
func (v *remoteViewer) SetLatency(rttMs int) {
	v.mu.Lock()
	v.rttMs = rttMs
	hwnd := v.hwnd
	v.mu.Unlock()
	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmViewerFrame, 0, 0)
	}
}

// CloseUI destroys the viewer window, unhooks keyboard capture, and frees GDI buffers on the UI thread.
func (v *remoteViewer) CloseUI() {
	v.uninstallKeyboardHook()
	v.releaseAllHeldKeys()

	v.mu.Lock()
	hwnd := v.hwnd
	endCh := v.decoderEnd
	inDone := v.inputDone
	v.decoderEnd = nil
	v.inputDone = nil
	v.sessionID = ""
	v.hwnd = 0
	v.mu.Unlock()

	if endCh != nil {
		close(endCh)
	}
	if inDone != nil {
		close(inDone)
	}
	if hwnd != 0 {
		removeClipboardListener(hwnd)
		procDestroyWindow.Call(hwnd)
	}
	v.cursorMu.Lock()
	if v.cursorHIcon != 0 {
		procDestroyIcon.Call(v.cursorHIcon)
		v.cursorHIcon = 0
	}
	v.cursorMu.Unlock()
	v.releaseBackbuffer()
	v.destroyFonts()
}

// PushFrame accepts an incoming binary MsgFrame from the Hub goroutine.
func (v *remoteViewer) PushFrame(sessionID string, frame []byte) {
	v.mu.Lock()
	if v.sessionID != sessionID || v.frameCh == nil {
		v.mu.Unlock()
		return
	}
	ch := v.frameCh
	v.mu.Unlock()

	cp := make([]byte, len(frame))
	copy(cp, frame)

	// Keep the freshest frame if the decoder is momentarily busy.
	if len(ch) == cap(ch) {
		select {
		case <-ch:
		default:
		}
	}
	select {
	case ch <- cp:
	default:
	}
}

func (v *remoteViewer) scale(px int32) int32 {
	return (px * v.dpi) / 96
}

func (v *remoteViewer) initFonts() {
	v.destroyFonts()
	face, _ := syscall.UTF16PtrFromString("Segoe UI")
	mono, _ := syscall.UTF16PtrFromString("Consolas")
	v.fontBold = createFont(v.scale(15), 600, face)
	v.fontBody = createFont(v.scale(14), 400, face)
	v.fontMono = createFont(v.scale(13), 400, mono)
	v.fontBtn = createFont(v.scale(13), 600, face)
}

func (v *remoteViewer) destroyFonts() {
	for _, f := range []*uintptr{&v.fontBold, &v.fontBody, &v.fontMono, &v.fontBtn} {
		if *f != 0 {
			procDeleteObject.Call(*f)
			*f = 0
		}
	}
}

func (v *remoteViewer) toggleFullscreen(hwnd uintptr) {
	if !v.fullscreen {
		v.prevStyle = getWindowStyle(hwnd)
		v.prevPlace = windowPlacement{Length: uint32(unsafe.Sizeof(windowPlacement{}))}
		procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&v.prevPlace)))

		mon, _, _ := procMonitorFromWindow.Call(hwnd, monitorDefaultNear)
		var mi monitorInfo
		mi.CbSize = uint32(unsafe.Sizeof(mi))
		if ok, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); ok != 0 {
			v.fullscreen = true
			setWindowStyle(hwnd, v.prevStyle&^wsOverlappedWindow)
			m := mi.RcMonitor
			procSetWindowPos.Call(
				hwnd, 0,
				uintptr(m.Left), uintptr(m.Top),
				uintptr(m.Right-m.Left), uintptr(m.Bottom-m.Top),
				swpNoOwnerZOrder|swpFrameChanged,
			)
		}
	} else {
		v.fullscreen = false
		v.toolbarHidden = false
		setWindowStyle(hwnd, v.prevStyle)
		procSetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&v.prevPlace)))
		procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoOwnerZOrder|swpFrameChanged)
	}
	v.hoverBtn = 0
	procInvalidateRect.Call(hwnd, 0, 0)
}

func getWindowStyle(hwnd uintptr) uintptr {
	r, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlStyle)
	return r
}

func setWindowStyle(hwnd uintptr, style uintptr) {
	procSetWindowLongPtrW.Call(hwnd, gwlStyle, style)
}

func viewerWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	activeViewerMu.Lock()
	v := activeViewer
	activeViewerMu.Unlock()

	if v == nil {
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
			mmi.PtMinTrackSize.X = v.scale(680)
			mmi.PtMinTrackSize.Y = v.scale(420)
		}
		return 0

	case wmDPIChanged:
		newDPI := int32(wParam & 0xFFFF)
		if newDPI >= 72 && newDPI <= 480 && newDPI != v.dpi {
			v.dpi = newDPI
			v.initFonts()
			if lParam != 0 && !v.fullscreen {
				ptr := *(*unsafe.Pointer)(unsafe.Pointer(&lParam))
				sug := (*rect)(ptr)
				procSetWindowPos.Call(hwnd, 0,
					uintptr(sug.Left), uintptr(sug.Top),
					uintptr(sug.Right-sug.Left), uintptr(sug.Bottom-sug.Top),
					0x0004|0x0010,
				)
			}
			procInvalidateRect.Call(hwnd, 0, 0)
		}
		return 0

	case wmSize, wmViewerFrame:
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0

	case wmViewerToggleFull:
		v.releaseAllHeldKeys()
		v.toggleFullscreen(hwnd)
		return 0

	case wmViewerToggleMenu:
		v.releaseAllHeldKeys()
		v.toolbarHidden = !v.toolbarHidden
		v.hoverBtn = 0
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0

	case wmViewerDisconnect:
		v.releaseAllHeldKeys()
		if v.cb.OnDisconnect != nil {
			go v.cb.OnDisconnect()
		}
		v.CloseUI()
		return 0

	case wmPaint:
		v.paint(hwnd)
		return 0

	case wmSetCursor:
		if (lParam & 0xFFFF) == 1 {
			if v.hoverBtn != 0 {
				procSetCursor.Call(v.cursorHnd)
			} else {
				procSetCursor.Call(v.cursorArr)
			}
			return 1
		}

	case wmMouseMove:
		x, y := unpackXY(lParam)
		// In fullscreen with menu collapsed, touching the very top-center edge reveals the menu automatically.
		if v.fullscreen && v.toolbarHidden && v.mouseBtns == 0 && y <= 1 && v.pillBox.Right > v.pillBox.Left {
			if x >= v.pillBox.Left-v.scale(40) && x <= v.pillBox.Right+v.scale(40) {
				v.toolbarHidden = false
				v.hoverBtn = 0
				procInvalidateRect.Call(hwnd, 0, 0)
				return 0
			}
		}

		hb := v.hitTestToolbar(x, y)
		if hb != v.hoverBtn {
			v.hoverBtn = hb
			procInvalidateRect.Call(hwnd, 0, 0)
		}
		if hb == 0 && !v.isInCollapsedPill(x, y) {
			now := time.Now()
			if now.Sub(v.lastMoveAt) >= 8*time.Millisecond {
				if nx, ny, ok := v.mapMouseToNormalized(x, y); ok {
					v.lastMoveAt = now
					v.enqueueMouseMove(protocol.MouseEvent{
						Action: protocol.MouseMove,
						X:      nx,
						Y:      ny,
					})
				}
			}
		}
		return 0

	case wmLButtonDown:
		x, y := unpackXY(lParam)
		procSetFocus.Call(hwnd)
		if hb := v.hitTestToolbar(x, y); hb != 0 {
			v.handleToolbarClick(hwnd, hb)
			return 0
		}
		if v.isInCollapsedPill(x, y) {
			return 0
		}
		v.handleMouseDown(hwnd, protocol.ButtonLeft, x, y)
		return 0

	case wmLButtonUp:
		v.handleMouseUp(protocol.ButtonLeft, lParam)
		return 0

	case wmRButtonDown:
		x, y := unpackXY(lParam)
		procSetFocus.Call(hwnd)
		v.handleMouseDown(hwnd, protocol.ButtonRight, x, y)
		return 0

	case wmRButtonUp:
		v.handleMouseUp(protocol.ButtonRight, lParam)
		return 0

	case wmMButtonDown:
		x, y := unpackXY(lParam)
		procSetFocus.Call(hwnd)
		v.handleMouseDown(hwnd, protocol.ButtonMiddle, x, y)
		return 0

	case wmMButtonUp:
		v.handleMouseUp(protocol.ButtonMiddle, lParam)
		return 0

	case wmXButtonDown:
		x, y := unpackXY(lParam)
		procSetFocus.Call(hwnd)
		v.handleMouseDown(hwnd, xButtonFromWParam(wParam), x, y)
		return 1

	case wmXButtonUp:
		v.handleMouseUp(xButtonFromWParam(wParam), lParam)
		return 1

	case wmMouseWheel, wmMouseHWheel:
		action := uint8(protocol.MouseWheel)
		if msg == wmMouseHWheel {
			action = protocol.MouseHWheel
		}
		delta := int16(uint16((wParam >> 16) & 0xFFFF))
		v.enqueueOrderedInput(queuedInput{mouse: protocol.MouseEvent{Action: action, Delta: delta}})
		return 0

	case wmClipboardUpdate:
		go v.onLocalClipboardChanged()
		return 0

	case wmKeyDown, wmSysKeyDown:
		// Fallback when low-level keyboard hook is not active.
		if v.kbHook == 0 {
			v.handleKeyMessage(true, wParam, lParam)
		}
		return 0

	case wmKeyUp, wmSysKeyUp:
		if v.kbHook == 0 {
			v.handleKeyMessage(false, wParam, lParam)
		}
		return 0

	case wmKillFocus:
		v.releaseAllHeldKeys()
		return 0

	case wmClose:
		if v.cb.OnDisconnect != nil {
			go v.cb.OnDisconnect()
		}
		v.CloseUI()
		return 0

	case wmDestroy:
		v.releaseBackbuffer()
		v.destroyFonts()
		return 0
	}

	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}
