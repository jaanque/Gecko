<#
.SYNOPSIS
  Compila Gecko 100% nativo en Rust (cero web):
    1. Agente Cliente (rmm-agent.exe -> agent_stub.exe, ~2 MB, CRT estático)
    2. Aplicación Admin Nativa (gecko-admin.exe, GPU egui + Media Foundation Decoder + QUIC)
    3. Carpeta portable lista para usar en dist-gecko\
#>
$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot

Push-Location $Root
try {
    Write-Host "==> 1/3 Compilando plantilla ligera rmm-agent (Zero-Copy + CRT estático)..." -ForegroundColor Cyan
    $env:RUSTFLAGS = "-C target-feature=+crt-static"
    cargo build --offline -p rmm-agent --profile release-agent
    Remove-Item Env:\RUSTFLAGS -ErrorAction SilentlyContinue

    $StubSrc = Join-Path $Root "target\release-agent\rmm-agent.exe"
    $AdminResDir = Join-Path $Root "apps\admin\resources"
    New-Item -ItemType Directory -Force -Path $AdminResDir | Out-Null
    Copy-Item -Force $StubSrc (Join-Path $AdminResDir "agent_stub.exe")

    $StubMb = [Math]::Round((Get-Item $StubSrc).Length / 1MB, 2)
    Write-Host "    Stub listo: $StubSrc ($StubMb MB)" -ForegroundColor Green

    Write-Host "==> 2/3 Compilando aplicación de escritorio nativa Gecko Admin (gecko-admin.exe)..." -ForegroundColor Cyan
    cargo build --offline -p gecko-admin --release

    Write-Host "==> 3/3 Empaquetando carpeta portable en dist-gecko\..." -ForegroundColor Cyan
    $DistDir = Join-Path $Root "dist-gecko"
    New-Item -ItemType Directory -Force -Path (Join-Path $DistDir "resources") | Out-Null
    Copy-Item -Force (Join-Path $Root "target\release\gecko-admin.exe") (Join-Path $DistDir "Gecko.exe")
    Copy-Item -Force $StubSrc (Join-Path $DistDir "resources\agent_stub.exe")

    $AdminMb = [Math]::Round((Get-Item (Join-Path $DistDir "Gecko.exe")).Length / 1MB, 2)
    Write-Host "    Gecko.exe listo ($AdminMb MB) en: $DistDir" -ForegroundColor Green
} finally {
    Pop-Location
}
