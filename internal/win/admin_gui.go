//go:build windows

package win

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"remote-control/internal/config"
	"remote-control/internal/protocol"
)

var (
	procSendMessageW        = user32GUI.NewProc("SendMessageW")
	procGetWindowTextW      = user32GUI.NewProc("GetWindowTextW")
	procGetWindowTextLenW   = user32GUI.NewProc("GetWindowTextLengthW")
	procOpenClipboard       = user32GUI.NewProc("OpenClipboard")
	procEmptyClipboard      = user32GUI.NewProc("EmptyClipboard")
	procSetClipboardData    = user32GUI.NewProc("SetClipboardData")
	procCloseClipboard      = user32GUI.NewProc("CloseClipboard")
	procGlobalAlloc         = kernel32GUI.NewProc("GlobalAlloc")
	procGlobalLock          = kernel32GUI.NewProc("GlobalLock")
	procGlobalUnlock        = kernel32GUI.NewProc("GlobalUnlock")
)

const (
	wmAdminRefresh     = 0x8010
	wmAdminOpenViewer  = 0x8011
	wmAdminCloseViewer = 0x8012

	wmSetFont       = 0x0030
	cbAddString     = 0x0143
	cbGetCurSel     = 0x0147
	cbSetCurSel     = 0x014E
	cbsDropdown     = 0x0002
	cbsDropdownList = 0x0003
	cbsAutoHScroll  = 0x0040
	wsVScroll       = 0x00200000
	wsClipChildren  = 0x02000000
	wsBorder        = 0x00800000
	wsTabStop       = 0x00010000
	esAutoHScroll   = 0x0080
	wsExClientEdge  = 0x00000200

	cfUnicodeText = 13
	gmemMoveable  = 0x0002

	idCfgAddr    = 2001
	idCfgTitle   = 2002
	idCfgPrompt  = 2003
	idCfgQual    = 2004
	idCfgTimeout = 2005
)

