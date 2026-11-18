//go:build windows

package win

import (
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"remote-control/internal/protocol"
)

var (
	procSetStretchBltMode  = gdi32GUI.NewProc("SetStretchBltMode")
	procSetBrushOrgEx      = gdi32GUI.NewProc("SetBrushOrgEx")
	procStretchDIBits      = gdi32GUI.NewProc("StretchDIBits")
	procGetWindowLongPtrW  = user32GUI.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW  = user32GUI.NewProc("SetWindowLongPtrW")
	procGetWindowPlacement = user32GUI.NewProc("GetWindowPlacement")
	procSetWindowPlacement = user32GUI.NewProc("SetWindowPlacement")
	procMonitorFromWindow  = user32GUI.NewProc("MonitorFromWindow")
	procGetMonitorInfoW    = user32GUI.NewProc("GetMonitorInfoW")
	procSetCapture         = user32GUI.NewProc("SetCapture")
	procReleaseCapture     = user32GUI.NewProc("ReleaseCapture")
	procSetFocus           = user32GUI.NewProc("SetFocus")
	procSetCursor          = user32GUI.NewProc("SetCursor")
	procGetWindowLongW     = user32GUI.NewProc("GetWindowLongW")
	procSetWindowLongW     = user32GUI.NewProc("SetWindowLongW")
	procGetForegroundWin   = user32GUI.NewProc("GetForegroundWindow")
	procGetAsyncKeyState   = user32GUI.NewProc("GetAsyncKeyState")
	procSetWindowsHookExW  = user32GUI.NewProc("SetWindowsHookExW")
	procUnhookWindowsHook  = user32GUI.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx     = user32GUI.NewProc("CallNextHookEx")
)

const (
	wsOverlappedWindow = 0x00CF0000

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
	wmViewerClose      = 0x8021
	wmViewerToggleFull = 0x8022
	wmViewerToggleMenu = 0x8023
	wmViewerDisconnect = 0x8024

	colorOnColorMode   = 3
	halftoneMode       = 4
	monitorDefaultNear = 2
	gwlStyle           = ^uintptr(15) // -16
	swpFrameChanged    = 0x0020
	swpNoOwnerZOrder   = 0x0200

	idcHand = 32649

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

	qualLowVal = 55
	qualMedVal = 78
	qualHiVal  = 92
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

type kbdLLHookStruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type viewerToolbarBtn struct {
	id    int
	label string
	rc    rect
}

type queuedInput struct {
	isKey bool
	mouse protocol.MouseEvent
	key   protocol.KeyEvent
}

const (
	vbtnQualLow   = 1
	vbtnQualMed   = 2
	vbtnQualHi    = 3
	vbtnFull      = 4
	vbtnDisc      = 5
	vbtnHideBar   = 6
	vbtnShowBar   = 7
	vbtnMonAll    = 100
	vbtnMonBase   = 200 // 200 + monitorIndex (0, 1, 2, ...)
)

// ViewerCallbacks wires the native remote desktop viewer back to the Hub.
type ViewerCallbacks struct {
	OnMouse         func(ev protocol.MouseEvent)
	OnKey           func(ev protocol.KeyEvent)
	OnFrameAck      func(frameID uint32, sentUnixNano int64)
	OnQuality       func(quality int)
	OnSelectMonitor func(monitorIndex int)
	OnDisconnect    func()
}

type remoteViewer struct {
	cb        ViewerCallbacks
	fb        *FrameBuffer
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

	frameCh    chan []byte
	decoderEnd chan struct{}

	// Non-blocking coalescing input pipeline
	inputCh      chan queuedInput
	inputWake    chan struct{}
	inputDone    chan struct{}
	moveMu       sync.Mutex
	pendingMove  protocol.MouseEvent
	hasPendMove  bool

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
	prevStyle   uintptr
	prevPlace   windowPlacement
	lastMoveAt  time.Time
	heldKeys    map[uint16]uint8
	mouseBtns   int
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
		fb:        NewFrameBuffer(),
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
	v.frameCh = make(chan []byte, 3)
	v.decoderEnd = make(chan struct{})
	v.inputCh = make(chan queuedInput, 256)
	v.inputWake = make(chan struct{}, 1)
	v.inputDone = make(chan struct{})
	fCh := v.frameCh
	fEnd := v.decoderEnd
	inCh := v.inputCh
	inWake := v.inputWake
	inDone := v.inputDone
	v.mu.Unlock()

	v.fb.Reset(remoteW, remoteH)
	v.fullscreen = false
	v.toolbarHidden = false
	v.hoverBtn = 0
	clear(v.heldKeys)
	v.mouseBtns = 0
	v.moveMu.Lock()
	v.hasPendMove = false
	v.moveMu.Unlock()

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

	title, _ := syscall.UTF16PtrFromString("Control Remoto — " + hostname)
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

	go v.decodeLoop(sessionID, fCh, fEnd)
	go v.inputLoop(inCh, inWake, inDone)

	procShowWindow.Call(hwnd, swShowNormal)
	procUpdateWindow.Call(hwnd)
	procSetFocus.Call(hwnd)
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
		procDestroyWindow.Call(hwnd)
	}
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

	select {
	case ch <- cp:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- cp:
		default:
		}
	}
}

