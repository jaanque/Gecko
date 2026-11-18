@echo off
setlocal

set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0

if not exist "cmd\admin\assets" mkdir "cmd\admin\assets"
if not exist "dist" mkdir "dist"

echo [1/2] Compilando plantilla del cliente (GUI nativa Windows)...
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "cmd\admin\assets\client_template.exe" ./cmd/client
if errorlevel 1 exit /b 1

echo [2/2] Compilando aplicacion de administracion (GUI nativa Windows)...
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "dist\AdminRemoto.exe" ./cmd/admin
if errorlevel 1 exit /b 1

echo.
echo Compilacion completada:
echo   - dist\AdminRemoto.exe
echo.
echo Haz doble clic en dist\AdminRemoto.exe para abrir el panel de administracion.
endlocal