const (
	adminTabDevices = 0
	adminTabBuilder = 1
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

// ClientGenParams carries all user-configured fields from the "Crear cliente" screen.
type ClientGenParams struct {
	ServerAddr        string
	LogoPNG           []byte
	AppTitle          string
	PromptMessage     string
	ConsentTimeoutSec int
	DefaultQuality    int
	StartMinimized    bool
	HideDisconnect    bool
	MuteSound         bool
}

// AdminCallbacks connects the native desktop Admin GUI to the Hub.
type AdminCallbacks struct {
	OnConnect        func(clientID string) error
	OnCancelPending  func()
	OnDisconnect     func()
	OnMouse          func(ev protocol.MouseEvent)
	OnKey            func(ev protocol.KeyEvent)
	OnFrameAck       func(frameID uint32, sentUnixNano int64)
	OnQuality        func(quality int)
	OnSelectMonitor  func(monitorIndex int)
	OnGenerateClient func(params ClientGenParams, destPath string) error
	OnClose          func()
}

// AdminConfig configures static header info for the Admin desktop window.
type AdminConfig struct {
	AgentPort       int
	SuggestedAddrs  []string // e.g. ["192.168.0.10:9443", "127.0.0.1:9443"]
	CertFingerprint string
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
	cfg       AdminConfig
	callbacks AdminCallbacks

	mu            sync.Mutex
	hwnd          uintptr
	clients       []AdminClientRow
	pendingHost   string
	noticeText    string
	noticeIsError bool
	openReq       *pendingOpenSession
	viewer        *remoteViewer
	readyCh       chan struct{}
	readyOnce     sync.Once

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

	// Native form controls for "Crear cliente" tab
	ctrlAddr    uintptr
	ctrlTitle   uintptr
	ctrlPrompt  uintptr
	ctrlQual    uintptr
	ctrlTimeout uintptr

	// Client builder toggles & logo state
	optStartMinimized bool
	optAllowDisc      bool
	optSoundOnReq     bool
	logoPNG           []byte
	logoName          string
	logoPreview       *logoGDI
}

var (
	activeAdminMu sync.Mutex
	activeAdmin   *AdminWindow
)

// NewAdminWindow creates a new desktop AdminWindow. Call Run() on the main goroutine.
func NewAdminWindow(cfg AdminConfig, cb AdminCallbacks) *AdminWindow {
	return &AdminWindow{
		cfg:          cfg,
		callbacks:    cb,
		dpi:          96,
		activeTab:    adminTabDevices,
		hoverIdx:     -1,
		optAllowDisc: true,
		optSoundOnReq: true,
		readyCh:      make(chan struct{}),
	}
}

// Ready is closed once the main Admin window has been created.
func (a *AdminWindow) Ready() <-chan struct{} {
	return a.readyCh
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

// OpenSession opens the native remote viewer window when a client clicks Permitir.
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
	title, _ := syscall.UTF16PtrFromString("Control Remoto · Administración")

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
		100, 100, 780, 560,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return err
	}

	a.dpi = getWindowDPI(hwnd)
	a.initFonts()
	defer a.destroyFonts()

	winW := a.scale(780)
	winH := a.scale(560)
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

	a.createBuilderControls(hwnd)
	a.syncBuilderControls(hwnd)

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

func (a *AdminWindow) primaryServerAddr() string {
	if len(a.cfg.SuggestedAddrs) > 0 {
		return a.cfg.SuggestedAddrs[0]
	}
	return fmt.Sprintf("127.0.0.1:%d", a.cfg.AgentPort)
}

func (a *AdminWindow) createBuilderControls(parent uintptr) {
	comboClass, _ := syscall.UTF16PtrFromString("COMBOBOX")
	editClass, _ := syscall.UTF16PtrFromString("EDIT")

	// 1. Server address combobox (editable)
	a.ctrlAddr, _, _ = procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(comboClass)), 0,
		wsChild|wsTabStop|wsVScroll|cbsDropdown|cbsAutoHScroll,
		0, 0, 200, uintptr(a.scale(150)),
		parent, idCfgAddr, a.hInst, 0,
	)
	procSendMessageW.Call(a.ctrlAddr, wmSetFont, a.fontMono, 1)
	for _, addr := range a.cfg.SuggestedAddrs {
		u16, _ := syscall.UTF16PtrFromString(addr)
		procSendMessageW.Call(a.ctrlAddr, cbAddString, 0, uintptr(unsafe.Pointer(u16)))
	}
	if len(a.cfg.SuggestedAddrs) > 0 {
		procSendMessageW.Call(a.ctrlAddr, cbSetCurSel, 0, 0)
	}

	// 2. Client window title edit
	defTitle, _ := syscall.UTF16PtrFromString("Cliente Remoto")
	a.ctrlTitle, _, _ = procCreateWindowExW.Call(
		wsExClientEdge, uintptr(unsafe.Pointer(editClass)), uintptr(unsafe.Pointer(defTitle)),
		wsChild|wsTabStop|wsBorder|esAutoHScroll,
		0, 0, 200, uintptr(a.scale(26)),
		parent, idCfgTitle, a.hInst, 0,
	)
	procSendMessageW.Call(a.ctrlTitle, wmSetFont, a.fontBody, 1)

	// 3. Prompt message edit
	defPrompt, _ := syscall.UTF16PtrFromString("El administrador solicita controlar este equipo.")
	a.ctrlPrompt, _, _ = procCreateWindowExW.Call(
		wsExClientEdge, uintptr(unsafe.Pointer(editClass)), uintptr(unsafe.Pointer(defPrompt)),
		wsChild|wsTabStop|wsBorder|esAutoHScroll,
		0, 0, 400, uintptr(a.scale(26)),
		parent, idCfgPrompt, a.hInst, 0,
	)
	procSendMessageW.Call(a.ctrlPrompt, wmSetFont, a.fontBody, 1)

	// 4. Default video quality combobox (fixed list)
	a.ctrlQual, _, _ = procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(comboClass)), 0,
		wsChild|wsTabStop|wsVScroll|cbsDropdownList,
		0, 0, 200, uintptr(a.scale(130)),
		parent, idCfgQual, a.hInst, 0,
	)
	procSendMessageW.Call(a.ctrlQual, wmSetFont, a.fontBody, 1)
	for _, q := range []string{"Estándar (recomendada)", "Alta (máxima nitidez)", "Baja (ahorro de red)"} {
		u16, _ := syscall.UTF16PtrFromString(q)
		procSendMessageW.Call(a.ctrlQual, cbAddString, 0, uintptr(unsafe.Pointer(u16)))
	}
	procSendMessageW.Call(a.ctrlQual, cbSetCurSel, 0, 0)

	// 5. Consent timeout combobox (editable seconds)
	a.ctrlTimeout, _, _ = procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(comboClass)), 0,
		wsChild|wsTabStop|wsVScroll|cbsDropdown|cbsAutoHScroll,
		0, 0, 200, uintptr(a.scale(140)),
		parent, idCfgTimeout, a.hInst, 0,
	)
	procSendMessageW.Call(a.ctrlTimeout, wmSetFont, a.fontMono, 1)
	for _, sec := range []string{"30", "60", "90", "120"} {
		u16, _ := syscall.UTF16PtrFromString(sec)
		procSendMessageW.Call(a.ctrlTimeout, cbAddString, 0, uintptr(unsafe.Pointer(u16)))
	}
	procSendMessageW.Call(a.ctrlTimeout, cbSetCurSel, 1, 0) // 60s
}

