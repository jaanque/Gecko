# Guía Completa de Uso y Arranque — Gecko

Esta guía explica **todo paso a paso desde el punto de vista del Administrador**: cómo abrir la pantalla de Gecko (tanto en modo desarrollo como en ejecutable/instalador de escritorio), cómo recorrer su interfaz, cómo generar clientes *Zero-Config*, cómo conectarte y controlar un equipo remoto, cómo configurar tu red y cómo resolver cualquier incidencia.

---

## 1. ¿Cómo abro la pantalla de Admin de Gecko?

Tienes **dos formas** de abrir la aplicación de escritorio **Gecko** en tu PC de Administrador:

### Opción A — Abrirla ahora mismo (Modo Desarrollo, en 1 comando)

Ideal para probar o desarrollar sin esperar a generar un instalador.

1. Abre **PowerShell** (o la terminal integrada de tu editor).
2. Ejecuta estos dos comandos:

```powershell
cd c:\Users\JAN\Desktop\remote\apps\admin
npx.cmd tauri dev
```

3. **Qué verás al ejecutarlo:**
   - La primera vez tardará ~20–40 segundos mientras compila el binario nativo de Tauri. Las siguientes veces abrirá en pocos segundos.
   - Se abrirá automáticamente la **ventana de escritorio de Gecko** (fondo oscuro, barra lateral izquierda con el logo **G Gecko**).
   - Al mismo tiempo, dentro de esa misma ventana arranca el **servidor QUIC embebido** escuchando en **`UDP :4433`**.

> **Importante:** No abras `http://localhost:5173` en Chrome/Edge. Aunque Vite levanta ese puerto internamente para servir la interfaz a Tauri, **toda la lógica nativa (servidor QUIC, generador de `.exe` y streaming binario)** solo existe dentro de la ventana de escritorio de **Gecko** que abre `npx.cmd tauri dev`.

> **Nota sobre PowerShell:** Si Windows bloquea scripts `.ps1`, usa siempre **`npm.cmd`** y **`npx.cmd`** (nunca `npm` ni `npx` a secas).

---

### Opción B — Compilarla como app de escritorio normal (para abrir con doble clic)

Si quieres tener **Gecko** como un programa normal de Windows (con acceso directo o instalador, sin abrir nunca la terminal):

1. Ejecuta el script de compilación completa desde PowerShell:

```powershell
cd c:\Users\JAN\Desktop\remote
powershell -ExecutionPolicy Bypass -File scripts\build-release.ps1
```

2. Al terminar, encontrarás tu aplicación lista en estas rutas:
   - **Ejecutable directo (sin instalar):**
     ```text
     c:\Users\JAN\Desktop\remote\target\release\rmm-admin.exe
     ```
     *(Puedes renombrarlo a `Gecko.exe` o crear un acceso directo en tu Escritorio; asegúrate de mantener al lado la carpeta `resources\agent_stub.exe` o usar el instalador).*
   - **Instalador de Windows (recomendado — incluye `agent_stub.exe` empaquetado):**
     ```text
     c:\Users\JAN\Desktop\remote\target\release\bundle\nsis\Gecko_0.1.0_x64-setup.exe
     c:\Users\JAN\Desktop\remote\target\release\bundle\msi\Gecko_0.1.0_x64_en-US.msi
     ```
3. Instala `Gecko_0.1.0_x64-setup.exe` y ya podrás abrir **Gecko** desde el Menú Inicio o Escritorio con un simple **doble clic**.

---

## 2. Qué ves al abrir la pantalla de Gecko Admin

En cuanto se abre la ventana principal, verás dos zonas claras:

### Barra lateral izquierda
1. **Logo `G Gecko [QUIC]`** en la parte superior.
2. **Botón verde `Generar Cliente`**: abre el asistente para fabricar un ejecutable cliente listo para enviar.
3. **Navegación**:
   - **`Equipos Online`** (con contador en vivo): lista en tiempo real de todos los PCs conectados a tu máquina.
   - **`Clientes Emitidos`**: historial de todos los `.exe` que has generado, con botón para localizar el archivo y botón **Revocar** para invalidar su acceso en el acto.
