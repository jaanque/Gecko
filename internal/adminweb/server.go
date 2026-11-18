package adminweb

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"remote-control/internal/admin"
	"remote-control/internal/config"
	"remote-control/internal/i18n"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
	"remote-control/internal/version"
)

//go:embed ui.html
var uiHTML []byte

const (
	browserCodecJPEG uint8 = 0
	browserCodecRGBA uint8 = 2
)

// Templates holds embedded client templates for each supported target OS.
type Templates struct {
	Windows []byte
	Linux   []byte
	Darwin  []byte
}

// Config configures the local cross-platform Web Admin server.
type Config struct {
	ListenAddr string // defaults to "127.0.0.1:0"
	OpenURL    bool
	DefaultLng i18n.Lang
	Templates  Templates
}

// Server exposes a secure loopback-only Web UI + WebSocket bridge for admin.RelayClient.
type Server struct {
	rc    *admin.RelayClient
	cfg   Config
	token string

	mu             sync.Mutex
	relayConnected bool
	relayDetail    string
	clients        []protocol.ClientSummary
	activeSession  string
	activeHost     string
	activeW        int
	activeH        int
	activeMonCount int
	pendingHost    string
	conns          map[*wsConn]struct{}
}

// NewServer constructs a local Web Admin server wrapping rc.
func NewServer(rc *admin.RelayClient, cfg Config) (*Server, error) {
	tok, err := security.RandomHex(24)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		cfg.ListenAddr = "127.0.0.1:0"
	}
	if cfg.DefaultLng == "" {
		cfg.DefaultLng = i18n.Detect(i18n.LangCA)
	}
	s := &Server{
		rc:    rc,
		cfg:   cfg,
		token: tok,
		conns: make(map[*wsConn]struct{}),
	}
	s.wireRelayEvents()
	return s, nil
}

func (s *Server) wireRelayEvents() {
	s.rc.SetEvents(admin.RelayEvents{
		OnRelayStatus: func(connected bool, detail string) {
			s.mu.Lock()
			s.relayConnected = connected
			s.relayDetail = detail
			s.mu.Unlock()
			s.broadcastJSON(map[string]any{
				"type":      "relay_status",
				"connected": connected,
				"detail":    detail,
			})
		},
		OnClientsChanged: func(clients []protocol.ClientSummary) {
			cp := append([]protocol.ClientSummary(nil), clients...)
			s.mu.Lock()
			s.clients = cp
			s.mu.Unlock()
			s.broadcastJSON(map[string]any{
				"type":    "clients",
				"clients": cp,
			})
		},
		OnSessionPending: func(sid, cid, host string) {
			s.mu.Lock()
			s.pendingHost = host
			s.mu.Unlock()
			s.broadcastJSON(map[string]any{
				"type":       "session_pending",
				"session_id": sid,
				"client_id":  cid,
				"hostname":   host,
			})
		},
		OnSessionStarted: func(sid, cid, host string, w, h, monCount int) {
			s.mu.Lock()
			s.pendingHost = ""
			s.activeSession = sid
			s.activeHost = host
			s.activeW = w
			s.activeH = h
			s.activeMonCount = monCount
			s.mu.Unlock()
			s.broadcastJSON(map[string]any{
				"type":          "session_started",
				"session_id":    sid,
				"client_id":     cid,
				"hostname":      host,
				"width":         w,
				"height":        h,
				"monitor_count": monCount,
			})
		},
		OnSessionDenied: func(sid, _, host, reason string) {
			s.mu.Lock()
			s.pendingHost = ""
			s.mu.Unlock()
			s.broadcastJSON(map[string]any{
				"type":       "session_denied",
				"session_id": sid,
				"hostname":   host,
				"reason":     reason,
			})
		},
		OnSessionEnded: func(sid, host, reason string) {
			s.mu.Lock()
			s.pendingHost = ""
			s.activeSession = ""
			s.activeHost = ""
			s.mu.Unlock()
			s.broadcastJSON(map[string]any{
				"type":       "session_ended",
				"session_id": sid,
				"hostname":   host,
				"reason":     reason,
			})
		},
		OnSessionError: func(reason string) {
			s.mu.Lock()
			s.pendingHost = ""
			s.mu.Unlock()
			s.broadcastJSON(map[string]any{
				"type":   "session_error",
				"reason": reason,
			})
		},
		OnFrame: func(_ string, frameData []byte) {
			out, frameID, ok := transcodeFrameForBrowser(frameData)
			if !ok {
				return
			}
			s.broadcastBinary(out)
			if frameID > 0 {
				s.rc.SendFrameAck(frameID)
			}
		},
		OnLatency: func(rttMs int) {
			s.broadcastJSON(map[string]any{
				"type":   "latency",
				"rtt_ms": rttMs,
			})
		},
		OnClipboard: func(text string) {
			s.broadcastJSON(map[string]any{
				"type": "clipboard",
				"text": text,
			})
		},
		OnFileStatus: func(name string, ok bool, detail string) {
			s.broadcastJSON(map[string]any{
				"type":   "file_status",
				"name":   name,
				"ok":     ok,
				"detail": detail,
			})
		},
	})
}

