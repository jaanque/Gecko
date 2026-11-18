$ErrorActionPreference = "Stop"

$env:GOOS = "windows"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"

function Show-Bar([int]$Pct, [string]$Msg) {
    $fill = [int]($Pct / 5)
    $bar = ("#" * $fill).PadRight(20, ".")
    Write-Host ("`r[{0}] {1,3}%  {2,-30}" -f $bar, $Pct, $Msg) -NoNewline
}

New-Item -ItemType Directory -Force -Path "cmd\admin\assets", "dist" | Out-Null

Show-Bar 10 "Cliente..."
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "cmd\admin\assets\client_template.exe" ./cmd/client
if ($LASTEXITCODE -ne 0) { Write-Host ""; exit $LASTEXITCODE }

Show-Bar 55 "Admin..."
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "dist\AdminRemoto.exe" ./cmd/admin
if ($LASTEXITCODE -ne 0) { Write-Host ""; exit $LASTEXITCODE }

Show-Bar 100 "Listo: dist\AdminRemoto.exe"
Write-Host ""
