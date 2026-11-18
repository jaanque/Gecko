<div align="center">

# 🦎 Gecko

**Control i suport remot per a Windows.**  
*Natiu · Lleuger · Segur · Codi Obert*

[![CI](https://github.com/jaanque/Gecko/actions/workflows/ci.yml/badge.svg)](https://github.com/jaanque/Gecko/actions/workflows/ci.yml)
[![Llicència: MIT](https://img.shields.io/badge/Llicència-MIT-18181B.svg)](LICENSE)
![Windows](https://img.shields.io/badge/Plataforma-Windows%20(amd64%20%7C%20arm64)-0078D4.svg?logo=windows&logoColor=white)
![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8.svg?logo=go&logoColor=white)
![Zero Dependencies](https://img.shields.io/badge/Dependències-0-16A34A.svg)

</div>

---

### ✨ Característiques

| ⚡ Rendiment | 🛡️ Seguretat | 🪟 Windows Natiu |
| :--- | :--- | :--- |
| **GPU DXGI 60 FPS**<br/>Captura directa per maquinari | **Zero-Trust mTLS 1.3**<br/>Xifratge extrem a extrem | **Win32 Natiu Pur**<br/>Sense runtimes ni dependències |
| **Tessel·les híbrides**<br/>Text nítid i moviment fluid | **Consentiment actiu**<br/>L'usuari autoritza el control | **DPI Awareness v2**<br/>Escalat nítid en qualsevol monitor |
| **Multimonitor**<br/>Pantalla individual o combinada | **Revocació en un clic**<br/>Tall d'accés instantani | **Portapapers i fitxers**<br/>Eines essencials integrades |

---

### 🔗 Com funciona

Sense obrir ports al router ni configurar tallafocs: tothom es connecta de manera sortint al servidor pont.

```mermaid
flowchart LR
    subgraph Client ["💻 Equip Assistit"]
        C["<b>ClientRemot.exe</b><br/>Sense instal·lació"]
    end

    subgraph Relay ["🌐 Servidor Pont"]
        R["<b>RelayServidor.exe</b><br/>mTLS 1.3 · Port 9443"]
    end

    subgraph Admin ["🎛️ Administrador"]
        A["<b>AdminRemoto.exe</b><br/>Consola i Visor Win32"]
    end

    C -- "mTLS sortint" --> R
    A -- "mTLS sortint" --> R

    classDef default fill:#18181b,stroke:#3f3f46,stroke-width:1px,color:#f4f4f5;
    classDef client fill:#1e293b,stroke:#38bdf8,stroke-width:1.5px,color:#f8fafc;
    classDef relay fill:#14532d,stroke:#22c55e,stroke-width:1.5px,color:#f0fdf4;
    classDef admin fill:#312e81,stroke:#818cf8,stroke-width:1.5px,color:#eef2ff;

    class C client;
    class R relay;
    class A admin;
```

---

### 🧩 Components

| Binari | Rol | Descripció |
| :--- | :--- | :--- |
| 🎛️ **`AdminRemoto.exe`** | Administrador | Consola de control, visor fluid a 60 FPS i generador de clients. |
| 🌐 **`RelayServidor.exe`** | Servidor | Servidor pont mTLS, gestor de credencials i auditoria de sessions. |
| 💻 **`ClientRemot.exe`** | Client | Executable portable sense instal·lació. Doble clic i llest. |

---

### 🚀 Inici ràpid

```sh
# 1. Compilar executables (Go 1.22+)
go run ./cmd/build

# 2. Iniciar el servidor pont (al servidor)
dist\RelayServidor.exe

# 3. Obrir la consola de control (a l'equip d'administració)
dist\AdminRemoto.exe
```

---

### 📚 Documentació

| Recurs | Contingut |
| :--- | :--- |
| 📘 **[Guia de producció](docs/PRODUCCIO.md)** | Configuració de xarxa, desplegament, seguretat i dreceres. |
| 🤝 **[Contribuir](CONTRIBUTING.md)** | Directrius de desenvolupament i estil de codi. |
| 🛡️ **[Seguretat](SECURITY.md)** | Política de reporte responsable de vulnerabilitats. |
| 📜 **[Llicència MIT](LICENSE)** | Codi obert per a ús personal i comercial. |
