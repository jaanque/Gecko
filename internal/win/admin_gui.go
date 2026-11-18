//go:build windows

package win

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"remote-control/internal/config"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
)

var (
	procSendMessageW      = user32GUI.NewProc("SendMessageW")
	procGetWindowTextW    = user32GUI.NewProc("GetWindowTextW")
	procGetWindowTextLenW = user32GUI.NewProc("GetWindowTextLengthW")
	procSetWindowTextW    = user32GUI.NewProc("SetWindowTextW")
	procOpenClipboard     = user32GUI.NewProc("OpenClipboard")
	procGetClipboardData  = user32GUI.NewProc("GetClipboardData")
	procCloseClipboard    = user32GUI.NewProc("CloseClipboard")
	procGlobalLock        = kernel32GUI.NewProc("GlobalLock")
	procGlobalUnlock      = kernel32GUI.NewProc("GlobalUnlock")
)

const (
	wmAdminRefresh     = 0x8010
	wmAdminOpenViewer  = 0x8011
	wmAdminCloseViewer = 0x8012

	wmSetFont      = 0x0030
	wsClipChildren = 0x02000000
	wsBorder       = 0x00800000
	wsTabStop      = 0x00010000
	esAutoHScroll  = 0x0080
	wsExClientEdge = 0x00000200

	cfUnicodeText = 13

	idSrvAddr = 2010
	idSrvKey  = 2011
)

const (
	adminTabDevices = 0
	adminTabBuilder = 1
	adminTabServer  = 2
)

// AdminClientRow holds the display data for one online PC in the Admin list.
type AdminClientRow struct {
	ID           string
	Hostname     string
	IP           string
	Width        int
	Height       int
	MonitorCount int
	Status       string // "online", "pending", "active"
}

// AdminCallbacks connects the native desktop Admin GUI to the RelayClient.
type AdminCallbacks struct {
	OnConnect        func(clientID string) error
	OnCancelPending  func()
	OnDisconnect     func()
	OnMouse          func(ev protocol.MouseEvent)
	OnKey            func(ev protocol.KeyEvent)
	OnFrameAck       func(frameID uint32)
	OnQuality        func(quality int)
	OnSelectMonitor  func(monitorIndex int)
	OnGenerateClient func(logoPNG []byte, destPath string) error
	OnSaveServer     func(profile security.RelayProfile) error
	OnClose          func()
}

// AdminConfig holds initial Relay profile state when launching the Admin window.
type AdminConfig struct {
	HasProfile bool
	Profile    security.RelayProfile
}

type adminHitRegion struct {
	kind     string
	clientID string
	rc       rect
}

type pendingOpenSession struct {
	sessionID    string
	hostname     string
	width        int
	height       int
	monitorCount int
}

// AdminWindow manages the native Windows desktop Administration UI.
type AdminWindow struct {
	callbacks AdminCallbacks

	mu             sync.Mutex
	hwnd           uintptr
	hasProfile     bool
	profile        security.RelayProfile
	relayConnected bool
	relayStatus    string
	clients        []AdminClientRow
	pendingHost    string
	noticeText     string
	noticeIsError  bool
	openReq        *pendingOpenSession
	viewer         *remoteViewer
	readyCh        chan struct{}
	readyOnce      sync.Once

	// UI thread-only state
	hInst      uintptr
	dpi        int32
	cursorArr  uintptr
	cursorHnd  uintptr
	fontBrand  uintptr
	fontTitle  uintptr
	fontBody   uintptr
	fontRow    uintptr
	fontMono   uintptr
	fontBtn    uintptr
	fontSmall  uintptr
	activeTab  int
	hits       []adminHitRegion
	hoverIdx   int
	scrollY    int32
	maxScrollY int32

	// Native controls for "Servidor" tab
	ctrlSrvAddr uintptr
	ctrlSrvKey  uintptr

	// Logo state for "Crear cliente" tab
	logoPNG     []byte
	logoName    string
	logoPreview *logoGDI
}

var (
	activeAdminMu sync.Mutex
	activeAdmin   *AdminWindow
)

// NewAdminWindow creates a new desktop AdminWindow. Call Run() on the main goroutine.
func NewAdminWindow(cfg AdminConfig, cb AdminCallbacks) *AdminWindow {
	startTab := adminTabDevices
	initStatus := "Conectando..."
	if !cfg.HasProfile {
		startTab = adminTabServer
		initStatus = "Sin servidor"
	}
	return &AdminWindow{
		callbacks:   cb,
		hasProfile:  cfg.HasProfile,
		profile:     cfg.Profile,
		relayStatus: initStatus,
		dpi:         96,
		activeTab:   startTab,
		hoverIdx:    -1,
		readyCh:     make(chan struct{}),
	}
}

// Ready is closed once the main Admin window has been created.
func (a *AdminWindow) Ready() <-chan struct{} {
	return a.readyCh
}

// SetRelayStatus updates the live Relay server connection state.
func (a *AdminWindow) SetRelayStatus(connected bool, detail string) {
	a.mu.Lock()
	a.relayConnected = connected
	a.relayStatus = detail
	hwnd := a.hwnd
	a.mu.Unlock()

	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmAdminRefresh, 0, 0)
	}
}