func (v *remoteViewer) decodeLoop(sessionID string, ch <-chan []byte, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case data := <-ch:
			meta, err := v.fb.ApplyFrame(data)
			if err != nil {
				continue
			}

			// Acknowledge immediately so the client's 2-frame sliding window advances with minimal latency.
			if v.cb.OnFrameAck != nil && meta.FrameID > 0 {
				v.cb.OnFrameAck(meta.FrameID, meta.SentUnixNano)
			}

			v.mu.Lock()
			if v.sessionID != sessionID {
				v.mu.Unlock()
				return
			}
			v.remoteW = meta.Width
			v.remoteH = meta.Height
			v.fpsCount++
			now := time.Now()
			if elapsed := now.Sub(v.fpsTick); elapsed >= time.Second {
				v.fps = int(float64(v.fpsCount)/elapsed.Seconds() + 0.5)
				v.fpsCount = 0
				v.fpsTick = now
			}
			hwnd := v.hwnd
			v.mu.Unlock()

			if hwnd != 0 {
				procPostMessageW.Call(hwnd, wmViewerFrame, 0, 0)
			}
		}
	}
}

func (v *remoteViewer) inputLoop(inCh <-chan queuedInput, wake <-chan struct{}, done <-chan struct{}) {
	for {
		// Prioritize ordered click/wheel/key events, then flush latest coalesced mouse position.
		select {
		case <-done:
			return
		case item := <-inCh:
			v.dispatchInput(item)
			continue
		default:
		}

		if mv, ok := v.takePendingMove(); ok {
			if v.cb.OnMouse != nil {
				v.cb.OnMouse(mv)
			}
			continue
		}

		select {
		case <-done:
			return
		case item := <-inCh:
			v.dispatchInput(item)
		case <-wake:
		}
	}
}

func (v *remoteViewer) dispatchInput(item queuedInput) {
	if item.isKey {
		if v.cb.OnKey != nil {
			v.cb.OnKey(item.key)
		}
	} else {
		if v.cb.OnMouse != nil {
			v.cb.OnMouse(item.mouse)
		}
	}
}

