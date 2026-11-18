# Guía de Despliegue, Arranque y Uso — Gecko

Esta guía explica paso a paso cómo preparar el entorno, levantar la **Aplicación Admin**, generar y ejecutar el **Agente Cliente (Zero-Config)**, probar en local/LAN/Internet, compilar para producción y resolver problemas comunes.

---

## 1. Requisitos Previos (Windows)

Asegúrate de tener instalados:

- **Rust y Cargo** (`rustup` con toolchain `x86_64-pc-windows-msvc`) — verificado en este equipo con `rustc 1.97.0`.
- **Visual Studio Build Tools** (carga de trabajo *Desarrollo de escritorio con C++* para `link.exe` y Windows SDK).
- **Node.js 20+** y `npm` — verificado en este equipo con `node v24.16.0` y `npm 11.12.0`.
- **Microsoft Edge WebView2 Runtime** (viene preinstalado en Windows 10/11).

> **Nota sobre PowerShell en Windows:** Si PowerShell muestra un error indicando que la ejecución de scripts está deshabilitada al correr `npm` o `npx`, usa siempre **`npm.cmd`** y **`npx.cmd`** (como en todos los comandos de esta guía).

---

## 2. Arranque Rápido (Modo Desarrollo)

Todos los comandos se ejecutan desde una terminal PowerShell en la raíz del proyecto (`c:\Users\JAN\Desktop\remote`).

### Paso 1: Compilar la plantilla del Agente Cliente (`agent_stub.exe`)

> Si no has modificado el código de `apps/agent` ni de los crates `crates/*`, **este paso ya está hecho** (`apps\admin\src-tauri\resources\agent_stub.exe` ya existe, 2.12 MB). Vuelve a ejecutarlo cada vez que hagas cambios en el agente o el protocolo.

```powershell
cd c:\Users\JAN\Desktop\remote
$env:RUSTFLAGS = "-C target-feature=+crt-static"
cargo build -p rmm-agent --profile release-agent
Remove-Item Env:\RUSTFLAGS -ErrorAction SilentlyContinue

New-Item -ItemType Directory -Force -Path "apps\admin\src-tauri\resources" | Out-Null
Copy-Item -Force "target\release-agent\rmm-agent.exe" "apps\admin\src-tauri\resources\agent_stub.exe"
```

### Paso 2: Instalar dependencias del Frontend (solo la primera vez)

```powershell
cd c:\Users\JAN\Desktop\remote\apps\admin
npm.cmd install
```

### Paso 3: Levantar la Aplicación Admin (Panel + Backend QUIC embebido)

```powershell
cd c:\Users\JAN\Desktop\remote\apps\admin
npx.cmd tauri dev
```

Al arrancar verás:
1. Se levanta Vite en `http://localhost:5173`.
2. Se compila y abre la ventana de escritorio **Gecko**.
3. El **servidor QUIC embebido** queda escuchando automáticamente en **`UDP 0.0.0.0:4433`** (visible en la esquina inferior izquierda del panel como `Backend Embebido · UDP :4433`).

#### Variables de entorno opcionales al arrancar el Admin

```powershell
# Cambiar el puerto UDP de escucha (por defecto: 4433)
$env:GECKO_PORT = "4433"

# Ver logs detallados de red y sesiones en la consola
$env:RUST_LOG = "rmm_admin=debug,rmm_net=debug"

npx.cmd tauri dev
```

---

## 3. Cómo Probar el Flujo Completo "Zero-Config"

### Escenario A: Prueba rápida en la misma máquina (Loopback)

1. Con la Aplicación Admin abierta, pulsa el botón verde **"Generar Cliente"** en la barra lateral izquierda.
2. En el modal:
   - **Etiqueta del Cliente:** `Prueba Local`
   - **Nombre del Administrador:** `Mi Admin`
   - **Endpoints:** `127.0.0.1:4433` (puedes hacer clic en la píldora `+ 127.0.0.1:4433` debajo del campo).
   - **Caducidad:** `24 horas` (o la que prefieras).