// UpdateClients updates the online device list from any goroutine.
func (a *AdminWindow) UpdateClients(rows []AdminClientRow) {
	cp := make([]AdminClientRow, len(rows))
	copy(cp, rows)

	a.mu.Lock()
	a.clients = cp
	hwnd := a.hwnd
	a.mu.Unlock()

	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmAdminRefresh, 0, 0)
	}
}

// SetPending displays the waiting-for-approval bar for hostname.
func (a *AdminWindow) SetPending(hostname string) {
	a.mu.Lock()
	a.pendingHost = hostname
	a.noticeText = ""
	a.noticeIsError = false
	hwnd := a.hwnd
	a.mu.Unlock()

	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmAdminRefresh, 0, 0)
	}
}

// OpenSession opens the native remote viewer window when a client clicks Permetre.
func (a *AdminWindow) OpenSession(sessionID, hostname string, width, height, monitorCount int) {
	a.mu.Lock()
	a.pendingHost = ""
	a.noticeText = ""
	a.openReq = &pendingOpenSession{
		sessionID:    sessionID,
		hostname:     hostname,
		width:        width,
		height:       height,
		monitorCount: monitorCount,
	}
	hwnd := a.hwnd
	a.mu.Unlock()

	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmAdminOpenViewer, 0, 0)
	}
}

// NotifyDenied clears pending state and shows a denial notice.
func (a *AdminWindow) NotifyDenied(hostname, reason string) {
	a.mu.Lock()
	a.pendingHost = ""
	a.noticeText = fmt.Sprintf("%s: %s", hostname, reason)
	a.noticeIsError = true
	hwnd := a.hwnd
	a.mu.Unlock()

	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmAdminRefresh, 0, 0)
	}
}

// NotifyError shows an error notice in the Admin window.
func (a *AdminWindow) NotifyError(reason string) {
	a.mu.Lock()
	a.pendingHost = ""
	a.noticeText = reason
	a.noticeIsError = true
	hwnd := a.hwnd
	a.mu.Unlock()

	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmAdminRefresh, 0, 0)
	}
}

// NotifyEnded closes the remote viewer (if open) and displays the disconnect reason.
func (a *AdminWindow) NotifyEnded(hostname, reason string) {
	a.mu.Lock()
	a.pendingHost = ""
	if hostname != "" {
		a.noticeText = fmt.Sprintf("%s: %s", hostname, reason)
	} else {
		a.noticeText = reason
	}
	a.noticeIsError = false
	hwnd := a.hwnd
	a.mu.Unlock()

	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmAdminCloseViewer, 0, 0)
	}
}

// PushFrame forwards an incoming MsgFrame payload to the active viewer window.
func (a *AdminWindow) PushFrame(sessionID string, frame []byte) {
	a.mu.Lock()
	v := a.viewer
	a.mu.Unlock()
	if v != nil {
		v.PushFrame(sessionID, frame)
	}
}

// SetLatency updates the measured round-trip latency (ms) in the viewer toolbar.
func (a *AdminWindow) SetLatency(rttMs int) {
	a.mu.Lock()
	v := a.viewer
	a.mu.Unlock()
	if v != nil {
		v.SetLatency(rttMs)
	}
}

// Run creates the native desktop Admin window and runs the Win32 message loop until closed.
func (a *AdminWindow) Run() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	activeAdminMu.Lock()
	activeAdmin = a
	activeAdminMu.Unlock()
	defer func() {
		activeAdminMu.Lock()
		if activeAdmin == a {
			activeAdmin = nil
		}
		activeAdminMu.Unlock()
	}()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	a.hInst = hInst
	a.cursorArr, _, _ = procLoadCursorW.Call(0, idcArrow)
	a.cursorHnd, _, _ = procLoadCursorW.Call(0, idcHand)

	className, _ := syscall.UTF16PtrFromString("RemoteAdminMainWindowClass")
	title, _ := syscall.UTF16PtrFromString("Control Remoto")

	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   syscall.NewCallback(adminWndProc),
		HInstance:     hInst,
		HCursor:       a.cursorArr,
		LpszClassName: className,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	v := newRemoteViewer(hInst, ViewerCallbacks{
		OnMouse:         a.callbacks.OnMouse,
		OnKey:           a.callbacks.OnKey,
		OnFrameAck:      a.callbacks.OnFrameAck,
		OnQuality:       a.callbacks.OnQuality,
		OnSelectMonitor: a.callbacks.OnSelectMonitor,
		OnDisconnect:    a.callbacks.OnDisconnect,
	})
	a.mu.Lock()
	a.viewer = v
	a.mu.Unlock()

	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow|wsClipChildren,
		100, 100, 740, 480,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return err
	}

	a.dpi = getWindowDPI(hwnd)
	a.initFonts()
	defer a.destroyFonts()

	winW := a.scale(740)
	winH := a.scale(480)
	screenW, _, _ := procGetSystemMetrics.Call(smCXScreen)
	screenH, _, _ := procGetSystemMetrics.Call(smCYScreen)
	posX := (int32(screenW) - winW) / 2
	posY := (int32(screenH) - winH) / 2
	if posX < 20 {
		posX = 20
	}
	if posY < 20 {
		posY = 20
	}
	procSetWindowPos.Call(hwnd, 0, uintptr(posX), uintptr(posY), uintptr(winW), uintptr(winH), 0)

	a.createServerControls(hwnd)
	a.syncServerControls(hwnd)

	a.mu.Lock()
	a.hwnd = hwnd
	a.mu.Unlock()

	procShowWindow.Call(hwnd, swShowNormal)
	procUpdateWindow.Call(hwnd)
	a.readyOnce.Do(func() { close(a.readyCh) })

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

