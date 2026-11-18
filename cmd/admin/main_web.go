//go:build !windows

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
	"remote-control/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "Mostrar versión y salir")
	dataDir := flag.String("data-dir", "", "Carpeta donde guardar la configuración del servidor puente (por defecto: ./data junto al ejecutable)")
	listenAddr := flag.String("listen", "127.0.0.1:0", "Dirección local donde servir la consola web")
	langFlag := flag.String("lang", "", "Idioma inicial de la interfaz (ca, es, en)")
	noBrowser := flag.Bool("no-browser", false, "No abrir el navegador automáticamente")
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
	srv, err := adminweb.NewServer(relayClient, adminweb.Config{
		ListenAddr: *listenAddr,
		OpenURL:    !*noBrowser,
		DefaultLng: i18n.Normalize(*langFlag, i18n.Detect(i18n.LangCA)),
		Templates: adminweb.Templates{
			Windows: embeddedClientTemplate(),
			Linux:   embeddedLinuxClientTemplate(),
			Darwin:  embeddedDarwinClientTemplate(),
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
}
