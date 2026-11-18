$ErrorActionPreference = "Stop"

$env:CGO_ENABLED = "0"
$env:GOARCH = "amd64"

function Show-Bar([int]$Pct, [string]$Msg) {
    $fill = [int]($Pct / 5)
    $bar = ("#" * $fill).PadRight(20, ".")
    Write-Host ("`r[{0}] {1,3}%  {2,-24}" -f $bar, $Pct, $Msg) -NoNewline
}

New-Item -ItemType Directory -Force -Path "cmd\admin\assets", "dist" | Out-Null

$env:GOOS = "windows"
Show-Bar 20 "Cliente (Win)..."
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "cmd\admin\assets\client_template.exe" ./cmd/client
if ($LASTEXITCODE -ne 0) { Write-Host ""; exit $LASTEXITCODE }

$env:GOOS = "linux"
Show-Bar 40 "Cliente (Linux)..."
go build -trimpath -ldflags="-s -w" -o "cmd\admin\assets\client_template_linux" ./cmd/client
if ($LASTEXITCODE -ne 0) { Write-Host ""; exit $LASTEXITCODE }

$env:GOOS = "windows"
Show-Bar 60 "Admin..."
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "dist\AdminRemoto.exe" ./cmd/admin
if ($LASTEXITCODE -ne 0) { Write-Host ""; exit $LASTEXITCODE }

Show-Bar 80 "Relay (Win)..."
go build -trimpath -ldflags="-s -w" -o "dist\RelayServidor.exe" ./cmd/relay
if ($LASTEXITCODE -ne 0) { Write-Host ""; exit $LASTEXITCODE }

$env:GOOS = "linux"
Show-Bar 90 "Relay (Linux)..."
go build -trimpath -ldflags="-s -w" -o "dist\RelayServidor" ./cmd/relay
if ($LASTEXITCODE -ne 0) { Write-Host ""; exit $LASTEXITCODE }

Show-Bar 100 "Listo: dist\"
Write-Host ""
