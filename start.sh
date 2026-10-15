#!/bin/bash

# ============================================================
# sgate 网关启动脚本 (Linux)
#
# 用法:
#   ./start.sh
#   CONF=config/cluster/gateway1.yaml ./start.sh
#   CONF=config/bench/batch-on.yaml LOG_CONF=config/log/bench.yaml ./start.sh
#
# 环境变量:
#   CONF      网关配置（默认 config/gateway.yaml）
#   LOG_CONF  日志配置（默认 config/log/default.yaml）
#
# 配置目录结构:
#   config/gateway.yaml          单机默认
#   config/cluster/gateway*.yaml 集群实例
#   config/bench/*.yaml          压测
#   config/log/*.yaml            日志
#   数值默认见 internal/config/defaults.go（无需在 yaml 重复）。
# ============================================================

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

CONF="${CONF:-config/gateway.yaml}"
LOG_CONF="${LOG_CONF:-config/log/default.yaml}"

# 编译
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 编译 sgate..."
go build -o sgate ./cmd/gateway
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 编译完成"

# 启动
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 启动 sgate..."
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 配置文件: $CONF"
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 日志配置: $LOG_CONF"
echo "[$(date '+%Y-%m-%d %H:%M:%S')] 等价命令: ./sgate -conf $CONF -logger $LOG_CONF"
echo "[$(date '+%Y-%m-%d %H:%M:%S')] PID: $$"

exec ./sgate -conf "$CONF" -logger "$LOG_CONF"
