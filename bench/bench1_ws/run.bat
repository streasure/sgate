@echo off
chcp 65001 >nul 2>&1
setlocal

:: ============================================================
:: bench1_ws 压测脚本 (Windows)
:: 测试: 客户端→sgate→逻辑服 WebSocket 转发性能
:: ============================================================

set CONF=config\config.yaml
set DURATION=10s
set PARALLEL=100

:: 编译
echo [%date% %time%] 编译...
go build -o sgate.exe .\cmd\gateway
go build -o bench\logic1_ws\logic1_ws.exe .\bench\logic1_ws
go build -o bench\bench1_ws\bench1_ws.exe .\bench\bench1_ws
echo [%date% %time%] 编译完成

:: 启动 logic
echo [%date% %time%] 启动 logic1_ws...
start /b "" ".\bench\logic1_ws\logic1_ws.exe" -port 50053 -id logic1-ws
timeout /t 2 /nobreak >nul

:: 启动 sgate
echo [%date% %time%] 启动 sgate...
start /b "" ".\sgate.exe" -conf %CONF%
timeout /t 3 /nobreak >nul

:: 运行压测
echo [%date% %time%] 开始压测 (WebSocket, %DURATION%, %PARALLEL% 连接)...
".\bench\bench1_ws\bench1_ws.exe" -addr 127.0.0.1:48081 -duration %DURATION% -parallel %PARALLEL%

:: 清理
echo [%date% %time%] 清理进程...
taskkill /f /im sgate.exe >nul 2>&1
taskkill /f /im logic1_ws.exe >nul 2>&1
echo [%date% %time%] 完成

endlocal
