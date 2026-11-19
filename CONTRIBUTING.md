# Guia per contribuir a Gecko

Gràcies pel teu interès a millorar **Gecko**! Totes les contribucions —codi, correccions, traduccions o documentació— són benvingudes.

## Principis del projecte

- **Natiu per a Windows en Go pur**: el projecte manté `go.mod` net, sense `cgo`, amb interfície Win32 nativa i captura DXGI Desktop Duplication.
- **Seguretat per defecte**: cap canvi no pot relaxar el consentiment explícit de l'usuari, el xifratge mTLS 1.3 ni les validacions estrictes del protocol.
- **Qualitat i rendiment**: cada millora ha de compilar netament per a `windows/amd64` i `windows/arm64` i passar els tests sense *data races*.

## Entorn de desenvolupament

Requereix **Go 1.22+**:

```sh
# Formatar el codi
gofmt -w .

# Verificació estàtica per a Windows amd64 i arm64
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./...
GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go vet ./...

# Passar tots els tests
go test -race -count=1 ./...

# Compilar els executables de Windows a dist/
go run ./cmd/build
```

## Flux per enviar una Pull Request

1. Fes un *fork* del repositori i crea una branca descriptiva (`feat/millora-x`, `fix/error-y`).
2. Afegeix o actualitza tests unitaris per als canvis introduïts.
3. Comprova que `gofmt -l .` no mostra fitxers pendents i que `go test -race ./...` passa sense errors.
4. Obre la Pull Request explicant clarament el problema que resol i com s'ha provat.
