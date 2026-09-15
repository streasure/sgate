@echo off
chcp 65001 >nul 2>&1
setlocal

:: ============================================================
:: bench2_tcp 压测脚本 (Windows)
:: 测试: 逻辑服→sgate→客户端 推送性能
:: 用法: run.bat [off|on]  (batchPush 模式，默认 off)
:: ============================================================

set MODE=%1
if "%MODE%"=="" set MODE=off

if "%MODE%"=="on" (
    set CONF=config\config_batch_on.yaml
) else (
    set CONF=config\config_batch_off.yaml
)

set DURATION=10s
set PARALLEL=100
set PUSH_INTERVAL=0
set PUSH_SIZE=64
set EXPECTED_MEMBERS=100
set PUSH_WORKERS=12

:: 编译
echo [%date% %time%] 编译...
go build -o sgate.exe .\cmd\gateway
go build -o bench\logic2_tcp\logic2_tcp.exe .\bench\logic2_tcp
go build -o bench\bench2_tcp\bench2_tcp.exe .\bench\bench2_tcp
echo [%date% %time%] 编译完成

:: 启动 logic
echo [%date% %time%] 启动 logic2_tcp...
start /b "" ".\bench\logic2_tcp\logic2_tcp.exe" -port 50060 -id logic2-tcp -push-interval %PUSH_INTERVAL% -push-size %PUSH_SIZE% -expected-members %EXPECTED_MEMBERS% -push-workers %PUSH_WORKERS%
timeout /t 2 /nobreak >nul

:: 启动 sgate
echo [%date% %time%] 启动 sgate (batchPush=%MODE%)...
start /b "" ".\sgate.exe" -conf %CONF%
timeout /t 3 /nobreak >nul

:: 运行压测
echo [%date% %time%] 开始压测 (TCP, %DURATION%, %PARALLEL% 连接, batchPush=%MODE%)...
".\bench\bench2_tcp\bench2_tcp.exe" -addr 127.0.0.1:48080 -duration %DURATION% -parallel %PARALLEL% -server-id logic2-tcp

:: 清理
echo [%date% %time%] 清理进程...
taskkill /f /im sgate.exe >nul 2>&1
taskkill /f /im logic2_tcp.exe >nul 2>&1
echo [%date% %time%] 完成

endlocal