func (a *AdminWindow) scale(px int32) int32 {
	return (px * a.dpi) / 96
}

func (a *AdminWindow) initFonts() {
	face, _ := syscall.UTF16PtrFromString("Segoe UI")
	mono, _ := syscall.UTF16PtrFromString("Consolas")
	a.fontBrand = createFont(a.scale(16), 600, face)
	a.fontTitle = createFont(a.scale(15), 600, face)
	a.fontBody = createFont(a.scale(13), 400, face)
	a.fontRow = createFont(a.scale(15), 600, face)
	a.fontMono = createFont(a.scale(12), 400, mono)
	a.fontBtn = createFont(a.scale(13), 600, face)
	a.fontSmall = createFont(a.scale(12), 400, face)
}

func (a *AdminWindow) destroyFonts() {
	for _, f := range []*uintptr{&a.fontBrand, &a.fontTitle, &a.fontBody, &a.fontRow, &a.fontMono, &a.fontBtn, &a.fontSmall} {
		if *f != 0 {
			procDeleteObject.Call(*f)
			*f = 0
		}
	}
}

func (a *AdminWindow) createServerControls(parent uintptr) {
	editClass, _ := syscall.UTF16PtrFromString("EDIT")

	a.mu.Lock()
	initAddr := a.profile.ServerAddr
	initKey := ""
	if a.hasProfile {
		if k, err := security.EncodeProfileKey(a.profile); err == nil {
			initKey = k
		}
	}
	a.mu.Unlock()

	srvAddrU16, _ := syscall.UTF16PtrFromString(initAddr)
	a.ctrlSrvAddr, _, _ = procCreateWindowExW.Call(
		wsExClientEdge, uintptr(unsafe.Pointer(editClass)), uintptr(unsafe.Pointer(srvAddrU16)),
		wsChild|wsTabStop|wsBorder|esAutoHScroll,
		0, 0, 400, uintptr(a.scale(28)),
		parent, idSrvAddr, a.hInst, 0,
	)
	procSendMessageW.Call(a.ctrlSrvAddr, wmSetFont, a.fontMono, 1)

	srvKeyU16, _ := syscall.UTF16PtrFromString(initKey)
	a.ctrlSrvKey, _, _ = procCreateWindowExW.Call(
		wsExClientEdge, uintptr(unsafe.Pointer(editClass)), uintptr(unsafe.Pointer(srvKeyU16)),
		wsChild|wsTabStop|wsBorder|esAutoHScroll,
		0, 0, 400, uintptr(a.scale(28)),
		parent, idSrvKey, a.hInst, 0,
	)
	procSendMessageW.Call(a.ctrlSrvKey, wmSetFont, a.fontMono, 1)
}

func (a *AdminWindow) contentTopY() int32 {
	top := a.scale(54) + a.scale(16)
	a.mu.Lock()
	hasBanner := a.pendingHost != "" || a.noticeText != ""
	a.mu.Unlock()
	if hasBanner {
		top += a.scale(56)
	}
	return top
}

func (a *AdminWindow) serverCardMetrics(cw, ch int32) (cardRC rect, innerL, innerW int32, addrY, keyY int32) {
	pad := a.scale(20)
	cardRC = rect{pad, a.contentTopY(), cw - pad, ch - pad}
	innerPad := a.scale(24)
	innerL = cardRC.Left + innerPad
	innerW = (cardRC.Right - cardRC.Left) - innerPad*2
	addrY = cardRC.Top + a.scale(24)
	keyY = addrY + a.scale(68)
	return
}

func (a *AdminWindow) syncServerControls(hwnd uintptr) {
	if a.ctrlSrvAddr == 0 {
		return
	}
	if a.activeTab != adminTabServer {
		procShowWindow.Call(a.ctrlSrvAddr, swHide)
		procShowWindow.Call(a.ctrlSrvKey, swHide)
		return
	}

	var cr rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr)))
	cw := cr.Right - cr.Left
	ch := cr.Bottom - cr.Top
	if cw <= 0 || ch <= 0 {
		return
	}

	_, innerL, innerW, addrY, keyY := a.serverCardMetrics(cw, ch)
	lblH := a.scale(22)
	editH := a.scale(28)
	pasteW := a.scale(78)
	gap := a.scale(8)

	procSetWindowPos.Call(a.ctrlSrvAddr, 0, uintptr(innerL), uintptr(addrY+lblH), uintptr(innerW), uintptr(editH), 0)
	procSetWindowPos.Call(a.ctrlSrvKey, 0, uintptr(innerL), uintptr(keyY+lblH), uintptr(innerW-pasteW-gap), uintptr(editH), 0)
	procShowWindow.Call(a.ctrlSrvAddr, swShowNormal)
	procShowWindow.Call(a.ctrlSrvKey, swShowNormal)
}

func (a *AdminWindow) switchTab(hwnd uintptr, tab int) {
	if a.activeTab == tab {
		return
	}
	a.activeTab = tab
	a.hoverIdx = -1
	a.syncServerControls(hwnd)
	procInvalidateRect.Call(hwnd, 0, 0)
}