func (a *AdminWindow) builderCardMetrics(cw, ch int32) (cardRC rect, colL, colR, colW int32, row1Y, row2Y, row3Y, row4Y, row5Y int32) {
	barH := a.scale(54)
	pad := a.scale(20)
	top := barH + a.scale(16)

	a.mu.Lock()
	hasBanner := a.pendingHost != "" || a.noticeText != ""
	a.mu.Unlock()
	if hasBanner {
		top += a.scale(56)
	}

	cardRC = rect{pad, top, cw - pad, ch - pad}
	innerPad := a.scale(22)
	gap := a.scale(18)
	innerW := (cardRC.Right - cardRC.Left) - innerPad*2
	colW = (innerW - gap) / 2
	colL = cardRC.Left + innerPad
	colR = colL + colW + gap

	row1Y = cardRC.Top + a.scale(20)
	row2Y = row1Y + a.scale(62)
	row3Y = row2Y + a.scale(62)
	row4Y = row3Y + a.scale(64)
	row5Y = row4Y + a.scale(84)
	return
}

func (a *AdminWindow) syncBuilderControls(hwnd uintptr) {
	if a.ctrlAddr == 0 {
		return
	}
	ctrls := []uintptr{a.ctrlAddr, a.ctrlTitle, a.ctrlPrompt, a.ctrlQual, a.ctrlTimeout}
	if a.activeTab != adminTabBuilder {
		for _, c := range ctrls {
			procShowWindow.Call(c, swHide)
		}
		return
	}

	var cr rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr)))
	cw := cr.Right - cr.Left
	ch := cr.Bottom - cr.Top
	if cw <= 0 || ch <= 0 {
		return
	}

	cardRC, colL, colR, colW, row1Y, row2Y, row3Y, _, _ := a.builderCardMetrics(cw, ch)
	innerPad := a.scale(22)
	fullW := (cardRC.Right - cardRC.Left) - innerPad*2
	lblH := a.scale(20)
	editH := a.scale(26)
	comboDropH := a.scale(150)

	procSetWindowPos.Call(a.ctrlAddr, 0, uintptr(colL), uintptr(row1Y+lblH), uintptr(colW), uintptr(comboDropH), 0)
	procSetWindowPos.Call(a.ctrlTitle, 0, uintptr(colR), uintptr(row1Y+lblH), uintptr(colW), uintptr(editH), 0)
	procSetWindowPos.Call(a.ctrlPrompt, 0, uintptr(colL), uintptr(row2Y+lblH), uintptr(fullW), uintptr(editH), 0)
	procSetWindowPos.Call(a.ctrlQual, 0, uintptr(colL), uintptr(row3Y+lblH), uintptr(colW), uintptr(comboDropH), 0)
	procSetWindowPos.Call(a.ctrlTimeout, 0, uintptr(colR), uintptr(row3Y+lblH), uintptr(colW), uintptr(comboDropH), 0)

	for _, c := range ctrls {
		procShowWindow.Call(c, swShowNormal)
	}
}

