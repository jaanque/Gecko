@echo off
setlocal

set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0

if not exist "cmd\admin\assets" mkdir "cmd\admin\assets"
if not exist "dist" mkdir "dist"

echo [##..................]  10%%  Cliente...
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "cmd\admin\assets\client_template.exe" ./cmd/client || exit /b 1

echo [###########.........]  55%%  Admin...
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "dist\AdminRemoto.exe" ./cmd/admin || exit /b 1

echo [####################] 100%%  Listo: dist\AdminRemoto.exe
endlocal