func (a *AdminWindow) paint(hwnd uintptr) {
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

	memDC, _, _ := procCreateCompatibleDC.Call(hdc)
	memBmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(cw), uintptr(ch))
	oldBmp, _, _ := procSelectObject.Call(memDC, memBmp)
	defer func() {
		procSelectObject.Call(memDC, oldBmp)
		procDeleteObject.Call(memBmp)
		procDeleteDC.Call(memDC)
	}()

	a.hits = a.hits[:0]
	procSetBkMode.Call(memDC, transparentBk)

	a.mu.Lock()
	clients := make([]AdminClientRow, len(a.clients))
	copy(clients, a.clients)
	pendingHost := a.pendingHost
	noticeText := a.noticeText
	noticeIsErr := a.noticeIsError
	relayOK := a.relayConnected
	relayDetail := a.relayStatus
	hasProfile := a.hasProfile
	profile := a.profile
	a.mu.Unlock()

	fillSolidRect(memDC, cr, rgb(246, 247, 249))

	// 1. Fixed Top Navigation Bar
	barH := a.scale(54)
	pad := a.scale(20)
	fillSolidRect(memDC, rect{0, 0, cw, barH}, rgb(255, 255, 255))
	drawLine(memDC, 0, barH-1, cw, barH-1, rgb(228, 228, 231))

	oldFont, _, _ := procSelectObject.Call(memDC, a.fontBrand)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))
	brandRC := rect{pad, 0, pad + a.scale(132), barH}
	drawText(memDC, "Control Remoto", &brandRC, dtLeft|dtSingleLn|dtVCenter)

	divX := brandRC.Right + a.scale(8)
	drawLine(memDC, divX, a.scale(15), divX, barH-a.scale(15), rgb(228, 228, 231))

	tabY := a.scale(11)
	tabH := barH - a.scale(22)
	tx := divX + a.scale(12)

	tab1RC := rect{tx, tabY, tx + a.scale(108), tabY + tabH}
	tab1Idx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "tab_devices", rc: tab1RC})
	tx = tab1RC.Right + a.scale(6)

	tab2RC := rect{tx, tabY, tx + a.scale(108), tabY + tabH}
	tab2Idx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "tab_builder", rc: tab2RC})
	tx = tab2RC.Right + a.scale(6)

	tab3RC := rect{tx, tabY, tx + a.scale(86), tabY + tabH}
	tab3Idx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "tab_server", rc: tab3RC})

	procSelectObject.Call(memDC, a.fontBtn)
	a.drawTopMenuTab(memDC, tab1RC, fmt.Sprintf("Equipos (%d)", len(clients)), a.activeTab == adminTabDevices, a.hoverIdx == tab1Idx)
	a.drawTopMenuTab(memDC, tab2RC, "Crear cliente", a.activeTab == adminTabBuilder, a.hoverIdx == tab2Idx)
	a.drawTopMenuTab(memDC, tab3RC, "Servidor", a.activeTab == adminTabServer, a.hoverIdx == tab3Idx)

	pillW := a.scale(116)
	pillH := a.scale(28)
	if cw-pad-pillW > tab3RC.Right+a.scale(12) {
		pillRC := rect{cw - pad - pillW, (barH - pillH) / 2, cw - pad, (barH + pillH) / 2}
		pillIdx := len(a.hits)
		a.hits = append(a.hits, adminHitRegion{kind: "tab_server", rc: pillRC})

		pillBg := rgb(246, 247, 249)
		if a.hoverIdx == pillIdx {
			pillBg = rgb(238, 240, 243)
		}
		drawRoundedCard(memDC, pillRC, a.scale(6), pillBg, rgb(228, 228, 231))
		dotCol := rgb(217, 119, 6)
		statusTxt := "Conectando..."
		if !hasProfile {
			dotCol = rgb(161, 161, 170)
			statusTxt = "Sin servidor"
		} else if relayOK {
			dotCol = rgb(22, 163, 74)
			statusTxt = "En línea"
		}
		drawStatusDot(memDC, pillRC.Left+a.scale(10), pillRC.Top+(pillH-a.scale(8))/2, a.scale(8), dotCol)
		procSelectObject.Call(memDC, a.fontSmall)
		procSetTextColor.Call(memDC, rgb(63, 63, 70))
		pTxt := rect{pillRC.Left + a.scale(24), pillRC.Top, pillRC.Right - a.scale(8), pillRC.Bottom}
		drawText(memDC, statusTxt, &pTxt, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)
	}

	// 2. Optional banner (pending request or notice)
	curY := barH + a.scale(16)
	if pendingHost != "" {
		bH := a.scale(44)
		bRC := rect{pad, curY, cw - pad, curY + bH}
		drawRoundedCard(memDC, bRC, a.scale(7), rgb(255, 255, 255), rgb(212, 212, 216))

		cancelW := a.scale(84)
		cancelH := a.scale(28)
		cancelRC := rect{bRC.Right - a.scale(10) - cancelW, curY + (bH-cancelH)/2, bRC.Right - a.scale(10), curY + (bH+cancelH)/2}
		cancelIdx := len(a.hits)
		a.hits = append(a.hits, adminHitRegion{kind: "cancel_pending", rc: cancelRC})

		drawStatusDot(memDC, bRC.Left+a.scale(14), curY+(bH-a.scale(9))/2, a.scale(9), rgb(217, 119, 6))
		procSelectObject.Call(memDC, a.fontBody)
		procSetTextColor.Call(memDC, rgb(24, 24, 27))
		msgRC := rect{bRC.Left + a.scale(32), curY, cancelRC.Left - a.scale(10), curY + bH}
		drawText(memDC, "Esperando confirmación en "+pendingHost+"...", &msgRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

		cBg := rgb(255, 255, 255)
		if a.hoverIdx == cancelIdx {
			cBg = rgb(244, 244, 245)
		}
		procSelectObject.Call(memDC, a.fontBtn)
		drawFlatButton(memDC, cancelRC, a.scale(5), cBg, rgb(212, 212, 216), rgb(24, 24, 27), "Cancelar")
		curY += a.scale(56)
	} else if noticeText != "" {
		bH := a.scale(42)
		bRC := rect{pad, curY, cw - pad, curY + bH}
		bg := rgb(255, 255, 255)
		border := rgb(212, 212, 216)
		fg := rgb(24, 24, 27)
		if noticeIsErr {
			bg = rgb(254, 242, 242)
			border = rgb(254, 202, 202)
			fg = rgb(185, 28, 28)
		}
		drawRoundedCard(memDC, bRC, a.scale(7), bg, border)

		closeW := a.scale(30)
		closeRC := rect{bRC.Right - a.scale(6) - closeW, curY + a.scale(6), bRC.Right - a.scale(6), curY + bH - a.scale(6)}
		a.hits = append(a.hits, adminHitRegion{kind: "close_notice", rc: closeRC})

		procSelectObject.Call(memDC, a.fontBody)
		procSetTextColor.Call(memDC, fg)
		msgRC := rect{bRC.Left + a.scale(14), curY, closeRC.Left - a.scale(8), curY + bH}
		drawText(memDC, noticeText, &msgRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

		closeTxtRC := closeRC
		drawText(memDC, "×", &closeTxtRC, dtCenter|dtSingleLn|dtVCenter)
		curY += a.scale(56)
	}

	// 3. Active tab content
	switch a.activeTab {
	case adminTabDevices:
		a.paintDevicesTab(memDC, cw, ch, pad, curY, clients, hasProfile, relayOK, relayDetail)
	case adminTabBuilder:
		a.paintBuilderTab(memDC, cw, ch, pad, curY, hasProfile)
	case adminTabServer:
		a.paintServerTab(memDC, cw, ch, hasProfile, relayOK, relayDetail, profile)
	}

	procSelectObject.Call(memDC, oldFont)
	procBitBlt.Call(hdc, 0, 0, uintptr(cw), uintptr(ch), memDC, 0, 0, srccopy)
}

func (a *AdminWindow) drawTopMenuTab(hdc uintptr, rc rect, label string, active, hovered bool) {
	if active {
		drawFlatButton(hdc, rc, a.scale(6), rgb(24, 24, 27), rgb(24, 24, 27), rgb(255, 255, 255), label)
		return
	}
	bg := rgb(255, 255, 255)
	if hovered {
		bg = rgb(244, 244, 245)
	}
	drawFlatButton(hdc, rc, a.scale(6), bg, bg, rgb(63, 63, 70), label)
}

func (a *AdminWindow) paintDevicesTab(memDC uintptr, cw, ch, pad, topY int32, clients []AdminClientRow, hasProfile, relayOK bool, relayDetail string) {
	cardRC := rect{pad, topY, cw - pad, ch - pad}
	if cardRC.Bottom-cardRC.Top < a.scale(140) {
		cardRC.Bottom = cardRC.Top + a.scale(140)
	}
	drawRoundedCard(memDC, cardRC, a.scale(8), rgb(255, 255, 255), rgb(228, 228, 231))

	listTop := cardRC.Top + a.scale(6)
	listBottom := cardRC.Bottom - a.scale(6)
	listH := listBottom - listTop

	if len(clients) == 0 {
		a.maxScrollY = 0
		a.scrollY = 0
		midY := listTop + listH/2

		title := "No hay equipos conectados"
		ctaLabel := "Crear cliente"
		ctaKind := "tab_builder"

		if !hasProfile {
			title = "Servidor sin configurar"
			ctaLabel = "Configurar servidor"
			ctaKind = "tab_server"
		} else if !relayOK {
			title = relayDetail
			ctaLabel = "Revisar servidor"
			ctaKind = "tab_server"
		}

		procSelectObject.Call(memDC, a.fontTitle)
		procSetTextColor.Call(memDC, rgb(24, 24, 27))
		tRC := rect{cardRC.Left + a.scale(24), midY - a.scale(30), cardRC.Right - a.scale(24), midY - a.scale(4)}
		drawText(memDC, title, &tRC, dtCenter|dtSingleLn|dtVCenter|dtEndEllip)

		ctaW := a.scale(168)
		ctaH := a.scale(34)
		ctaX := cardRC.Left + (cardRC.Right-cardRC.Left-ctaW)/2
		ctaRC := rect{ctaX, midY + a.scale(8), ctaX + ctaW, midY + a.scale(8) + ctaH}
		ctaIdx := len(a.hits)
		a.hits = append(a.hits, adminHitRegion{kind: ctaKind, rc: ctaRC})
		ctaBg := rgb(24, 24, 27)
		if a.hoverIdx == ctaIdx {
			ctaBg = rgb(39, 39, 42)
		}
		procSelectObject.Call(memDC, a.fontBtn)
		drawFlatButton(memDC, ctaRC, a.scale(6), ctaBg, ctaBg, rgb(255, 255, 255), ctaLabel)
		return
	}

	rowH := a.scale(60)
	totalH := int32(len(clients)) * rowH
	if totalH > listH {
		a.maxScrollY = totalH - listH
	} else {
		a.maxScrollY = 0
	}
	if a.scrollY > a.maxScrollY {
		a.scrollY = a.maxScrollY
	}
	if a.scrollY < 0 {
		a.scrollY = 0
	}

	for i, c := range clients {
		ry := listTop + int32(i)*rowH - a.scrollY
		if ry+rowH < listTop || ry > listBottom {
			continue
		}

		visTop := ry
		if visTop < listTop {
			visTop = listTop
		}
		visBot := ry + rowH
		if visBot > listBottom {
			visBot = listBottom
		}
		rowRC := rect{cardRC.Left + 1, visTop, cardRC.Right - 1, visBot}

		rowIdx := -1
		if c.Status == "online" {
			rowIdx = len(a.hits)
			a.hits = append(a.hits, adminHitRegion{kind: "row", clientID: c.ID, rc: rowRC})
		}

		if rowIdx >= 0 && a.hoverIdx == rowIdx {
			fillSolidRect(memDC, rowRC, rgb(250, 250, 250))
		}
		if i < len(clients)-1 && ry+rowH <= listBottom {
			drawLine(memDC, cardRC.Left+a.scale(16), ry+rowH-1, cardRC.Right-a.scale(16), ry+rowH-1, rgb(244, 244, 245))
		}
		if visBot-visTop < a.scale(34) {
			continue
		}

		dotColor := rgb(22, 163, 74)
		statusLabel := "Disponible"
		switch c.Status {
		case "pending":
			dotColor = rgb(217, 119, 6)
			statusLabel = "Esperando..."
		case "active":
			dotColor = rgb(24, 24, 27)
			statusLabel = "En sesión"
		}

		dotSz := a.scale(9)
		drawStatusDot(memDC, cardRC.Left+a.scale(18), ry+(rowH-dotSz)/2, dotSz, dotColor)

		btnW := a.scale(90)
		btnH := a.scale(30)
		btnRC := rect{cardRC.Right - a.scale(18) - btnW, ry + (rowH-btnH)/2, cardRC.Right - a.scale(18), ry + (rowH+btnH)/2}

		statusW := a.scale(116)
		statusRC := rect{btnRC.Left - a.scale(12) - statusW, ry, btnRC.Left - a.scale(12), ry + rowH}

		textLeft := cardRC.Left + a.scale(38)
		textRight := statusRC.Left - a.scale(10)

		procSelectObject.Call(memDC, a.fontRow)
		procSetTextColor.Call(memDC, rgb(24, 24, 27))
		nameRC := rect{textLeft, ry + a.scale(10), textRight, ry + a.scale(32)}
		drawText(memDC, c.Hostname, &nameRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

		meta := c.IP
		if c.Width > 0 && c.Height > 0 {
			meta = fmt.Sprintf("%s · %d×%d", c.IP, c.Width, c.Height)
		}
		if c.MonitorCount > 1 {
			meta = fmt.Sprintf("%s · %d monitores", meta, c.MonitorCount)
		}
		procSelectObject.Call(memDC, a.fontMono)
		procSetTextColor.Call(memDC, rgb(113, 113, 122))
		metaRC := rect{textLeft, ry + a.scale(32), textRight, ry + a.scale(50)}
		drawText(memDC, meta, &metaRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

		procSelectObject.Call(memDC, a.fontBody)
		procSetTextColor.Call(memDC, rgb(82, 82, 91))
		drawText(memDC, statusLabel, &statusRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

		procSelectObject.Call(memDC, a.fontBtn)
		if c.Status == "online" {
			bBg := rgb(255, 255, 255)
			if rowIdx >= 0 && a.hoverIdx == rowIdx {
				bBg = rgb(244, 244, 245)
			}
			drawFlatButton(memDC, btnRC, a.scale(5), bBg, rgb(212, 212, 216), rgb(24, 24, 27), "Conectar")
		} else {
			drawFlatButton(memDC, btnRC, a.scale(5), rgb(250, 250, 250), rgb(228, 228, 231), rgb(161, 161, 170), "Conectar")
		}
	}
}

func (a *AdminWindow) paintBuilderTab(memDC uintptr, cw, ch, pad, topY int32, hasProfile bool) {
	cardRC := rect{pad, topY, cw - pad, ch - pad}
	drawRoundedCard(memDC, cardRC, a.scale(8), rgb(255, 255, 255), rgb(228, 228, 231))

	innerL := cardRC.Left + a.scale(24)
	rightEdge := cardRC.Right - a.scale(24)
	rowY := cardRC.Top + a.scale(28)

	boxSz := a.scale(56)
	boxRC := rect{innerL, rowY, innerL + boxSz, rowY + boxSz}
	drawRoundedCard(memDC, boxRC, a.scale(8), rgb(246, 247, 249), rgb(228, 228, 231))
	if a.logoPreview != nil {
		inner := rect{boxRC.Left + a.scale(5), boxRC.Top + a.scale(5), boxRC.Right - a.scale(5), boxRC.Bottom - a.scale(5)}
		a.logoPreview.DrawFit(memDC, inner)
	}

	pickW := a.scale(88)
	remW := a.scale(68)
	btnH := a.scale(32)
	btnY := rowY + (boxSz-btnH)/2

	pickRight := rightEdge
	if len(a.logoPNG) > 0 {
		remRC := rect{rightEdge - remW, btnY, rightEdge, btnY + btnH}
		remIdx := len(a.hits)
		a.hits = append(a.hits, adminHitRegion{kind: "remove_logo", rc: remRC})
		remBg := rgb(255, 255, 255)
		if a.hoverIdx == remIdx {
			remBg = rgb(254, 242, 242)
		}
		procSelectObject.Call(memDC, a.fontBtn)
		drawFlatButton(memDC, remRC, a.scale(5), remBg, rgb(254, 202, 202), rgb(185, 28, 28), "Quitar")
		pickRight = remRC.Left - a.scale(8)
	}

	pickRC := rect{pickRight - pickW, btnY, pickRight, btnY + btnH}
	pickIdx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "pick_logo", rc: pickRC})
	pickBg := rgb(255, 255, 255)
	if a.hoverIdx == pickIdx {
		pickBg = rgb(244, 244, 245)
	}
	procSelectObject.Call(memDC, a.fontBtn)
	drawFlatButton(memDC, pickRC, a.scale(5), pickBg, rgb(212, 212, 216), rgb(24, 24, 27), "Elegir")

	txtLeft := boxRC.Right + a.scale(14)
	txtRight := pickRC.Left - a.scale(12)
	procSelectObject.Call(memDC, a.fontTitle)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))
	lt1 := rect{txtLeft, rowY + a.scale(6), txtRight, rowY + a.scale(28)}
	drawText(memDC, "Logo", &lt1, dtLeft|dtSingleLn|dtVCenter)

	procSelectObject.Call(memDC, a.fontBody)
	procSetTextColor.Call(memDC, rgb(113, 113, 122))
	logoSub := "Sin logo"
	if a.logoName != "" {
		logoSub = a.logoName
	}
	lt2 := rect{txtLeft, rowY + a.scale(28), txtRight, rowY + a.scale(50)}
	drawText(memDC, logoSub, &lt2, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

	footerH := a.scale(58)
	footerTop := cardRC.Bottom - footerH
	if footerTop < rowY+boxSz+a.scale(20) {
		footerTop = rowY + boxSz + a.scale(20)
	}
	drawLine(memDC, cardRC.Left+1, footerTop, cardRC.Right-1, footerTop, rgb(228, 228, 231))

	saveW := a.scale(148)
	saveH := a.scale(36)
	saveRC := rect{rightEdge - saveW, footerTop + (footerH-saveH)/2, rightEdge, footerTop + (footerH+saveH)/2}
	saveIdx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "save_client", rc: saveRC})

	if !hasProfile {
		procSelectObject.Call(memDC, a.fontSmall)
		procSetTextColor.Call(memDC, rgb(185, 28, 28))
		fRC := rect{innerL, footerTop, saveRC.Left - a.scale(12), footerTop + footerH}
		drawText(memDC, "Configura el servidor primero", &fRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)
	}

	saveBg := rgb(24, 24, 27)
	if a.hoverIdx == saveIdx {
		saveBg = rgb(39, 39, 42)
	}
	procSelectObject.Call(memDC, a.fontBtn)
	drawFlatButton(memDC, saveRC, a.scale(6), saveBg, saveBg, rgb(255, 255, 255), "Guardar .exe")
}

