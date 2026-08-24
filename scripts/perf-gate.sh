#!/usr/bin/env bash
# 性能基准纳入发布流程：每次代码变更后运行压测，对比 QPS/延迟基线，
# 退化超过阈值则阻断发布。
#
# 用法：
#   scripts/perf-gate.sh                     # 与基线对比
#   scripts/perf-gate.sh --update-baseline  # 更新基线
#
# 依赖：bench 工具（examples/bench）+ jq
set -euo pipefail

SCENARIO=${BENCH_SCENARIO:-bidirectional}
CONNECTIONS=${BENCH_CONNS:-200}
BATCH_SIZE=${BENCH_BATCH:-256}
DURATION=${BENCH_DURATION:-10s}
THRESHOLD_QPS=${THRESHOLD_QPS:-0.90}      # QPS 不低于基线 90%
THRESHOLD_P99=${THRESHOLD_P99:-1.20}      # P99 不高于基线 120%

BASELINE_FILE=scripts/perf-baseline.json
RESULT_FILE=scripts/perf-result.json

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT_DIR"

echo "==> 编译 bench 工具"
go build -o ./bin/bench ./examples/bench

echo "==> 启动 logic_server"
./examples/logic_server/logic_server --port=50051 &
LOGIC_PID=$!
trap "kill $LOGIC_PID 2>/dev/null || true" EXIT

sleep 2

echo "==> 启动 gateway"
./examples/high_concurrency_gateway/high_concurrency_gateway &
GW_PID=$!
trap "kill $LOGIC_PID $GW_PID 2>/dev/null || true" EXIT

# 等待 gateway ready
for i in $(seq 1 30); do
  if curl -s http://127.0.0.1:8082/ready >/dev/null 2>&1; then
    echo "gateway ready"
    break
  fi
  sleep 1
done

echo "==> 运行压测：scenario=$SCENARIO conns=$CONNECTIONS batch=$BATCH_SIZE duration=$DURATION"
./bin/bench \
  -scenario=$SCENARIO \
  -conns=$CONNECTIONS \
  -batch-size=$BATCH_SIZE \
  -duration=$DURATION \
  -output=$RESULT_FILE

# 解析结果
QPS=$(jq -r '.qps' $RESULT_FILE)
P99=$(jq -r '.p99_us' $RESULT_FILE)
echo "==> 本次结果：QPS=$QPS  P99=${P99}us"

# 与基线对比
if [[ ! -f "$BASELINE_FILE" ]] || [[ "${1:-}" == "--update-baseline" ]]; then
  cp $RESULT_FILE $BASELINE_FILE
  echo "==> 基线已更新"
  exit 0
fi

BASE_QPS=$(jq -r '.qps' $BASELINE_FILE)
BASE_P99=$(jq -r '.p99_us' $BASELINE_FILE)

# 浮点比较
QPS_RATIO=$(awk "BEGIN {print ($QPS / $BASE_QPS)}")
P99_RATIO=$(awk "BEGIN {print ($P99 / $BASE_P99)}")

echo "==> 基线：QPS=$BASE_QPS  P99=${BASE_P99}us"
echo "==> 对比：QPS ratio=$QPS_RATIO  P99 ratio=$P99_RATIO"

PASS=1
if awk "BEGIN {exit !($QPS_RATIO < $THRESHOLD_QPS)}"; then
  echo "ERROR：QPS 退化超过阈值（$QPS_RATIO < $THRESHOLD_QPS）"
  PASS=0
fi
if awk "BEGIN {exit !($P99_RATIO > $THRESHOLD_P99)}"; then
  echo "ERROR：P99 延迟退化超过阈值（$P99_RATIO > $THRESHOLD_P99）"
  PASS=0
fi

if [[ $PASS -eq 1 ]]; then
  echo "==> 性能门禁通过 ✅"
  exit 0
else
  echo "==> 性能门禁失败 ❌（如有意为之，请运行 scripts/perf-gate.sh --update-baseline 更新基线）"
  exit 1
fi
