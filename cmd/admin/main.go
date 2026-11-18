//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"remote-control/internal/admin"
	"remote-control/internal/adminweb"
	"remote-control/internal/i18n"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
	"remote-control/internal/version"
	"remote-control/internal/win"
)

func main() {
	showVersion := flag.Bool("version", false, "Mostrar versión y salir")
	dataDir := flag.String("data-dir", "", "Carpeta donde guardar la configuración del servidor puente (por defecto: ./data junto al ejecutable)")
	webMode := flag.Bool("web", false, "Usar interfaz web local en el navegador en lugar de la ventana Win32")
	listenAddr := flag.String("listen", "127.0.0.1:0", "Dirección local para el modo -web")
	langFlag := flag.String("lang", "", "Idioma inicial para el modo -web (ca, es, en)")
	noBrowser := flag.Bool("no-browser", false, "No abrir el navegador automáticamente en modo -web")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Banner("AdminRemoto"))
		return
	}

	resolvedDataDir := *dataDir
	if resolvedDataDir == "" {
		if exe, err := os.Executable(); err == nil {
			resolvedDataDir = filepath.Join(filepath.Dir(exe), "data")
		} else {
			resolvedDataDir = "data"
		}
	}

	relayClient := admin.NewRelayClient(resolvedDataDir)
	winTemplate := embeddedClientTemplate()
	linuxTemplate := embeddedLinuxClientTemplate()
	darwinTemplate := embeddedDarwinClientTemplate()

	if *webMode {
		srv, err := adminweb.NewServer(relayClient, adminweb.Config{
			ListenAddr: *listenAddr,
			OpenURL:    !*noBrowser,
			DefaultLng: i18n.Normalize(*langFlag, i18n.Detect(i18n.LangCA)),
			Templates: adminweb.Templates{
				Windows: winTemplate,
				Linux:   linuxTemplate,
				Darwin:  darwinTemplate,
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error iniciando Admin Web: %v\n", err)
			os.Exit(1)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := srv.Run(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Admin Web detenido con error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	win.EnableDPIAwareness()
	prof, hasProf := relayClient.Profile()

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
		OnGenerateClient: func(platform string, logoPNG []byte, destPath string) error {
			tmpl := winTemplate
			if platform == "linux" {
				tmpl = linuxTemplate
			} else if platform == "darwin" {
				tmpl = darwinTemplate
			}
			label := filepath.Base(destPath)
			built, _, err := relayClient.GenerateClientBinaryWithLabel(tmpl, logoPNG, label)
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
			rows := make([]win.AdminClientRow, 0, len(clients))
			for _, c := range clients {
				rows = append(rows, win.AdminClientRow{
					ID:           c.ID,
					CredentialID: c.CredentialID,
					Hostname:     c.Hostname,
					OS:           c.OS,
					Username:     c.Username,
					Version:      c.Version,
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