func (a *AdminWindow) paintServerTab(memDC uintptr, cw, ch int32, hasProfile, relayOK bool, relayDetail string, profile security.RelayProfile) {
	cardRC, innerL, innerW, addrY, keyY := a.serverCardMetrics(cw, ch)
	drawRoundedCard(memDC, cardRC, a.scale(8), rgb(255, 255, 255), rgb(228, 228, 231))
	rightEdge := innerL + innerW

	procSelectObject.Call(memDC, a.fontBtn)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))
	l1 := rect{innerL, addrY, rightEdge, addrY + a.scale(20)}
	drawText(memDC, "Dirección (host:puerto)", &l1, dtLeft|dtSingleLn|dtVCenter)

	l2 := rect{innerL, keyY, rightEdge, keyY + a.scale(20)}
	drawText(memDC, "Clave (RMT1:...)", &l2, dtLeft|dtSingleLn|dtVCenter)

	pasteW := a.scale(78)
	pasteH := a.scale(28)
	pasteRC := rect{rightEdge - pasteW, keyY + a.scale(22), rightEdge, keyY + a.scale(22) + pasteH}
	pasteIdx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "paste_key", rc: pasteRC})
	pasteBg := rgb(255, 255, 255)
	if a.hoverIdx == pasteIdx {
		pasteBg = rgb(244, 244, 245)
	}
	drawFlatButton(memDC, pasteRC, a.scale(5), pasteBg, rgb(212, 212, 216), rgb(24, 24, 27), "Pegar")

	stTop := keyY + a.scale(66)
	stH := a.scale(40)
	stRC := rect{innerL, stTop, rightEdge, stTop + stH}
	drawRoundedCard(memDC, stRC, a.scale(6), rgb(249, 250, 251), rgb(228, 228, 231))

	dotCol := rgb(161, 161, 170)
	stLine := "Sin configurar"
	if hasProfile {
		if relayOK {
			dotCol = rgb(22, 163, 74)
			stLine = "Conectado · " + profile.ServerAddr
		} else {
			dotCol = rgb(217, 119, 6)
			stLine = relayDetail
		}
	}
	drawStatusDot(memDC, stRC.Left+a.scale(14), stRC.Top+(stH-a.scale(9))/2, a.scale(9), dotCol)
	procSelectObject.Call(memDC, a.fontBody)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))
	sRC := rect{stRC.Left + a.scale(32), stRC.Top, stRC.Right - a.scale(12), stRC.Bottom}
	drawText(memDC, stLine, &sRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

	footerH := a.scale(58)
	footerTop := cardRC.Bottom - footerH
	if footerTop < stRC.Bottom+a.scale(14) {
		footerTop = stRC.Bottom + a.scale(14)
	}
	drawLine(memDC, cardRC.Left+1, footerTop, cardRC.Right-1, footerTop, rgb(228, 228, 231))

	saveW := a.scale(164)
	saveH := a.scale(36)
	saveRC := rect{rightEdge - saveW, footerTop + (footerH-saveH)/2, rightEdge, footerTop + (footerH+saveH)/2}
	saveIdx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "save_server", rc: saveRC})

	saveBg := rgb(24, 24, 27)
	if a.hoverIdx == saveIdx {
		saveBg = rgb(39, 39, 42)
	}
	procSelectObject.Call(memDC, a.fontBtn)
	drawFlatButton(memDC, saveRC, a.scale(6), saveBg, saveBg, rgb(255, 255, 255), "Guardar y conectar")
}

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

	case "save_client":
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

