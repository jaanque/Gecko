<div align="center">

```
 ██████╗ ███████╗ ██████╗██╗  ██╗ ██████╗ 
██╔════╝ ██╔════╝██╔════╝██║ ██╔╝██╔═══██╗
██║  ███╗█████╗  ██║     █████═╝ ██║   ██║
██║   ██║██╔══╝  ██║     ██╔═██╗ ██║   ██║
╚██████╔╝███████╗╚██████╗██║ ╚██╗╚██████╔╝
 ╚═════╝ ╚══════╝ ╚═════╝╚═╝  ╚═╝ ╚═════╝ 
```

# 🦎 Gecko

**Soporte y control remoto nativo para Windows.**  
*Ultraligero · 60 FPS DXGI GPU · Zero-Trust mTLS 1.3 · Cero dependencias · Código abierto*

[![CI](https://github.com/jaanque/Gecko/actions/workflows/ci.yml/badge.svg)](https://github.com/jaanque/Gecko/actions/workflows/ci.yml)
[![Licencia: MIT](https://img.shields.io/badge/Licencia-MIT-18181B.svg)](LICENSE)
![Windows](https://img.shields.io/badge/Plataforma-Windows%20(amd64%20%7C%20arm64)-0078D4.svg?logo=windows&logoColor=white)
![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8.svg?logo=go&logoColor=white)
![Cero Dependencias](https://img.shields.io/badge/Dependencias-0-16A34A.svg)

---

</div>

## 🖥️ Visor Remoto

```text
┌────────────────────────────────────────────────────────────────────────┐
│ 🦎 Gecko Viewer ─ PC-OFICINA (1920×1080 · 60 FPS · 8 ms)          [—][□][×]│
├────────────────────────────────────────────────────────────────────────┤
│ [M1] [M2] [Todos] │ Calidad: [Baja] [Estándar] [● Alta] │ [📁 Archivo] │
├────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│          ╔══════════════════════════════════════════════════╗          │
│          ║                                                  ║          │
│          ║        ESCRITORIO REMOTO ACELERADO (DXGI)        ║          │
│          ║                                                  ║          │
│          ╚══════════════════════════════════════════════════╝          │
│                                                                        │
└────────────────────────────────────────────────────────────────────────┘
```
<div align="center">
<sub>Visor nativo Win32: aceleración directa DirectX 11, Per-Monitor DPI v2 y portapapeles sincronizado.</sub>
</div>

---

## 🛡️ Consentimiento Obligatorio

```text
┌────────────────────────────────────────────────────┐
│ 🦎 Asistencia Remota                        [—][×] │
├────────────────────────────────────────────────────┤
│                                                    │
│   Un técnico solicita conectarse a su equipo:      │
│                                                    │
│   Operador: Soporte IT (f9bbdef02173)              │
│   Seguridad: Cifrado mTLS 1.3 extremo a extremo    │
│                                                    │
│          [ ✔ PERMITIR ]      [ ✖ DENEGAR ]         │
│                                                    │
└────────────────────────────────────────────────────┘
```
<div align="center">
<sub>Sin instalación: el usuario asistido solo hace doble clic y decide si autoriza el control.</sub>
</div>

---

## ⚡ Comparativa

| Capacidad | 🦎 Gecko | Herramientas Tradicionales |
| :--- | :---: | :---: |
| **Instalación** | ❌ **0 MB** (Portable) | ⚠️ 50–150 MB (Instalador + drivers) |
| **Dependencias** | ✅ **0** (Go puro + Win32) | ❌ Decenas de librerías y runtimes |
| **Captura GPU** | ✅ **DXGI Direct 60 FPS** | ⚠️ Hooking lento / polling GDI |
| **Privacidad** | ✅ **100% Auto-alojado** | ❌ Nubes de terceros con telemetría |
| **Seguridad de red** | ✅ **mTLS 1.3 + Pinning** | ⚠️ Propietaria / Cerrada |
| **Revocación** | ✅ **Instantánea en 1 clic** | ⚠️ Panel web externo |

---

## 🔗 Arquitectura

```mermaid
flowchart LR
    subgraph Client ["💻 Cliente Asistido"]
        C["<b>ClientRemot.exe</b><br/>Portable · Sin instalación"]
    end

    subgraph Relay ["🌐 Servidor Puente"]
        R["<b>RelayServidor.exe</b><br/>mTLS 1.3 · TCP 9443"]
    end

    subgraph Admin ["🎛️ Administrador"]
        A["<b>AdminRemoto.exe</b><br/>Consola y Visor Win32"]
    end

    C -- "mTLS saliente" --> R
    A -- "mTLS saliente" --> R

    classDef default fill:#18181b,stroke:#3f3f46,stroke-width:1px,color:#f4f4f5;
    classDef client fill:#1e293b,stroke:#38bdf8,stroke-width:1.5px,color:#f8fafc;
    classDef relay fill:#14532d,stroke:#22c55e,stroke-width:1.5px,color:#f0fdf4;
    classDef admin fill:#312e81,stroke:#818cf8,stroke-width:1.5px,color:#eef2ff;

    class C client;
    class R relay;
    class A admin;
```
<div align="center">
<sub>Sin abrir puertos en clientes ni administradores: todas las conexiones son salientes hacia el Relay.</sub>
</div>

---

## 🚀 Inicio en 3 Pasos

```text
 [1] Compilar       go run ./cmd/build       ──▶   dist/
 [2] Servidor       dist\RelayServidor.exe   ──▶   TCP :9443 (mTLS)
 [3] Conectar       dist\AdminRemoto.exe     ──▶   Soporte instantáneo
```

---

## 📚 Documentación

<div align="center">

[![Guía de Producción](https://img.shields.io/badge/📘%20Guía-Producción%20y%20Despliegue-2563EB?style=for-the-badge)](docs/PRODUCCIO.md)
[![Contribuir](https://img.shields.io/badge/🤝%20Comunidad-Contribuir-7C3AED?style=for-the-badge)](CONTRIBUTING.md)
[![Seguridad](https://img.shields.io/badge/🛡️%20Seguridad-Reportar-059669?style=for-the-badge)](SECURITY.md)
[![Licencia](https://img.shields.io/badge/⚖️%20Licencia-MIT-D97706?style=for-the-badge)](LICENSE)

</div>