// Run starts the loopback HTTP/WebSocket Admin server and blocks until ctx is canceled.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	defer ln.Close()

	url := fmt.Sprintf("http://%s/?token=%s", ln.Addr().String(), s.token)
	fmt.Printf("Gecko Admin Web (%s)\nURL: %s\n", version.String(), url)

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/api/server", s.handleSaveServer)
	mux.HandleFunc("/api/generate-client", s.handleGenerateClient)
	mux.HandleFunc("/api/upload", s.handleUploadFile)

	httpSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	s.rc.Start()
	if s.cfg.OpenURL {
		go openBrowser(url)
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		s.rc.Close()
	}()

	err = httpSrv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) checkAuth(r *http.Request) bool {
	tok := r.URL.Query().Get("token")
	if tok == "" {
		tok = r.Header.Get("X-Gecko-Token")
	}
	return len(tok) == len(s.token) && subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.checkAuth(r) {
		http.Error(w, "Unauthorized (use the full startup URL including ?token=...)", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(uiHTML)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if !strings.HasPrefix(origin, "http://127.0.0.1:") && !strings.HasPrefix(origin, "http://localhost:") {
			http.Error(w, "Forbidden Origin", http.StatusForbidden)
			return
		}
	}

	ws, err := upgradeWebSocket(w, r)
	if err != nil {
		return
	}
	defer ws.Close()

	s.mu.Lock()
	s.conns[ws] = struct{}{}
	prof, hasProf := s.rc.Profile()
	initSnap := map[string]any{
		"type":            "init",
		"version":         version.String(),
		"default_lang":    string(s.cfg.DefaultLng),
		"has_profile":     hasProf,
		"server_addr":     prof.ServerAddr,
		"relay_connected": s.relayConnected,
		"relay_detail":    s.relayDetail,
		"clients":         s.clients,
		"pending_host":    s.pendingHost,
		"active_session":  s.activeSession,
		"active_host":     s.activeHost,
		"active_width":    s.activeW,
		"active_height":   s.activeH,
		"active_monitors": s.activeMonCount,
		"templates": map[string]bool{
			"windows": len(s.cfg.Templates.Windows) > 0,
			"linux":   len(s.cfg.Templates.Linux) > 0,
			"darwin":  len(s.cfg.Templates.Darwin) > 0,
		},
	}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.conns, ws)
		s.mu.Unlock()
	}()

	if raw, err := json.Marshal(initSnap); err == nil {
		_ = ws.WriteFrame(wsOpText, raw)
	}

	for {
		op, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if op != wsOpText {
			continue
		}
		s.handleWSCommand(data)
	}
}

