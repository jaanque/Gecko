//go:build windows

package win

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"remote-control/internal/protocol"
)

var (
	procSendMessageW        = user32GUI.NewProc("SendMessageW")
	procGetWindowTextW      = user32GUI.NewProc("GetWindowTextW")
	procGetWindowTextLenW   = user32GUI.NewProc("GetWindowTextLengthW")
	procEnableWindow        = user32GUI.NewProc("EnableWindow")
	procSetForegroundWindow = user32GUI.NewProc("SetForegroundWindow")
	procGetWindowRect       = user32GUI.NewProc("GetWindowRect")
)

const (
	wmAdminRefresh     = 0x8010
	wmAdminOpenViewer  = 0x8011
	wmAdminCloseViewer = 0x8012

	wmSetFont       = 0x0030
	wmCtlColorEdit  = 0x0133
	wmCtlColorStat  = 0x0138
	cbAddString     = 0x0143
	cbSetCurSel     = 0x014E
	cbsDropdown     = 0x0002
	cbsAutoHScroll  = 0x0040
	wsVScroll       = 0x00200000
	wsBorder        = 0x00800000
	wsTabStop       = 0x00010000
	esAutoHScroll   = 0x0080
	wsExDlgModalFrm = 0x00000001

	idGenCombo  = 2001
	idGenCancel = 2002
	idGenSave   = 2003
)

// AdminClientRow holds the display data for one online PC in the Admin list.
type AdminClientRow struct {
	ID       string
	Hostname string
	IP       string
	Width    int
	Height   int
	Status   string // "online", "pending", "active"
}

// AdminCallbacks connects the native desktop Admin GUI to the Hub.
type AdminCallbacks struct {
	OnConnect        func(clientID string) error
	OnCancelPending  func()
	OnDisconnect     func()
	OnMouse          func(ev protocol.MouseEvent)
	OnKey            func(ev protocol.KeyEvent)
	OnQuality        func(quality int)
	OnGenerateClient func(serverAddr, destPath string) error
	OnClose          func()
}

// AdminConfig configures static header info for the Admin desktop window.
type AdminConfig struct {
	AgentPort       int
	SuggestedAddrs  []string // e.g. ["192.168.0.10:9443", "127.0.0.1:9443"]
	CertFingerprint string
}

type adminHitRegion struct {
	kind     string // "gen", "cancel_pending", "close_notice", "row"
	clientID string
	rc       rect
}

type pendingOpenSession struct {
	sessionID string
	hostname  string
	width     int
	height    int
}

// AdminWindow manages the native Windows desktop Administration UI.
type AdminWindow struct {
	cfg       AdminConfig
	callbacks AdminCallbacks

	mu             sync.Mutex
	hwnd           uintptr
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
	fontSub    uintptr
	fontTitle  uintptr
	fontBody   uintptr
	fontRow    uintptr
	fontMono   uintptr
	fontBtn    uintptr
	bgWhiteBr  uintptr
	hits       []adminHitRegion
	hoverIdx   int
	scrollY    int32
	maxScrollY int32

	// Generate-client modal state
	genClassName *uint16
	genHwnd      uintptr
	genCombo     uintptr
	genBtnCancel uintptr
	genBtnSave   uintptr
}

var (
	activeAdminMu sync.Mutex
	activeAdmin   *AdminWindow
)

// NewAdminWindow creates a new desktop AdminWindow. Call Run() on the main goroutine.
func NewAdminWindow(cfg AdminConfig, cb AdminCallbacks) *AdminWindow {
	return &AdminWindow{
		cfg:       cfg,
		callbacks: cb,
		dpi:       96,
		hoverIdx:  -1,
		readyCh:   make(chan struct{}),
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
func (a *AdminWindow) OpenSession(sessionID, hostname string, width, height int) {
	a.mu.Lock()
	a.pendingHost = ""
	a.noticeText = ""
	a.openReq = &pendingOpenSession{
		sessionID: sessionID,
		hostname:  hostname,
		width:     width,
		height:    height,
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
	a.bgWhiteBr, _, _ = procCreateSolidBrush.Call(rgb(255, 255, 255))
	defer procDeleteObject.Call(a.bgWhiteBr)

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

	// Register Generate-Client modal dialog window class.
	a.genClassName, _ = syscall.UTF16PtrFromString("RemoteAdminGenModalClass")
	gwc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   syscall.NewCallback(genModalWndProc),
		HInstance:     hInst,
		HCursor:       a.cursorArr,
		HbrBackground: a.bgWhiteBr,
		LpszClassName: a.genClassName,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&gwc)))

	// Create viewer controller on this UI thread.
	v := newRemoteViewer(hInst, ViewerCallbacks{
		OnMouse:      a.callbacks.OnMouse,
		OnKey:        a.callbacks.OnKey,
		OnQuality:    a.callbacks.OnQuality,
		OnDisconnect: a.callbacks.OnDisconnect,
	})
	a.mu.Lock()
	a.viewer = v
	a.mu.Unlock()

	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow,
		100, 100, 720, 480,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return err
	}

	a.dpi = getWindowDPI(hwnd)
	a.initFonts()
	defer a.destroyFonts()

	winW := a.scale(740)
	winH := a.scale(500)
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
	a.fontBrand = createFont(a.scale(19), 600, face)
	a.fontSub = createFont(a.scale(12), 400, mono)
	a.fontTitle = createFont(a.scale(15), 600, face)
	a.fontBody = createFont(a.scale(14), 400, face)
	a.fontRow = createFont(a.scale(15), 600, face)
	a.fontMono = createFont(a.scale(12), 400, mono)
	a.fontBtn = createFont(a.scale(13), 600, face)
}

