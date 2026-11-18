# Control Remoto Simple

Aplicación de escritorio nativa para Windows, ligera, rápida y segura para control remoto entre PCs, construida en **Go puro sin dependencias externas**.

## Cómo funciona

1. **Aplicación de Administración (`AdminRemoto.exe`)**:
   - Aplicación de escritorio nativa de Windows (sin navegador ni consola) con menú superior fijo:
     - **Equipos**: lista en tiempo real de equipos en línea; pulsa cualquiera para solicitar acceso.
     - **Crear cliente**: configura IP:Puerto, título de ventana, mensaje de solicitud, calidad inicial, tiempo de espera, inicio minimizado, sonido, botón Desconectar y logo personalizado (PNG/JPG), y guarda **`ClienteRemoto.exe`** (un único ejecutable válido para todos los equipos).
   - Visor remoto nativo:
     - Muestra por defecto el **Monitor 1**; si hay varios monitores, permite saltar entre ellos (`M1`, `M2`, …) o ver **`Todos`** a la vez.
     - Barra superior ocultable (`▲` / `▼ Menú`), calidad **Baja / Estándar / Alta**, FPS y latencia en tiempo real, y atajos **`F11`** (pantalla completa), **`Ctrl+Alt+M`** (menú) y **`Ctrl+Alt+Q`** (desconectar).

2. **Aplicación Cliente (`ClienteRemoto.exe`)**:
   - Ejecutable nativo de Windows sin instalación ni dependencias.
   - Muestra el logo (si se configuró), el **nombre del equipo** y su estado de conexión.
   - Al recibir solicitud del administrador, ofrece únicamente **Permitir** o **Denegar**.

## Compilación

Requisitos: Go 1.22 o superior.

```cmd
build.bat
```

*(También incluido `build.ps1` para PowerShell.)*

Genera `dist\AdminRemoto.exe` con la plantilla del cliente ya incrustada.

## Uso rápido

1. Abre `dist\AdminRemoto.exe` en el PC del administrador (en el primer arranque crea `data\` con el certificado TLS 1.3 y el token).
2. Ve a **Crear cliente**, ajusta las opciones que quieras y guarda **`ClienteRemoto.exe`**.
3. Ejecuta ese mismo **`ClienteRemoto.exe`** en cualquier equipo a administrar.
4. En **Equipos**, pulsa el equipo conectado para solicitar control.

> **Firewall de Windows**: permite el puerto TCP **9443** (configurable con `-agent-port`) en el PC del administrador si conectas equipos en red local.

## Opciones de línea de comandos (`AdminRemoto.exe`)

| Flag | Por defecto | Descripción |
| :--- | :--- | :--- |
| `-agent-port` | `9443` | Puerto TCP (TLS 1.3) para clientes |
| `-data-dir` | `./data` | Carpeta para certificados y token |

## Arquitectura y seguridad

- **Cero dependencias externas**: 100 % biblioteca estándar de Go + API nativa Win32.
- **TLS 1.3 con Certificate Pinning (SHA-256)** + token aleatorio de 256 bits verificado en tiempo constante.
- **Consentimiento explícito en el cliente**: sin captura ni control de entrada hasta pulsar **Permitir**.
- **Vídeo y control optimizados**: bloques `64×64` con códec híbrido (sin pérdida `Zlib-BGR` para texto/UI + JPEG paralelo para vídeo), control de flujo por ACK hasta 60 FPS, ratón de 5 botones a 120 Hz coalescente y reenvío nativo de atajos de sistema en primer plano.
