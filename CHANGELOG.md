# Changelog

Tots els canvis notables de **Gecko** es documenten en aquest fitxer seguint el format [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Afegit
- **Captura Windows DXGI Desktop Duplication** en Go pur (COM D3D11/DXGI) amb suport de *dirty rects* de GPU a `video.TileEncoder` i *fallback* transparent a GDI `BitBlt`.
- **Consola d'administració multiplataforma (Web Admin local)** per a Linux, macOS i Windows (`-web`) amb WebSocket natiu en Go pur, visor HTML5 Canvas, portapapers, enviament de fitxers, `CAD`, cerca i generador de clients.
- **Suport inicial de client per a macOS** (`internal/darwin`) i **compatibilitat ampliada amb Wayland en Linux** (`wl-copy`/`wl-paste`, captura `grim`/portals i *fallback* de diàleg).
- **Internacionalització (`ca`, `es`, `en`)** amb detecció automàtica, flag `-lang` i selecció d'idioma al generador de clients.
- **Versionat integrat** (`-version` a tots els binaris i reportat al protocol).
- **Credencials mTLS individuals revocables** per client, *rate limiting* per IP al Relay, auditoria de sessions, portapapers bidireccional, transferència de fitxers verificada amb SHA-256 i botó `Ctrl+Alt+Supr`.
- **Documentació Open Source**: `LICENSE` (MIT), `SECURITY.md`, `CONTRIBUTING.md`, `CHANGELOG.md`, `docs/PRODUCCIO.md` i plantilles d'Issues/PR.

### Canviat
- Substituïts `build.bat` i `build.ps1` per `go run ./cmd/build` multiplataforma (`amd64` i `arm64`).
- `RelayServidor` només imprimeix la clau mestra `RMT2:...` en la primera inicialització o amb `-show-key`, elimina crides HTTP externes d'IP pública i inclou `-healthcheck` natiu per a Docker `scratch`.
- Mogut `FrameBuffer` a `internal/video` i unificats els tipus de UI/monitors a `internal/clientui`.
