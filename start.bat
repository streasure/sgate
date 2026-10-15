@echo off
chcp 65001 >nul 2>&1

setlocal

:: ============================================================
:: sgate 网关启动脚本 (Windows)
::
:: 用法:
::   start.bat
::   set CONF=config\sgate1.yaml && start.bat
::   set CONF=config\config_batch_on.yaml & set LOG_CONF=config\log_bench.yaml && start.bat
::
:: 环境变量:
::   CONF      网关配置（默认 config\config.yaml）
::   LOG_CONF  日志配置（默认 config\log.yaml）
::
:: 配置文件分工：
::   yaml 只放环境项（身份/端口/etcd/特性开关）；
::   数值默认见 internal\config\defaults.go（无需在 yaml 重复）。
:: ============================================================

if "%CONF%"=="" set CONF=config\config.yaml
if "%LOG_CONF%"=="" set LOG_CONF=config\log.yaml

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
