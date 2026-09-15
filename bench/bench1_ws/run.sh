#!/bin/bash

# ============================================================
# bench1_ws 压测脚本 (Linux)
# 测试: 客户端→sgate→逻辑服 WebSocket 转发性能
# ============================================================

set -e
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR/../.."

CONF="config/config.yaml"
DURATION="10s"
PARALLEL=100

# 编译
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 编译..."
go build -o sgate ./cmd/
go build -o bench/logic1_ws/logic1_ws ./bench/logic1_ws
go build -o bench/bench1_ws/bench1_ws ./bench/bench1_ws
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 编译完成"

# 启动 logic
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 启动 logic1_ws..."
./bench/logic1_ws/logic1_ws -port 50053 -id logic1-ws &
LOGIC_PID=$!
sleep 2

# 启动 sgate
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 启动 sgate..."
./sgate -conf "$CONF" &
SGATE_PID=$!
sleep 3

# 运行压测
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 开始压测 (WebSocket, $DURATION, $PARALLEL 连接)..."
./bench/bench1_ws/bench1_ws -addr 127.0.0.1:48081 -duration "$DURATION" -parallel "$PARALLEL"

# 清理
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 清理进程..."
kill $SGATE_PID 2>/dev/null || true
kill $LOGIC_PID 2>/dev/null || true
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 完成"
