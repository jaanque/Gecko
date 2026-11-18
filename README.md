# Gecko — Control Remoto Nativo de Conexión Inversa y Baja Latencia

Sistema RMM **100% nativo en Rust** (sin web, sin navegador, sin Electron ni Node.js) basado en **Conexión Inversa (Reverse Connection)**, **Captura GPU Zero-Copy**, **Codificación/Decodificación Hardware Windows Media Foundation** y **Transporte QUIC por Datagramas (RFC 9221)** con experiencia **Zero-Config**.

## Arquitectura 100% Nativa

- `crates/rmm-proto`: Protocolo compartido, mensajes de control, identidad de máquina (`machine_id`), *LAN Auto-Discovery*, eventos de input con *epoch* de sesión y **Stub Overlay** (`RMMCFG01` + CRC32 + firma Ed25519).
- `crates/rmm-net`: Transporte **QUIC (quinn)** con *certificate pinning* TLS 1.3, fragmentador/reensamblador MTU-safe de video y **Auto-Discovery LAN firmado (`UDP :4434`)**.
- `crates/rmm-capture-win`: Captura **DXGI Desktop Duplication** + conversión **BGRA → NV12 en VRAM** mediante `ID3D11VideoProcessorBlt` (cero copias a RAM).
- `crates/rmm-encode-win`: Codificación hardware **Windows Media Foundation MFT** (NVENC / AMD AMF / Intel QuickSync) sobre `ID3D11Device` compartido, `AVLowLatencyMode` y 0 B-frames.
- `crates/rmm-decode-win`: Decodificador nativo **Windows Media Foundation H.264/HEVC** en modo baja latencia (`AVLowLatencyMode`) sin capas web.
- `crates/rmm-input-win`: Inyección Win32 `SetCursorPos` + `SendInput` con **Per-Monitor DPI Awareness V2**, **Hardware Scan Codes**, multi-monitor (`VIRTUALDESK`) y liberación automática de teclas (*stuck-key guard*).
- `apps/agent`: Agente cliente nativo de un solo `.exe` (~2.1 MB, CRT estático) con icono de bandeja y reconexión + auto-descubrimiento LAN.
- `apps/admin`: Aplicación de escritorio **100% nativa** (`Gecko.exe`, GPU `eframe`/`egui`) con servidor QUIC embebido, reconocimiento persistente de equipos (Online/Offline), generador de clientes en 1 clic y visor remoto nativo.

## Compilar y Abrir Gecko

```powershell
# Compila Gecko.exe (Admin nativo) + agent_stub.exe en dist-gecko\
powershell -ExecutionPolicy Bypass -File scripts\build-release.ps1

# O arranca directamente en desarrollo:
cargo run -p gecko-admin
```

Consulta la guía paso a paso completa en [GUIA_DE_USO.md](GUIA_DE_USO.md).
