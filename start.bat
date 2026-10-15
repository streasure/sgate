@echo off
chcp 65001 >nul 2>&1

setlocal

:: ============================================================
:: sgate 网关启动脚本 (Windows)
::
:: 用法:
::   start.bat
::   set CONF=config\cluster\gateway1.yaml && start.bat
::   set CONF=config\bench\batch-on.yaml & set LOG_CONF=config\log\bench.yaml && start.bat
::
:: 环境变量:
::   CONF      网关配置（默认 config\gateway.yaml）
::   LOG_CONF  日志配置（默认 config\log\default.yaml）
::
:: 配置目录结构:
::   config\gateway.yaml          单机默认
::   config\cluster\gateway*.yaml 集群实例
::   config\bench\*.yaml          压测
::   config\log\*.yaml            日志
::   数值默认见 internal\config\defaults.go（无需在 yaml 重复）。
:: ============================================================

if "%CONF%"=="" set CONF=config\gateway.yaml
if "%LOG_CONF%"=="" set LOG_CONF=config\log\default.yaml

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
echo [%date% %time%] 等价命令: .\sgate.exe -conf %CONF% -logger %LOG_CONF%

.\sgate.exe -conf %CONF% -logger %LOG_CONF%
set EXIT_CODE=%ERRORLEVEL%

endlocal & exit /b %EXIT_CODE%
