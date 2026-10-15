@echo off
chcp 65001 >nul 2>&1
setlocal

:: ============================================================
:: bench1_tcp 压测脚本 (Windows)
:: 测试: 客户端→sgate→逻辑服 转发性能
:: ============================================================

:: 必须用 bench\batch-off.yaml：config.yaml 开发配置已启用限流(100/s)，
:: 100 连接同 IP 会超限被拒，违反 AGENTS.md「压测零鉴权失败」。
set CONF=config\bench\batch-off.yaml
set DURATION=10s
set PARALLEL=100

:: 编译
echo [%date% %time%] 编译...
go build -o sgate.exe .\cmd\gateway
go build -o bench\logic1_tcp\logic1_tcp.exe .\bench\logic1_tcp
go build -o bench\bench1_tcp\bench1_tcp.exe .\bench\bench1_tcp
echo [%date% %time%] 编译完成

:: 启动 logic
echo [%date% %time%] 启动 logic1_tcp...
start /b "" ".\bench\logic1_tcp\logic1_tcp.exe" -port 50052 -id logic1-tcp -pprof 127.0.0.1:6061
ping -n 3 127.0.0.1 >nul

:: 启动 sgate
echo [%date% %time%] 启动 sgate...
start /b "" ".\sgate.exe" -conf %CONF%
ping -n 4 127.0.0.1 >nul

:: 运行压测
echo [%date% %time%] 开始压测 (TCP, %DURATION%, %PARALLEL% 连接)...
".\bench\bench1_tcp\bench1_tcp.exe" -addr 127.0.0.1:48080 -duration %DURATION% -parallel %PARALLEL% -server-id logic1-tcp

:: 清理
echo [%date% %time%] 清理进程...
taskkill /f /im sgate.exe >nul 2>&1
taskkill /f /im logic1_tcp.exe >nul 2>&1
echo [%date% %time%] 完成

endlocal