func (a *AdminWindow) handleSaveServer(hwnd uintptr) {
	keyStr := strings.TrimSpace(getControlText(a.ctrlSrvKey))
	prof, err := security.DecodeProfileKey(keyStr)
	if err != nil {
		ShowMessageBox(hwnd, "Clave inválida", err.Error(), true)
		return
	}

	addr := strings.TrimSpace(getControlText(a.ctrlSrvAddr))
	if addr != "" {
		prof.ServerAddr = addr
	}
	if prof.ServerAddr == "" {
		ShowMessageBox(hwnd, "Dirección requerida", "Introduce host:puerto del servidor.", true)
		return
	}
	if !strings.Contains(prof.ServerAddr, ":") {
		prof.ServerAddr += ":9443"
	}

	if a.callbacks.OnSaveServer != nil {
		if err := a.callbacks.OnSaveServer(prof); err != nil {
			ShowMessageBox(hwnd, "Error", err.Error(), true)
			return
		}
	}

	a.mu.Lock()
	a.profile = prof
	a.hasProfile = true
	a.noticeText = ""
	a.noticeIsError = false
	a.mu.Unlock()

	setControlText(a.ctrlSrvAddr, prof.ServerAddr)
	a.switchTab(hwnd, adminTabDevices)
}

func (a *AdminWindow) handlePickLogo(hwnd uintptr) {
	path, ok := ShowOpenLogoDialog(hwnd)
	if !ok || path == "" {
		return
	}
	pngBytes, err := config.LoadAndNormalizeLogo(path)
	if err != nil {
		ShowMessageBox(hwnd, "Logo inválido", err.Error(), true)
		return
	}
	if a.logoPreview != nil {
		a.logoPreview.Close()
	}
	a.logoPNG = pngBytes
	a.logoName = filepath.Base(path)
	a.logoPreview = newLogoGDI(pngBytes)
	procInvalidateRect.Call(hwnd, 0, 0)
}

