#!/bin/bash

# ============================================================
# sgate 网关启动脚本 (Linux)
# ============================================================

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# 配置路径
CONF="config/config.yaml"
LOG_CONF="config/log.yaml"

# 编译
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 编译 sgate..."
go build -o sgate ./cmd/
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 编译完成"

# 启动
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 启动 sgate..."
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 配置文件: $CONF"
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 日志配置: $LOG_CONF"
echo "[$(date '+%Y-%m-%d %H:%M:%S')] PID: $$"

exec ./sgate -conf "$CONF" -logger "$LOG_CONF"
