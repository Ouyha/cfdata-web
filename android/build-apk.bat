@echo off
setlocal EnableExtensions

rem ============================================================
rem  cfdata Android APK build script
rem  Toolchain: D:\Android (JDK 21, SDK 36, NDK r28c, Gradle 8.13)
rem  Output:    app\build\outputs\apk\debug\app-debug.apk
rem             ..\build\CFScan-Android-<versionName>.apk
rem ============================================================

set "SRCDIR=%~dp0"
pushd "%SRCDIR%" || exit /b 1

rem ---- APK version: the only place to edit is app\build.gradle versionName ----
set "VER="
for /f "tokens=2 delims='" %%v in ('findstr /c:"versionName " app\build.gradle') do set "VER=%%v"
if not defined VER (
    echo [ERROR] Cannot read versionName from app\build.gradle
    pause
    exit /b 1
)
set "APKNAME=CFScan-Android-%VER%.apk"

echo ============================================================
echo   cfdata Android %VER%
echo   Copy to  : ..\build\%APKNAME%
echo ============================================================
echo.

set "JAVA_HOME=D:\Android\jdk21\jdk-21.0.2"
set "ANDROID_HOME=D:\Android\Sdk"
set "ANDROID_SDK_ROOT=%ANDROID_HOME%"
set "ANDROID_NDK_HOME=%ANDROID_HOME%\ndk\28.2.13676358"
set "GRADLE_USER_HOME=D:\Android\gradle-home"
set "GOPATH=D:\Android\gopath"
set "GOMODCACHE=D:\Android\gomodcache"
set "GOCACHE=D:\Android\gocache"
set "GOPROXY=https://goproxy.cn,direct"
set "GOSUMDB=off"
set "GOTOOLCHAIN=local"
set "CGO_ENABLED=1"
set "PATH=%JAVA_HOME%\bin;%GOPATH%\bin;D:\Android\gradle-8.13\bin;%ANDROID_HOME%\platform-tools;%PATH%"

if not exist "%GOPATH%\bin\gobind.exe" (
    echo [INFO] Initializing gomobile...
    call gomobile init
    if errorlevel 1 goto :fail
)

echo [1/2] Building Go AAR for arm64...
pushd go
call gomobile bind -target=android/arm64 -androidapi 26 -javapkg=com.cfdata.mobile -ldflags "-s -w" -trimpath -o ..\app\libs\cfdata.aar .
if errorlevel 1 (
    popd
    goto :fail
)
popd

echo [2/2] Building debug APK...
rem Delete the old APK first: Gradle incremental packaging can leave stale native-lib bytes
if exist "app\build\outputs\apk\debug\app-debug.apk" del /F /Q "app\build\outputs\apk\debug\app-debug.apk"
call gradle.bat assembleDebug --no-daemon --console=plain
if errorlevel 1 goto :fail

if not exist "..\build" mkdir "..\build"
copy /Y "app\build\outputs\apk\debug\app-debug.apk" "..\build\%APKNAME%" >nul

echo.
echo [OK] APK: %SRCDIR%app\build\outputs\apk\debug\app-debug.apk
echo [OK] Copy: %SRCDIR%..\build\%APKNAME%
echo.
echo Install on a USB-debugging phone:
echo   adb install -r "%SRCDIR%..\build\%APKNAME%"
echo.
popd
endlocal
exit /b 0

:fail
echo.
echo [FAILED] Build error, see messages above.
popd
endlocal
exit /b 1
