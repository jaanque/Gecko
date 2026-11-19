package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"remote-control/internal/relay"
	"remote-control/internal/security"
	"remote-control/internal/version"
)

func main() {
	defaultListen := envOr("RELAY_LISTEN", ":9443")
	defaultData := envOr("RELAY_DATA_DIR", "")
	defaultPublic := envOr("RELAY_PUBLIC_ADDR", "")
	defaultShowKey := os.Getenv("RELAY_SHOW_KEY") == "1" || strings.EqualFold(os.Getenv("RELAY_SHOW_KEY"), "true")

	showVersion := flag.Bool("version", false, "Mostrar versión y salir")
	listenAddr := flag.String("listen", defaultListen, "Puerto/dirección TCP TLS 1.3 (ej. :9443)")
	dataDir := flag.String("data-dir", defaultData, "Carpeta de datos TLS y tokens")
	publicAddr := flag.String("public-addr", defaultPublic, "Dominio o IP pública (ej. remoto.tudominio.com:9443)")
	showKey := flag.Bool("show-key", defaultShowKey, "Mostrar la clave maestra RMT2:... y salir (o imprimir al arrancar con RELAY_SHOW_KEY=1)")
	listClients := flag.Bool("list-clients", false, "Listar las credenciales de cliente emitidas y salir")
	revokeID := flag.String("revoke", "", "Revocar una credencial de cliente por ID (o 'default') y salir")
	healthcheck := flag.Bool("healthcheck", false, "Comprobar si el puerto del Relay responde localmente y salir")
	installService := flag.Bool("install-service", false, "Instalar RelayServidor como servicio de Windows")
	uninstallService := flag.Bool("uninstall-service", false, "Desinstalar el servicio de Windows de RelayServidor")
	serviceMode := flag.Bool("service", false, "Ejecutar como servicio de Windows (invocado por el sistema)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Banner("RelayServidor"))
		return
	}

	if *healthcheck {
		if err := runHealthcheck(*listenAddr); err != nil {
			fmt.Fprintf(os.Stderr, "healthcheck fallido: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if *installService {
		var extraArgs []string
		if *listenAddr != defaultListen {
			extraArgs = append(extraArgs, fmt.Sprintf("-listen %s", *listenAddr))
		}
		if *dataDir != "" {
			extraArgs = append(extraArgs, fmt.Sprintf("-data-dir \"%s\"", *dataDir))
		}
		if *publicAddr != "" {
			extraArgs = append(extraArgs, fmt.Sprintf("-public-addr %s", *publicAddr))
		}
		if err := relay.InstallService("GeckoRelay", "Gecko Relay Server", extraArgs); err != nil {
			log.Fatalf("Error instalando servicio: %v", err)
		}
		fmt.Println("Servicio de Windows 'GeckoRelay' instalado con inicio automático correctamente.")
		return
	}

	if *uninstallService {
		if err := relay.UninstallService("GeckoRelay"); err != nil {
			log.Fatalf("Error desinstalando servicio: %v", err)
		}
		fmt.Println("Servicio de Windows 'GeckoRelay' desinstalado correctamente.")
		return
	}

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
		log.Fatalf("Error inicializando TLS: %v", err)
	}

	if *listClients {
		clients := identity.ListIssuedClients()
		if len(clients) == 0 {
			fmt.Println("No hay credenciales de cliente individuales emitidas todavía.")
			return
		}
		for _, c := range clients {
			state := "ACTIVA"
			if c.Revoked {
				state = "REVOCADA"
			}
			fmt.Printf("%-14s %-9s %-24s %s\n", c.ID, state, c.Label, c.CreatedAt.Format(time.RFC3339))
		}
		return
	}

	if strings.TrimSpace(*revokeID) != "" {
		if err := identity.RevokeClient(*revokeID); err != nil {
			log.Fatalf("No se pudo revocar %q: %v", *revokeID, err)
		}
		fmt.Printf("Credencial %q revocada correctamente.\n", strings.TrimSpace(*revokeID))
		return
	}

	// If explicitly invoked with -show-key (not via env), print the profile key and exit cleanly.
	if *showKey && !defaultShowKey {
		port := extractPort(*listenAddr, "9443")
		resolvedAddr := resolveConnectAddr(*publicAddr, port, detectLANIP())
		key, err := encodeProfile(identity, resolvedAddr)
		if err != nil {
			log.Fatalf("Error generando clave: %v", err)
		}
		fmt.Printf("Direccion: %s\n", resolvedAddr)
		fmt.Printf("Clave:     %s\n", key)
		return
	}

	ln, err := tls.Listen("tcp", *listenAddr, security.ServerTLSConfig(identity.Certificate, identity.ClientCAs))
	if err != nil {
		log.Fatalf("No se pudo abrir %s: %v", *listenAddr, err)
	}
	defer ln.Close()

	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	lanIP := detectLANIP()
	resolvedAddr := resolveConnectAddr(*publicAddr, port, lanIP)

	fmt.Printf("Direccion: %s\n", resolvedAddr)
	if lanIP != "" {
		lanAddr := net.JoinHostPort(lanIP, port)
		if lanAddr != resolvedAddr {
			fmt.Printf("LAN:       %s\n", lanAddr)
		}
	}

	if identity.IsNew || defaultShowKey {
		key, err := encodeProfile(identity, resolvedAddr)
		if err != nil {
			log.Fatalf("Error generando clave: %v", err)
		}
		fmt.Printf("Clave:     %s\n", key)
		if identity.IsNew {
			fmt.Println("Nota: guarda esta clave en un lugar seguro. No volverá a imprimirse al reiniciar (usa -show-key para verla).")
		}
	} else {
		fmt.Println("Clave:     (oculta por seguridad; ejecuta con -show-key para mostrarla)")
	}

	srv := relay.NewServer(identity)

	if *serviceMode {
		log.Println("Iniciando RelayServidor bajo el Service Control Manager de Windows...")
		err := relay.RunAsService("GeckoRelay", func(stopCh <-chan struct{}) {
			go func() {
				if err := srv.Serve(ln); err != nil {
					log.Printf("Relay finalizado: %v", err)
				}
			}()
			<-stopCh
			log.Println("Servicio recibiendo señal de parada...")
			_ = ln.Close()
			srv.Close()
		})
		if err != nil {
			log.Fatalf("Error en despachador de servicios: %v", err)
		}
		return
	}

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if err := srv.Serve(ln); err != nil {
			log.Printf("Relay detenido: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	log.Println("Deteniendo RelayServidor de forma ordenada...")
	_ = ln.Close()
	srv.Close()
	select {
	case <-serveDone:
	case <-time.After(3 * time.Second):
	}
}

func encodeProfile(identity *security.ServerIdentity, serverAddr string) (string, error) {
	return security.EncodeProfileKey(security.RelayProfile{
		ServerAddr:    serverAddr,
		CertSHA256:    identity.CertSHA256,
		AdminToken:    identity.AdminToken,
		ClientToken:   identity.ClientToken,
		AdminCertDER:  identity.AdminCertDER,
		AdminKeyDER:   identity.AdminKeyDER,
		ClientCertDER: identity.ClientCertDER,
		ClientKeyDER:  identity.ClientKeyDER,
	})
}

func runHealthcheck(listenAddr string) error {
	port := extractPort(listenAddr, "9443")
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 2*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func extractPort(listenAddr, fallback string) string {
	if _, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr)); err == nil && port != "" {
		return port
	}
	return fallback
}

func resolveConnectAddr(configured, port, lanIP string) string {
	s := strings.TrimSpace(configured)
	if s != "" {
		if _, _, err := net.SplitHostPort(s); err == nil {
			return s
		}
		if !strings.Contains(s, ":") {
			return net.JoinHostPort(s, port)
		}
		return s
	}

	if lanIP != "" {
		return net.JoinHostPort(lanIP, port)
	}
	return net.JoinHostPort("127.0.0.1", port)
}

func detectLANIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
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
	if len(list) == 0 {
		return ""
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score < list[j].score
		}
		return list[i].ip < list[j].ip
	})
	return list[0].ip
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
