package admin

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"remote-control/internal/config"
	"remote-control/internal/security"
	"remote-control/internal/ws"
)

const sessionCookieName = "remote_admin_session"

// WebServer serves the local Admin UI, API endpoints, and WebSocket stream.
type WebServer struct {
	hub            *Hub
	identity       *security.ServerIdentity
	clientTemplate []byte
	agentPort      int
	uiToken        string
	staticFS       fs.FS
}

// NewWebServer constructs a WebServer.
func NewWebServer(hub *Hub, identity *security.ServerIdentity, clientTemplate []byte, agentPort int, uiToken string, staticFS fs.FS) *WebServer {
	return &WebServer{
		hub:            hub,
		identity:       identity,
		clientTemplate: clientTemplate,
		agentPort:      agentPort,
		uiToken:        uiToken,
		staticFS:       staticFS,
	}
}

// Handler builds the HTTP handler mux with localhost auth protection.
func (s *WebServer) Handler() http.Handler {
	mux := http.NewServeMux()
	fileServer := http.FileServer(http.FS(s.staticFS))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Accept initial bootstrap token from launch URL, store in HttpOnly cookie, then redirect cleanly.
		if tok := r.URL.Query().Get("token"); tok != "" {
			if !security.ConstantTimeTokenEqual(tok, s.uiToken) {
				http.Error(w, "Token de sesión inválido", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookieName,
				Value:    s.uiToken,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		if !s.isAuthorized(r) {
			http.Error(w, "Acceso no autorizado. Abre el panel desde el enlace iniciado por AdminRemoto.exe.", http.StatusForbidden)
			return
		}

		setSecurityHeaders(w)
		fileServer.ServeHTTP(w, r)
	})

	mux.HandleFunc("/api/info", s.handleInfo)
	mux.HandleFunc("/api/generate-client", s.handleGenerateClient)
	mux.HandleFunc("/ws", s.handleWS)

	return mux
}

func (s *WebServer) handleInfo(w http.ResponseWriter, r *http.Request) {
	if !s.isAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ips := DetectLocalIPs()
	defaultAddr := fmt.Sprintf("127.0.0.1:%d", s.agentPort)
	if len(ips) > 0 {
		defaultAddr = fmt.Sprintf("%s:%d", ips[0], s.agentPort)
	}

	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"agent_port":       s.agentPort,
		"local_ips":        ips,
		"default_address":  defaultAddr,
		"cert_fingerprint": formatShortFingerprint(s.identity.CertSHA256),
		"client_ready":     len(s.clientTemplate) > 0,
	})
}

func (s *WebServer) handleGenerateClient(w http.ResponseWriter, r *http.Request) {
	if !s.isAuthorized(r) || !isAllowedOrigin(r) {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(s.clientTemplate) == 0 {
		http.Error(w, "Plantilla de cliente no incrustada. Compila el proyecto con build.ps1.", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		ServerAddr string `json:"server_addr"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "solicitud inválida", http.StatusBadRequest)
		return
	}

	addr := strings.TrimSpace(req.ServerAddr)
	if addr == "" {
		http.Error(w, "La dirección del servidor es obligatoria", http.StatusBadRequest)
		return
	}
	// If user entered only an IP/hostname without port, append the active agent port.
	if _, _, err := net.SplitHostPort(addr); err != nil {
		if !strings.Contains(addr, ":") {
			addr = net.JoinHostPort(addr, strconv.Itoa(s.agentPort))
		} else {
			http.Error(w, "Formato inválido. Usa IP:PUERTO (ej. 192.168.1.50:9443)", http.StatusBadRequest)
			return
		}
	}

	built, err := config.AppendToBinary(s.clientTemplate, config.ClientConfig{
		ServerAddr: addr,
		CertSHA256: s.identity.CertSHA256,
		AuthToken:  s.identity.AuthToken,
	})
	if err != nil {
		http.Error(w, "Error generando el cliente: "+err.Error(), http.StatusInternalServerError)
		return
	}

	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="ClienteRemoto.exe"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(built)))
	_, _ = w.Write(built)
}

func (s *WebServer) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.isAuthorized(r) || !isAllowedOrigin(r) {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}
	conn, err := ws.Upgrade(w, r)
	if err != nil {
		return
	}
	s.hub.ServeViewer(conn)
}

func (s *WebServer) isAuthorized(r *http.Request) bool {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if security.ConstantTimeTokenEqual(c.Value, s.uiToken) {
			return true
		}
	}
	if hdr := r.Header.Get("X-Session-Token"); hdr != "" {
		if security.ConstantTimeTokenEqual(hdr, s.uiToken) {
			return true
		}
	}
	return false
}

func isAllowedOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost"
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
}

// DetectLocalIPs returns non-loopback IPv4 addresses sorted with private LAN addresses first.
func DetectLocalIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	type scoredIP struct {
		ip    string
		score int
	}
	var list []scoredIP

	for _, iface := range ifaces {
		if (iface.Flags&net.FlagUp) == 0 || (iface.Flags&net.FlagLoopback) != 0 {
			continue
		}
		nameLow := strings.ToLower(iface.Name)
		isVirtual := strings.Contains(nameLow, "vethernet") ||
			strings.Contains(nameLow, "vmware") ||
			strings.Contains(nameLow, "virtualbox") ||
			strings.Contains(nameLow, "wsl") ||
			strings.Contains(nameLow, "docker")

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			score := 10
			if ip4[0] == 192 && ip4[1] == 168 {
				score = 1
			} else if ip4[0] == 10 {
				score = 2
			} else if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
				score = 3
			}
			if isVirtual {
				score += 20
			}
			list = append(list, scoredIP{ip: ip4.String(), score: score})
		}
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score < list[j].score
		}
		return list[i].ip < list[j].ip
	})

	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, item := range list {
		if _, dup := seen[item.ip]; dup {
			continue
		}
		seen[item.ip] = struct{}{}
		out = append(out, item.ip)
	}
	return out
}

func formatShortFingerprint(hexFP string) string {
	if len(hexFP) < 16 {
		return hexFP
	}
	upper := strings.ToUpper(hexFP[:16])
	var parts []string
	for i := 0; i < len(upper); i += 4 {
		parts = append(parts, upper[i:i+4])
	}
	return strings.Join(parts, ":") + "…"
}