func (a *AdminWindow) destroyFonts() {
	for _, f := range []*uintptr{&a.fontBrand, &a.fontSub, &a.fontTitle, &a.fontBody, &a.fontRow, &a.fontMono, &a.fontBtn} {
		if *f != 0 {
			procDeleteObject.Call(*f)
			*f = 0
		}
	}
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

	// Background (#F6F7F9)
	fillSolidRect(memDC, cr, rgb(246, 247, 249))

	// 1. Topbar (white, 64px)
	barH := a.scale(64)
	pad := a.scale(22)
	fillSolidRect(memDC, rect{0, 0, cw, barH}, rgb(255, 255, 255))
	drawLine(memDC, 0, barH-1, cw, barH-1, rgb(228, 228, 231))

	// Primary button: "Generar cliente (.exe)"
	genW := a.scale(178)
	genH := a.scale(36)
	genRC := rect{cw - pad - genW, (barH - genH) / 2, cw - pad, (barH + genH) / 2}
	genIdx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "gen", rc: genRC})

	oldFont, _, _ := procSelectObject.Call(memDC, a.fontBrand)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))
	brandRC := rect{pad, a.scale(11), genRC.Left - a.scale(12), a.scale(35)}
	drawText(memDC, "Control Remoto", &brandRC, dtLeft|dtSingleLn|dtVCenter)

	subText := fmt.Sprintf("Puerto %d · TLS 1.3 · %s", a.cfg.AgentPort, a.cfg.CertFingerprint)
	if len(a.cfg.SuggestedAddrs) > 0 {
		subText = fmt.Sprintf("%s · TLS 1.3", a.cfg.SuggestedAddrs[0])
	}
	procSelectObject.Call(memDC, a.fontSub)
	procSetTextColor.Call(memDC, rgb(113, 113, 122))
	subRC := rect{pad, a.scale(36), genRC.Left - a.scale(12), a.scale(54)}
	drawText(memDC, subText, &subRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

	genBg := rgb(24, 24, 27)
	if a.hoverIdx == genIdx {
		genBg = rgb(39, 39, 42)
	}
	procSelectObject.Call(memDC, a.fontBtn)
	drawFlatButton(memDC, genRC, a.scale(6), genBg, genBg, rgb(255, 255, 255), "Generar cliente (.exe)")

	// Snapshot state
	a.mu.Lock()
	clients := make([]AdminClientRow, len(a.clients))
	copy(clients, a.clients)
	pendingHost := a.pendingHost
	noticeText := a.noticeText
	noticeIsErr := a.noticeIsError
	a.mu.Unlock()

	curY := barH + a.scale(18)

	// 2. Pending confirmation banner OR notice banner
	if pendingHost != "" {
		bH := a.scale(48)
		bRC := rect{pad, curY, cw - pad, curY + bH}
		drawRoundedCard(memDC, bRC, a.scale(7), rgb(255, 255, 255), rgb(212, 212, 216))

		cancelW := a.scale(86)
		cancelH := a.scale(30)
		cancelRC := rect{bRC.Right - a.scale(12) - cancelW, curY + (bH-cancelH)/2, bRC.Right - a.scale(12), curY + (bH+cancelH)/2}
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

		curY += bH + a.scale(14)
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

		curY += bH + a.scale(14)
	}

	// 3. Main card: Equipos en línea
	cardRC := rect{pad, curY, cw - pad, ch - pad}
	if cardRC.Bottom-cardRC.Top < a.scale(120) {
		cardRC.Bottom = cardRC.Top + a.scale(120)
	}
	drawRoundedCard(memDC, cardRC, a.scale(8), rgb(255, 255, 255), rgb(228, 228, 231))

	cardHdrH := a.scale(48)
	drawLine(memDC, cardRC.Left+1, cardRC.Top+cardHdrH, cardRC.Right-1, cardRC.Top+cardHdrH, rgb(228, 228, 231))

	procSelectObject.Call(memDC, a.fontTitle)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))
	hdrTxt := fmt.Sprintf("Equipos en línea (%d)", len(clients))
	hdrRC := rect{cardRC.Left + a.scale(18), cardRC.Top, cardRC.Right - a.scale(18), cardRC.Top + cardHdrH}
	drawText(memDC, hdrTxt, &hdrRC, dtLeft|dtSingleLn|dtVCenter)

	listTop := cardRC.Top + cardHdrH + 1
	listBottom := cardRC.Bottom - 2
	listH := listBottom - listTop

	if len(clients) == 0 {
		a.maxScrollY = 0
		a.scrollY = 0
		midY := listTop + listH/2
		procSelectObject.Call(memDC, a.fontTitle)
		procSetTextColor.Call(memDC, rgb(24, 24, 27))
		tRC := rect{cardRC.Left + a.scale(24), midY - a.scale(26), cardRC.Right - a.scale(24), midY - a.scale(2)}
		drawText(memDC, "No hay equipos conectados", &tRC, dtCenter|dtSingleLn|dtVCenter)

		procSelectObject.Call(memDC, a.fontBody)
		procSetTextColor.Call(memDC, rgb(113, 113, 122))
		dRC := rect{cardRC.Left + a.scale(24), midY + a.scale(2), cardRC.Right - a.scale(24), midY + a.scale(26)}
		drawText(memDC, "Genera ClienteRemoto.exe arriba y ejecútalo en los equipos que quieras controlar.", &dRC, dtCenter|dtSingleLn|dtVCenter|dtEndEllip)
	} else {
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

			var rowIdx int = -1
			if c.Status == "online" {
				rowIdx = len(a.hits)
				a.hits = append(a.hits, adminHitRegion{kind: "row", clientID: c.ID, rc: rowRC})
			}

			if rowIdx >= 0 && a.hoverIdx == rowIdx {
				fillSolidRect(memDC, rowRC, rgb(250, 250, 250))
			}

			if i < len(clients)-1 && ry+rowH <= listBottom {
				drawLine(memDC, cardRC.Left+1, ry+rowH-1, cardRC.Right-1, ry+rowH-1, rgb(244, 244, 245))
			}

			// Skip detailed drawing if heavily clipped at edges
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

			btnW := a.scale(88)
			btnH := a.scale(30)
			btnRC := rect{cardRC.Right - a.scale(18) - btnW, ry + (rowH-btnH)/2, cardRC.Right - a.scale(18), ry + (rowH+btnH)/2}

			statusW := a.scale(136)
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

	procSelectObject.Call(memDC, oldFont)
	procBitBlt.Call(hdc, 0, 0, uintptr(cw), uintptr(ch), memDC, 0, 0, srccopy)
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
	case "gen":
		a.openGenerateModal(hwnd)

	case "cancel_pending":
		a.mu.Lock()
		a.pendingHost = ""
		a.mu.Unlock()
		if a.callbacks.OnCancelPending != nil {
			go a.callbacks.OnCancelPending()
		}
		procInvalidateRect.Call(hwnd, 0, 0)

	case "close_notice":
		a.mu.Lock()
		a.noticeText = ""
		a.mu.Unlock()
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

// --- Native Generate Client Modal ---

func (a *AdminWindow) openGenerateModal(ownerHwnd uintptr) {
	if a.genHwnd != 0 {
		procSetForegroundWindow.Call(a.genHwnd)
		return
	}

	dlgW := a.scale(460)
	dlgH := a.scale(235)
	style := uintptr(wsOverlapped | wsCaption | wsSysMenu)
	rc := rect{0, 0, dlgW, dlgH}
	procAdjustWindowRectE.Call(uintptr(unsafe.Pointer(&rc)), style, 0, wsExDlgModalFrm)
	winW := rc.Right - rc.Left
	winH := rc.Bottom - rc.Top

	var parentRC rect
	procGetWindowRect.Call(ownerHwnd, uintptr(unsafe.Pointer(&parentRC)))
	posX := parentRC.Left + ((parentRC.Right - parentRC.Left) - winW)/2
	posY := parentRC.Top + ((parentRC.Bottom - parentRC.Top) - winH)/2

	title, _ := syscall.UTF16PtrFromString("Generar cliente (.exe)")
	dlg, _, _ := procCreateWindowExW.Call(
		wsExDlgModalFrm,
		uintptr(unsafe.Pointer(a.genClassName)),
		uintptr(unsafe.Pointer(title)),
		style,
		uintptr(posX), uintptr(posY), uintptr(winW), uintptr(winH),
		ownerHwnd, 0, a.hInst, 0,
	)
	if dlg == 0 {
		return
	}
	a.genHwnd = dlg

	pad := a.scale(22)
	comboClass, _ := syscall.UTF16PtrFromString("COMBOBOX")
	combo, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(comboClass)),
		0,
		wsChild|wsVisible|wsTabStop|wsVScroll|cbsDropdown|cbsAutoHScroll,
		uintptr(pad), uintptr(a.scale(118)), uintptr(dlgW-pad*2), uintptr(a.scale(160)),
		dlg, idGenCombo, a.hInst, 0,
	)
	a.genCombo = combo
	procSendMessageW.Call(combo, wmSetFont, a.fontMono, 1)

	for _, addr := range a.cfg.SuggestedAddrs {
		u16, _ := syscall.UTF16PtrFromString(addr)
		procSendMessageW.Call(combo, cbAddString, 0, uintptr(unsafe.Pointer(u16)))
	}
	if len(a.cfg.SuggestedAddrs) > 0 {
		procSendMessageW.Call(combo, cbSetCurSel, 0, 0)
	}

	btnClass, _ := syscall.UTF16PtrFromString("BUTTON")
	cancelTxt, _ := syscall.UTF16PtrFromString("Cancelar")
	saveTxt, _ := syscall.UTF16PtrFromString("Guardar ClienteRemoto.exe...")

	btnH := a.scale(36)
	btnY := dlgH - pad - btnH
	saveW := a.scale(210)
	cancelW := a.scale(96)
	gap := a.scale(10)

	a.genBtnSave, _, _ = procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(btnClass)), uintptr(unsafe.Pointer(saveTxt)),
		wsChild|wsVisible|bsOwnerDraw,
		uintptr(dlgW-pad-saveW), uintptr(btnY), uintptr(saveW), uintptr(btnH),
		dlg, idGenSave, a.hInst, 0,
	)
	a.genBtnCancel, _, _ = procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(btnClass)), uintptr(unsafe.Pointer(cancelTxt)),
		wsChild|wsVisible|bsOwnerDraw,
		uintptr(dlgW-pad-saveW-gap-cancelW), uintptr(btnY), uintptr(cancelW), uintptr(btnH),
		dlg, idGenCancel, a.hInst, 0,
	)

	procEnableWindow.Call(ownerHwnd, 0)
	procShowWindow.Call(dlg, swShowNormal)
	procUpdateWindow.Call(dlg)
	procSetFocus.Call(combo)
}

