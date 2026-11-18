<#
.SYNOPSIS
  Compila el Agente Cliente (stub único sin dependencias) y la Aplicación Admin (Tauri v2).
#>
$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot

Write-Host "==> 1/3 Compilando plantilla ligera rmm-agent (Zero-Copy + CRT estático)..." -ForegroundColor Cyan
Push-Location $Root
try {
    $env:RUSTFLAGS = "-C target-feature=+crt-static"
    cargo build -p rmm-agent --profile release-agent
    Remove-Item Env:\RUSTFLAGS -ErrorAction SilentlyContinue

    $StubSrc = Join-Path $Root "target\release-agent\rmm-agent.exe"
    $ResDir = Join-Path $Root "apps\admin\src-tauri\resources"
    New-Item -ItemType Directory -Force -Path $ResDir | Out-Null
    Copy-Item -Force $StubSrc (Join-Path $ResDir "agent_stub.exe")

    $SizeMb = [Math]::Round((Get-Item $StubSrc).Length / 1MB, 2)
    Write-Host "    Stub generado: $StubSrc ($SizeMb MB)" -ForegroundColor Green

    Write-Host "==> 2/3 Compilando frontend Admin (React + Tailwind + WebCodecs)..." -ForegroundColor Cyan
    Push-Location (Join-Path $Root "apps\admin")
    try {
        npm.cmd install
        npm.cmd run build
    } finally {
        Pop-Location
    }

    Write-Host "==> 3/3 Compilando aplicación de escritorio Admin (Tauri v2)..." -ForegroundColor Cyan
    Push-Location (Join-Path $Root "apps\admin")
    try {
        npx.cmd tauri build
    } finally {
        Pop-Location
    }

    Write-Host "==> Compilación completada." -ForegroundColor Green
} finally {
    Pop-Location
}
