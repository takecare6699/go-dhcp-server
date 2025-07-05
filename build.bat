@echo off
setlocal enabledelayedexpansion

REM 多平台构建脚本 (Windows 版本)
REM 生成 macOS、Linux、Windows 平台的可执行文件

set APP_NAME=dhcp-server
set VERSION=1.0.0
set BUILD_DIR=build
set LDFLAGS=-s -w -X main.Version=%VERSION%

echo ================================
echo   DHCP Server 多平台构建脚本
echo      Version: %VERSION%
echo ================================

REM 清理旧的构建文件
echo 清理旧的构建文件...
if exist %BUILD_DIR% rmdir /s /q %BUILD_DIR%
mkdir %BUILD_DIR%

REM 整理依赖
echo 整理 Go 模块依赖...
go mod tidy

REM 检查编译环境
echo 检查编译环境...
go version

echo.
echo 开始构建各平台版本...

REM macOS
echo 构建 macOS amd64...
set GOOS=darwin
set GOARCH=amd64
go build -ldflags="%LDFLAGS%" -o "%BUILD_DIR%\%APP_NAME%-macos-amd64" .
if %errorlevel% equ 0 (
    echo ✓ macOS amd64 构建成功
) else (
    echo ✗ macOS amd64 构建失败
)

echo 构建 macOS arm64...
set GOOS=darwin
set GOARCH=arm64
go build -ldflags="%LDFLAGS%" -o "%BUILD_DIR%\%APP_NAME%-macos-arm64" .
if %errorlevel% equ 0 (
    echo ✓ macOS arm64 构建成功
) else (
    echo ✗ macOS arm64 构建失败
)

REM Linux
echo 构建 Linux amd64...
set GOOS=linux
set GOARCH=amd64
go build -ldflags="%LDFLAGS%" -o "%BUILD_DIR%\%APP_NAME%-linux-amd64" .
if %errorlevel% equ 0 (
    echo ✓ Linux amd64 构建成功
) else (
    echo ✗ Linux amd64 构建失败
)

echo 构建 Linux arm64...
set GOOS=linux
set GOARCH=arm64
go build -ldflags="%LDFLAGS%" -o "%BUILD_DIR%\%APP_NAME%-linux-arm64" .
if %errorlevel% equ 0 (
    echo ✓ Linux arm64 构建成功
) else (
    echo ✗ Linux arm64 构建失败
)

echo 构建 Linux 386...
set GOOS=linux
set GOARCH=386
go build -ldflags="%LDFLAGS%" -o "%BUILD_DIR%\%APP_NAME%-linux-386" .
if %errorlevel% equ 0 (
    echo ✓ Linux 386 构建成功
) else (
    echo ✗ Linux 386 构建失败
)

REM Windows
echo 构建 Windows amd64...
set GOOS=windows
set GOARCH=amd64
go build -ldflags="%LDFLAGS%" -o "%BUILD_DIR%\%APP_NAME%-windows-amd64.exe" .
if %errorlevel% equ 0 (
    echo ✓ Windows amd64 构建成功
) else (
    echo ✗ Windows amd64 构建失败
)

echo 构建 Windows 386...
set GOOS=windows
set GOARCH=386
go build -ldflags="%LDFLAGS%" -o "%BUILD_DIR%\%APP_NAME%-windows-386.exe" .
if %errorlevel% equ 0 (
    echo ✓ Windows 386 构建成功
) else (
    echo ✗ Windows 386 构建失败
)

echo.
echo ================================
echo 构建完成！
echo ================================

echo.
echo 构建文件列表:
dir %BUILD_DIR%

echo.
echo 创建发布包...
cd %BUILD_DIR%

REM 为每个平台创建压缩包
for %%f in (dhcp-server-windows-*.exe) do (
    set filename=%%f
    set platform=!filename:dhcp-server-=!
    set platform=!platform:.exe=!
    powershell -Command "Compress-Archive -Path '%%f' -DestinationPath '%APP_NAME%-!platform!-v%VERSION%.zip' -Force"
    echo ✓ 创建 %APP_NAME%-!platform!-v%VERSION%.zip
)

cd ..

echo.
echo ================================
echo 发布包创建完成！
echo ================================

echo.
echo 发布包列表:
dir %BUILD_DIR%\*.zip

echo.
echo 使用说明:
echo 1. 选择对应平台的可执行文件
echo 2. 将 config.yaml 文件放在同一目录
echo 3. 运行程序: dhcp-server.exe -config config.yaml
echo 4. 或者使用参数: dhcp-server.exe --help

echo.
echo 构建脚本执行完成！
pause 