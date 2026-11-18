# Control Remoto Simple

Aplicación ligera, directa y segura de control remoto entre PCs con Windows, construida en **Go puro sin dependencias externas**.

## Cómo funciona

1. **Panel de Administración (`AdminRemoto.exe`)**:
   - Muestra en tiempo real la lista de equipos conectados.
   - Permite generar y descargar **`ClienteRemoto.exe`** con un clic (el mismo ejecutable sirve para todos los equipos).
   - Al pulsar un equipo en línea, solicita permiso al usuario y, si acepta, abre el escritorio remoto con control de ratón y teclado.

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
- `dist\AdminRemoto.exe`: aplicación principal de administración (lleva incrustada la plantilla del cliente).

## Uso rápido

1. Ejecuta `dist\AdminRemoto.exe` en el PC del administrador.
   - Se abrirá automáticamente el panel web local en tu navegador (`127.0.0.1`).
   - En el primer arranque se crea la carpeta `data\` con el certificado TLS 1.3 y el token de autenticación.
2. Pulsa **Generar cliente (.exe)**, verifica la IP de tu servidor (se detecta automáticamente tu IP de red local) y descarga **`ClienteRemoto.exe`**.
3. Copia y ejecuta ese mismo **`ClienteRemoto.exe`** en cualquier PC que quieras administrar.
4. Los equipos aparecerán en la lista de **Equipos en línea**. Pulsa cualquiera para solicitar acceso.

> **Nota sobre Firewall de Windows**: Si los clientes están en otros PCs de tu red local, permite el puerto TCP **9443** (configurable con `-agent-port`) en el Firewall de Windows del PC del administrador.

## Opciones de línea de comandos (`AdminRemoto.exe`)

| Flag | Por defecto | Descripción |
| :--- | :--- | :--- |
| `-agent-port` | `9443` | Puerto TCP (TLS 1.3) para conexiones de clientes |
| `-ui-port` | `8090` | Puerto local (`127.0.0.1`) del panel web |
| `-data-dir` | `./data` | Carpeta donde se guardan certificado y token |
| `-no-browser` | `false` | Evita abrir el navegador automáticamente |

## Arquitectura y seguridad

- **Cero dependencias de terceros**: 100% biblioteca estándar de Go + API nativa Win32 (`user32`, `gdi32`).
- **TLS 1.3 obligatorio con Certificate Pinning (SHA-256)**: `ClienteRemoto.exe` lleva incrustada la huella SHA-256 exacta del certificado del administrador y rechaza cualquier conexión que no coincida.
- **Autenticación por token**: token aleatorio de 256 bits verificado en tiempo constante.
- **Consentimiento explícito en el cliente**: el cliente no inicia la captura de pantalla ni acepta eventos de ratón/teclado hasta que el usuario pulsa **Permitir**. El diálogo no roba el foco del teclado para evitar aprobaciones accidentales mientras el usuario escribe.
- **Panel local protegido**: el panel web solo escucha en `127.0.0.1`, requiere token de sesión por arranque en cookie `HttpOnly; SameSite=Strict` y valida la cabecera `Origin`.
- **Rendimiento**: captura GDI nativa con detección de regiones modificadas (*dirty tiles* 128×128 en JPEG) y envío binario directo sin sobrecarga base64. Si la pantalla no cambia, el consumo de red es 0.
