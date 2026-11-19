# Guia tècnica i de producció (Windows)

Aquest document recull els detalls d'arquitectura, compilació, desplegament en producció, administració de seguretat i resolució de problemes de **Gecko** per a Windows.

---

## Components

| Binari | Arquitectura | Funció |
| :--- | :--- | :--- |
| `dist\RelayServidor.exe` | Windows (`amd64` / `arm64`) | Servidor pont mTLS 1.3, gestió de PKI/revocació per client, *rate limiting* per IP i auditoria. |
| `dist\AdminRemoto.exe` | Windows (`amd64` / `arm64`) | Consola nativa Win32, visor remot, gestió d'equips i generador de `ClientRemot.exe` (amb logotip i idioma). |
| `ClientRemot.exe` *(generat des d'Admin)* | Windows (`amd64` / `arm64`) | Agent portable sense instal·lació amb captura GPU **DXGI Desktop Duplication** (+ *fallback* GDI), multiidioma (`ca`/`es`/`en`). |

---

## Compilació i verificació

Requereix **Go 1.22+** (`CGO_ENABLED=0`; es pot compilar en creuat des de Windows, macOS o Linux):

```sh
# Compilar binaris de Windows amd64 a dist/
go run ./cmd/build

# Compilar amb versió incrustada o per a Windows ARM64
go run ./cmd/build -version 1.0.0 -arch amd64
go run ./cmd/build -version 1.0.0 -arch arm64

# Comprovar la versió d'un executable
RelayServidor.exe -version

# Executar tota la bateria de tests
go test -race -count=1 ./...
```

---

## Desplegament en producció (Windows Server)

> [!IMPORTANT]
> **Requisits de xarxa**: només cal obrir el port **TCP 9443** (o el configurat a `-listen`) d'entrada al servidor on s'executa `RelayServidor.exe`. Els equips `ClientRemot.exe` i `AdminRemoto.exe` només fan connexions TCP sortints.

### Arrencada bàsica

```cmd
RelayServidor.exe -listen :9443 -data-dir C:\GeckoRelay\data -public-addr suport.el-teu-domini.cat:9443
```

### Regla de Windows Firewall (PowerShell com a administrador)

```powershell
New-NetFirewallRule -DisplayName "Gecko Relay (TCP 9443)" -Direction Inbound -Protocol TCP -LocalPort 9443 -Action Allow
```

### Execució persistent en segon pla (Tasca programada / Servei)

Pots registrar `RelayServidor.exe` com a tasca a l'arrencada del sistema (`SYSTEM`) o mitjançant un gestor de serveis com NSSM apuntant a:
- **Executable**: `C:\GeckoRelay\RelayServidor.exe`
- **Arguments**: `-listen :9443 -data-dir C:\GeckoRelay\data -public-addr suport.el-teu-domini.cat:9443`

Per consultar la clau mestra en qualsevol moment sense exposar-la als logs:

```cmd
RelayServidor.exe -data-dir C:\GeckoRelay\data -public-addr suport.el-teu-domini.cat:9443 -show-key
```

---

## Administració i seguretat

### Flags CLI i variables d'entorn (`RelayServidor.exe`)

| Flag CLI | Variable d'entorn | Valor per defecte | Descripció |
| :--- | :--- | :--- | :--- |
| `-listen` | `RELAY_LISTEN` | `:9443` | Adreça i port TCP TLS 1.3 d'escolta. |
| `-data-dir` | `RELAY_DATA_DIR` | `.\data` | Directori on es guarden la CA, certificats, tokens i registre de revocació. |
| `-public-addr` | `RELAY_PUBLIC_ADDR` | IP LAN detectada | Domini o IP pública incorporada a la clau `RMT2:...`. |
| `-show-key` | `RELAY_SHOW_KEY` | `false` | Mostra la direcció i la clau mestra `RMT2:...` i surt. |
| `-list-clients` | — | `false` | Llista totes les credencials individuals emeses i el seu estat (`ACTIVA`/`REVOCADA`). |
| `-revoke <id>` | — | — | Revoca una credencial concreta per ID (o `default` per invalidar la compartida). |
| `-healthcheck` | — | `false` | Comprova localment si el port TCP del Relay respon (codi de sortida `0` o `1`). |
| `-version` | — | `false` | Mostra la versió incrustada i surt. |

### Gestió i revocació de credencials per client

Cada `ClientRemot.exe` generat des d'`AdminRemoto.exe` estant connectat al Relay rep un certificat mTLS (`CN=client:<id>`) i un token únics.

- **Des d'`AdminRemoto.exe`**: prem **Revocar** a la fila de l'equip per desconnectar-lo a l'instant i bloquejar futures reconnexions.
- **Des de la consola del servidor**:
  ```cmd
  RelayServidor.exe -data-dir C:\GeckoRelay\data -list-clients
  RelayServidor.exe -data-dir C:\GeckoRelay\data -revoke <credential_id>
  ```

### Protecció i còpies de seguretat de `data\`

> [!CAUTION]
> El directori `data\` del servidor conté les claus privades de la CA (`ca.key`), el servidor (`server.key`), els certificats mTLS, els tokens (`relay_secret.json`) i l'estat de revocació (`issued_clients.json`).
> - Restringeix els permisos NTFS de `data\` exclusivament al compte que executa el Relay.
> - Fes còpies de seguretat xifrades de `data\`: si es perd, caldrà reconfigurar l'Admin i regenerar els `ClientRemot.exe`.
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
| **`Archivo`** | Enviar un fitxer (fins a 50 MB, amb progrés i verificació SHA-256) a `Downloads` de l'equip remot. |
| **`CAD`** | Enviar `Ctrl+Alt+Supr` mitjançant `sas.dll!SendSAS`. |
| **Portapapers** | Sincronització automàtica de text en tots dos sentits durant la sessió activa. |
| **Indicador de latència** | Punt de color a la barra superior: verd (<50 ms), ambre (50–120 ms), vermell (>120 ms). |

---

## Motor de captura Windows (DXGI + GDI)

- **Monitor individual (`M1`, `M2`, ...)**: Gecko enumera tots els adaptadors GPU via `IDXGIFactory1::EnumAdapters1` (compatible amb portàtils híbrids / Optimus i múltiples targetes gràfiques) i fa servir **DXGI Desktop Duplication**. Llegeix els rectangles modificats (`GetFrameDirtyRects` / `GetFrameMoveRects`) directament de la GPU, copia només les regions canviades a una textura *staging* i codifica únicament les tessel·les 64×64 afectades.
- **Fallback automàtic i transparent a GDI `BitBlt`**: s'activa automàticament en vista multimonitor completa (`Todos`), monitors rotats en vertical, pantalles Windows 11 amb HDR actiu (`R16G16B16A16_FLOAT`), sessions RDP o màquines virtuals sense GPU hardware.

---

## Estructura del projecte

```text
.
├── cmd/
│   ├── admin/          # Consola i visor natiu Win32 (AdminRemoto.exe)
│   ├── build/          # Compilador Windows en Go pur (go run ./cmd/build)
│   ├── client/         # Agent client Windows portable (ClientRemot.exe)
│   └── relay/          # Servidor pont mTLS 1.3 i CLI d'administració (RelayServidor.exe)
├── internal/
│   ├── admin/          # Client de connexió de l'Admin cap al Relay i enviament de fitxers
│   ├── config/         # Incrustació/lectura de configuració al binari i normalització de logos
│   ├── i18n/           # Localització del client (ca / es / en) i detecció automàtica
│   ├── protocol/       # Protocol binari/JSON amb validació estricta de tipus i mides
│   ├── relay/          # Nucli del servidor pont, rate limiting per IP i auditoria
│   ├── security/       # PKI mTLS 1.3, pinning SHA-256, claus RMT2 i revocació per client
│   ├── transfer/       # Receptor segur de fitxers amb SHA-256 i prevenció de path traversal
│   ├── version/        # Versió, commit i data incrustats en temps de compilació
│   ├── video/          # Codificador (amb fast-path dirty rects) i descodificador de tessel·les 64×64
│   └── win/            # DXGI Desktop Duplication (multi-GPU COM D3D11), GDI fallback, Win32 UI, DPI v2, SendInput/SendSAS i portapapers
├── docs/               # Documentació tècnica i d'operació
└── .github/            # CI de Windows i plantilles d'Issues / Pull Requests
```

---

## Resolució de problemes

- **Pantalla pausada en aparèixer UAC o pantalla bloquejada**: per seguretat de Windows, les captures d'usuari estàndard no accedeixen a l'escriptori segur (*Secure Desktop*). Per operar sobre UAC o utilitzar `CAD` (`SendSAS`) cal executar `ClientRemot.exe` elevat / com a servei `SYSTEM` amb la política *SoftwareSASGeneration* habilitada.
- **El Relay no mostra la clau en reiniciar**: és intencionat per no deixar la clau mestra als logs. Executa `RelayServidor.exe -show-key` per visualitzar-la de manera controlada.
- **Pantalla HDR en Windows 11**: Gecko detecta automàticament superfícies HDR de 16 bits en DXGI i commuta al *fallback* GDI amb *tonemapping* SDR per evitar pantalles negres.
