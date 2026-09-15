#!/bin/bash

# ============================================================
# bench2_ws 压测脚本 (Linux)
# 测试: 逻辑服→sgate→客户端 WebSocket 推送性能
# 用法: ./run.sh [off|on]  (batchPush 模式，默认 off)
# ============================================================

set -e
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR/../.."

MODE="${1:-off}"
if [ "$MODE" = "on" ]; then
    CONF="config/config_batch_on.yaml"
else
    CONF="config/config_batch_off.yaml"
fi

DURATION="10s"
PARALLEL=100
PUSH_INTERVAL=0
PUSH_SIZE=64
EXPECTED_MEMBERS=100
PUSH_WORKERS=12

# 编译
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 编译..."
go build -o sgate ./cmd/
go build -o bench/logic2_ws/logic2_ws ./bench/logic2_ws
go build -o bench/bench2_ws/bench2_ws ./bench/bench2_ws
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 编译完成"

# 启动 logic
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 启动 logic2_ws..."
./bench/logic2_ws/logic2_ws -port 50061 -id logic2-ws \
    -push-interval "$PUSH_INTERVAL" -push-size "$PUSH_SIZE" \
    -expected-members "$EXPECTED_MEMBERS" -push-workers "$PUSH_WORKERS" &
LOGIC_PID=$!
sleep 2

# 启动 sgate
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 启动 sgate (batchPush=$MODE)..."
./sgate -conf "$CONF" &
SGATE_PID=$!
sleep 3

# 运行压测
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 开始压测 (WebSocket, $DURATION, $PARALLEL 连接, batchPush=$MODE)..."
./bench/bench2_ws/bench2_ws -addr 127.0.0.1:48081 -duration "$DURATION" -parallel "$PARALLEL" -server-id logic2-ws

# 清理
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 清理进程..."
kill $SGATE_PID 2>/dev/null || true
kill $LOGIC_PID 2>/dev/null || true
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 完成"