func (a *AdminWindow) closeGenerateModal() {
	if a.genHwnd == 0 {
		return
	}
	dlg := a.genHwnd
	a.genHwnd = 0
	a.genCombo = 0
	a.genBtnCancel = 0
	a.genBtnSave = 0

	a.mu.Lock()
	owner := a.hwnd
	a.mu.Unlock()

	if owner != 0 {
		procEnableWindow.Call(owner, 1)
	}
	procDestroyWindow.Call(dlg)
	if owner != 0 {
		procSetForegroundWindow.Call(owner)
	}
}

func (a *AdminWindow) handleGenerateSave() {
	addr := strings.TrimSpace(getControlText(a.genCombo))
	if addr == "" {
		ShowMessageBox(a.genHwnd, "Dirección requerida", "Introduce la dirección IP y puerto del servidor (ej. 192.168.1.50:9443).", true)
		return
	}

	destPath, ok := ShowSaveExeDialog(a.genHwnd, "ClienteRemoto.exe")
	if !ok || destPath == "" {
		return
	}

	if a.callbacks.OnGenerateClient != nil {
		if err := a.callbacks.OnGenerateClient(addr, destPath); err != nil {
			ShowMessageBox(a.genHwnd, "Error al generar cliente", err.Error(), true)
			return
		}
	}

	a.closeGenerateModal()

	a.mu.Lock()
	a.noticeText = fmt.Sprintf("ClienteRemoto.exe guardado (%s). Es válido para todos los equipos.", addr)
	a.noticeIsError = false
	hwnd := a.hwnd
	a.mu.Unlock()

	if hwnd != 0 {
		procInvalidateRect.Call(hwnd, 0, 0)
	}
}