func (v *remoteViewer) enqueueMouseMove(ev protocol.MouseEvent) {
	v.moveMu.Lock()
	v.pendingMove = ev
	v.hasPendMove = true
	v.moveMu.Unlock()

	v.mu.Lock()
	wake := v.inputWake
	v.mu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (v *remoteViewer) takePendingMove() (protocol.MouseEvent, bool) {
	v.moveMu.Lock()
	defer v.moveMu.Unlock()
	if !v.hasPendMove {
		return protocol.MouseEvent{}, false
	}
	v.hasPendMove = false
	return v.pendingMove, true
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

func (v *remoteViewer) ensureBackbuffer(hdc uintptr, w, h int32) uintptr {
	if v.backDC != 0 && v.backBmp != 0 && v.backW == w && v.backH == h {
		return v.backDC
	}
	v.releaseBackbuffer()
	memDC, _, _ := procCreateCompatibleDC.Call(hdc)
	memBmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(w), uintptr(h))
	oldBmp, _, _ := procSelectObject.Call(memDC, memBmp)
	v.backDC = memDC
	v.backBmp = memBmp
	v.backOld = oldBmp
	v.backW = w
	v.backH = h
	return v.backDC
}

func (v *remoteViewer) releaseBackbuffer() {
	if v.backDC != 0 && v.backOld != 0 {
		procSelectObject.Call(v.backDC, v.backOld)
		v.backOld = 0
	}
	if v.backBmp != 0 {
		procDeleteObject.Call(v.backBmp)
		v.backBmp = 0
	}
	if v.backDC != 0 {
		procDeleteDC.Call(v.backDC)
		v.backDC = 0
	}
	v.backW = 0
	v.backH = 0
}

func (v *remoteViewer) layoutToolbar(width int32) int32 {
	v.toolbarDividers = v.toolbarDividers[:0]
	v.pillBox = rect{}

	if v.toolbarHidden {
		pillH := v.scale(28)
		padX := v.scale(4)
		padY := v.scale(3)
		gap := v.scale(4)
		btnH := pillH - padY*2

		showW := v.scale(88)
		discW := v.scale(30)
		fullW := v.scale(104)

		totalW := padX*2 + showW + gap + discW
		if v.fullscreen {
			totalW += gap + fullW
		}
		px := (width - totalW) / 2
		v.pillBox = rect{px, 0, px + totalW, pillH}

		x := px + padX
		btns := make([]viewerToolbarBtn, 0, 3)
		btns = append(btns, viewerToolbarBtn{
			id:    vbtnShowBar,
			label: "▼ Menú",
			rc:    rect{x, padY, x + showW, padY + btnH},
		})
		x += showW + gap

		if v.fullscreen {
			btns = append(btns, viewerToolbarBtn{
				id:    vbtnFull,
				label: "Salir pantalla",
				rc:    rect{x, padY, x + fullW, padY + btnH},
			})
			x += fullW + gap
		}

		btns = append(btns, viewerToolbarBtn{
			id:    vbtnDisc,
			label: "×",
			rc:    rect{x, padY, x + discW, padY + btnH},
		})

		v.toolbarBtns = btns
		return 0
	}

	barH := v.scale(44)
	pad := v.scale(12)
	btnH := v.scale(28)
	btnY := (barH - btnH) / 2
	gap := v.scale(6)
	smallGap := v.scale(4)

	hideW := v.scale(30)
	discW := v.scale(94)
	fullW := v.scale(122)
	qHiW := v.scale(44)
	qMedW := v.scale(68)
	qLowW := v.scale(44)

	x := width - pad

	rcHide := rect{x - hideW, btnY, x, btnY + btnH}
	x = rcHide.Left - gap

	rcDisc := rect{x - discW, btnY, x, btnY + btnH}
	x = rcDisc.Left - gap

	fullLabel := "Pantalla completa"
	if v.fullscreen {
		fullLabel = "Salir pantalla"
	}
	rcFull := rect{x - fullW, btnY, x, btnY + btnH}
	x = rcFull.Left - v.scale(8)
	v.toolbarDividers = append(v.toolbarDividers, x)
	x -= v.scale(8)

	rcHi := rect{x - qHiW, btnY, x, btnY + btnH}
	x = rcHi.Left - smallGap
	rcMed := rect{x - qMedW, btnY, x, btnY + btnH}
	x = rcMed.Left - smallGap
	rcLow := rect{x - qLowW, btnY, x, btnY + btnH}
	x = rcLow.Left

	btns := make([]viewerToolbarBtn, 0, 14)

	v.mu.Lock()
	monCount := v.monitorCount
	v.mu.Unlock()
	if monCount > 1 {
		if monCount > 6 {
			monCount = 6
		}
		x -= v.scale(8)
		v.toolbarDividers = append(v.toolbarDividers, x)
		x -= v.scale(8)

		allW := v.scale(54)
		rcAll := rect{x - allW, btnY, x, btnY + btnH}
		x = rcAll.Left - smallGap

		monBtns := make([]viewerToolbarBtn, 0, monCount+1)
		monW := v.scale(36)
		for i := monCount - 1; i >= 0; i-- {
			rcM := rect{x - monW, btnY, x, btnY + btnH}
			monBtns = append(monBtns, viewerToolbarBtn{
				id:    vbtnMonBase + i,
				label: fmt.Sprintf("M%d", i+1),
				rc:    rcM,
			})
			x = rcM.Left - smallGap
		}
		for i := len(monBtns) - 1; i >= 0; i-- {
			btns = append(btns, monBtns[i])
		}
		btns = append(btns, viewerToolbarBtn{id: vbtnMonAll, label: "Todos", rc: rcAll})
	}

	btns = append(btns,
		viewerToolbarBtn{id: vbtnQualLow, label: "Baja", rc: rcLow},
		viewerToolbarBtn{id: vbtnQualMed, label: "Estándar", rc: rcMed},
		viewerToolbarBtn{id: vbtnQualHi, label: "Alta", rc: rcHi},
		viewerToolbarBtn{id: vbtnFull, label: fullLabel, rc: rcFull},
		viewerToolbarBtn{id: vbtnDisc, label: "Desconectar", rc: rcDisc},
		viewerToolbarBtn{id: vbtnHideBar, label: "▲", rc: rcHide},
	)
	v.toolbarBtns = btns
	return barH
}

func (v *remoteViewer) paint(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	var cr rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr)))
	cw := cr.Right - cr.Left
	ch := cr.Bottom - cr.Top
	if cw <= 0 || ch <= 0 {
		return
	}

	memDC := v.ensureBackbuffer(hdc, cw, ch)
	barH := v.layoutToolbar(cw)

	v.mu.Lock()
	hostname := v.hostname
	rw, rh := v.remoteW, v.remoteH
	fps := v.fps
	rtt := v.rttMs
	qual := v.quality
	activeMon := v.activeMonitor
	v.mu.Unlock()

	procSetBkMode.Call(memDC, transparentBk)
	oldFont, _, _ := procSelectObject.Call(memDC, v.fontBold)

	// 1. Toolbar (only painted when visible)
	if !v.toolbarHidden && barH > 0 {
		fillSolidRect(memDC, rect{0, 0, cw, barH}, rgb(255, 255, 255))
		drawLine(memDC, 0, barH-1, cw, barH-1, rgb(228, 228, 231))

		pad := v.scale(14)
		dotSz := v.scale(9)
		drawStatusDot(memDC, pad, (barH-dotSz)/2, dotSz, rgb(22, 163, 74))

		procSetTextColor.Call(memDC, rgb(24, 24, 27))
		leftLimit := v.toolbarBtns[0].rc.Left - v.scale(12)
		hostRC := rect{pad + dotSz + v.scale(9), 0, leftLimit, barH}
		drawText(memDC, hostname, &hostRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

		statsText := "Conectando vídeo..."
		if rw > 0 && rh > 0 {
			if rtt >= 0 {
				statsText = fmt.Sprintf("%d×%d · %d fps · %d ms", rw, rh, fps, rtt)
			} else {
				statsText = fmt.Sprintf("%d×%d · %d fps", rw, rh, fps)
			}
		}
		statsStart := pad + dotSz + v.scale(9) + int32(len(hostname))*v.scale(9) + v.scale(14)
		if statsStart < leftLimit-v.scale(90) {
			procSelectObject.Call(memDC, v.fontMono)
			procSetTextColor.Call(memDC, rgb(113, 113, 122))
			statsRC := rect{statsStart, 0, leftLimit, barH}
			drawText(memDC, statsText, &statsRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)
		}

		// Subtle vertical section dividers
		for _, dx := range v.toolbarDividers {
			drawLine(memDC, dx, v.scale(11), dx, barH-v.scale(11), rgb(228, 228, 231))
		}

		// Toolbar buttons
		procSelectObject.Call(memDC, v.fontBtn)
		for _, b := range v.toolbarBtns {
			hovered := v.hoverBtn == b.id
			active := (b.id == vbtnQualLow && qual == qualLowVal) ||
				(b.id == vbtnQualMed && qual == qualMedVal) ||
				(b.id == vbtnQualHi && qual == qualHiVal) ||
				(b.id == vbtnMonAll && activeMon == -1) ||
				(b.id >= vbtnMonBase && activeMon == b.id-vbtnMonBase)

			bg := rgb(255, 255, 255)
			border := rgb(212, 212, 216)
			fg := rgb(24, 24, 27)

			switch {
			case b.id == vbtnDisc:
				border = rgb(254, 202, 202)
				fg = rgb(185, 28, 28)
				if hovered {
					bg = rgb(254, 242, 242)
				}
			case b.id == vbtnHideBar:
				fg = rgb(82, 82, 91)
				if hovered {
					bg = rgb(244, 244, 245)
					fg = rgb(24, 24, 27)
				}
			case active:
				bg = rgb(24, 24, 27)
				border = rgb(24, 24, 27)
				fg = rgb(255, 255, 255)
			case hovered:
				bg = rgb(244, 244, 245)
			}
			drawFlatButton(memDC, b.rc, v.scale(5), bg, border, fg, b.label)
		}
	}

	// 2. Viewport area (dark neutral #141518).
	fillSolidRect(memDC, rect{0, barH, cw, ch}, rgb(20, 21, 24))

	stageW := cw
	stageH := ch - barH
	v.imgRect = rect{}

	v.fb.WithReadLock(func(srcW, srcH int, bgra []byte) {
		if srcW <= 0 || srcH <= 0 || len(bgra) < srcW*srcH*4 || stageW <= 0 || stageH <= 0 {
			procSelectObject.Call(memDC, v.fontBody)
			procSetTextColor.Call(memDC, rgb(161, 161, 170))
			msgRC := rect{0, barH, cw, ch}
			drawText(memDC, "Cargando pantalla de "+hostname+"...", &msgRC, dtCenter|dtSingleLn|dtVCenter)
			return
		}

		dstW := stageW
		dstH := int32(int64(stageW) * int64(srcH) / int64(srcW))
		if dstH > stageH {
			dstH = stageH
			dstW = int32(int64(stageH) * int64(srcW) / int64(srcH))
		}
		dstX := (stageW - dstW) / 2
		dstY := barH + (stageH-dstH)/2
		v.imgRect = rect{dstX, dstY, dstX + dstW, dstY + dstH}

		var bi bitmapInfo
		bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
		bi.Header.Width = int32(srcW)
		bi.Header.Height = -int32(srcH) // top-down DIB
		bi.Header.Planes = 1
		bi.Header.BitCount = 32
		bi.Header.Compression = biRGB

		// Pixel-exact 1:1 blit when unscaled, crisp HALFTONE when scaled.
		if int(dstW) == srcW && int(dstH) == srcH {
			procSetStretchBltMode.Call(memDC, colorOnColorMode)
		} else {
			procSetStretchBltMode.Call(memDC, halftoneMode)
			procSetBrushOrgEx.Call(memDC, 0, 0, 0)
		}
		procStretchDIBits.Call(
			memDC,
			uintptr(dstX), uintptr(dstY), uintptr(dstW), uintptr(dstH),
			0, 0, uintptr(srcW), uintptr(srcH),
			uintptr(unsafe.Pointer(&bgra[0])),
			uintptr(unsafe.Pointer(&bi)),
			dibRGBColors,
			srccopy,
		)
	})

	// 3. When toolbar is hidden, draw the compact top-center floating control capsule on top of the viewport.
	if v.toolbarHidden && v.pillBox.Right > v.pillBox.Left {
		// Slightly raise top edge so only the bottom corners are rounded like a crisp pull-tab.
		capRC := rect{v.pillBox.Left, v.pillBox.Top - v.scale(6), v.pillBox.Right, v.pillBox.Bottom}
		drawRoundedCard(memDC, capRC, v.scale(8), rgb(24, 24, 27), rgb(63, 63, 70))

		procSelectObject.Call(memDC, v.fontBtn)
		for _, b := range v.toolbarBtns {
			hovered := v.hoverBtn == b.id
			bg := rgb(24, 24, 27)
			border := rgb(24, 24, 27)
			fg := rgb(244, 244, 245)

			switch {
			case b.id == vbtnDisc:
				fg = rgb(248, 113, 113)
				if hovered {
					bg = rgb(127, 29, 29)
					border = rgb(153, 27, 27)
					fg = rgb(255, 255, 255)
				}
			case hovered:
				bg = rgb(63, 63, 70)
				border = rgb(82, 82, 91)
			}
			drawFlatButton(memDC, b.rc, v.scale(5), bg, border, fg, b.label)
		}
	}

	procSelectObject.Call(memDC, oldFont)
	procBitBlt.Call(hdc, 0, 0, uintptr(cw), uintptr(ch), memDC, 0, 0, srccopy)
}

