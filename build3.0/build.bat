@echo off
setlocal

rem ============================================================
rem  cfdata 3.0 build script
rem
rem  Usage: double-click this file after editing index.html
rem  Output: ..\build\CF3.0.exe
rem
rem  NOTE: keep this file ASCII-only on purpose.
rem        cmd.exe reads .bat files using the system OEM code page
rem        (GBK on Chinese Windows), not UTF-8.
rem ============================================================

set "SRCDIR=%~dp0"
pushd "%SRCDIR%" || (echo [ERROR] Cannot enter "%SRCDIR%" & pause & exit /b 1)

set "GOEXE=D:\ruanjian\Go\bin\go.exe"
if not exist "%GOEXE%" (
    for /f "delims=" %%i in ('where go 2^>nul') do set "GOEXE=%%i"
)
if not exist "%GOEXE%" (
    echo [ERROR] Go compiler not found.
    echo         Expected: D:\ruanjian\Go\bin\go.exe
    pause
    exit /b 1
)

set "GOCACHE=%~dp0.gocache"
set "GOMODCACHE=%~dp0..\.gomodcache"
set "GOTOOLCHAIN=local"
set "GOPROXY=off"
set "GOSUMDB=off"
set "GOFLAGS="
set "CGO_ENABLED=0"
set "GOOS=windows"
set "GOARCH=amd64"

if not exist "%GOMODCACHE%\github.com\gorilla\websocket@v1.5.3" (
    echo [ERROR] Dependency cache missing:
    echo         %GOMODCACHE%\github.com\gorilla\websocket@v1.5.3
    pause
    exit /b 1
)

echo ============================================================
echo   Building cfdata 3.0
echo   Go     : %GOEXE%
echo   Output : ..\build\CF3.0.exe
echo ============================================================
echo.

set "OUTFILE=..\build\CF3.0.exe"

rem ---- running exe cannot be overwritten ----
if not exist "%OUTFILE%" goto :build
set "LOCKERR="
powershell -NoProfile -NonInteractive -Command "try { $f=[IO.File]::Open('%OUTFILE%',[IO.FileMode]::Open,[IO.FileAccess]::ReadWrite,[IO.FileShare]::None); $f.Close() } catch { exit 1 }"
if errorlevel 1 set "LOCKERR=1"
if not defined LOCKERR goto :build
echo [ERROR] Cannot overwrite the output file:
echo         %CD%\%OUTFILE%
echo.
echo         The program is probably still running.
echo         Close it, then run this script again.
echo.
pause
exit /b 1

:build

set "T0=%TIME%"
"%GOEXE%" build -trimpath -ldflags "-s -w" -o "%OUTFILE%" .
if errorlevel 1 (
    echo.
    echo [FAILED] Build error, see messages above.
    echo.
    pause
    exit /b 1
)

echo.
echo [OK] Build succeeded.
echo      started %T0%  finished %TIME%
for %%f in ("%OUTFILE%") do echo      %%~ff
for %%f in ("%OUTFILE%") do echo      %%~zf bytes
echo.
echo Reminder: index.html is embedded into the exe.
echo           Restart the exe to see your page changes.
echo.

popd
endlocal