3. Pulsa **"Generar .EXE"** → en menos de 100 ms inyecta el overlay firmado Ed25519 al final del stub sin recompilar.
4. Pulsa **"Abrir en Explorador"** y haz **doble clic** sobre el ejecutable generado (ej. `Gecko-Prueba-Local.exe`).
5. **Resultado inmediato:**
   - Aparece un icono del agente en la bandeja del sistema de Windows (junto al reloj).
   - En la Aplicación Admin, tu equipo aparece instantáneamente como **`Online`** mostrando nombre del PC, usuario de Windows, GPU y monitores.
   - Haz **doble clic** sobre la tarjeta del equipo (o pulsa **"Controlar"**) para iniciar el streaming acelerado por hardware.

> **Consejo en prueba local (misma pantalla):** Como verás tu propio escritorio dentro del visor (efecto espejo infinito), minimiza o mueve ligeramente la ventana del visor o selecciona un segundo monitor si tienes dos pantallas para evitar confusión con el ratón.

---

### Escenario B: Prueba entre dos equipos en Red Local (LAN)

1. **En la máquina del Administrador**, abre el puerto **UDP 4433** en el Firewall de Windows (ejecuta PowerShell como Administrador una sola vez):
   ```powershell
   New-NetFirewallRule -DisplayName "Gecko Admin QUIC (UDP 4433)" -Direction Inbound -Protocol UDP -LocalPort 4433 -Action Allow
   ```
2. En el Panel Admin, pulsa **"Generar Cliente"**. El campo **Endpoints** ya sugerirá automáticamente tu IP de red local (por ejemplo `192.168.1.50:4433`).
3. Genera el `.exe`, envíalo al segundo PC Windows y haz **doble clic** en ese equipo (no requiere abrir puertos ni configurar firewall en el equipo cliente).
4. El equipo remoto aparecerá como **`Online`** en segundos y podrás controlarlo con baja latencia.

---

### Escenario C: Conexión por Internet (WAN)

El cliente remoto **nunca** necesita abrir puertos ni configurar nada. Solo el Administrador necesita ser alcanzable por UDP:

- **Opción 1 (Port Forwarding / DDNS):** Redirige el puerto **UDP 4433** en el router del Administrador hacia la IP LAN del PC Admin y coloca `tu-dominio.ddns.net:4433` (o tu IP pública `:4433`) en el campo **Endpoints** al generar el cliente.
- **Opción 2 (Tailscale / WireGuard / ZeroTier):** Si el Admin está detrás de CGNAT, usa la IP de tu red VPN privada en el campo **Endpoints** (ej. `100.x.y.z:4433`).
- **Failover automático:** Puedes escribir **varios endpoints (1 por línea)** al generar el cliente (por ejemplo primero la IP LAN y en la segunda línea el dominio público); el agente los probará en orden automáticamente.

---

## 4. Controles del Panel Admin y del Visor

### Dashboard Principal
- **`Ctrl + K`**: Enfoca el buscador para filtrar equipos por nombre, usuario, etiqueta o IP.
- **Doble clic en una tarjeta**: Abre la sesión de control remoto.
- **Pestaña "Clientes Emitidos"**: Muestra todos los `.exe` generados con su token Ed25519 único. Puedes abrir el archivo o pulsar **"Revocar"** para invalidar ese ejecutable y expulsar inmediatamente al cliente si estaba conectado.

### Visor de Control Remoto (WebCodecs Hardware)
En la barra flotante superior puedes:
- Ver el **codificador hardware activo** en el cliente (`NVIDIA H.264 Encoder MFT`, `AMD`, `Intel Quick Sync`, o fallback software).
- Cambiar de **monitor** (`#1`, `#2`, ...) si el cliente tiene varias pantallas.
- Ajustar **Bitrate** (`4` a `35 Mbps`) y **FPS** (`30`, `60`, `120`) al vuelo sin cortar la sesión.
- Pulsar el icono de refresco (**Forzar IDR**) si deseas pedir un fotograma clave inmediato.
- Activar/ocultar el **HUD de Telemetría** inferior izquierdo (FPS reales renderizados, RTT de red en ms, tiempo de captura DXGI en ms, tiempo de codificación GPU en ms, bitrate real y resolución).
- Entrar en **Pantalla Completa** o pulsar **Finalizar** para detener el streaming (el equipo sigue `Online` en el dashboard).

