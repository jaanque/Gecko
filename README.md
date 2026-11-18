# Control Remoto Simple

Aplicación de escritorio nativa para Windows, ligera, directa y segura para control remoto entre PCs, construida en **Go puro sin dependencias externas** ni servidor web.

## Cómo funciona

1. **Aplicación de Administración (`AdminRemoto.exe`)**:
   - Aplicación de escritorio nativa de Windows (sin navegador ni ventana de consola).
   - Muestra en tiempo real la lista de **equipos en línea**.
   - Incluye el botón **Generar cliente (.exe)** para elegir la IP del servidor y guardar **`ClienteRemoto.exe`** directamente en disco (el mismo ejecutable sirve para todos los equipos).
   - Al pulsar un equipo disponible, solicita permiso al usuario y, si acepta, abre una ventana nativa de control remoto con ratón, teclado, selector de calidad y pantalla completa (`F11`).

2. **Aplicación Cliente (`ClienteRemoto.exe`)**:
   - Ejecutable nativo de Windows (sin ventana de consola, sin instalar nada).
   - Muestra únicamente el **nombre del equipo** y su estado de conexión.
   - Cuando el administrador solicita acceso, aparecen solo dos botones: **Permitir** y **Denegar**.
   - Durante una sesión activa muestra un aviso visible con el botón **Desconectar** para cortar el acceso en cualquier momento.

## Compilación

Requisitos: Go 1.22 o superior.

```cmd
build.bat
```

*(También incluido `build.ps1` si usas PowerShell con ejecución de scripts habilitada.)*

Esto genera:
- `dist\AdminRemoto.exe`: aplicación de escritorio del administrador (con la plantilla del cliente incrustada).

## Uso rápido

1. Haz doble clic en `dist\AdminRemoto.exe` en el PC del administrador.
   - En el primer arranque se crea la carpeta `data\` junto al ejecutable con el certificado TLS 1.3 y el token de autenticación.
2. Pulsa **Generar cliente (.exe)**, elige la IP del servidor (se detecta automáticamente tu IP LAN, además de `127.0.0.1` para pruebas locales) y guarda **`ClienteRemoto.exe`**.
3. Copia y ejecuta ese mismo **`ClienteRemoto.exe`** en cualquier PC que quieras controlar.
4. Los equipos aparecerán automáticamente en **Equipos en línea**. Pulsa cualquiera para solicitar acceso.

> **Nota sobre Firewall de Windows**: Si los clientes están en otros PCs de tu red local, permite el puerto TCP **9443** (configurable con `-agent-port`) en el Firewall de Windows del PC del administrador.

## Opciones opcionales (`AdminRemoto.exe`)

| Flag | Por defecto | Descripción |
| :--- | :--- | :--- |
| `-agent-port` | `9443` | Puerto TCP (TLS 1.3) donde escuchan los clientes |
| `-data-dir` | `./data` | Carpeta donde se guardan certificado y token |

## Arquitectura y seguridad

- **100% nativo y cero dependencias de terceros**: biblioteca estándar de Go + API nativa Win32 (`user32`, `gdi32`, `comdlg32`). Sin navegador, sin Electron, sin WebView, sin servidor HTTP.
- **TLS 1.3 obligatorio con Certificate Pinning (SHA-256)**: `ClienteRemoto.exe` lleva incrustada la huella SHA-256 exacta del certificado del administrador y rechaza cualquier conexión que no coincida.
- **Autenticación por token**: token aleatorio de 256 bits verificado en tiempo constante.
- **Consentimiento explícito en el cliente**: el cliente no captura pantalla ni acepta eventos de ratón/teclado hasta que el usuario pulsa **Permitir**. El aviso no roba el foco del teclado para evitar aprobaciones accidentales mientras el usuario escribe.
- **Rendimiento**: captura GDI nativa con detección de regiones modificadas (*dirty tiles* 128×128 en JPEG), decodificación en segundo plano y renderizado directo con `StretchDIBits` (doble búfer sin parpadeos). Si la pantalla no cambia, el tráfico de red es 0.
