# Guia tècnica i de producció

Aquest document recull els detalls d'arquitectura, compilació, desplegament en producció, administració de seguretat i resolució de problemes de **Gecko**.

---

## Components

| Binari | Plataforma | Funció |
| :--- | :--- | :--- |
| `RelayServidor` / `RelayServidor.exe` | Linux (amd64/arm64), Docker (`scratch`), Windows Server | Servidor pont mTLS 1.3, gestió de PKI/revocació, auditoria i enrutament de sessions. |
| `AdminRemoto.exe` | Windows (amd64/arm64) | Consola d'administració, visor remot natiu Win32 i generador de clients amb logotip. |
| `ClientRemot.exe` | Windows (amd64/arm64) | Agent portable sense instal·lació (interfície en català). |
| `ClientRemot` | Linux X11 (amd64/arm64) | Agent portable en Go pur sobre protocol X11 natiu (interfície en català). |

---

## Compilació i verificació

Requereix **Go 1.22+** (sense `cgo` ni dependències externes; funciona des de Windows, Linux o macOS):

```sh
# Compilar tots els binaris (amd64 per defecte) a dist/
go run ./cmd/build

# Compilar per a arquitectura ARM64
go run ./cmd/build -arch arm64

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
   sudo install -m 0755 dist/RelayServidor /usr/local/bin/gecko-relay
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
│   ├── admin/          # Consola i visor natiu de Windows (AdminRemoto.exe)
│   ├── build/          # Compilador multiplataforma en Go pur (go run ./cmd/build)
│   ├── client/         # Agent client Windows i Linux X11 (ClientRemot)
│   └── relay/          # Servidor pont mTLS 1.3 i CLI d'administració (RelayServidor)
├── internal/
│   ├── admin/          # Client de connexió de l'Admin cap al Relay i enviament de fitxers
│   ├── clientui/       # Estats, callbacks i tipus de monitor compartits
│   ├── config/         # Incrustació/lectura de configuració al binari i normalització de logos
│   ├── linux/          # Client X11 en Go pur (RANDR, XTEST, XFIXES, UI, Wayland, portapapers)
│   ├── protocol/       # Protocol binari/JSON amb validació estricta de tipus i mides
│   ├── relay/          # Nucli del servidor pont, rate limiting per IP i auditoria
│   ├── security/       # PKI mTLS 1.3, pinning SHA-256, claus RMT2 i revocació per client
│   ├── transfer/       # Receptor segur de fitxers amb SHA-256 i prevenció de path traversal
│   ├── video/          # Codificador i descodificador híbrid de tessel·les 64×64 (Zlib-BGR + JPEG)
│   └── win/            # Interfície Win32, captura GDI, injecció SendInput/SendSAS i portapapers
├── docs/               # Documentació tècnica i d'operació
├── Dockerfile          # Imatge mínima sobre scratch amb HEALTHCHECK
└── .github/workflows/  # Integració contínua (gofmt, go vet multi-SO, tests i cross-build)
```

---

## Resolució de problemes

- **Pantalla negra o avís en Linux (Wayland)**: el client Linux utilitza el protocol X11 natiu (`GetImage` + `XTEST`). Si la sessió corre sobre Wayland, la finestra avisarà automàticament; canvia a sessió **Xorg / X11** a la pantalla d'inici de sessió.
- **El portapapers no se sincronitza en Linux**: comprova que l'equip remot té instal·lat `xclip` o `xsel` (`sudo apt install xclip`).
- **Pantalla pausada en aparèixer UAC o pantalla bloquejada en Windows**: per seguretat de Windows, la captura GDI (`BitBlt`) i `SendSAS` des d'un procés d'usuari estàndard no accedeixen a l'escriptori segur (*Secure Desktop*). Cal executar el client com a servei `SYSTEM` amb la política *SoftwareSASGeneration* habilitada.
- **El Relay no mostra la clau en reiniciar**: és intencionat per no deixar la clau mestra als logs. Executa `RelayServidor -show-key` per consultar-la.