func (a *AdminWindow) handleSaveClient(hwnd uintptr) {
	a.mu.Lock()
	hasProf := a.hasProfile
	a.mu.Unlock()
	if !hasProf {
		a.switchTab(hwnd, adminTabServer)
		return
	}

	destPath, ok := ShowSaveExeDialog(hwnd, "ClientRemot.exe")
	if !ok || destPath == "" {
		return
	}

	if a.callbacks.OnGenerateClient != nil {
		if err := a.callbacks.OnGenerateClient(a.logoPNG, destPath); err != nil {
			ShowMessageBox(hwnd, "Error", err.Error(), true)
			return
		}
	}

	a.mu.Lock()
	a.noticeText = "Guardado: " + filepath.Base(destPath)
	a.noticeIsError = false
	a.mu.Unlock()
	a.syncServerControls(hwnd)
	procInvalidateRect.Call(hwnd, 0, 0)
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
		procPostQuitMessage.Call(0)
		return 0
	}

	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func drawRoundedCard(hdc uintptr, rc rect, radius int32, bg, border uintptr) {
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

func readTextFromClipboard(hwnd uintptr) string {
	if ok, _, _ := procOpenClipboard.Call(hwnd); ok == 0 {
		return ""
	}
	defer procCloseClipboard.Call()

	hMem, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if hMem == 0 {
		return ""
	}
	locked, _, _ := procGlobalLock.Call(hMem)
	if locked == 0 {
		return ""
	}
	defer procGlobalUnlock.Call(hMem)

	ptr := *(*unsafe.Pointer)(unsafe.Pointer(&locked))
	u16 := unsafe.Slice((*uint16)(ptr), 4096)
	end := 0
	for end < len(u16) && u16[end] != 0 {
		end++
	}
	return syscall.UTF16ToString(u16[:end])
}
