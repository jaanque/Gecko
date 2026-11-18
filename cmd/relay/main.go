package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
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
)

func main() {
	defaultListen := envOr("RELAY_LISTEN", ":9443")
	defaultData := envOr("RELAY_DATA_DIR", "")
	defaultPublic := envOr("RELAY_PUBLIC_ADDR", "")

	listenAddr := flag.String("listen", defaultListen, "Puerto/dirección TCP TLS 1.3 (ej. :9443)")
	dataDir := flag.String("data-dir", defaultData, "Carpeta de datos TLS y tokens")
	publicAddr := flag.String("public-addr", defaultPublic, "Dominio o IP pública (ej. remoto.tudominio.com:9443)")
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
		log.Fatalf("Error inicializando TLS: %v", err)
	}

	ln, err := tls.Listen("tcp", *listenAddr, security.ServerTLSConfig(identity.Certificate, identity.ClientCAs))
	if err != nil {
		log.Fatalf("No se pudo abrir %s: %v", *listenAddr, err)
	}
	defer ln.Close()

	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	lanIP := detectLANIP()
	resolvedAddr := resolveConnectAddr(*publicAddr, port, lanIP)

	key, err := security.EncodeProfileKey(security.RelayProfile{
		ServerAddr:    resolvedAddr,
		CertSHA256:    identity.CertSHA256,
		AdminToken:    identity.AdminToken,
		ClientToken:   identity.ClientToken,
		AdminCertDER:  identity.AdminCertDER,
		AdminKeyDER:   identity.AdminKeyDER,
		ClientCertDER: identity.ClientCertDER,
		ClientKeyDER:  identity.ClientKeyDER,
	})
	if err != nil {
		log.Fatalf("Error generando clave: %v", err)
	}

	fmt.Printf("Direccion: %s\n", resolvedAddr)
	if lanIP != "" {
		lanAddr := net.JoinHostPort(lanIP, port)
		if lanAddr != resolvedAddr {
			fmt.Printf("LAN:       %s\n", lanAddr)
		}
	}
	fmt.Printf("Clave:     %s\n", key)

	srv := relay.NewServer(identity)
	go func() {
		if err := srv.Serve(ln); err != nil {
			log.Printf("Relay detenido: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	_ = ln.Close()
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

	if pubIP := detectPublicIP(); pubIP != "" {
		return net.JoinHostPort(pubIP, port)
	}
	if lanIP != "" {
		return net.JoinHostPort(lanIP, port)
	}
	return net.JoinHostPort("127.0.0.1", port)
}

func detectPublicIP() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	client := &http.Client{Timeout: 2500 * time.Millisecond}
	endpoints := []string{
		"https://api.ipify.org",
		"https://icanhazip.com",
	}
	for _, ep := range endpoints {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}
		ipStr := strings.TrimSpace(string(body))
		if ip := net.ParseIP(ipStr); ip != nil && ip.To4() != nil {
			return ip.To4().String()
		}
	}
	return ""
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
