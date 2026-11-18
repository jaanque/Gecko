$ErrorActionPreference = "Stop"

$env:GOOS = "windows"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"

New-Item -ItemType Directory -Force -Path "cmd\admin\assets" | Out-Null
New-Item -ItemType Directory -Force -Path "dist" | Out-Null

Write-Host "[1/2] Compilando plantilla del cliente (GUI nativa Windows)..."
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "cmd\admin\assets\client_template.exe" ./cmd/client

Write-Host "[2/2] Compilando aplicación de administración (GUI nativa Windows)..."
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "dist\AdminRemoto.exe" ./cmd/admin

$clientSize = [math]::Round((Get-Item "cmd\admin\assets\client_template.exe").Length / 1MB, 2)
$adminSize  = [math]::Round((Get-Item "dist\AdminRemoto.exe").Length / 1MB, 2)

Write-Host ""
Write-Host "Compilación completada:"
Write-Host "  - Plantilla cliente: cmd\admin\assets\client_template.exe ($clientSize MB)"
Write-Host "  - Aplicación Admin:  dist\AdminRemoto.exe ($adminSize MB)"
Write-Host ""
Write-Host "Haz doble clic en .\dist\AdminRemoto.exe para abrir el panel."
