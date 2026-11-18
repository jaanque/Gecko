@echo off
setlocal

set CGO_ENABLED=0
set GOARCH=amd64

if not exist "cmd\admin\assets" mkdir "cmd\admin\assets"
if not exist "dist" mkdir "dist"

set GOOS=windows
echo [####................]  20%%  Cliente (Win)...
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "cmd\admin\assets\client_template.exe" ./cmd/client || exit /b 1

set GOOS=linux
echo [########............]  40%%  Cliente (Linux)...
go build -trimpath -ldflags="-s -w" -o "cmd\admin\assets\client_template_linux" ./cmd/client || exit /b 1

set GOOS=windows
echo [############........]  60%%  Admin...
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "dist\AdminRemoto.exe" ./cmd/admin || exit /b 1

echo [################....]  80%%  Relay (Win)...
go build -trimpath -ldflags="-s -w" -o "dist\RelayServidor.exe" ./cmd/relay || exit /b 1

set GOOS=linux
echo [##################..]  90%%  Relay (Linux)...
go build -trimpath -ldflags="-s -w" -o "dist\RelayServidor" ./cmd/relay || exit /b 1

echo [####################] 100%%  Listo: dist\
endlocal
