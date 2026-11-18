# Control Remoto Simple (con Servidor Puente)

Aplicación de control remoto nativa para Windows con **servidor puente (Relay TLS 1.3)** para conectar PCs en **cualquier red** sin abrir puertos en el PC del administrador ni en los clientes, construida en **Go puro sin dependencias externas**.

```text
[ ClienteRemoto.exe ] ──TLS 1.3──▶ [ RelayServidor (VPS) ] ◀──TLS 1.3── [ AdminRemoto.exe ]
  (Cualquier red)                     (Puente seguro)                     (Cualquier red)
```

## Componentes

1. **Servidor Puente (`RelayServidor` / `RelayServidor.exe`)**:
   - Se levanta en tu VPS, servidor Linux/Windows o Docker (puerto TCP `9443`).
   - Genera automáticamente su certificado TLS 1.3 y dos tokens independientes (**Admin** y **Cliente**) en `./data`.
   - Muestra en consola una única clave **`RMT1:...`** para pegar en `AdminRemoto.exe`.

2. **Aplicación de Administración (`AdminRemoto.exe`)**:
   - Aplicación de escritorio nativa con menú superior fijo:
     - **Equipos**: lista en tiempo real de todos los PCs conectados a tu servidor puente.
     - **Crear cliente**: genera **`ClienteRemoto.exe`** (el mismo archivo sirve para todos los equipos) con dirección del servidor puente, título, mensaje, calidad inicial, tiempo de espera, inicio minimizado, sonido, botón Desconectar y logo personalizado (PNG/JPG).
     - **Servidor**: pega la dirección de tu servidor y su clave **`RMT1:...`** una sola vez.
   - Visor remoto nativo: Monitor 1 por defecto con selector **`M1 / M2 / Todos`**, calidad **Baja / Estándar / Alta**, FPS/latencia en vivo y atajos **`F11`**, **`Ctrl+Alt+M`** y **`Ctrl+Alt+Q`**.

3. **Aplicación Cliente (`ClienteRemoto.exe`)**:
   - Ejecutable nativo de Windows sin instalación.
   - Muestra el logo (si se configuró), el **nombre del equipo** y su estado; al recibir solicitud del administrador solo ofrece **Permitir** o **Denegar**.

## Compilación

Requisitos: Go 1.22 o superior.

```cmd
build.bat
```

Genera en `dist\`:
- `AdminRemoto.exe` (Windows 64-bit)
- `RelayServidor` (Linux 64-bit estático, listo para subir a cualquier VPS)
- `RelayServidor.exe` (Windows 64-bit)

## Despliegue del servidor puente

### Opción A: VPS Linux (binario directo)
```bash
chmod +x RelayServidor
./RelayServidor -listen :9443 -public-addr remoto.tudominio.com:9443
```

### Opción B: Docker
```bash
docker build -t remote-relay .
docker run -d --name remote-relay --restart unless-stopped \
  -p 9443:9443 \
  -v relay_data:/data \
  -e RELAY_PUBLIC_ADDR=remoto.tudominio.com:9443 \
  remote-relay
docker logs remote-relay
```

### Opción C: Servidor Windows
```cmd
RelayServidor.exe -listen :9443 -public-addr remoto.tudominio.com:9443
```

> Abre únicamente el puerto TCP **9443** en el firewall del servidor puente. Ni el PC del administrador ni los PCs cliente necesitan abrir puertos.

## Uso rápido

1. Arranca `RelayServidor` en tu servidor y copia la línea **`RMT1:...`** que imprime al iniciar.
2. Abre `dist\AdminRemoto.exe`, pega la dirección de tu servidor y la clave **`RMT1:...`** en **Servidor**, y pulsa **Guardar y conectar**.
3. Ve a **Crear cliente**, personaliza las opciones que quieras y guarda **`ClienteRemoto.exe`**.
4. Ejecuta ese **`ClienteRemoto.exe`** en cualquier PC (esté en tu red o en otra distinta) y aparecerá automáticamente en **Equipos**.

## Seguridad

- **TLS 1.3 obligatorio + Certificate Pinning (SHA-256)** tanto en `AdminRemoto.exe` como en `ClienteRemoto.exe`.
- **Separación de roles por token**: `ClienteRemoto.exe` solo recibe `ClientToken` y nunca puede autenticarse como administrador ni listar otros equipos.
- **Consentimiento explícito**: el cliente nunca envía vídeo ni acepta entrada hasta que el usuario pulsa **Permitir**.
