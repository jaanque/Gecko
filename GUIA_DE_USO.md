# Guía Completa de Uso y Arranque — Gecko (100% Nativo)

**Gecko** es un sistema de control remoto de escritorio **100% nativo en Rust** tanto en el **Administrador (`Gecko.exe`)** como en el **Cliente (`Gecko-*.exe`)** — sin navegador, sin Node.js, sin WebView y sin servidores intermedios.

---

## 1. ¿Cómo abro la pantalla de Admin de Gecko?

Tienes **dos formas directas** de abrir la aplicación de escritorio **Gecko** en tu PC de Administrador:

### Opción A — Doble clic en el ejecutable nativo (Recomendado)

Tras compilar (ya está compilado en `dist-gecko\`), solo abre esta carpeta en el Explorador de Windows y haz **doble clic en `Gecko.exe`**:

```text
c:\Users\JAN\Desktop\remote\dist-gecko\Gecko.exe
```

*(Mantén la subcarpeta `resources\agent_stub.exe` al lado de `Gecko.exe` para que el botón **Generar Cliente** funcione desde cualquier carpeta donde muevas `dist-gecko\`.)*

Si quieres recompilar `dist-gecko\` tras hacer cambios en el código:

```powershell
cd c:\Users\JAN\Desktop\remote
powershell -ExecutionPolicy Bypass -File scripts\build-release.ps1
```

---

### Opción B — Abrirla desde terminal en modo desarrollo

```powershell
cd c:\Users\JAN\Desktop\remote
cargo run -p gecko-admin
```

Al abrirse la ventana de escritorio **Gecko**, arrancan automáticamente en segundo plano dentro del mismo proceso:
- **Servidor QUIC de conexión inversa** en **`UDP :4433`** (TLS 1.3 + *certificate pinning*).
- **Auto-Discovery LAN firmado** en **`UDP :4434`** (permite que los clientes te encuentren automáticamente en tu red local incluso si cambia tu IP de Wi-Fi/DHCP).

---

## 2. Qué ves en la pantalla de Gecko Admin

### Barra lateral izquierda
1. **Logo `G Gecko [NATIVO]`**.
2. **Botón verde `+ Generar Cliente (.EXE)`**: genera un ejecutable cliente listo para enviar en <100 ms.
3. **Pestañas**:
   - **`Equipos`**: muestra los equipos **ONLINE** (con latencia RTT en tiempo real) y debajo el historial de equipos **RECONOCIDOS OFFLINE** (identificados de forma persistente por su `machine_id` de hardware).
   - **`Clientes Emitidos`**: listado de todos los `.exe` generados, con botón **Abrir Archivo** y botón **Revocar** para cortar el acceso permanentemente.
4. **Tarjeta `Backend QUIC Activo` (abajo a la izquierda)**:
   - Puerto **`QUIC UDP :4433`** y **`Auto-LAN UDP :4434`**.
   - Tu **`IP Local`** detectada automáticamente.
   - Estado de la plantilla **`Stub OK`**.
   - Botón para copiar la huella criptográfica **TLS SHA-256**.

---

## 3. Paso a paso: Generar Cliente, Conectar y Controlar

### Paso 1 — Generar el ejecutable cliente (`Gecko-*.exe`)
1. Pulsa **`+ Generar Cliente (.EXE)`**.
2. Configura:
   - **Etiqueta del Cliente:** nombre descriptivo (ej. `PC Oficina`, `Prueba Local`).
   - **Nombre del Administrador:** se muestra en la bandeja del sistema del cliente para transparencia.
   - **Endpoints del Admin:**
     - **En tu mismo PC:** `127.0.0.1:4433` (puedes pulsar `+ 127.0.0.1:4433`).
     - **En otro PC de tu LAN/Wi-Fi:** tu IP local sugerida (ej. `192.168.1.50:4433`). Además, gracias al **Auto-Discovery LAN**, aunque cambie tu IP local el agente te reencontrará solo.
     - **Por Internet (WAN):** tu dominio DDNS o IP pública `:4433` (o IP Tailscale/WireGuard `:4433`). Puedes poner varias líneas (1 por línea) para *failover* automático.
   - **Caducidad:** `1 hora`, `24 horas`, `7 días` o `Sin caducidad`.
3. Pulsa **`Generar .EXE`** y luego **`Abrir en Explorador`**.

### Paso 2 — Ejecutar el cliente (Zero-Config)
1. Envía el `.exe` (~2.1 MB, sin dependencias) al equipo remoto y haz **doble clic**.
2. No pide IDs, contraseñas ni instalación. Aparece un icono discreto en la bandeja del sistema de Windows (`Gecko — Online`) desde el cual el usuario puede elegir **Clic derecho → Desconectar y Salir** en cualquier momento.
3. En tu pantalla **Gecko Admin**, el equipo aparece al instante bajo **`ONLINE`** (si ese equipo se reconecta más tarde, Gecko lo reconoce por su `machine_id` sin crear duplicados).

### Paso 3 — Entrar en control remoto y usar ratón y teclado
1. Haz **doble clic** en la tarjeta del equipo o pulsa **`▶ Controlar`**.
2. Se abre el **Visor Remoto Nativo** (decodificación H.264/HEVC nativa por Windows Media Foundation renderizada en GPU):
   - **En cualquier equipo remoto:** el control de ratón y teclado está **ACTIVO automáticamente**. Solo coloca el puntero sobre la pantalla remota para mover el ratón, hacer clic izquierdo/derecho/central, arrastrar, usar la rueda y escribir (incluye `Shift`, `Ctrl`, `Alt`, `Win`, flechas y teclas de función independientes del idioma del teclado).
   - **Si estás probando en tu mismo PC (`ESTE PC`):** para evitar un bucle donde tu ratón se persigue a sí mismo, el control arranca en modo **`Control: PAUSADO (F9)`**; pulsa **`F9`** o el botón de la barra superior cuando quieras activar/pausar el control de ratón y teclado.

---

## 4. Controles de la Barra Superior del Visor

| Control | Función |
| :--- | :--- |
| **Nombre + Encoder** | Muestra el PC remoto y el codificador hardware activo (`NVIDIA`, `AMD`, `Intel Quick Sync`). |
| **`#1` `#2`** | Cambia entre monitores si el equipo remoto tiene varias pantallas. |
| **`4`–`35 Mbps` / `30`–`120 FPS`** | Ajusta bitrate y tasa de refresco del encoder hardware al vuelo sin cortar la sesión. |
| **`Control: ACTIVO / PAUSADO (F9)`** | Activa o pausa la inyección de ratón y teclado con un clic o pulsando `F9`. |
| **`IDR`** | Fuerza un fotograma clave inmediato. |
| **`HUD ✓`** | Muestra u oculta la barra inferior con métricas en vivo (`FPS`, `RTT`, `CAP`, `ENC`, `DEC`, `RATE` y resolución). |
| **`Pantalla Completa`** | Expande el visor a pantalla completa real. |
| **`Finalizar`** | Detiene el streaming de video y vuelve al dashboard (el equipo sigue `Online`). |

---

## 5. Firewall y Red (LAN e Internet)

El equipo **Cliente nunca necesita abrir puertos**. Solo el PC del **Administrador** recibe tráfico UDP entrante:

### Abrir puertos en Windows Firewall del Administrador (PowerShell como Administrador, una sola vez)
```powershell
New-NetFirewallRule -DisplayName "Gecko Admin QUIC (UDP 4433)" -Direction Inbound -Protocol UDP -LocalPort 4433 -Action Allow
New-NetFirewallRule -DisplayName "Gecko Admin Auto-LAN (UDP 4434)" -Direction Inbound -Protocol UDP -LocalPort 4434 -Action Allow
```

### Conexión por Internet (WAN)
- **Router con IP pública / DDNS:** redirige el puerto **UDP 4433** en tu router hacia la IP local del PC Admin y pon `tu-dominio.ddns.net:4433` en Endpoints.
- **Sin abrir puertos en router (CGNAT):** usa Tailscale o WireGuard y coloca la IP VPN del Admin (`100.x.y.z:4433`).

### Variables de entorno opcionales
```powershell
$env:GECKO_PORT = "4433"
$env:GECKO_DISCOVERY_PORT = "4434"
$env:RUST_LOG = "gecko_admin=debug,rmm_net=debug"
cargo run -p gecko-admin
```

---

## 6. Datos Persistentes y Tests

Gecko guarda sus datos locales del Administrador en:

```text
%LOCALAPPDATA%\gecko\admin\data\
├── admin_identity.json     # Clave Ed25519 + certificado TLS 1.3 (pinning persistente)
├── known_devices.json      # Historial de equipos reconocidos (machine_id, GPU, última conexión)
├── issued_tokens.json      # Registro de clientes emitidos y revocaciones
└── generated-clients\      # Ejecutables Gecko-*.exe generados
```

### Ejecutar tests automatizados
```powershell
cd c:\Users\JAN\Desktop\remote
cargo test -p rmm-proto -p rmm-net
```

---

## 7. Solución de Problemas Frecuentes

| Problema | Causa habitual | Solución |
| :--- | :--- | :--- |
| El cliente se ejecuta en otro PC de la LAN pero no sale `Online` | Firewall de Windows del Admin bloqueando UDP `4433` / `4434`. | Ejecuta las 2 reglas de `New-NetFirewallRule` de la Sección 5 como Administrador. |
| Al probar en mi mismo PC el ratón no se mueve al abrir el visor | Protección anti-bucle activada automáticamente cuando Admin y Cliente son el mismo PC (`ESTE PC`). | Pulsa **`F9`** o el botón **`Control: PAUSADO (F9)`** en la barra superior para activarlo/pausarlo. |
| No puedo hacer clic en ventanas de Administrador o diálogos UAC del cliente | El cliente corre sin privilegios elevados (restricción UIPI de Windows). | Ejecuta el `.exe` cliente con *Clic derecho → Ejecutar como administrador* para controlar ventanas elevadas. |
| SmartScreen muestra aviso en otro PC al abrir `Gecko-*.exe` | El binario no tiene firma Authenticode comercial. | Pulsa *Más información → Ejecutar de todas formas* en pruebas, o firma el `.exe` con `signtool` en producción. |
