@echo off
chcp 65001 >nul 2>&1
setlocal

:: ============================================================
:: bench2_ws 压测脚本 (Windows)
:: 测试: 逻辑服→sgate→客户端 WebSocket 推送性能
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
go build -o bench\logic2_ws\logic2_ws.exe .\bench\logic2_ws
go build -o bench\bench2_ws\bench2_ws.exe .\bench\bench2_ws
echo [%date% %time%] 编译完成

:: 启动 logic
echo [%date% %time%] 启动 logic2_ws...
start /b "" ".\bench\logic2_ws\logic2_ws.exe" -port 50061 -id logic2-ws -push-interval %PUSH_INTERVAL% -push-size %PUSH_SIZE% -expected-members %EXPECTED_MEMBERS% -push-workers %PUSH_WORKERS%
timeout /t 2 /nobreak >nul

:: 启动 sgate
echo [%date% %time%] 启动 sgate (batchPush=%MODE%)...
start /b "" ".\sgate.exe" -conf %CONF%
timeout /t 3 /nobreak >nul

:: 运行压测
echo [%date% %time%] 开始压测 (WebSocket, %DURATION%, %PARALLEL% 连接, batchPush=%MODE%)...
".\bench\bench2_ws\bench2_ws.exe" -addr 127.0.0.1:48081 -duration %DURATION% -parallel %PARALLEL% -server-id logic2-ws

:: 清理
echo [%date% %time%] 清理进程...
taskkill /f /im sgate.exe >nul 2>&1
taskkill /f /im logic2_ws.exe >nul 2>&1
echo [%date% %time%] 完成

endlocal
