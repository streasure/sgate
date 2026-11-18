#!/usr/bin/env bash
# 架构依赖方向检查（AGENTS.md Architecture；logicserver-design §4.3 CI 缺口的本仓部分）。
#
# 方法：go list 取各包**直接依赖边**（.Imports，不含测试），断言禁边。直接边即可
# 覆盖架构规则——传递违规必然表现为某个包的直接边；比逐包 go list -deps 更省。
#
# 规则（sgate 平面边界，package = plane）：
#   1. internal/dataplane ↛ internal/{transport,logiclink,control}
#      （dataplane 只持消费方接口视图，见 AGENTS.md）
#   2. internal/transport  ↛ internal/{logiclink,control}   （transport → dataplane 允许）
#   3. internal/logiclink  ↛ internal/{transport,control}   （logiclink → dataplane 允许）
#   4. internal/control 只允许 cmd/* 引用（控制壳由组合根装配）
# 规则（跨仓零反向，logicserver-design §4.2）：
#   5. sgate ↛ github.com/streasure/logic（bench/* 的 SDK 替身用法除外）/ loginserver
#   6. logic ↛ sgate/loginserver；loginserver ↛ sgate/logic
#   7. protocol / util / treasure-slog ↛ sgate/logic/loginserver
#
# 兄弟仓（../logic 等）缺失时跳过对应规则（sgate 单仓检出仍可运行）。
# 用法：bash scripts/check-deps.sh        退出码 0=通过，1=存在违规
set -uo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
parent="$(dirname "$root")"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
fail=0

collect() { # $1=模块目录 $2=标签（缺失/失败时提示用）
    local dir="$1" label="$2" list
    if [ ! -d "$dir" ]; then
        echo "skip  $label（目录不存在: $dir）"
        return 1
    fi
    if ! list="$(cd "$dir" && go list -f '{{.ImportPath}}|{{range .Imports}}{{.}};{{end}}' ./... 2>/dev/null)"; then
        echo "skip  $label（go list 失败）"
        return 1
    fi
    if [ -z "$list" ]; then
        echo "skip  $label（无包）"
        return 1
    fi
    printf '%s\n' "$list" | awk -F'|' 'NF >= 2 {
        n = split($2, d, ";")
        for (i = 1; i <= n; i++) if (d[i] != "") print $1 "\t" d[i]
    }' >>"$tmp"
    return 0
}

# rule <描述> <pkg-ERE> <dep-ERE>：pkg 匹配且 dep 匹配即违规
rule() {
    local hits
    hits="$(awk -F'\t' -v p="$2" -v d="$3" '$1 ~ p && $2 ~ d { print "    " $1 " -> " $2 }' "$tmp")"
    if [ -n "$hits" ]; then
        echo "FAIL  $1"
        printf '%s\n' "$hits"
        fail=1
    else
        echo "ok    $1"
    fi
}

echo "== 收集依赖边 =="
have_sgate=no
if collect "$root" "sgate"; then have_sgate=yes; fi
collect "$parent/logic" "logic" || true
collect "$parent/loginserver" "loginserver" || true
collect "$parent/protocol" "protocol" || true
collect "$parent/util" "util" || true
collect "$parent/treasure-slog" "treasure-slog" || true

if [ "$have_sgate" = no ]; then
    echo "FAIL  sgate 依赖边收集失败（go list ./... 不可用）"
    exit 1
fi

SG="github.com/streasure/sgate"
echo "== sgate 平面边界 =="
rule "dataplane ↛ transport/logiclink/control" \
    "^$SG/internal/dataplane$" \
    "^$SG/internal/(transport|logiclink|control)$"
rule "transport ↛ logiclink/control" \
    "^$SG/internal/transport$" \
    "^$SG/internal/(logiclink|control)$"
rule "logiclink ↛ transport/control" \
    "^$SG/internal/logiclink$" \
    "^$SG/internal/(transport|control)$"

hits="$(awk -F'\t' -v ctl="$SG/internal/control" \
    '$2 == ctl && $1 !~ /\/cmd\// { print "    " $1 " -> " $2 }' "$tmp")"
if [ -n "$hits" ]; then
    echo "FAIL  internal/control 只允许 cmd/* 引用"
    printf '%s\n' "$hits"
    fail=1
else
    echo "ok    internal/control 只允许 cmd/* 引用"
fi

echo "== sgate 跨仓零反向 =="
hits="$(awk -F'\t' \
    '$1 ~ /^github\.com\/streasure\/sgate\// && $1 !~ /\/bench\// && \
     $2 ~ /^github\.com\/streasure\/logic(\/|$)/ { print "    " $1 " -> " $2 }' "$tmp")"
if [ -n "$hits" ]; then
    echo "FAIL  sgate ↛ logic SDK（bench/* 替身用法除外）"
    printf '%s\n' "$hits"
    fail=1
else
    echo "ok    sgate ↛ logic SDK（bench/* 替身用法除外）"
fi
rule "sgate ↛ loginserver" "^$SG/" "^github.com/streasure/loginserver"

echo "== 兄弟仓零反向 =="
if [ -d "$parent/logic" ]; then
    rule "logic ↛ sgate/loginserver" "^github.com/streasure/logic" \
        "^github.com/streasure/(sgate|loginserver)"
else
    echo "skip  logic 仓缺失"
fi
if [ -d "$parent/loginserver" ]; then
    rule "loginserver ↛ sgate/logic" "^github.com/streasure/loginserver" \
        "^github.com/streasure/(sgate|logic)"
else
    echo "skip  loginserver 仓缺失"
fi
for repo in protocol util treasure-slog; do
    if [ -d "$parent/$repo" ]; then
        rule "$repo ↛ sgate/logic/loginserver" "^github.com/streasure/$repo" \
            "^github.com/streasure/(sgate|logic|loginserver)"
    else
        echo "skip  $repo 仓缺失"
    fi
done

echo ""
if [ "$fail" -eq 0 ]; then
    echo "check-deps: ALL PASS"
    exit 0
fi
echo "check-deps: FAILED（见上方 FAIL 行）"
exit 1