func (a *AdminWindow) paintGenModal(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	var cr rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr)))
	fillSolidRect(hdc, cr, rgb(255, 255, 255))
	procSetBkMode.Call(hdc, transparentBk)

	pad := a.scale(22)
	oldFont, _, _ := procSelectObject.Call(hdc, a.fontTitle)
	procSetTextColor.Call(hdc, rgb(24, 24, 27))
	tRC := rect{pad, a.scale(18), cr.Right - pad, a.scale(42)}
	drawText(hdc, "Generar cliente (.exe)", &tRC, dtLeft|dtSingleLn|dtVCenter)

	procSelectObject.Call(hdc, a.fontBody)
	procSetTextColor.Call(hdc, rgb(82, 82, 91))
	d1RC := rect{pad, a.scale(46), cr.Right - pad, a.scale(66)}
	drawText(hdc, "Este ejecutable es el mismo para todos los equipos.", &d1RC, dtLeft|dtSingleLn|dtVCenter)
	d2RC := rect{pad, a.scale(66), cr.Right - pad, a.scale(86)}
	drawText(hdc, "Incluye cifrado TLS 1.3 con certificado fijado y token.", &d2RC, dtLeft|dtSingleLn|dtVCenter)

	procSelectObject.Call(hdc, a.fontBtn)
	procSetTextColor.Call(hdc, rgb(24, 24, 27))
	lblRC := rect{pad, a.scale(96), cr.Right - pad, a.scale(116)}
	drawText(hdc, "Dirección de este servidor (IP:Puerto)", &lblRC, dtLeft|dtSingleLn|dtVCenter)

	procSelectObject.Call(hdc, oldFont)
}

