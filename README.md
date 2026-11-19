<div align="center">

<img src="assets/banner.svg" alt="Gecko Remote Desktop" width="100%" />

# 🦎 Gecko

**Control y asistencia remota para Windows.**  
*Rápido, privado, sin instalación y de código abierto.*

[![CI](https://github.com/jaanque/Gecko/actions/workflows/ci.yml/badge.svg)](https://github.com/jaanque/Gecko/actions/workflows/ci.yml)
[![Licencia: MIT](https://img.shields.io/badge/licencia-MIT-18181B)](LICENSE)
![Windows](https://img.shields.io/badge/plataforma-Windows%20(amd64%20%7C%20arm64)-0078D4?logo=windows&logoColor=white)
![Go 1.22+](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)

| ⚡ **60 FPS GPU** | 🔒 **mTLS 1.3** | 🪟 **Win32 Nativo** | 📦 **0 Instalación** |
| :---: | :---: | :---: | :---: |
| Captura DirectX 11 DXGI | Cifrado mutuo con Pinning | Cero dependencias externas | Doble clic y listo |

</div>

---

### 🚀 Inicio Rápido

```sh
# 1. Compilar todo (Windows amd64 / arm64)
go run ./cmd/build

# 2. Iniciar el servidor puente
dist\RelayServidor.exe

# 3. Abrir la consola de control
dist\AdminRemoto.exe
```

---

<div align="center">

[📘 **Guía de Producción**](docs/PRODUCCIO.md) &nbsp;•&nbsp; [🤝 **Contribuir**](CONTRIBUTING.md) &nbsp;•&nbsp; [🛡️ **Seguridad**](SECURITY.md) &nbsp;•&nbsp; [⚖️ **Licencia MIT**](LICENSE)

</div>