func (v *remoteViewer) hitTestToolbar(x, y int32) int {
	for _, b := range v.toolbarBtns {
		if x >= b.rc.Left && x <= b.rc.Right && y >= b.rc.Top && y <= b.rc.Bottom {
			return b.id
		}
	}
	return 0
}

func (v *remoteViewer) isInCollapsedPill(x, y int32) bool {
	if !v.toolbarHidden {
		return false
	}
	p := v.pillBox
	return p.Right > p.Left && x >= p.Left && x <= p.Right && y >= p.Top && y <= p.Bottom
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

func (v *remoteViewer) handleToolbarClick(hwnd uintptr, btnID int) {
	switch {
	case btnID == vbtnQualLow:
		v.setQuality(hwnd, qualLowVal)
	case btnID == vbtnQualMed:
		v.setQuality(hwnd, qualMedVal)
	case btnID == vbtnQualHi:
		v.setQuality(hwnd, qualHiVal)
	case btnID == vbtnMonAll:
		v.selectMonitor(hwnd, -1)
	case btnID >= vbtnMonBase:
		v.selectMonitor(hwnd, btnID-vbtnMonBase)
	case btnID == vbtnFull:
		v.toggleFullscreen(hwnd)
	case btnID == vbtnHideBar:
		v.toolbarHidden = true
		v.hoverBtn = 0
		procInvalidateRect.Call(hwnd, 0, 0)
	case btnID == vbtnShowBar:
		v.toolbarHidden = false
		v.hoverBtn = 0
		procInvalidateRect.Call(hwnd, 0, 0)
	case btnID == vbtnDisc:
		if v.cb.OnDisconnect != nil {
			go v.cb.OnDisconnect()
		}
		v.CloseUI()
	}
}

func (v *remoteViewer) setQuality(hwnd uintptr, q int) {
	v.mu.Lock()
	v.quality = q
	v.mu.Unlock()
	if v.cb.OnQuality != nil {
		go v.cb.OnQuality(q)
	}
	procInvalidateRect.Call(hwnd, 0, 0)
}

func (v *remoteViewer) selectMonitor(hwnd uintptr, monitorIndex int) {
	v.mu.Lock()
	if v.activeMonitor == monitorIndex {
		v.mu.Unlock()
		return
	}
	v.activeMonitor = monitorIndex
	v.mu.Unlock()

	if v.cb.OnSelectMonitor != nil {
		go v.cb.OnSelectMonitor(monitorIndex)
	}
	procInvalidateRect.Call(hwnd, 0, 0)
}

func isAsyncKeyDown(vk uintptr) bool {
	r, _, _ := procGetAsyncKeyState.Call(vk)
	return (r & 0x8000) != 0
}

// viewerKeyboardLLProc captures system keys (Win, Alt+Tab, Ctrl+Esc, PrintScreen, etc.)
// strictly while the remote viewer window is the active foreground window.
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
		if nx, ny, ok := v.mapMouseToNormalized(x, y); ok {
			v.mouseBtns++
			procSetCapture.Call(hwnd)
			v.enqueueOrderedInput(queuedInput{mouse: protocol.MouseEvent{Action: protocol.MouseDown, Button: protocol.ButtonLeft, X: nx, Y: ny}})
		}
		return 0

	case wmLButtonUp:
		x, y := unpackXY(lParam)
		if v.mouseBtns > 0 {
			v.mouseBtns--
			if v.mouseBtns == 0 {
				procReleaseCapture.Call()
			}
		}
		if nx, ny, ok := v.mapMouseToNormalized(x, y); ok {
			v.enqueueOrderedInput(queuedInput{mouse: protocol.MouseEvent{Action: protocol.MouseUp, Button: protocol.ButtonLeft, X: nx, Y: ny}})
		}
		return 0

	case wmRButtonDown:
		x, y := unpackXY(lParam)
		procSetFocus.Call(hwnd)
		if nx, ny, ok := v.mapMouseToNormalized(x, y); ok {
			v.mouseBtns++
			procSetCapture.Call(hwnd)
			v.enqueueOrderedInput(queuedInput{mouse: protocol.MouseEvent{Action: protocol.MouseDown, Button: protocol.ButtonRight, X: nx, Y: ny}})
		}
		return 0

	case wmRButtonUp:
		x, y := unpackXY(lParam)
		if v.mouseBtns > 0 {
			v.mouseBtns--
			if v.mouseBtns == 0 {
				procReleaseCapture.Call()
			}
		}
		if nx, ny, ok := v.mapMouseToNormalized(x, y); ok {
			v.enqueueOrderedInput(queuedInput{mouse: protocol.MouseEvent{Action: protocol.MouseUp, Button: protocol.ButtonRight, X: nx, Y: ny}})
		}
		return 0

	case wmMButtonDown:
		x, y := unpackXY(lParam)
		procSetFocus.Call(hwnd)
		if nx, ny, ok := v.mapMouseToNormalized(x, y); ok {
			v.mouseBtns++
			procSetCapture.Call(hwnd)
			v.enqueueOrderedInput(queuedInput{mouse: protocol.MouseEvent{Action: protocol.MouseDown, Button: protocol.ButtonMiddle, X: nx, Y: ny}})
		}
		return 0

	case wmMButtonUp:
		x, y := unpackXY(lParam)
		if v.mouseBtns > 0 {
			v.mouseBtns--
			if v.mouseBtns == 0 {
				procReleaseCapture.Call()
			}
		}
		if nx, ny, ok := v.mapMouseToNormalized(x, y); ok {
			v.enqueueOrderedInput(queuedInput{mouse: protocol.MouseEvent{Action: protocol.MouseUp, Button: protocol.ButtonMiddle, X: nx, Y: ny}})
		}
		return 0

	case wmXButtonDown:
		x, y := unpackXY(lParam)
		procSetFocus.Call(hwnd)
		btn := uint8(protocol.ButtonX1)
		if ((wParam >> 16) & 0xFFFF) == xButton2 {
			btn = protocol.ButtonX2
		}
		if nx, ny, ok := v.mapMouseToNormalized(x, y); ok {
			v.mouseBtns++
			procSetCapture.Call(hwnd)
			v.enqueueOrderedInput(queuedInput{mouse: protocol.MouseEvent{Action: protocol.MouseDown, Button: btn, X: nx, Y: ny}})
		}
		return 1

	case wmXButtonUp:
		x, y := unpackXY(lParam)
		if v.mouseBtns > 0 {
			v.mouseBtns--
			if v.mouseBtns == 0 {
				procReleaseCapture.Call()
			}
		}
		btn := uint8(protocol.ButtonX1)
		if ((wParam >> 16) & 0xFFFF) == xButton2 {
			btn = protocol.ButtonX2
		}
		if nx, ny, ok := v.mapMouseToNormalized(x, y); ok {
			v.enqueueOrderedInput(queuedInput{mouse: protocol.MouseEvent{Action: protocol.MouseUp, Button: btn, X: nx, Y: ny}})
		}
		return 1

	case wmMouseWheel:
		delta := int16(uint16((wParam >> 16) & 0xFFFF))
		v.enqueueOrderedInput(queuedInput{mouse: protocol.MouseEvent{Action: protocol.MouseWheel, Delta: delta}})
		return 0

	case wmMouseHWheel:
		delta := int16(uint16((wParam >> 16) & 0xFFFF))
		v.enqueueOrderedInput(queuedInput{mouse: protocol.MouseEvent{Action: protocol.MouseHWheel, Delta: delta}})
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

	case wmViewerClose:
		v.CloseUI()
		return 0

	case wmClose:
		if v.cb.OnDisconnect != nil {
			go v.cb.OnDisconnect()
		}
		v.CloseUI()
		return 0
	}

	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func unpackXY(lParam uintptr) (int32, int32) {
	x := int32(int16(lParam & 0xFFFF))
	y := int32(int16((lParam >> 16) & 0xFFFF))
	return x, y
}

