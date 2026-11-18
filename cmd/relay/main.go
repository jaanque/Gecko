package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"remote-control/internal/relay"
	"remote-control/internal/security"
)

func main() {
	defaultListen := envOr("RELAY_LISTEN", ":9443")
	defaultData := envOr("RELAY_DATA_DIR", "")
	defaultPublic := envOr("RELAY_PUBLIC_ADDR", "")

	listenAddr := flag.String("listen", defaultListen, "Dirección y puerto TCP (TLS 1.3) donde escucha el servidor puente")
	dataDir := flag.String("data-dir", defaultData, "Carpeta donde guardar certificados y tokens (por defecto: ./data junto al ejecutable)")
	publicAddr := flag.String("public-addr", defaultPublic, "Dirección pública opcional (ej. remoto.tudominio.com:9443) para incluirla en la clave")
	flag.Parse()

	resolvedData := *dataDir
	if resolvedData == "" {
		if exe, err := os.Executable(); err == nil {
			resolvedData = filepath.Join(filepath.Dir(exe), "data")
		} else {
			resolvedData = "data"
		}
	}

	identity, err := security.LoadOrCreateIdentity(resolvedData)
	if err != nil {
		log.Fatalf("Error inicializando identidad TLS: %v", err)
	}

	key, err := security.EncodeProfileKey(security.RelayProfile{
		ServerAddr:  *publicAddr,
		CertSHA256:  identity.CertSHA256,
		AdminToken:  identity.AdminToken,
		ClientToken: identity.ClientToken,
	})
	if err != nil {
		log.Fatalf("Error generando clave de conexión: %v", err)
	}

	ln, err := tls.Listen("tcp", *listenAddr, security.ServerTLSConfig(identity.Certificate))
	if err != nil {
		log.Fatalf("No se pudo abrir %s: %v", *listenAddr, err)
	}
	defer ln.Close()

	srv := relay.NewServer(identity)

	fmt.Println("================================================================")
	fmt.Println("  Control Remoto · Servidor Puente (Relay TLS 1.3)")
	fmt.Println("================================================================")
	fmt.Printf("  Escuchando en:  %s\n", *listenAddr)
	if *publicAddr != "" {
		fmt.Printf("  Dirección pub.: %s\n", *publicAddr)
	}
	fmt.Printf("  Datos TLS:      %s\n", resolvedData)
	fmt.Printf("  Huella SHA-256: %s\n", security.ShortFingerprint(identity.CertSHA256))
	fmt.Println("")
	fmt.Println("  Clave para pegar en AdminRemoto.exe (pestaña Servidor):")
	fmt.Printf("  %s\n", key)
	fmt.Println("================================================================")

	go func() {
		if err := srv.Serve(ln); err != nil {
			log.Printf("Servidor puente detenido: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\nCerrando servidor puente...")
	_ = ln.Close()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
