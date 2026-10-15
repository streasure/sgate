#!/bin/bash

# ============================================================
# sgate 网关启动脚本 (Linux)
#
# 用法:
#   ./start.sh
#   CONF=config/sgate1.yaml ./start.sh
#   CONF=config/config_batch_on.yaml LOG_CONF=config/log_bench.yaml ./start.sh
#
# 环境变量:
#   CONF      网关配置（默认 config/config.yaml）
#   LOG_CONF  日志配置（默认 config/log.yaml）
#
# 配置文件分工：
#   yaml 只放环境项（身份/端口/etcd/特性开关）；
#   数值默认见 internal/config/defaults.go（无需在 yaml 重复）。
# ============================================================

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

CONF="${CONF:-config/config.yaml}"
LOG_CONF="${LOG_CONF:-config/log.yaml}"

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
