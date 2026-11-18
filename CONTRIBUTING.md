# Guia per contribuir a Gecko

Gràcies pel teu interès a millorar **Gecko**! Totes les contribucions —codi, correccions, traduccions o documentació— són benvingudes.

## Principis del projecte

- **Go pur**: el projecte manté `go.mod` sense dependències externes. Abans de proposar qualsevol llibreria externa, obre una discussió per valorar-ho.
- **Seguretat per defecte**: cap canvi no pot relaxar el consentiment explícit de l'usuari, el xifratge mTLS 1.3 ni les validacions estrictes del protocol.
- **Senzillesa i multiplataforma**: el codi ha de compilar i passar els tests de manera neta en Windows, Linux i macOS.

## Entorn de desenvolupament

Requereix **Go 1.22+**:

```sh
# Formatar el codi
gofmt -w .

# Verificació estàtica multiplataforma
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go vet ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./...
GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go vet ./...
GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go vet ./...

# Passar tots els tests
go test -race -count=1 ./...

# Compilar tots els binaris
go run ./cmd/build
```

## Flux per enviar una Pull Request

1. Fes un *fork* del repositori i crea una branca descriptiva (`feat/millora-x`, `fix/error-y`).
2. Afegeix o actualitza tests unitaris per als canvis introduïts.
3. Comprova que `gofmt -l .` no mostra fitxers pendents i que `go test -race ./...` passa sense errors.
4. Obre la Pull Request explicant clarament el problema que resol i com s'ha provat.