func (s *Server) handleWSCommand(data []byte) {
	var msg struct {
		Type         string `json:"type"`
		ClientID     string `json:"client_id"`
		CredentialID string `json:"credential_id"`
		Action       uint8  `json:"action"`
		SysAction    string `json:"sys_action"`
		Button       uint8  `json:"button"`
		X            uint16 `json:"x"`
		Y            uint16 `json:"y"`
		Delta        int16  `json:"delta"`
		VK           uint16 `json:"vk"`
		Scan         uint16 `json:"scan"`
		Flags        uint8  `json:"flags"`
		Quality      int    `json:"quality"`
		Monitor      int    `json:"monitor"`
		Text         string `json:"text"`
	}
	if json.Unmarshal(data, &msg) != nil {
		return
	}

	switch msg.Type {
	case "connect":
		if err := s.rc.RequestConnect(msg.ClientID); err != nil {
			s.broadcastJSON(map[string]any{"type": "session_error", "reason": err.Error()})
		}
	case "cancel_pending":
		s.rc.DisconnectSession("Cancelado por el administrador")
	case "disconnect":
		s.rc.DisconnectSession("Desconectado por el administrador")
	case "revoke":
		if err := s.rc.RevokeClient(msg.CredentialID, msg.ClientID); err != nil {
			s.broadcastJSON(map[string]any{"type": "session_error", "reason": err.Error()})
		}
	case "mouse":
		s.rc.SendMouse(protocol.MouseEvent{
			Action: msg.Action,
			Button: msg.Button,
			X:      msg.X,
			Y:      msg.Y,
			Delta:  msg.Delta,
		})
	case "key":
		s.rc.SendKey(protocol.KeyEvent{
			Action: msg.Action,
			VK:     msg.VK,
			Scan:   msg.Scan,
			Flags:  msg.Flags,
		})
	case "quality":
		s.rc.SendStreamConfig(msg.Quality, true)
	case "monitor":
		s.rc.SendSelectMonitor(msg.Monitor)
	case "clipboard":
		s.rc.SendClipboard(msg.Text)
	case "system_action":
		s.rc.SendSystemAction(msg.SysAction)
	}
}

