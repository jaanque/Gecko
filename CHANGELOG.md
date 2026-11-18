# Changelog

Tots els canvis notables de **Gecko** es documenten en aquest fitxer seguint el format [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Afegit
- **Captura Windows DXGI Desktop Duplication** en Go pur (COM D3D11/DXGI) amb suport multi-GPU/Optimus (`IDXGIFactory1::EnumAdapters1`), protecció automàtica per a pantalles HDR i rotades, *dirty rects* de GPU a `video.TileEncoder` i *fallback* transparent a GDI `BitBlt`.
- **Suport Per-Monitor DPI v2 dinàmic (`WM_DPICHANGED`)** a `AdminRemoto.exe`, el visor remot i `ClientRemot.exe`.
- **Internacionalització del client (`ca`, `es`, `en`)** seleccionable directament des de la pestanya *Crear cliente* d'`AdminRemoto.exe`, per flag `-lang` o detecció automàtica.
- **Versionat integrat** (`-version` a tots els binaris, títol de finestra i columna de versió al llistat d'equips).
- **Credencials mTLS individuals revocables** per client, *rate limiting* per IP al Relay, auditoria de sessions, portapapers bidireccional, transferència de fitxers amb progrés i verificació SHA-256, indicador de latència per colors i botó `Ctrl+Alt+Supr` (`SendSAS`).
- **Documentació Open Source**: `LICENSE` (MIT), `SECURITY.md`, `CONTRIBUTING.md`, `CHANGELOG.md`, `docs/PRODUCCIO.md` i plantilles d'Issues/PR.

### Canviat
- Enfocament 100% natiu per a **Windows** (`amd64` i `arm64`): eliminats els backends de Linux/X11/Wayland, macOS, el panell web i `Dockerfile`, reduint la mida d'`AdminRemoto.exe` a ~11 MB.
- Substituïts `build.bat` i `build.ps1` per `go run ./cmd/build` (`-arch amd64|arm64`, `-version`).
- `RelayServidor.exe` només imprimeix la clau mestra `RMT2:...` en la primera inicialització o amb `-show-key`, i elimina crides HTTP externes d'IP pública.
- Mogut `FrameBuffer` a `internal/video`.
