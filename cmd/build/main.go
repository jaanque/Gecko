package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type buildTarget struct {
	label   string
	goos    string
	goarch  string
	ldflags string
	output  string
	pkg     string
}

func main() {
	arch := flag.String("arch", "amd64", "Arquitectura destino para los binarios (amd64 o arm64)")
	flag.Parse()

	goarch := strings.TrimSpace(*arch)
	if goarch != "amd64" && goarch != "arm64" {
		fmt.Fprintf(os.Stderr, "Arquitectura no soportada: %q (usa amd64 o arm64)\n", goarch)
		os.Exit(1)
	}

	for _, dir := range []string{filepath.Join("cmd", "admin", "assets"), "dist"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "Error creando directorio %s: %v\n", dir, err)
			os.Exit(1)
		}
	}

	targets := []buildTarget{
		{
			label:   "Cliente (Windows)",
			goos:    "windows",
			goarch:  goarch,
			ldflags: "-H=windowsgui -s -w",
			output:  filepath.Join("cmd", "admin", "assets", "client_template.exe"),
			pkg:     "./cmd/client",
		},
		{
			label:   "Cliente (Linux)",
			goos:    "linux",
			goarch:  goarch,
			ldflags: "-s -w",
			output:  filepath.Join("cmd", "admin", "assets", "client_template_linux"),
			pkg:     "./cmd/client",
		},
		{
			label:   "Admin (Windows)",
			goos:    "windows",
			goarch:  goarch,
			ldflags: "-H=windowsgui -s -w",
			output:  filepath.Join("dist", "AdminRemoto.exe"),
			pkg:     "./cmd/admin",
		},
		{
			label:   "Relay (Windows)",
			goos:    "windows",
			goarch:  goarch,
			ldflags: "-s -w",
			output:  filepath.Join("dist", "RelayServidor.exe"),
			pkg:     "./cmd/relay",
		},
		{
			label:   "Relay (Linux)",
			goos:    "linux",
			goarch:  goarch,
			ldflags: "-s -w",
			output:  filepath.Join("dist", "RelayServidor"),
			pkg:     "./cmd/relay",
		},
	}

	for i, t := range targets {
		pct := ((i + 1) * 100) / len(targets)
		fmt.Printf("[%3d%%] Compilando %-20s -> %s\n", pct, t.label, t.output)

		cmd := exec.Command("go", "build", "-trimpath", "-ldflags="+t.ldflags, "-o", t.output, t.pkg)
		cmd.Env = append(os.Environ(),
			"CGO_ENABLED=0",
			"GOOS="+t.goos,
			"GOARCH="+t.goarch,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error compilando %s: %v\n", t.label, err)
			os.Exit(1)
		}
	}

	fmt.Println("Compilación completada en dist/")
}
