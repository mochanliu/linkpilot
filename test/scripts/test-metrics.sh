#!/bin/bash
# test-metrics.sh - 隧道内遥测：四项指标 + 两端 BFD 收敛 + 断链检测（任务 #30）
#
# 这条脚本回答的是"遥测真的在工作吗"，四个问题各对应一组断言：
#
#   (A) 质量三项（延迟 / 抖动 / 丢包）来自自研探针，方向无关，所以两个方向
#       各测一遍 —— 一条腿回程差的时候，只测单向会把结论说反。
#   (B) 可达性来自 core BFD，而且必须是**多跳**会话。这里只按多跳形式建会话；
#       状态探针也只认 sw_if_index=~0 的那种，绑接口的会话不会被误当成答案。
#   (C) BFD 是双向协议：只有一端有会话时状态到不了 up，只有一端断了另一端才
#       会 down。所以 up 和 down 都要求"对端先有/先没"这个次序，脚本按次序摆。
#   (D) 多跳 BFD 不得碰转发：全程结束后 overlay 必须仍然通、FIB 的
#       forwarding 不能出现 drop —— 这正是绑接口会话踩过的坑
#       （2026-10-24 实测，详见 internal/vppapi/telemetry.go 文件头）。
#
# 探针与 BFD 的配置/测量走 Go 客户端（TestLiveTelemetryAgainstVPP），也就是
# agent 将来走的同一条 API 路径，而不是 vppctl 手敲；状态断言走
# TestLiveBFDState（这个 VPP 构建的 `show bfd udp session` 只打印计数，
# 状态只能从 API dump 读）。
#
# 前置：**必须先跑过 test/scripts/test-e2e.sh --keep**（wg0@51820 单隧道已通，
# 三个容器在跑）。脚本只在两端建/删探针会话与多跳 BFD 会话，退出时（含失败
# 与 Ctrl-C）清干净，不碰接口、路由和 WG 配置。
#
# 用法:
#   test/scripts/test-metrics.sh
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)

C_NODE1=srwg-node1
C_NODE2=srwg-node2

OVERLAY_N1=${OVERLAY_N1:-192.168.99.1}
OVERLAY_N2=${OVERLAY_N2:-192.168.99.2}
WG_IF=${WG_IF:-wg0}
MODULE=/workspace/controller

# 与 wire.DefaultBFDMinTxUS / DefaultBFDMinRxUS / DefaultBFDDetectMult 同一组值。
# 检测时间 = detect_mult × max(tx, rx) = 150ms；预算给 3s，把 docker exec 和
# go test 启动都算进去，仍然足够严。
BFD_TX=${BFD_TX:-50000}
BFD_RX=${BFD_RX:-50000}
BFD_MULT=${BFD_MULT:-3}
DETECT_BUDGET_MS=${DETECT_BUDGET_MS:-3000}

for a in "$@"; do
    case "$a" in
      -h|--help) awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 0 ;;
      *)         echo "unknown option: $a" >&2; exit 2 ;;
    esac
done

PASS=0
FAIL=0
ok()   { PASS=$((PASS+1)); echo "  [PASS] $*"; }
bad()  { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "== $*"; }
die()  { echo "FATAL: $*" >&2; exit 2; }

dx()  { docker exec "$1" bash -lc "$2"; }
vpp() { dx "$1" "vppctl $2" 2>&1 | tr -d '\r'; }

now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }

swidx_of() {   # <container> -> wg0 的 sw_if_index（空=接口不存在）
    vpp "$1" "show interface $WG_IF" | awk -v n="$WG_IF" '$1==n {print $2; exit}'
}

probe_count() {   # <container> -> 探针会话数
    vpp "$1" "show sr-wg probe" |
        awk '{ for (i=1;i<=NF;i++) if ($(i+1) ~ /^session\(s\)/) { print $i; exit } }'
}

bfd_count() {   # <container> -> BFD 会话数（CLI 只给计数，状态见状态探针）
    vpp "$1" "show bfd udp session" |
        awk '/Number of configured BFD sessions:/ { print $NF; exit }'
}

ibfd_add() {   # <container> <local> <peer>
    vpp "$1" "bfd udp session add multihop local-addr $2 peer-addr $3 desired-min-tx $BFD_TX required-min-rx $BFD_RX detect-mult $BFD_MULT" >/dev/null
}

ibfd_del() {   # <container> <local> <peer>：不存在时 CLI 会报错，忽略
    vpp "$1" "bfd udp session del multihop local-addr $2 peer-addr $3" >/dev/null 2>&1 || true
}

probe_del() {   # <container> <sw_if_index>
    [ -n "$2" ] || return 0
    vpp "$1" "set sr-wg probe $2 del" >/dev/null 2>&1 || true
}

ping_overlay() {   # <container> <dst> -> 0=通
    vpp "$1" "ping $2 repeat 3 interval 0.2" | grep -q '0% packet loss'
}

fib_has_drop() {   # <container> <dst> -> 0=forwarding 里出现 drop
    vpp "$1" "show ip fib $2/32" | grep -q 'dpo-drop'
}

