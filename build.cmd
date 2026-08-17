@echo off
setlocal
cd /d "%~dp0"

echo Building examples to bin\ ...
if not exist bin mkdir bin

go build -o bin\pty-cli.exe .\examples\pty-cli || goto :err
go build -o bin\pty-web.exe .\examples\pty-web || goto :err
go build -o bin\pty-web-signalr.exe .\examples\pty-web-signalr || goto :err
go build -o bin\pty-session.exe .\examples\pty-session || goto :err

REM Windows + winpty: copy the winpty DLLs next to the executables so
REM -winpty / ForceWinPty work when running the built binaries.
if exist ptygo\deps\winpty (
    if not exist bin\deps\winpty mkdir bin\deps\winpty
    copy /Y ptygo\deps\winpty\* bin\deps\winpty\ >nul
)

echo.
echo Build OK. Binaries are in bin\.
exit /b 0

:err
echo.
echo Build FAILED.
exit /b 1