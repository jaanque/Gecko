//go:build windows

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"remote-control/internal/admin"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
	"remote-control/internal/win"
)

func main() {
	win.EnableDPIAwareness()

	agentPort := flag.Int("agent-port", 9443, "Puerto TLS 1.3 donde escuchan los clientes")
	dataDir := flag.String("data-dir", "", "Carpeta donde guardar certificados y token (por defecto: ./data junto al ejecutable)")
	flag.Parse()

	resolvedDataDir := *dataDir
	if resolvedDataDir == "" {
		if exe, err := os.Executable(); err == nil {
			resolvedDataDir = filepath.Join(filepath.Dir(exe), "data")
		} else {
			resolvedDataDir = "data"
		}
	}

	identity, err := security.LoadOrCreateIdentity(resolvedDataDir)
	if err != nil {
		win.ShowMessageBox(0, "Control Remoto · Error", fmt.Sprintf("No se pudo inicializar el certificado TLS:\n%v", err), true)
		return
	}

	hub := admin.NewHub(identity, *agentPort)

	agentAddr := fmt.Sprintf("0.0.0.0:%d", *agentPort)
	agentListener, err := tls.Listen("tcp", agentAddr, security.ServerTLSConfig(identity.Certificate))
	if err != nil {
		win.ShowMessageBox(0, "Control Remoto · Error", fmt.Sprintf("No se pudo abrir el puerto %d:\n%v\n\nComprueba que no haya otra instancia abierta.", *agentPort, err), true)
		return
	}
	defer agentListener.Close()

	// Build suggested LAN addresses for the Generate Client modal.
	ips := admin.DetectLocalIPs()
	suggested := make([]string, 0, len(ips)+1)
	for _, ip := range ips {
		suggested = append(suggested, fmt.Sprintf("%s:%d", ip, *agentPort))
	}
	suggested = append(suggested, fmt.Sprintf("127.0.0.1:%d", *agentPort))

	template := embeddedClientTemplate()

	window := win.NewAdminWindow(win.AdminConfig{
		AgentPort:       *agentPort,
		SuggestedAddrs:  suggested,
		CertFingerprint: hub.CertFingerprintShort(),
	}, win.AdminCallbacks{
		OnConnect: func(clientID string) error {
			return hub.RequestConnect(clientID)
		},
		OnCancelPending: func() {
			hub.DisconnectSession("Cancelado por el administrador")
		},
		OnDisconnect: func() {
			hub.DisconnectSession("Desconectado por el administrador")
		},
		OnMouse: func(ev protocol.MouseEvent) {
			hub.SendMouse(ev)
		},
		OnKey: func(ev protocol.KeyEvent) {
			hub.SendKey(ev)
		},
		OnFrameAck: func(frameID uint32, sentUnixNano int64) {
			hub.SendFrameAck(frameID, sentUnixNano)
		},
		OnQuality: func(quality int) {
			hub.SendStreamConfig(quality, true)
		},
		OnSelectMonitor: func(monitorIndex int) {
			hub.SendSelectMonitor(monitorIndex)
		},
		OnGenerateClient: func(params win.ClientGenParams, destPath string) error {
			built, err := hub.GenerateClientBinary(template, admin.ClientBuildOptions{
				ServerAddr:        params.ServerAddr,
				LogoPNG:           params.LogoPNG,
				AppTitle:          params.AppTitle,
				PromptMessage:     params.PromptMessage,
				ConsentTimeoutSec: params.ConsentTimeoutSec,
				DefaultQuality:    params.DefaultQuality,
				StartMinimized:    params.StartMinimized,
				HideDisconnect:    params.HideDisconnect,
				MuteSound:         params.MuteSound,
			})
			if err != nil {
				return err
			}
			return os.WriteFile(destPath, built, 0o755)
		},
		OnClose: func() {
			hub.DisconnectSession("Administrador cerrado")
			_ = agentListener.Close()
		},
	})

	hub.SetEvents(admin.HubEvents{
		OnClientsChanged: func(clients []admin.ClientInfo) {
			rows := make([]win.AdminClientRow, 0, len(clients))
			for _, c := range clients {
				rows = append(rows, win.AdminClientRow{
					ID:           c.ID,
					Hostname:     c.Hostname,
					IP:           c.IP,
					Width:        c.Width,
					Height:       c.Height,
					MonitorCount: c.MonitorCount,
					Status:       c.Status,
				})
			}
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
		OnSessionEnded: func(_, hostname, reason string) {
			window.NotifyEnded(hostname, reason)
		},
		OnFrame: func(sessionID string, frameData []byte) {
			window.PushFrame(sessionID, frameData)
		},
		OnLatency: func(rttMs int) {
			window.SetLatency(rttMs)
		},
	})

	go func() {
		<-window.Ready()
		_ = hub.ServeAgents(agentListener)
	}()

	_ = window.Run()
}
