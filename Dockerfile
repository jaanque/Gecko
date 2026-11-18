FROM golang:1.22-alpine AS builder
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod ./
COPY internal ./internal
COPY cmd/relay ./cmd/relay
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/RelayServidor ./cmd/relay

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/RelayServidor /RelayServidor
VOLUME ["/data"]
ENV RELAY_LISTEN=":9443"
ENV RELAY_DATA_DIR="/data"
EXPOSE 9443
ENTRYPOINT ["/RelayServidor"]