# 状态探针：只读、不配置。成功时 stdout 打印 "<state> <waited_ms>"。
bfd_expect() {   # <container> <local> <peer> <want> <wait_ms>
    local c=$1 l=$2 p=$3 want=$4 wait=$5 out rc
    out=$(dx "$c" "cd $MODULE && SRWG_VPP_SOCK=/run/vpp/api.sock \
        SRWG_OVERLAY_LOCAL=$l SRWG_OVERLAY_PEER=$p \
        SRWG_BFD_WANT=$want SRWG_BFD_WAIT_MS=$wait \
        go test ./internal/vppapi -run TestLiveBFDState -count=1 -v" 2>&1)
    rc=$?
    if [ $rc -ne 0 ]; then
        echo "    状态探针失败（期望 $want）：" >&2
        printf '%s\n' "$out" | grep -E 'bfd state=|期望|连 VPP|FAIL' | tail -6 >&2
        return 1
    fi
    printf '%s\n' "$out" |
        sed -n 's/.*bfd state=\([a-z-]*\) (waited \([0-9]*\)ms).*/\1 \2/p' | head -1
}

# 四项指标 + BFD up 的实测：走 agent 的 Go 客户端路径，断言在测试里。
live_telemetry() {   # <container> <local> <peer>
    local c=$1 l=$2 p=$3 out rc
    out=$(dx "$c" "cd $MODULE && SRWG_VPP_SOCK=/run/vpp/api.sock SRWG_WG_IF=$WG_IF \
        SRWG_OVERLAY_LOCAL=$l SRWG_OVERLAY_PEER=$p SRWG_BFD_EXPECT_UP=1 \
        go test ./internal/vppapi -run TestLiveTelemetryAgainstVPP -count=1 -v" 2>&1)
    rc=$?
    # 把测试打出的实测值原样展示：这是"四项都有值"的直接证据。
    printf '%s\n' "$out" |
        grep -E '(probe|bfd):' | sed -E 's/^.*(probe:|bfd:)/\1/' | sed 's/^/     /'
    if [ $rc -ne 0 ]; then
        printf '%s\n' "$out" | grep -E -- '--- FAIL|期望|不合理|不自洽|折返率|丢包率|抖动' | head -10 >&2
        return 1
    fi
    return 0
}

SW1=""
SW2=""
cleanup_all() {
    probe_del "$C_NODE1" "$SW1"
    probe_del "$C_NODE2" "$SW2"
    ibfd_del "$C_NODE1" "$OVERLAY_N1" "$OVERLAY_N2"
    ibfd_del "$C_NODE2" "$OVERLAY_N2" "$OVERLAY_N1"
}
trap 'cleanup_all' EXIT
trap 'exit 130' INT TERM

# ------------------------------------------------------------------ 0
info "0. 前置：wg0 与 overlay 通路"
SW1=$(swidx_of "$C_NODE1")
SW2=$(swidx_of "$C_NODE2")
[ -n "$SW1" ] || die "$C_NODE1 没有 $WG_IF：先跑 test/scripts/test-e2e.sh --keep"
[ -n "$SW2" ] || die "$C_NODE2 没有 $WG_IF：先跑 test/scripts/test-e2e.sh --keep"
ok "wg0 存在：node1 sw_if_index=$SW1，node2 sw_if_index=$SW2"

ping_overlay "$C_NODE1" "$OVERLAY_N2" || die "node1 -> node2 overlay 不通：先跑 test/scripts/test-e2e.sh --keep"
ping_overlay "$C_NODE2" "$OVERLAY_N1" || die "node2 -> node1 overlay 不通：先跑 test/scripts/test-e2e.sh --keep"
ok "overlay 双向可达"

# ------------------------------------------------------------------ A
info "A. 起点干净（上一轮残留会污染计数与状态）"
cleanup_all
p1=$(probe_count "$C_NODE1"); p2=$(probe_count "$C_NODE2")
b1=$(bfd_count "$C_NODE1");   b2=$(bfd_count "$C_NODE2")
[ -n "$p1" ] && [ -n "$p2" ] && [ -n "$b1" ] && [ -n "$b2" ] || die "读不到探针/BFD 计数（插件或 VPP 没起来？）"
[ "$p1" = 0 ] && [ "$p2" = 0 ] || die "探针会话未清空（node1=$p1 node2=$p2）"
# 只清多跳会话：别人手配的绑接口会话不归本脚本管，不去"顺手"删掉。
[ "$b1" = 0 ] && [ "$b2" = 0 ] || die "仍有 BFD 会话（node1=$b1 node2=$b2）；本脚本只清多跳 (local,peer) 会话，请先手工确认"
ok "两端 0 条探针、0 条 BFD 会话"

# ------------------------------------------------------------------ B
info "B. 方向 node1 -> node2：延迟 / 抖动 / 丢包 + BFD up"
# 对端先有会话，BFD 才会 up；探针不需要对端配置（折返在插件里做），下面单独验证。
ibfd_add "$C_NODE2" "$OVERLAY_N2" "$OVERLAY_N1" || bad "node2 建 BFD 会话失败"
p2=$(probe_count "$C_NODE2")
[ "$p2" = 0 ] && ok "node2 没有任何探针会话，仍应能折返 node1 的探针（echo 在插件层）" ||
    bad "node2 意外存在探针会话（$p2）"
