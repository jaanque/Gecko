package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"remote-control/internal/admin"
	"remote-control/internal/security"
)

func main() {
	agentPort := flag.Int("agent-port", 9443, "Puerto TLS 1.3 donde escuchan los clientes")
	uiPort := flag.Int("ui-port", 8090, "Puerto local (127.0.0.1) para el panel web de administración")
	dataDir := flag.String("data-dir", "", "Carpeta donde guardar certificados y token (por defecto: ./data junto al ejecutable)")
	noBrowser := flag.Bool("no-browser", false, "No abrir automáticamente el navegador al iniciar")
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
		log.Fatalf("Error inicializando seguridad TLS: %v", err)
	}

	uiToken, err := security.RandomHex(24)
	if err != nil {
		log.Fatalf("Error generando token de sesión local: %v", err)
	}

	hub := admin.NewHub(identity)

	// Start TLS 1.3 listener for remote clients.
	agentAddr := fmt.Sprintf("0.0.0.0:%d", *agentPort)
	agentListener, err := tls.Listen("tcp", agentAddr, security.ServerTLSConfig(identity.Certificate))
	if err != nil {
		log.Fatalf("No se pudo abrir el puerto %d para clientes: %v", *agentPort, err)
	}
	defer agentListener.Close()

	// Bind local Admin UI strictly to 127.0.0.1.
	uiListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *uiPort))
	if err != nil {
		uiListener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatalf("No se pudo abrir el puerto local del panel: %v", err)
		}
	}
	defer uiListener.Close()

	actualUIPort := uiListener.Addr().(*net.TCPAddr).Port
	panelURL := fmt.Sprintf("http://127.0.0.1:%d/?token=%s", actualUIPort, uiToken)

	webServer := admin.NewWebServer(
		hub,
		identity,
		embeddedClientTemplate(),
		*agentPort,
		uiToken,
		staticWebFS(),
	)

	httpSrv := &http.Server{
		Handler:           webServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := hub.ServeAgents(agentListener); err != nil {
			log.Printf("Error en servidor de clientes: %v", err)
		}
	}()

	go func() {
		if err := httpSrv.Serve(uiListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Error en panel web local: %v", err)
		}
	}()

	fmt.Println("====================================================")
	fmt.Println("  Control Remoto · Panel de Administración")
	fmt.Println("====================================================")
	fmt.Printf("  Clientes (TLS 1.3): 0.0.0.0:%d\n", *agentPort)
	if ips := admin.DetectLocalIPs(); len(ips) > 0 {
		fmt.Printf("  IP sugerida LAN:    %s:%d\n", ips[0], *agentPort)
	}
	fmt.Printf("  Panel local:        %s\n", panelURL)
	fmt.Printf("  Datos TLS/Token:    %s\n", resolvedDataDir)
	fmt.Println("  Pulsa Ctrl+C para cerrar.")
	fmt.Println("====================================================")

	if !*noBrowser {
		openBrowser(panelURL)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\nCerrando servidor...")
	_ = agentListener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

func openBrowser(targetURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", targetURL)
	case "darwin":
		cmd = exec.Command("open", targetURL)
	default:
		cmd = exec.Command("xdg-open", targetURL)
	}
	_ = cmd.Start()
}
