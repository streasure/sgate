@echo off
chcp 65001 >nul 2>&1

setlocal

:: ============================================================
:: sgate 网关启动脚本 (Windows)
:: ============================================================

:: 配置路径
set CONF=config\config.yaml
set LOG_CONF=config\tlog.yaml

:: 编译
echo [%date% %time%] 编译 sgate...
go build -o sgate.exe .\cmd\gateway
if %ERRORLEVEL% neq 0 (
    echo [%date% %time%] 编译失败
    exit /b 1
)
echo [%date% %time%] 编译完成

:: 启动
echo [%date% %time%] 启动 sgate...
echo [%date% %time%] 配置文件: %CONF%
echo [%date% %time%] 日志配置: %LOG_CONF%
echo [%date% %time%] PID: %~dp0sgate.exe

.\sgate.exe -conf %CONF% -logger %LOG_CONF%

endlocal
