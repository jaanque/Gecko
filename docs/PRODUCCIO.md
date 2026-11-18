# Guia tècnica i de producció

Aquest document recull els detalls d'arquitectura, compilació, desplegament en producció, administració de seguretat i resolució de problemes de **Gecko**.

---

## Components

| Binari | Plataforma | Funció |
| :--- | :--- | :--- |
| `dist/linux/RelayServidor` · `dist/windows/RelayServidor.exe` · `dist/macos/RelayServidor` | Linux, Windows, macOS, Docker (`scratch`) | Servidor pont mTLS 1.3, gestió de PKI/revocació, auditoria i enrutament de sessions. |
| `dist/windows/AdminRemoto.exe` | Windows (amd64/arm64) | Consola d'administració nativa Win32 (o consola web local amb `-web`), visor remot i generador de clients. |
| `dist/linux/AdminRemoto` · `dist/macos/AdminRemoto` | Linux · macOS | Consola d'administració web local (enllaçada a `127.0.0.1` amb token de sessió) i visor HTML5 Canvas. |
| `ClientRemot.exe` | Windows (amd64/arm64) | Agent portable amb captura GPU **DXGI Desktop Duplication** (+ *fallback* GDI), multiidioma (`ca`/`es`/`en`). |
| `ClientRemot` | Linux X11 / Wayland | Agent portable amb protocol X11 natiu i suport Wayland (`grim`/`spectacle`/`gnome-screenshot`, `ydotool`, `wl-copy`). |
| `ClientRemot-macos` | macOS (arm64/amd64) | Agent portable amb `screencapture`, CoreGraphics (`CGEvent`), `pbcopy`/`pbpaste` i diàleg natiu. |

---

## Compilació i verificació

Requereix **Go 1.22+** (sense `cgo`; funciona des de Windows, Linux o macOS):

```sh
# Compilar tots els binaris separats per SO a dist/{windows,linux,macos}/
go run ./cmd/build

# Compilar amb versió personalitzada o arquitectura ARM64
go run ./cmd/build -version 1.0.0 -arch arm64 -macos-arch arm64

# Comprovar la versió incrustada de qualsevol binari
./dist/linux/RelayServidor -version

# Executar tota la bateria de tests amb detector de condicions de cursa
go test -race -count=1 ./...
```

---

## Desplegament en producció

> [!IMPORTANT]
> **Requisits de xarxa**: només cal obrir el port **TCP 9443** (o el configurat a `-listen`) d'entrada al servidor on s'executa `RelayServidor`. Els equips clients i l'administrador només fan connexions TCP sortints.

### Opció 1 · Docker / Docker Compose (Recomanat)

La imatge final es construeix sobre `scratch` (~5,5 MB), sense shell ni eines externes, i incorpora **`HEALTHCHECK` natiu**.

```sh
docker build -t gecko-relay .

docker run -d \
  --name gecko-relay \
  --restart unless-stopped \
  -e RELAY_PUBLIC_ADDR=suport.el-teu-domini.cat:9443 \
  -p 9443:9443 \
  -v gecko_relay_data:/data \
  gecko-relay
```

Exemple de `docker-compose.yml`:

```yaml
services:
  gecko-relay:
    build: .
    container_name: gecko-relay
    restart: unless-stopped
    ports:
      - "9443:9443"
    environment:
      RELAY_LISTEN: ":9443"
      RELAY_DATA_DIR: "/data"
      RELAY_PUBLIC_ADDR: "suport.el-teu-domini.cat:9443"
    volumes:
      - gecko_relay_data:/data

volumes:
  gecko_relay_data:
```

Per obtenir la clau inicial o consultar-la puntualment més endavant:

```sh
# Primera inicialització (es mostra un sol cop)
docker logs gecko-relay

# Consultar la clau mestra puntualment
docker exec gecko-relay /RelayServidor -show-key
```

### Opció 2 · Linux natiu amb `systemd`

1. Instal·la el binari i crea un usuari dedicat sense privilegis:
   ```sh
   sudo useradd --system --home /var/lib/gecko-relay --shell /usr/sbin/nologin gecko
   sudo install -m 0755 dist/linux/RelayServidor /usr/local/bin/gecko-relay
   sudo install -d -o gecko -g gecko -m 0700 /var/lib/gecko-relay
   ```