func (a *AdminWindow) switchTab(hwnd uintptr, tab int) {
	if a.activeTab == tab {
		return
	}
	a.activeTab = tab
	a.hoverIdx = -1
	a.syncBuilderControls(hwnd)
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
	a.mu.Unlock()

	// Neutral light canvas (#F6F7F9)
	fillSolidRect(memDC, cr, rgb(246, 247, 249))

	// 1. Clean Top Navigation Bar (54px, fixed, non-collapsible)
	barH := a.scale(54)
	pad := a.scale(20)
	fillSolidRect(memDC, rect{0, 0, cw, barH}, rgb(255, 255, 255))
	drawLine(memDC, 0, barH-1, cw, barH-1, rgb(228, 228, 231))

	oldFont, _, _ := procSelectObject.Call(memDC, a.fontBrand)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))
	brandRC := rect{pad, 0, pad + a.scale(136), barH}
	drawText(memDC, "Control Remoto", &brandRC, dtLeft|dtSingleLn|dtVCenter)

	// Subtle divider between brand and top menu tabs
	divX := brandRC.Right + a.scale(8)
	drawLine(memDC, divX, a.scale(15), divX, barH-a.scale(15), rgb(228, 228, 231))

	// Top menu tabs: "Equipos (N)" | "Crear cliente"
	tabY := a.scale(11)
	tabH := barH - a.scale(22)
	tab1X := divX + a.scale(14)
	tab1W := a.scale(116)
	tab1RC := rect{tab1X, tabY, tab1X + tab1W, tabY + tabH}
	tab1Idx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "tab_devices", rc: tab1RC})

	tab2X := tab1RC.Right + a.scale(6)
	tab2W := a.scale(112)
	tab2RC := rect{tab2X, tabY, tab2X + tab2W, tabY + tabH}
	tab2Idx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "tab_builder", rc: tab2RC})

	procSelectObject.Call(memDC, a.fontBtn)
	a.drawTopMenuTab(memDC, tab1RC, fmt.Sprintf("Equipos (%d)", len(clients)), a.activeTab == adminTabDevices, a.hoverIdx == tab1Idx)
	a.drawTopMenuTab(memDC, tab2RC, "Crear cliente", a.activeTab == adminTabBuilder, a.hoverIdx == tab2Idx)

	// Right side: clickable server address pill (copy to clipboard)
	addr := a.primaryServerAddr()
	addrW := a.scale(168)
	addrH := a.scale(28)
	if cw-pad-addrW > tab2RC.Right+a.scale(12) {
		addrRC := rect{cw - pad - addrW, (barH - addrH) / 2, cw - pad, (barH + addrH) / 2}
		addrIdx := len(a.hits)
		a.hits = append(a.hits, adminHitRegion{kind: "copy_addr", rc: addrRC})

		pillBg := rgb(246, 247, 249)
		if a.hoverIdx == addrIdx {
			pillBg = rgb(238, 240, 243)
		}
		procSelectObject.Call(memDC, a.fontMono)
		drawFlatButton(memDC, addrRC, a.scale(6), pillBg, rgb(228, 228, 231), rgb(82, 82, 91), addr)
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
	if a.activeTab == adminTabDevices {
		a.paintDevicesTab(memDC, cw, ch, pad, curY, clients)
	} else {
		a.paintBuilderTab(memDC, cw, ch)
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

func (a *AdminWindow) paintDevicesTab(memDC uintptr, cw, ch, pad, topY int32, clients []AdminClientRow) {
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

		procSelectObject.Call(memDC, a.fontTitle)
		procSetTextColor.Call(memDC, rgb(24, 24, 27))
		tRC := rect{cardRC.Left + a.scale(24), midY - a.scale(44), cardRC.Right - a.scale(24), midY - a.scale(20)}
		drawText(memDC, "No hay equipos conectados", &tRC, dtCenter|dtSingleLn|dtVCenter)

		procSelectObject.Call(memDC, a.fontBody)
		procSetTextColor.Call(memDC, rgb(113, 113, 122))
		dRC := rect{cardRC.Left + a.scale(24), midY - a.scale(16), cardRC.Right - a.scale(24), midY + a.scale(6)}
		drawText(memDC, "Crea ClienteRemoto.exe y ejecútalo en los PCs que quieras controlar.", &dRC, dtCenter|dtSingleLn|dtVCenter|dtEndEllip)

		ctaW := a.scale(186)
		ctaH := a.scale(34)
		ctaX := cardRC.Left + (cardRC.Right-cardRC.Left-ctaW)/2
		ctaRC := rect{ctaX, midY + a.scale(18), ctaX + ctaW, midY + a.scale(18) + ctaH}
		ctaIdx := len(a.hits)
		a.hits = append(a.hits, adminHitRegion{kind: "tab_builder", rc: ctaRC})
		ctaBg := rgb(24, 24, 27)
		if a.hoverIdx == ctaIdx {
			ctaBg = rgb(39, 39, 42)
		}
		procSelectObject.Call(memDC, a.fontBtn)
		drawFlatButton(memDC, ctaRC, a.scale(6), ctaBg, ctaBg, rgb(255, 255, 255), "Crear cliente (.exe)")
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
			statusLabel = "Esperando aprobación"
		case "active":
			dotColor = rgb(24, 24, 27)
			statusLabel = "En sesión"
		}

		dotSz := a.scale(9)
		drawStatusDot(memDC, cardRC.Left+a.scale(18), ry+(rowH-dotSz)/2, dotSz, dotColor)

		btnW := a.scale(90)
		btnH := a.scale(30)
		btnRC := rect{cardRC.Right - a.scale(18) - btnW, ry + (rowH-btnH)/2, cardRC.Right - a.scale(18), ry + (rowH+btnH)/2}

		statusW := a.scale(130)
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

func (a *AdminWindow) paintBuilderTab(memDC uintptr, cw, ch int32) {
	cardRC, colL, colR, colW, row1Y, row2Y, row3Y, row4Y, row5Y := a.builderCardMetrics(cw, ch)
	drawRoundedCard(memDC, cardRC, a.scale(8), rgb(255, 255, 255), rgb(228, 228, 231))

	lblH := a.scale(18)
	procSelectObject.Call(memDC, a.fontBtn)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))

	// Labels for the 5 native form fields
	l1 := rect{colL, row1Y, colL + colW, row1Y + lblH}
	drawText(memDC, "Dirección del servidor (IP:Puerto)", &l1, dtLeft|dtSingleLn|dtVCenter)

	l2 := rect{colR, row1Y, colR + colW, row1Y + lblH}
	drawText(memDC, "Título de la ventana cliente", &l2, dtLeft|dtSingleLn|dtVCenter)

	l3 := rect{colL, row2Y, cardRC.Right - a.scale(22), row2Y + lblH}
	drawText(memDC, "Mensaje al solicitar control", &l3, dtLeft|dtSingleLn|dtVCenter)

	l4 := rect{colL, row3Y, colL + colW, row3Y + lblH}
	drawText(memDC, "Calidad de vídeo inicial", &l4, dtLeft|dtSingleLn|dtVCenter)

	l5 := rect{colR, row3Y, colR + colW, row3Y + lblH}
	drawText(memDC, "Tiempo de espera de solicitud (seg)", &l5, dtLeft|dtSingleLn|dtVCenter)

	// Row 4: Custom crisp checkboxes (2x2 grid)
	chkH := a.scale(28)
	a.drawCheckbox(memDC, rect{colL, row4Y, colL + colW, row4Y + chkH}, "opt_minimized", "Iniciar minimizado en la barra de tareas", a.optStartMinimized)
	a.drawCheckbox(memDC, rect{colR, row4Y, colR + colW, row4Y + chkH}, "opt_sound", "Sonido al recibir solicitud de control", a.optSoundOnReq)
	a.drawCheckbox(memDC, rect{colL, row4Y + a.scale(34), cardRC.Right - a.scale(22), row4Y + a.scale(34) + chkH}, "opt_allow_disc", "Mostrar botón Desconectar en el cliente durante la sesión", a.optAllowDisc)

	// Row 5: Logo selector with live alpha-blended preview
	drawLine(memDC, colL, row5Y-a.scale(10), cardRC.Right-a.scale(22), row5Y-a.scale(10), rgb(244, 244, 245))

	boxSz := a.scale(42)
	boxRC := rect{colL, row5Y, colL + boxSz, row5Y + boxSz}
	drawRoundedCard(memDC, boxRC, a.scale(6), rgb(246, 247, 249), rgb(228, 228, 231))
	if a.logoPreview != nil {
		inner := rect{boxRC.Left + a.scale(4), boxRC.Top + a.scale(4), boxRC.Right - a.scale(4), boxRC.Bottom - a.scale(4)}
		a.logoPreview.DrawFit(memDC, inner)
	}

	pickW := a.scale(124)
	remW := a.scale(70)
	btnH := a.scale(32)
	btnY := row5Y + (boxSz-btnH)/2
	rightEdge := cardRC.Right - a.scale(22)

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
	drawFlatButton(memDC, pickRC, a.scale(5), pickBg, rgb(212, 212, 216), rgb(24, 24, 27), "Elegir logo...")

	logoTxtLeft := boxRC.Right + a.scale(12)
	logoTxtRight := pickRC.Left - a.scale(10)
	procSelectObject.Call(memDC, a.fontBtn)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))
	lt1 := rect{logoTxtLeft, row5Y + a.scale(2), logoTxtRight, row5Y + a.scale(20)}
	drawText(memDC, "Logo del cliente (opcional · PNG o JPG)", &lt1, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

	procSelectObject.Call(memDC, a.fontSmall)
	procSetTextColor.Call(memDC, rgb(113, 113, 122))
	logoSub := "Sin logo (predeterminado)"
	if a.logoName != "" {
		logoSub = a.logoName + " (se mostrará en la ventana y barra de tareas)"
	}
	lt2 := rect{logoTxtLeft, row5Y + a.scale(22), logoTxtRight, row5Y + a.scale(40)}
	drawText(memDC, logoSub, &lt2, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

	// Footer inside card: TLS info + Save button
	footerH := a.scale(58)
	footerTop := cardRC.Bottom - footerH
	if footerTop < row5Y+boxSz+a.scale(12) {
		footerTop = row5Y + boxSz + a.scale(12)
	}
	drawLine(memDC, cardRC.Left+1, footerTop, cardRC.Right-1, footerTop, rgb(228, 228, 231))

	saveW := a.scale(220)
	saveH := a.scale(36)
	saveRC := rect{rightEdge - saveW, footerTop + (footerH-saveH)/2, rightEdge, footerTop + (footerH+saveH)/2}
	saveIdx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "save_client", rc: saveRC})

	procSelectObject.Call(memDC, a.fontSmall)
	procSetTextColor.Call(memDC, rgb(113, 113, 122))
	footTxt := fmt.Sprintf("TLS 1.3 · %s · Mismo .exe para todos los equipos", a.cfg.CertFingerprint)
	fRC := rect{colL, footerTop, saveRC.Left - a.scale(12), footerTop + footerH}
	drawText(memDC, footTxt, &fRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

	saveBg := rgb(24, 24, 27)
	if a.hoverIdx == saveIdx {
		saveBg = rgb(39, 39, 42)
	}
	procSelectObject.Call(memDC, a.fontBtn)
	drawFlatButton(memDC, saveRC, a.scale(6), saveBg, saveBg, rgb(255, 255, 255), "Guardar ClienteRemoto.exe...")
}

