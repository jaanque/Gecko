//go:build windows

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"remote-control/internal/admin"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
	"remote-control/internal/version"
	"remote-control/internal/win"
)

func main() {
	showVersion := flag.Bool("version", false, "Mostrar versión y salir")
	dataDir := flag.String("data-dir", "", "Carpeta donde guardar la configuración del servidor puente (por defecto: ./data junto al ejecutable)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Banner("AdminRemoto"))
		return
	}

	win.EnableDPIAwareness()

	resolvedDataDir := *dataDir
	if resolvedDataDir == "" {
		if exe, err := os.Executable(); err == nil {
			resolvedDataDir = filepath.Join(filepath.Dir(exe), "data")
		} else {
			resolvedDataDir = "data"
		}
	}

	relayClient := admin.NewRelayClient(resolvedDataDir)
	prof, hasProf := relayClient.Profile()
	winTemplate := embeddedClientTemplate()

	var (
		clientMu     sync.Mutex
		knownClients = make(map[string]win.AdminClientRow)
		clientOrder  []string
	)

	logSessionEvent := func(sessionID, hostname, reason string) {
		_ = os.MkdirAll(resolvedDataDir, 0o700)
		logPath := filepath.Join(resolvedDataDir, "sessions.log")
		line := fmt.Sprintf("%s | Sesión=%s | Equipo=%s | Motivo=%s\n",
			time.Now().Format("2006-01-02 15:04:05"),
			sessionID, hostname, reason)
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
		}
	}

	buildClientRows := func() []win.AdminClientRow {
		onlineRows := make([]win.AdminClientRow, 0, len(knownClients))
		offlineRows := make([]win.AdminClientRow, 0, len(knownClients))
		for _, id := range clientOrder {
			row := knownClients[id]
			if row.Status == "offline" {
				offlineRows = append(offlineRows, row)
			} else {
				onlineRows = append(onlineRows, row)
			}
		}
		return append(onlineRows, offlineRows...)
	}

	window := win.NewAdminWindow(win.AdminConfig{
		HasProfile: hasProf,
		Profile:    prof,
	}, win.AdminCallbacks{
		OnConnect: func(clientID string) error {
			return relayClient.RequestConnect(clientID)
		},
		OnRevokeClient: func(credentialID, clientID string) error {
			return relayClient.RevokeClient(credentialID, clientID)
		},
		OnCancelPending: func() {
			relayClient.DisconnectSession("Cancelado por el administrador")
		},
		OnDisconnect: func() {
			relayClient.DisconnectSession("Desconectado por el administrador")
		},
		OnMouse: func(ev protocol.MouseEvent) {
			relayClient.SendMouse(ev)
		},
		OnKey: func(ev protocol.KeyEvent) {
			relayClient.SendKey(ev)
		},
		OnFrameAck: func(frameID uint32) {
			relayClient.SendFrameAck(frameID)
		},
		OnQuality: func(quality int) {
			relayClient.SendStreamConfig(quality, true)
		},
		OnSelectMonitor: func(monitorIndex int) {
			relayClient.SendSelectMonitor(monitorIndex)
		},
		OnClipboard: func(text string) {
			relayClient.SendClipboard(text)
		},
		OnSystemAction: func(action string) {
			relayClient.SendSystemAction(action)
		},
		OnSendFile: func(path string) error {
			return relayClient.SendFile(path)
		},
		OnGenerateClient: func(logoPNG []byte, lang string, destPath string) error {
			label := filepath.Base(destPath)
			built, _, err := relayClient.GenerateClientBinary(winTemplate, logoPNG, label, lang)
			if err != nil {
				return err
			}
			return os.WriteFile(destPath, built, 0o755)
		},
		OnSaveServer: func(p security.RelayProfile) error {
			return relayClient.Configure(p)
		},
		OnClose: func() {
			relayClient.Close()
		},
	})

	relayClient.SetEvents(admin.RelayEvents{
		OnRelayStatus: func(connected bool, detail string) {
			window.SetRelayStatus(connected, detail)
		},
		OnClientsChanged: func(clients []protocol.ClientSummary) {
			clientMu.Lock()
			activeSet := make(map[string]bool, len(clients))
			for _, c := range clients {
				activeSet[c.ID] = true
				if _, exists := knownClients[c.ID]; !exists {
					clientOrder = append(clientOrder, c.ID)
				}
				knownClients[c.ID] = win.AdminClientRow{
					ID:           c.ID,
					CredentialID: c.CredentialID,
					Hostname:     c.Hostname,
					Username:     c.Username,
					Version:      c.Version,
					IP:           c.IP,
					Width:        c.Width,
					Height:       c.Height,
					MonitorCount: c.MonitorCount,
					Status:       c.Status,
				}
			}
			for id, row := range knownClients {
				if !activeSet[id] {
					row.Status = "offline"
					knownClients[id] = row
				}
			}
			rows := buildClientRows()
			clientMu.Unlock()
			window.UpdateClients(rows)
		},
		OnSessionPending: func(_, _, hostname string) {
			window.SetPending(hostname)
		},
		OnSessionStarted: func(sessionID, _, hostname string, w, h, monCount int) {
			window.OpenSession(sessionID, hostname, w, h, monCount)
		},
		OnSessionDenied: func(_, _, hostname, reason string) {
			window.NotifyDenied(hostname, reason)
		},
		OnSessionEnded: func(sessionID, hostname, reason string) {
			logSessionEvent(sessionID, hostname, reason)
			clientMu.Lock()
			for id, row := range knownClients {
				if row.Hostname == hostname && row.Status == "active" {
					row.Status = "offline"
					knownClients[id] = row
				}
			}
			rows := buildClientRows()
			clientMu.Unlock()
			window.UpdateClients(rows)
			window.NotifyEnded(hostname, reason)
		},
		OnSessionError: func(reason string) {
			window.NotifyError(reason)
		},
		OnFrame: func(sessionID string, frameData []byte) {
			window.PushFrame(sessionID, frameData)
		},
		OnLatency: func(rttMs int) {
			window.SetLatency(rttMs)
		},
		OnClipboard: func(text string) {
			window.PushClipboard(text)
		},
		OnFileStatus: func(name string, ok bool, detail string) {
			window.SetFileStatus(name, ok, detail)
		},
	})

	go func() {
		<-window.Ready()
		relayClient.Start()
	}()

	_ = window.Run()
}