func (s *Server) handleSaveServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !s.checkAuth(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		Key  string `json:"key"`
		Addr string `json:"addr"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 128*1024)).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}
	prof, err := security.DecodeProfileKey(req.Key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if addr := strings.TrimSpace(req.Addr); addr != "" {
		prof.ServerAddr = addr
	}
	if err := s.rc.Configure(prof); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "server_addr": prof.ServerAddr})
}

func (s *Server) handleGenerateClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !s.checkAuth(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseMultipartForm(12 << 20); err != nil {
		http.Error(w, "Formulario inválido", http.StatusBadRequest)
		return
	}

	platform := strings.ToLower(strings.TrimSpace(r.FormValue("platform")))
	lang := string(i18n.Normalize(r.FormValue("lang"), i18n.LangCA))
	label := strings.TrimSpace(r.FormValue("label"))

	var tmpl []byte
	var fileName string
	switch platform {
	case "windows":
		tmpl = s.cfg.Templates.Windows
		fileName = "ClientRemot.exe"
	case "linux":
		tmpl = s.cfg.Templates.Linux
		fileName = "ClientRemot"
	case "darwin":
		tmpl = s.cfg.Templates.Darwin
		fileName = "ClientRemot-macos"
	default:
		http.Error(w, "Plataforma no soportada", http.StatusBadRequest)
		return
	}

	var logoPNG []byte
	if file, _, err := r.FormFile("logo"); err == nil {
		defer file.Close()
		rawLogo, err := io.ReadAll(io.LimitReader(file, 10<<20))
		if err != nil {
			http.Error(w, "Error leyendo logo", http.StatusBadRequest)
			return
		}
		if len(rawLogo) > 0 {
			norm, err := config.NormalizeLogoBytes(rawLogo)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			logoPNG = norm
		}
	}

	if label == "" {
		label = fileName
	}
	built, _, err := s.rc.GenerateClientBinaryWithOptions(tmpl, logoPNG, label, lang)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
	_, _ = w.Write(built)
}

func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !s.checkAuth(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, protocol.MaxFileTransferSize+1024*1024)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "Archivo demasiado grande o inválido", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Archivo requerido", http.StatusBadRequest)
		return
	}
	defer file.Close()

	tmpDir, err := os.MkdirTemp("", "gecko-upload-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	safeName := protocol.SanitizeFileName(header.Filename)
	tmpPath := filepath.Join(tmpDir, safeName)
	dst, err := os.Create(tmpPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(dst, file); err != nil {
		_ = dst.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = dst.Close()

	if err := s.rc.SendFile(tmpPath); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) broadcastJSON(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.mu.Lock()
	conns := make([]*wsConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.WriteFrame(wsOpText, raw)
	}
}

func (s *Server) broadcastBinary(b []byte) {
	s.mu.Lock()
	conns := make([]*wsConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.WriteFrame(wsOpBinary, b)
	}
}

// transcodeFrameForBrowser preserves JPEG tiles (codec 0) zero-copy and expands lossless
// Zlib-BGR tiles (codec 1) into raw RGBA tiles (codec 2) so any HTML5 Canvas can render them
// directly with ImageData without extra browser libraries.
func transcodeFrameForBrowser(data []byte) ([]byte, uint32, bool) {
	if len(data) < protocol.FrameHeaderSize {
		return nil, 0, false
	}
	frameID := binary.BigEndian.Uint32(data[0:4])
	tileCount := int(binary.BigEndian.Uint16(data[8:10]))

	var out bytes.Buffer
	out.Grow(len(data))
	out.Write(data[:protocol.FrameHeaderSize])

	off := protocol.FrameHeaderSize
	var valid uint16
	var th [protocol.TileHeaderSize]byte
	for i := 0; i < tileCount; i++ {
		if off > len(data)-protocol.TileHeaderSize {
			break
		}
		tx := binary.BigEndian.Uint16(data[off : off+2])
		ty := binary.BigEndian.Uint16(data[off+2 : off+4])
		tw := binary.BigEndian.Uint16(data[off+4 : off+6])
		thVal := binary.BigEndian.Uint16(data[off+6 : off+8])
		codec := data[off+8]
		dlen := int(binary.BigEndian.Uint32(data[off+9 : off+13]))
		off += protocol.TileHeaderSize
		if dlen <= 0 || dlen > len(data)-off {
			break
		}
		payload := data[off : off+dlen]
		off += dlen

		switch codec {
		case protocol.TileCodecJPEG:
			binary.BigEndian.PutUint16(th[0:2], tx)
			binary.BigEndian.PutUint16(th[2:4], ty)
			binary.BigEndian.PutUint16(th[4:6], tw)
			binary.BigEndian.PutUint16(th[6:8], thVal)
			th[8] = browserCodecJPEG
			binary.BigEndian.PutUint32(th[9:13], uint32(len(payload)))
			out.Write(th[:])
			out.Write(payload)
			valid++

		case protocol.TileCodecZlibBGR:
			zr, err := zlib.NewReader(bytes.NewReader(payload))
			if err != nil {
				continue
			}
			expected := int(tw) * int(thVal) * 3
			bgr := make([]byte, expected)
			_, err = io.ReadFull(io.LimitReader(zr, int64(expected)+1), bgr)
			_ = zr.Close()
			if err != nil {
				continue
			}
			rgba := make([]byte, int(tw)*int(thVal)*4)
			p := 0
			for j := 0; j < len(rgba); j += 4 {
				rgba[j+0] = bgr[p+2] // R
				rgba[j+1] = bgr[p+1] // G
				rgba[j+2] = bgr[p+0] // B
				rgba[j+3] = 255      // A
				p += 3
			}
			binary.BigEndian.PutUint16(th[0:2], tx)
			binary.BigEndian.PutUint16(th[2:4], ty)
			binary.BigEndian.PutUint16(th[4:6], tw)
			binary.BigEndian.PutUint16(th[6:8], thVal)
			th[8] = browserCodecRGBA
			binary.BigEndian.PutUint32(th[9:13], uint32(len(rgba)))
			out.Write(th[:])
			out.Write(rgba)
			valid++
		}
	}

	if valid == 0 {
		return nil, frameID, false
	}
	res := out.Bytes()
	binary.BigEndian.PutUint16(res[8:10], valid)
	return res, frameID, true
}

func openBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", url).Start()
	case "linux":
		_ = exec.Command("xdg-open", url).Start()
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
}
