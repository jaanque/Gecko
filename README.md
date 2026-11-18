# ZeroRMM — Sistema de Control Remoto de Conexión Inversa y Baja Latencia

Sistema RMM de alto rendimiento basado en **Arquitectura de Conexión Inversa (Reverse Connection)**, **Captura GPU Zero-Copy**, **Codificación por Hardware (NVENC / AMF / QuickSync)** y **Transporte QUIC por Datagramas (RFC 9221)** con experiencia **Zero-Config** para el cliente final.

## Estructura del Monorepo

- `crates/rmm-proto`: Protocolo compartido, mensajes de control, eventos de input y mecanismo **Stub Overlay** (`RMMCFG01` + CRC32 + firma Ed25519).
- `crates/rmm-net`: Transporte **QUIC (quinn)**, TLS 1.3 con *certificate pinning* SHA-256 (sin CA pública), fragmentador MTU-safe y reensamblador de video con recuperación PLI/IDR.
- `crates/rmm-capture-win`: Captura **DXGI Desktop Duplication** + conversión **BGRA → NV12 en VRAM** mediante `ID3D11VideoProcessorBlt` (cero copias a CPU RAM) + captura de cursor hardware.
- `crates/rmm-encode-win`: Codificación hardware **Windows Media Foundation MFT** (NVENC / AMD AMF / Intel QuickSync) compartiendo `ID3D11Device` vía `IMFDXGIDeviceManager`, `AVLowLatencyMode`, 0 B-frames y soporte de MFTs asíncronos.
- `crates/rmm-input-win`: Inyección Win32 `SendInput` mediante **Hardware Scan Codes** (independiente del idioma del teclado), soporte multi-monitor (`MOUSEEVENTF_VIRTUALDESK`) y protección contra teclas pegadas (*stuck-key guard*).
- `apps/agent`: Ejecutable cliente ligero de un solo archivo con icono de bandeja y opción de desconexión para el usuario final.
- `apps/admin`: Aplicación de escritorio **Tauri v2 + React + TypeScript + Tailwind CSS** con servidor QUIC embebido, generador instantáneo de clientes y visor acelerado por **WebCodecs (`VideoDecoder`)**.

## Inicio Rápido (Desarrollo)

1. **Compilar la plantilla del Agente Cliente (stub):**
   ```powershell
   cargo build -p rmm-agent --profile release-agent
   ```

2. **Instalar dependencias del frontend y arrancar la Aplicación Admin:**
   ```powershell
   cd apps\admin
   npm.cmd install
   npx.cmd tauri dev
   ```

3. **Probar el flujo Zero-Config:**
   - En la Aplicación Admin, pulsa **Generar Cliente**.
   - Haz doble clic en el `.exe` generado (o ejecútalo desde otra máquina en la misma LAN/red).
   - El equipo aparecerá instantáneamente como **Online** en el dashboard.
   - Haz doble clic en el equipo para abrir el visor de control remoto acelerado por hardware.

## Compilación Release Completa

```powershell
powershell -ExecutionPolicy Bypass -File scripts\build-release.ps1
```