2. Crea `/etc/systemd/system/gecko-relay.service`:
   ```ini
   [Unit]
   Description=Gecko RelayServidor (mTLS 1.3)
   After=network-online.target
   Wants=network-online.target

   [Service]
   Type=simple
   User=gecko
   Group=gecko
   ExecStart=/usr/local/bin/gecko-relay -listen :9443 -data-dir /var/lib/gecko-relay -public-addr suport.el-teu-domini.cat:9443
   Restart=always
   RestartSec=3
   NoNewPrivileges=true
   PrivateTmp=true
   ProtectSystem=strict
   ReadWritePaths=/var/lib/gecko-relay

   [Install]
   WantedBy=multi-user.target
   ```

3. Activa el servei i consulta la clau mestra:
   ```sh
   sudo systemctl daemon-reload
   sudo systemctl enable --now gecko-relay
   sudo -u gecko /usr/local/bin/gecko-relay -data-dir /var/lib/gecko-relay -public-addr suport.el-teu-domini.cat:9443 -show-key
   ```

### Opció 3 · Windows Server

```cmd
RelayServidor.exe -listen :9443 -data-dir C:\GeckoRelay\data -public-addr suport.el-teu-domini.cat:9443
```

---

## Administració i seguretat

### Flags CLI i variables d'entorn (`RelayServidor`)

| Flag CLI | Variable d'entorn | Valor per defecte | Descripció |
| :--- | :--- | :--- | :--- |
| `-listen` | `RELAY_LISTEN` | `:9443` | Adreça i port TCP TLS 1.3 d'escolta. |
| `-data-dir` | `RELAY_DATA_DIR` | `./data` | Directori on es guarden la CA, certificats, tokens i registre de revocació. |
| `-public-addr` | `RELAY_PUBLIC_ADDR` | IP LAN detectada | Domini o IP pública que s'incorpora a la clau `RMT2:...`. |
| `-show-key` | `RELAY_SHOW_KEY` | `false` | Mostra la direcció i la clau mestra `RMT2:...` (en CLI imprimeix i surt). |
| `-list-clients` | — | `false` | Llista totes les credencials individuals emeses i el seu estat (`ACTIVA`/`REVOCADA`). |
| `-revoke <id>` | — | — | Revoca una credencial concreta per ID (o `default` per invalidar la compartida). |
| `-healthcheck` | — | `false` | Comprova localment si el port TCP del Relay respon (codi de sortida `0` o `1`). |

### Gestió i revocació de credencials per client

Cada executable generat des de `AdminRemoto.exe` estant connectat al Relay rep un certificat mTLS (`CN=client:<id>`) i un token únics.

- **Des de l'Admin**: prem **Revocar** a la fila de l'equip per tallar la connexió a l'instant i bloquejar futures reconnexions.
- **Des de la consola del servidor**:
  ```sh
  ./RelayServidor -data-dir /var/lib/gecko-relay -list-clients
  ./RelayServidor -data-dir /var/lib/gecko-relay -revoke <credential_id>
  ```

### Protecció i còpies de seguretat de `data/`

> [!CAUTION]
> El directori `data/` del servidor conté les claus privades de la CA (`ca.key`), el servidor (`server.key`), els certificats mTLS, els tokens (`relay_secret.json`) i l'estat de revocació (`issued_clients.json`).
> - Mantén permisos estrictes (`0700` al directori, `0600` a les claus).
> - Fes còpies de seguretat xifrades de `data/`: si es perd, caldrà reconfigurar l'Admin i regenerar els clients.
> - No publiquis mai la clau `RMT2:...` en repositoris ni xats no segurs.

---

## Dreceres i controls del visor remot

| Control / Drecera | Acció |
| :--- | :--- |
| **`F11`** | Activar o sortir de pantalla completa. |
| **`Ctrl+Alt+M`** | Amagar o mostrar la barra superior del visor. |
| **`Ctrl+Alt+Q`** | Tancar la sessió remota immediatament. |
| **`M1` `M2` ... `Todos`** | Canviar entre monitors individuals o veure tot l'escriptori virtual. |
| **`Baja` / `Estándar` / `Alta`** | Ajustar la qualitat de compressió en directe (`55` / `78` / `92`). |
| **`Archivo`** | Enviar un fitxer (fins a 50 MB, verificat amb SHA-256) a `Downloads` de l'equip remot. |
| **`CAD`** | Enviar la seqüència `Ctrl+Alt+Supr` a l'equip remot. |
| **Portapapers** | Sincronització automàtica de text en tots dos sentits durant la sessió activa. |