func (a *AdminWindow) drawGenModalButton(dis *drawItemStruct) {
	hdc := dis.HDC
	rc := dis.RcItem
	pressed := (dis.ItemState & odsSelect) != 0

	fillSolidRect(hdc, rc, rgb(255, 255, 255))
	oldFont, _, _ := procSelectObject.Call(hdc, a.fontBtn)
	procSetBkMode.Call(hdc, transparentBk)

	switch dis.CtlID {
	case idGenSave:
		bg := rgb(24, 24, 27)
		if pressed {
			bg = rgb(39, 39, 42)
		}
		drawFlatButton(hdc, rc, a.scale(6), bg, bg, rgb(255, 255, 255), "Guardar ClienteRemoto.exe...")
	case idGenCancel:
		bg := rgb(255, 255, 255)
		if pressed {
			bg = rgb(244, 244, 245)
		}
		drawFlatButton(hdc, rc, a.scale(6), bg, rgb(212, 212, 216), rgb(24, 24, 27), "Cancelar")
	}

	procSelectObject.Call(hdc, oldFont)
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
			mmi.PtMinTrackSize.X = a.scale(540)
			mmi.PtMinTrackSize.Y = a.scale(360)
		}
		return 0

	case wmSize, wmAdminRefresh:
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0

	case wmAdminOpenViewer:
		a.mu.Lock()
		req := a.openReq
		a.openReq = nil
		v := a.viewer
		a.mu.Unlock()
		if req != nil && v != nil {
			v.Open(req.sessionID, req.hostname, req.width, req.height)
		}
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0

	case wmAdminCloseViewer:
		a.mu.Lock()
		v := a.viewer
		a.mu.Unlock()
		if v != nil {
			v.CloseUI()
		}
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
		delta := int16(uint16((wParam >> 16) & 0xFFFF))
		if a.maxScrollY > 0 {
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
		a.closeGenerateModal()
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

func genModalWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	activeAdminMu.Lock()
	a := activeAdmin
	activeAdminMu.Unlock()

	if a == nil {
		ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	}

	switch msg {
	case wmPaint:
		a.paintGenModal(hwnd)
		return 0

	case wmDrawItem:
		if lParam != 0 {
			ptr := *(*unsafe.Pointer)(unsafe.Pointer(&lParam))
			dis := (*drawItemStruct)(ptr)
			a.drawGenModalButton(dis)
			return 1
		}

	case wmCommand:
		id := uint16(wParam & 0xFFFF)
		code := uint16((wParam >> 16) & 0xFFFF)
		if code == bnClicked {
			switch id {
			case idGenCancel:
				a.closeGenerateModal()
				return 0
			case idGenSave:
				a.handleGenerateSave()
				return 0
			}
		}

	case wmClose:
		a.closeGenerateModal()
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