func getWindowStyle(hwnd uintptr) uintptr {
	if err := procGetWindowLongPtrW.Find(); err == nil {
		r, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlStyle)
		return r
	}
	r, _, _ := procGetWindowLongW.Call(hwnd, gwlStyle)
	return r
}

func setWindowStyle(hwnd uintptr, style uintptr) {
	if err := procSetWindowLongPtrW.Find(); err == nil {
		procSetWindowLongPtrW.Call(hwnd, gwlStyle, style)
		return
	}
	procSetWindowLongW.Call(hwnd, gwlStyle, style)
}

func fillSolidRect(hdc uintptr, rc rect, color uintptr) {
	br, _, _ := procCreateSolidBrush.Call(color)
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), br)
	procDeleteObject.Call(br)
}

func drawLine(hdc uintptr, x1, y1, x2, y2 int32, color uintptr) {
	pen, _, _ := procCreatePen.Call(psSolid, 1, color)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	procMoveToEx.Call(hdc, uintptr(x1), uintptr(y1), 0)
	procLineTo.Call(hdc, uintptr(x2), uintptr(y2))
	procSelectObject.Call(hdc, oldPen)
	procDeleteObject.Call(pen)
}

func drawFlatButton(hdc uintptr, rc rect, radius int32, bg, border, fg uintptr, label string) {
	br, _, _ := procCreateSolidBrush.Call(bg)
	pen, _, _ := procCreatePen.Call(psSolid, 1, border)
	oldBr, _, _ := procSelectObject.Call(hdc, br)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	procRoundRect.Call(
		hdc,
		uintptr(rc.Left), uintptr(rc.Top), uintptr(rc.Right), uintptr(rc.Bottom),
		uintptr(radius), uintptr(radius),
	)
	procSelectObject.Call(hdc, oldBr)
	procSelectObject.Call(hdc, oldPen)
	procDeleteObject.Call(br)
	procDeleteObject.Call(pen)

	procSetTextColor.Call(hdc, fg)
	r := rc
	drawText(hdc, label, &r, dtCenter|dtSingleLn|dtVCenter)
}
