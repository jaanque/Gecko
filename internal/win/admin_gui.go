//go:build windows

package win

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"remote-control/internal/protocol"
	"remote-control/internal/security"
)

var (
	procGetWindowTextW    = user32GUI.NewProc("GetWindowTextW")
	procGetWindowTextLenW = user32GUI.NewProc("GetWindowTextLengthW")
	procSetWindowTextW    = user32GUI.NewProc("SetWindowTextW")
	procSendMessageW      = user32GUI.NewProc("SendMessageW")
)

const (
	wsClipChildren = 0x02000000
	wsTabStop      = 0x00010000
	wsBorder       = 0x00800000
	wsExClientEdge = 0x00000200
	esAutoHScroll  = 0x0080

	wmSetFont     = 0x0030
	enChange      = 0x0300
	emSetCueBaner = 0x1501

	wmAdminRefresh     = 0x8002
	wmAdminOpenViewer  = 0x8003
	wmAdminCloseViewer = 0x8004

	adminTabDevices = 0
	adminTabBuilder = 1
	adminTabServer  = 2

	idSearchBox = 2001
	idSrvAddr   = 2002
	idSrvKey    = 2003
)

// AdminConfig holds initial server configuration state.
type AdminConfig struct {
	HasProfile bool
	Profile    security.RelayProfile
}

// AdminCallbacks wires user actions from the Admin UI to the Relay client.
type AdminCallbacks struct {
	OnConnect        func(clientID string) error
	OnRevokeClient   func(credentialID, clientID string) error
	OnCancelPending  func()
	OnDisconnect     func()
	OnMouse          func(ev protocol.MouseEvent)
	OnKey            func(ev protocol.KeyEvent)
	OnFrameAck       func(frameID uint32)
	OnQuality        func(quality int)
	OnSelectMonitor  func(monitorIndex int)
	OnClipboard      func(text string)
	OnSystemAction   func(action string)
	OnSendFile       func(path string) error
	OnGenerateClient func(logoPNG []byte, lang string, destPath string) error
	OnSaveServer     func(p security.RelayProfile) error
	OnClose          func()
}

// AdminClientRow represents a single connected remote PC in the table.
type AdminClientRow struct {
	ID           string
	CredentialID string
	Hostname     string
	Username     string
	Version      string
	IP           string
	Width        int
	Height       int
	MonitorCount int
	Status       string // "online", "pending", "active"
}

type adminHitRegion struct {
	kind     string // "tab_devices", "tab_builder", "tab_server", "row", "revoke", "save_server", etc.
	clientID string
	credID   string
	hostname string
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

	// Native controls for "Equipos" & "Servidor" tabs
	ctrlSearch  uintptr
	searchQuery string
	ctrlSrvAddr uintptr
	ctrlSrvKey  uintptr

	// Builder state for "Crear cliente" tab
	logoPNG     []byte
	logoName    string
	logoPreview *logoGDI
	clientLang  string // "ca", "es", "en"
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
		clientLang:  "ca",
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

// PushCursor forwards an incoming MsgCursor packet to the active viewer window.
func (a *AdminWindow) PushCursor(pkt protocol.CursorPacket) {
	a.mu.Lock()
	v := a.viewer
	a.mu.Unlock()
	if v != nil {
		v.ApplyRemoteCursor(pkt)
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

// PushClipboard forwards clipboard text received from the remote client to the active viewer.
func (a *AdminWindow) PushClipboard(text string) {
	a.mu.Lock()
	v := a.viewer
	a.mu.Unlock()
	if v != nil {
		v.ApplyRemoteClipboard(text)
	}
}

// SetFileStatus forwards a file transfer progress/completion status to the active viewer.
func (a *AdminWindow) SetFileStatus(name string, ok bool, detail string) {
	a.mu.Lock()
	v := a.viewer
	a.mu.Unlock()
	if v != nil {
		v.SetFileStatus(name, ok, detail)
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
		OnClipboard:     a.callbacks.OnClipboard,
		OnSystemAction:  a.callbacks.OnSystemAction,
		OnSendFile:      a.callbacks.OnSendFile,
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

	emptyU16, _ := syscall.UTF16PtrFromString("")
	a.ctrlSearch, _, _ = procCreateWindowExW.Call(
		wsExClientEdge, uintptr(unsafe.Pointer(editClass)), uintptr(unsafe.Pointer(emptyU16)),
		wsChild|wsTabStop|wsBorder|esAutoHScroll,
		0, 0, 400, uintptr(a.scale(28)),
		parent, idSearchBox, a.hInst, 0,
	)
	procSendMessageW.Call(a.ctrlSearch, wmSetFont, a.fontBody, 1)
	if cue, err := syscall.UTF16PtrFromString("Buscar equipo..."); err == nil {
		procSendMessageW.Call(a.ctrlSearch, emSetCueBaner, 1, uintptr(unsafe.Pointer(cue)))
	}

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