4. **Tarjeta `Backend Embebido` (abajo a la izquierda)**:
   - **`UDP :4433`**: confirma que tu servidor de conexión inversa está escuchando.
   - **`IP LAN`**: tu IP local detectada automáticamente.
   - **`Stub: Listo`**: confirma que la plantilla precompilada (`agent_stub.exe`) está disponible para inyectar configuraciones en milisegundos.
   - **Huella SHA-256 (icono escudo)**: huella del certificado TLS 1.3 de tu servidor (puedes hacer clic para copiarla).

### Zona principal derecha
- Mientras no haya equipos conectados, muestra *"Esperando conexiones inversas en UDP :4433"* y un botón rápido **"Generar primer cliente"**.
- En cuanto un usuario ejecuta su `.exe`, su tarjeta aparece aquí automáticamente con estado **`Online`** pulsante.

---

## 3. Paso a paso: Generar un Cliente y Tomar el Control

### Paso 1 — Generar el ejecutable cliente (en tu pantalla Admin)
1. Pulsa el botón verde **`Generar Cliente`**.
2. Rellena los campos del modal:
   - **Etiqueta del Cliente:** un nombre para identificar al equipo o empresa (ej. `PC Oficina Juan` o `Prueba Local`).
   - **Nombre del Administrador:** el nombre que verá el usuario final en su bandeja del sistema (ej. `Soporte IT`).
   - **Endpoints de Conexión Inversa:** la dirección `IP:puerto` o `dominio:puerto` a la que se conectará el cliente:
     - **Para probar en tu mismo PC:** usa `127.0.0.1:4433` (puedes pulsar la píldora `+ 127.0.0.1:4433` debajo del cuadro).
     - **Para otro PC en tu Wi-Fi/oficina (LAN):** usa tu IP local que aparece sugerida (ej. `192.168.1.50:4433`).
     - **Para un PC fuera de tu red (Internet):** pon tu dominio DDNS o IP pública `:4433` (o tu IP de Tailscale/WireGuard `:4433`).
     - *Truco:* puedes poner **varias líneas** (ej. línea 1 IP LAN, línea 2 dominio público); el cliente probará una tras otra automáticamente si la primera no responde.
   - **Nombre del Ejecutable:** se rellena solo (ej. `Gecko-Prueba-Local.exe`).
   - **Caducidad del Token:** elige cuánto tiempo será válido ese ejecutable (`1 hora`, `24 horas`, `7 días` o `Sin caducidad`).
3. Pulsa **`Generar .EXE`**.
   - En menos de 100 ms, Gecko toma `agent_stub.exe`, firma la configuración con tu clave privada **Ed25519** y la anexa al final del binario.