### En la Máquina del Cliente (Transparencia)
- El agente se ejecuta sin ventana de consola y muestra un **icono en la bandeja del sistema** indicando si está *Conectando*, *Online* o en *SESIÓN REMOTA ACTIVA*.
- El usuario final puede hacer **clic derecho sobre el icono → "Desconectar y Salir"** en cualquier momento para cerrar el agente inmediatamente.

---

## 5. Compilación de Producción (Release)

Para compilar tanto el stub cliente optimizado (`+crt-static`, LTO, strip) como el ejecutable/instalador final de la Aplicación Admin (`.exe` / `.msi` / `.nsis`):

```powershell
cd c:\Users\JAN\Desktop\remote
powershell -ExecutionPolicy Bypass -File scripts\build-release.ps1
```

Este script ejecuta automáticamente:
1. Compila `rmm-agent.exe` optimizado para tamaño y sin dependencias externas (`target\release-agent\rmm-agent.exe`, ~2.1 MB).
2. Lo copia a `apps\admin\src-tauri\resources\agent_stub.exe` para que quede empaquetado dentro del Admin.
3. Compila el frontend React/Tailwind en `apps\admin\dist`.
4. Genera la aplicación Admin y sus instaladores en `target\release\bundle\`.

---

## 6. Rutas de Persistencia, Datos y Seguridad

Al iniciarse por primera vez, la Aplicación Admin crea y persiste su estado local en:

```text
%LOCALAPPDATA%\gecko\admin\data\
├── admin_identity.json     # Par de claves Ed25519 y certificado TLS 1.3 (mantiene el pinning entre reinicios)
├── issued_tokens.json      # Registro de ejecutables generados y estado de revocación
└── generated-clients\      # Carpeta donde se guardan los .exe personalizados generados
```

> **Importante:** Protege `admin_identity.json` como una clave privada. Si borras este archivo, el Admin generará una nueva identidad criptográfica y los clientes `.exe` generados anteriormente dejarán de poder conectarse (por seguridad del *certificate pinning*).

---

## 7. Tests y Verificación del Código

Desde la raíz del proyecto (`c:\Users\JAN\Desktop\remote`):

```powershell
# Verificar compilación de los 7 paquetes del workspace Rust
cargo check --workspace

# Ejecutar tests de protocolo, overlay Ed25519, reensamblador y QUIC TLS Pinning
cargo test -p rmm-proto -p rmm-net

# Verificar TypeScript y bundle de producción del frontend Admin
npm.cmd --prefix apps\admin run build
```

---

## 8. Solución de Problemas Frecuentes

| Problema | Causa habitual | Solución |
| :--- | :--- | :--- |
| PowerShell rechaza `npm` o `npx` (*UnauthorizedAccess*) | Política de ejecución de scripts `.ps1` restringida en Windows. | Usa **`npm.cmd`** y **`npx.cmd`** en su lugar. |
| El cliente se ejecuta pero nunca aparece `Online` | Firewall de Windows en la máquina Admin bloqueando **UDP 4433**, o IP incorrecta en Endpoints. | Crea la regla de firewall para UDP 4433 (Sección 3, Escenario B) y verifica la IP del Admin. |
| Modal dice *"Plantilla precompilada no encontrada"* | No se ha compilado `rmm-agent` todavía. | Ejecuta el Paso 1 de la Sección 2 (`cargo build -p rmm-agent --profile release-agent`). |
| No puedo hacer clic en ventanas de Administrador o diálogos UAC del cliente | El agente se ejecutó sin privilegios elevados (restricción UIPI de Windows). | Ejecuta el `.exe` cliente como Administrador si necesitas interactuar con ventanas elevadas. |
| Quiero ver logs de depuración del Agente Cliente | En modo Release el agente oculta la consola (`windows_subsystem = "windows"`). | Para depurar, compila en debug (`cargo build -p rmm-agent`), genera el cliente apuntando a ese binario y ejecútalo desde PowerShell con `$env:RUST_LOG="debug"`. |
| Windows SmartScreen avisa al abrir el `.exe` en otro PC | El ejecutable no tiene firma Authenticode comercial. | Haz clic en *Más información → Ejecutar de todas formas* en pruebas, o firma el `.exe` generado con `signtool` usando tu certificado de firma de código en producción. |