---

## Estructura del projecte

```text
.
├── cmd/
│   ├── admin/          # Consola nativa Windows + consola Web local per a Linux/macOS/Windows (-web)
│   ├── build/          # Compilador multiplataforma en Go pur (go run ./cmd/build)
│   ├── client/         # Agent client Windows, Linux (X11/Wayland) i macOS
│   └── relay/          # Servidor pont mTLS 1.3 i CLI d'administració (RelayServidor)
├── internal/
│   ├── admin/          # Client de connexió de l'Admin cap al Relay i enviament de fitxers
│   ├── adminweb/       # Servidor WebSocket RFC 6455 i UI web multiplataforma embebida
│   ├── clientui/       # Estats, callbacks i tipus de monitor compartits
│   ├── config/         # Incrustació/lectura de configuració al binari i normalització de logos
│   ├── darwin/         # Backend natiu de client per a macOS (screencapture, CGEvent, pbcopy)
│   ├── i18n/           # Localització (ca / es / en) i detecció automàtica d'idioma
│   ├── linux/          # Client X11 natiu i backend Wayland (grim, ydotool, wl-copy, zenity)
│   ├── protocol/       # Protocol binari/JSON amb validació estricta de tipus i mides
│   ├── relay/          # Nucli del servidor pont, rate limiting per IP i auditoria
│   ├── security/       # PKI mTLS 1.3, pinning SHA-256, claus RMT2 i revocació per client
│   ├── transfer/       # Receptor segur de fitxers amb SHA-256 i prevenció de path traversal
│   ├── version/        # Versió, commit i data incrustats en temps de compilació
│   ├── video/          # Codificador (amb fast-path dirty rects) i descodificador de tessel·les 64×64
│   └── win/            # DXGI Desktop Duplication (COM D3D11), GDI fallback, Win32 UI, SendInput/SendSAS
├── docs/               # Documentació tècnica i d'operació
├── Dockerfile          # Imatge mínima sobre scratch amb HEALTHCHECK
└── .github/            # CI i plantilles d'Issues / Pull Requests
```

---

## Resolució de problemes

- **Linux en sessió Wayland**: en Wayland pur, Gecko fa servir `grim` (Sway/Hyprland/wlroots), `spectacle` (KDE) o `gnome-screenshot` (GNOME) per a captura, `ydotool` per a ratolí/teclat, `wl-copy`/`wl-paste` per al portapapers i `zenity`/`kdialog` per al diàleg de permís. Per a màxima fluïdesa sense eines auxiliars, es recomana una sessió **Xorg / X11**.
- **Permisos en macOS**: la primera vegada que s'executa `ClientRemot-macos`, cal autoritzar **Enregistrament de pantalla** (*Screen Recording*) i **Accessibilitat** (*Accessibility*) a *Configuració del Sistema → Privadesa i seguretat*.
- **El portapapers no se sincronitza en Linux**: comprova que l'equip remot té instal·lat `wl-clipboard` (Wayland) o `xclip`/`xsel` (X11).
- **Captura en Windows (DXGI vs GDI)**: en monitors individuals Windows 8/10/11, Gecko utilitza automàticament **DXGI Desktop Duplication** amb rectangles modificats per GPU per minimitzar l'ús de CPU. En vistes multimonitor completes (`Todos`), sessions RDP o VMs sense GPU, canvia automàticament a GDI `BitBlt`.
- **Pantalla pausada en aparèixer UAC o pantalla bloquejada en Windows**: per seguretat de Windows, les captures d'usuari estàndard no accedeixen al *Secure Desktop*. Cal executar el client elevat / com a servei `SYSTEM` amb la política *SoftwareSASGeneration* habilitada.
- **El Relay no mostra la clau en reiniciar**: és intencionat per no deixar la clau mestra als logs. Executa `RelayServidor -show-key` per consultar-la.