func (a *AdminWindow) drawCheckbox(hdc uintptr, rc rect, kind, label string, checked bool) {
	idx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: kind, rc: rc})

	boxSz := a.scale(16)
	boxY := rc.Top + (rc.Bottom-rc.Top-boxSz)/2
	boxRC := rect{rc.Left, boxY, rc.Left + boxSz, boxY + boxSz}

	if checked {
		drawFlatButton(hdc, boxRC, a.scale(4), rgb(24, 24, 27), rgb(24, 24, 27), rgb(255, 255, 255), "✓")
	} else {
		bg := rgb(255, 255, 255)
		if a.hoverIdx == idx {
			bg = rgb(244, 244, 245)
		}
		drawRoundedCard(hdc, boxRC, a.scale(4), bg, rgb(161, 161, 170))
	}

	procSelectObject.Call(hdc, a.fontBody)
	procSetTextColor.Call(hdc, rgb(24, 24, 27))
	txtRC := rect{boxRC.Right + a.scale(9), rc.Top, rc.Right, rc.Bottom}
	drawText(hdc, label, &txtRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)
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

	case "copy_addr":
		addr := a.primaryServerAddr()
		if copyTextToClipboard(hwnd, addr) {
			a.mu.Lock()
			a.noticeText = "Dirección copiada: " + addr
			a.noticeIsError = false
			a.mu.Unlock()
			a.syncBuilderControls(hwnd)
			procInvalidateRect.Call(hwnd, 0, 0)
		}

	case "opt_minimized":
		a.optStartMinimized = !a.optStartMinimized
		procInvalidateRect.Call(hwnd, 0, 0)

	case "opt_sound":
		a.optSoundOnReq = !a.optSoundOnReq
		procInvalidateRect.Call(hwnd, 0, 0)

	case "opt_allow_disc":
		a.optAllowDisc = !a.optAllowDisc
		procInvalidateRect.Call(hwnd, 0, 0)

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
		a.syncBuilderControls(hwnd)
		procInvalidateRect.Call(hwnd, 0, 0)

	case "close_notice":
		a.mu.Lock()
		a.noticeText = ""
		a.mu.Unlock()
		a.syncBuilderControls(hwnd)
		procInvalidateRect.Call(hwnd, 0, 0)

	case "row":
		cid := hit.clientID
		if a.callbacks.OnConnect != nil {
			go func() {
				if err := a.callbacks.OnConnect(cid); err != nil {
					a.mu.Lock()
					a.noticeText = err.Error()
					a.noticeIsError = true
					a.mu.Unlock()
					procPostMessageW.Call(hwnd, wmAdminRefresh, 0, 0)
				}
			}()
		}
	}
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
	addr := strings.TrimSpace(getControlText(a.ctrlAddr))
	if addr == "" {
		ShowMessageBox(hwnd, "Dirección requerida", "Introduce la dirección IP y puerto del servidor (ej. 192.168.1.50:9443).", true)
		return
	}

	timeoutSec, err := strconv.Atoi(strings.TrimSpace(getControlText(a.ctrlTimeout)))
	if err != nil || timeoutSec < 10 || timeoutSec > 300 {
		ShowMessageBox(hwnd, "Tiempo inválido", "Introduce un tiempo de espera entre 10 y 300 segundos.", true)
		return
	}

	qualSel, _, _ := procSendMessageW.Call(a.ctrlQual, cbGetCurSel, 0, 0)
	defaultQual := qualMedVal
	switch int32(qualSel) {
	case 1:
		defaultQual = qualHiVal
	case 2:
		defaultQual = qualLowVal
	}

	destPath, ok := ShowSaveExeDialog(hwnd, "ClienteRemoto.exe")
	if !ok || destPath == "" {
		return
	}

	params := ClientGenParams{
		ServerAddr:        addr,
		LogoPNG:           a.logoPNG,
		AppTitle:          strings.TrimSpace(getControlText(a.ctrlTitle)),
		PromptMessage:     strings.TrimSpace(getControlText(a.ctrlPrompt)),
		ConsentTimeoutSec: timeoutSec,
		DefaultQuality:    defaultQual,
		StartMinimized:    a.optStartMinimized,
		HideDisconnect:    !a.optAllowDisc,
		MuteSound:         !a.optSoundOnReq,
	}

	if a.callbacks.OnGenerateClient != nil {
		if err := a.callbacks.OnGenerateClient(params, destPath); err != nil {
			ShowMessageBox(hwnd, "Error al generar cliente", err.Error(), true)
			return
		}
	}

	a.mu.Lock()
	a.noticeText = fmt.Sprintf("Cliente guardado en %s (%s).", filepath.Base(destPath), addr)
	a.noticeIsError = false
	a.mu.Unlock()
	a.syncBuilderControls(hwnd)
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
			mmi.PtMinTrackSize.X = a.scale(680)
			mmi.PtMinTrackSize.Y = a.scale(520)
		}
		return 0

	case wmSize:
		a.syncBuilderControls(hwnd)
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0

	case wmAdminRefresh:
		a.syncBuilderControls(hwnd)
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
		a.syncBuilderControls(hwnd)
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0

	case wmAdminCloseViewer:
		a.mu.Lock()
		v := a.viewer
		a.mu.Unlock()
		if v != nil {
			v.CloseUI()
		}
		a.syncBuilderControls(hwnd)
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

func copyTextToClipboard(hwnd uintptr, text string) bool {
	u16, err := syscall.UTF16FromString(text)
	if err != nil || len(u16) == 0 {
		return false
	}
	if ok, _, _ := procOpenClipboard.Call(hwnd); ok == 0 {
		return false
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()

	byteLen := uintptr(len(u16) * 2)
	hMem, _, _ := procGlobalAlloc.Call(gmemMoveable, byteLen)
	if hMem == 0 {
		return false
	}
	locked, _, _ := procGlobalLock.Call(hMem)
	if locked == 0 {
		return false
	}
	dstPtr := *(*unsafe.Pointer)(unsafe.Pointer(&locked))
	dst := unsafe.Slice((*uint16)(dstPtr), len(u16))
	copy(dst, u16)
	procGlobalUnlock.Call(hMem)

	res, _, _ := procSetClipboardData.Call(cfUnicodeText, hMem)
	return res != 0
}