if live_telemetry "$C_NODE1" "$OVERLAY_N1" "$OVERLAY_N2"; then
    ok "node1 方向：rtt/抖动/丢包有值且自洽，BFD 收敛 up（Go 客户端路径）"
else
    bad "node1 方向的遥测断言失败"
fi

# ------------------------------------------------------------------ C
info "C. 方向 node2 -> node1：反向同样有值"
ibfd_add "$C_NODE1" "$OVERLAY_N1" "$OVERLAY_N2" || bad "node1 建 BFD 会话失败"
if live_telemetry "$C_NODE2" "$OVERLAY_N2" "$OVERLAY_N1"; then
    ok "node2 方向：rtt/抖动/丢包有值且自洽，BFD 收敛 up"
else
    bad "node2 方向的遥测断言失败"
fi

# ------------------------------------------------------------------ D
info "D. 两端同时 up（多跳），断链检测的前提"
ibfd_add "$C_NODE2" "$OVERLAY_N2" "$OVERLAY_N1" || bad "node2 重建 BFD 会话失败"
r1=$(bfd_expect "$C_NODE1" "$OVERLAY_N1" "$OVERLAY_N2" up 8000) || bad "node1 的 BFD 未在 8s 内 up"
r2=$(bfd_expect "$C_NODE2" "$OVERLAY_N2" "$OVERLAY_N1" up 8000) || bad "node2 的 BFD 未在 8s 内 up"
b1=$(bfd_count "$C_NODE1"); b2=$(bfd_count "$C_NODE2")
if [ -n "$r1" ] && [ -n "$r2" ] && [ "$b1" = 1 ] && [ "$b2" = 1 ]; then
    ok "两端 BFD 均 up（各自恰好 1 条多跳会话；探针等待 node1=${r1#* }ms node2=${r2#* }ms）"
else
    bad "两端未同时 up：node1='$r1'/$b1 条，node2='$r2'/$b2 条"
fi

# ------------------------------------------------------------------ E
info "E. 断链检测：删掉 node1 的会话，node2 必须自己发现"
t0=$(now_ms)
ibfd_del "$C_NODE1" "$OVERLAY_N1" "$OVERLAY_N2"
r2=$(bfd_expect "$C_NODE2" "$OVERLAY_N2" "$OVERLAY_N1" down 5000) || bad "node2 未在 5s 内变 down"
t1=$(now_ms)
elapsed=$(( t1 - t0 ))
if [ -n "${r2:-}" ]; then
    if [ "$elapsed" -lt "$DETECT_BUDGET_MS" ]; then
        ok "node2 检测到断链：${elapsed}ms（预算 ${DETECT_BUDGET_MS}ms；BFD ${BFD_MULT}×$((BFD_TX/1000))ms）"
    else
        bad "node2 检测断链用了 ${elapsed}ms，超过预算 ${DETECT_BUDGET_MS}ms"
    fi
else
    bad "node2 没有给出 down 状态"
fi
r1=$(bfd_expect "$C_NODE1" "$OVERLAY_N1" "$OVERLAY_N2" absent 0) || bad "node1 删除后仍能读到会话"
[ -n "${r1:-}" ] && ok "node1 侧会话确实已删（absent）"

# ------------------------------------------------------------------ F
info "F. 多跳 BFD 不碰转发（绑接口会话的回归点）"
if fib_has_drop "$C_NODE1" "$OVERLAY_N2" || fib_has_drop "$C_NODE2" "$OVERLAY_N1"; then
    bad "overlay 路由的 forwarding 出现 dpo-drop"
else
    ok "两端 overlay /32 的 FIB 没有 drop 桶"
fi
if ping_overlay "$C_NODE1" "$OVERLAY_N2" && ping_overlay "$C_NODE2" "$OVERLAY_N1"; then
    ok "全程 BFD/探针之后 overlay 仍然双向可达"
else
    bad "BFD/探针活动之后 overlay 不通"
fi

# ------------------------------------------------------------------ G
info "G. 清理与终态"
cleanup_all
p1=$(probe_count "$C_NODE1"); p2=$(probe_count "$C_NODE2")
b1=$(bfd_count "$C_NODE1");   b2=$(bfd_count "$C_NODE2")
[ "$p1" = 0 ] && [ "$p2" = 0 ] && ok "探针会话已清空" || bad "探针残留：node1=$p1 node2=$p2"
[ "$b1" = 0 ] && [ "$b2" = 0 ] && ok "BFD 会话已清空" || bad "BFD 残留：node1=$b1 node2=$b2"
ping_overlay "$C_NODE1" "$OVERLAY_N2" && ping_overlay "$C_NODE2" "$OVERLAY_N1" &&
    ok "终态 overlay 双向可达" || bad "终态 overlay 不通"

# ------------------------------------------------------------------ summary
echo
echo "==================== SUMMARY ===================="
echo "  PASS=$PASS  FAIL=$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
exit 0