4. Pulsa **`Abrir en Explorador`** para ir directo al `.exe` generado (se guarda en `%LOCALAPPDATA%\gecko\admin\data\generated-clients\`).

### Paso 2 — Ejecutar el cliente (en tu PC para pruebas, o en el PC del usuario)
1. Envía ese único archivo `.exe` (~2.12 MB) al usuario final por correo, chat, enlace o USB (o ejecútalo tú mismo si estás probando en local).
2. El usuario hace **doble clic**.
   - **No instala nada.**
   - **No pide IDs, ni IPs, ni contraseñas.**
   - Aparece un icono discreto en la bandeja del sistema de Windows (junto al reloj) indicando `Gecko — Online (Soporte IT)`.
   - El usuario puede hacer **clic derecho sobre el icono → `Desconectar y Salir`** en cualquier momento para cortar el acceso.

### Paso 3 — Ver el equipo Online y tomar el control (en tu pantalla Admin)
1. En tu panel **Gecko**, el equipo aparece al instante en **`Equipos Online`** mostrando:
   - Nombre del PC (`Hostname`) y usuario de Windows.
   - Etiqueta con la que generaste el cliente.
   - Tarjeta gráfica (GPU), cantidad de monitores, IP remota y latencia **RTT en ms**.
2. Haz **doble clic sobre la tarjeta del equipo** (o pulsa el botón **`Controlar`**).
3. Se abre automáticamente el **Visor de Control Remoto** a pantalla completa dentro de la app:
   - El agente remoto captura directamente desde VRAM (**DXGI Desktop Duplication → NV12 en GPU → NVENC/AMF/QuickSync**) y envía los fotogramas por **datagramas QUIC**.
   - Tu aplicación Admin decodifica por hardware (**WebCodecs**) y ya puedes mover el ratón, hacer clic, hacer scroll y escribir (incluso con distinto idioma de teclado gracias a los *Hardware Scan Codes*).

---

## 4. Controles del Visor Remoto y Gestión de Sesiones

### Barra flotante superior del visor
| Control | Función |
| :--- | :--- |
| **Nombre + punto verde** | Equipo y usuario que estás controlando. |
| **Chip de Encoder** | Muestra qué codificador hardware está activo en el cliente (`NVIDIA`, `AMD`, `Intel Quick Sync` o fallback software). |
| **Selector `#1` `#2`** | Aparece si el cliente tiene varios monitores; permite cambiar de pantalla al instante. |
| **Bitrate (`4`–`35 Mbps`)** | Ajusta la calidad/ancho de banda del codificador hardware en vivo sin reiniciar la sesión. |
| **FPS (`30` / `60` / `120`)** | Cambia la tasa de fotogramas objetivo al vuelo. |
| **Icono refresco (IDR)** | Fuerza un fotograma clave inmediato si hubo un pico de pérdida de red. |
| **Icono pulso (HUD)** | Muestra u oculta el panel técnico inferior izquierdo (FPS reales, RTT en ms, tiempo de captura DXGI, tiempo de encode GPU, bitrate real y resolución). |
| **Pantalla completa** | Expande el visor a pantalla completa real (`Esc` o el mismo botón para salir). |
| **Botón rojo `Finalizar`** | Detiene la transmisión de video y vuelve al Dashboard (el equipo remoto sigue `Online` listo para reconectar). |

### Desconectar o Revocar un Cliente
- **Desconectar sesión temporalmente:** pasa el ratón sobre la tarjeta del equipo en el dashboard y pulsa el icono de papelera arriba a la derecha.
- **Revocar un `.exe` permanentemente:** entra en **`Clientes Emitidos`** en la barra lateral y pulsa **`Revocar`** junto al cliente que quieras bloquear. Si estaba conectado, se desconecta al instante y ese `.exe` nunca podrá volver a entrar.

---

## 5. Configuración de Red (Local, LAN e Internet)

El cliente remoto **nunca** requiere configuración de red ni abrir puertos (hace conexión saliente). Solo la máquina del **Administrador** recibe conexiones UDP en el puerto **4433**:

### En Red Local (LAN / Wi-Fi)
Abre el puerto **UDP 4433** en el Firewall de Windows del PC del Administrador (PowerShell como Administrador, una sola vez):

```powershell
New-NetFirewallRule -DisplayName "Gecko Admin QUIC (UDP 4433)" -Direction Inbound -Protocol UDP -LocalPort 4433 -Action Allow
```

### Por Internet (WAN)
Elige la opción que mejor se adapte a tu red:
- **Router con IP pública / DDNS:** abre y redirige el puerto **UDP 4433** en tu router hacia la IP local del PC del Administrador, y usa `tu-dominio.ddns.net:4433` al generar clientes.
- **Sin abrir puertos en router (CGNAT):** usa una red privada tipo **Tailscale** o **WireGuard** y coloca la IP VPN del Administrador (ej. `100.x.y.z:4433`) en los endpoints.

### Cambiar el puerto UDP o activar logs detallados (opcional)
```powershell
$env:GECKO_PORT = "4433"
$env:RUST_LOG = "rmm_admin=debug,rmm_net=debug"
cd c:\Users\JAN\Desktop\remote\apps\admin
npx.cmd tauri dev
```

---

## 6. Requisitos y Comandos para Desarrollo

### Requisitos previos
- **Rust / Cargo** (`x86_64-pc-windows-msvc`).
- **Visual Studio Build Tools** (*Desarrollo de escritorio con C++*).
- **Node.js 20+** (`npm.cmd` / `npx.cmd`).
- **WebView2 Runtime** (incluido en Windows 10/11).

### Si modificas el código del Agente (`apps/agent` o `crates/*`)
Debes recompilar el stub `agent_stub.exe` para que los nuevos clientes que generes incluyan tus cambios:

```powershell
cd c:\Users\JAN\Desktop\remote
$env:RUSTFLAGS = "-C target-feature=+crt-static"
cargo build -p rmm-agent --profile release-agent
Remove-Item Env:\RUSTFLAGS -ErrorAction SilentlyContinue
Copy-Item -Force "target\release-agent\rmm-agent.exe" "apps\admin\src-tauri\resources\agent_stub.exe"
```

### Ejecutar tests y verificaciones
```powershell
cd c:\Users\JAN\Desktop\remote

# Verificar todo el workspace Rust (7 paquetes)
cargo check --workspace

# Ejecutar tests unitarios e integración (overlay Ed25519, reensamblador, QUIC TLS Pinning)
cargo test -p rmm-proto -p rmm-net

# Verificar TypeScript y compilar frontend React
npm.cmd --prefix apps\admin run build
```

---

## 7. Dónde se guardan tus datos y archivos generados

Gecko guarda automáticamente su identidad criptográfica y archivos en:

```text
%LOCALAPPDATA%\gecko\admin\data\
├── admin_identity.json     # Clave privada Ed25519 + certificado TLS 1.3 (pinning persistente)
├── issued_tokens.json      # Historial de clientes generados y lista de revocación
└── generated-clients\      # Todos los archivos Gecko-*.exe generados
```

> **Importante:** No borres `admin_identity.json` salvo que quieras invalidar todos los clientes `.exe` generados anteriormente (si se borra, Gecko creará un nuevo certificado TLS y una nueva clave Ed25519 al volver a abrirse).

---

## 8. Solución de Problemas Frecuentes

| Problema | Causa habitual | Solución |
| :--- | :--- | :--- |
| Abrí `localhost:5173` en Chrome y no funciona nada | El navegador normal no tiene el backend nativo de Tauri ni el servidor QUIC. | Usa la **ventana de escritorio de Gecko** que abre `npx.cmd tauri dev` (o el ejecutable compilado). |
| PowerShell bloquea `npm` o `npx` (*UnauthorizedAccess*) | Política de ejecución de scripts `.ps1` de Windows. | Usa siempre **`npm.cmd`** y **`npx.cmd`**. |
| El cliente se abre pero nunca sale `Online` en LAN | Firewall de Windows del Admin bloqueando **UDP 4433** o IP incorrecta en Endpoints. | Crea la regla de firewall de la Sección 5 y verifica tu IP LAN en la barra lateral de Gecko. |
| En prueba local (`127.0.0.1`) veo "efecto espejo infinito" | Estás capturando la misma pantalla donde tienes abierto el visor Admin. | Normal al probar en un único PC con un solo monitor; mueve/redimensiona la ventana o prueba con un segundo monitor o segundo PC. |
| No puedo hacer clic en ventanas de Administrador o UAC del cliente | El cliente se abrió sin privilegios elevados (protección UIPI de Windows). | Ejecuta el `.exe` cliente con *Clic derecho → Ejecutar como administrador* cuando necesites controlar diálogos UAC o apps elevadas. |
| Windows SmartScreen muestra aviso azul al ejecutar el `.exe` en otro PC | El ejecutable no lleva firma digital Authenticode comercial. | En pruebas pulsa *Más información → Ejecutar de todas formas*. En producción firma el `.exe` resultante con tu certificado mediante `signtool`. |
| Quiero ver logs en consola del Agente Cliente | En perfil Release el agente oculta la ventana negra (`windows_subsystem = "windows"`). | Compila en debug (`cargo build -p rmm-agent`), genera el cliente apuntando a ese stub y ejecútalo desde PowerShell con `$env:RUST_LOG="debug"`. |
