#!/bin/bash
# test-mpls.sh - MPLS over WireGuard 跨 VRF 端到端测试（在宿主机执行）
#
# 拓扑复用 test-e2e.sh（node1 / transit / node2 同一套容器），但只测 MPLS：
#
#   node1(VRF 100) 10.10.0.1/32 === wg0 192.168.99.1
#        |  ip route 10.20.0.1/32 table 100 via 192.168.99.2 wg0 out-labels 200
#        v
#   mpls-label push -> "mpls via 192.168.99.2 wg0" -> sr-wg4-mpls-out
#        -> wg4-output-tun (WG 加密) -> adj-midchain-tx -> ip4-load-balance
#        -> sr-wg4-encap -> transit -> node2
#   node2: sr-wg4-input -> wg4-input (解密) -> sr-wg4-post (nibble 0 -> MPLS)
#        -> mpls-input -> mpls-lookup (local label 200, eos)
#        -> lookup-ip4-dst-itf table 100 -> 10.20.0.1 (loop0)
#   回程对称：local label 100，标签值对调。
#
# 覆盖的断言：
#   step 2  邻接改向：wg0 上 MPLS 邻接的 next 槽 = sr-wg4-mpls-out 所在槽
#           （改槽会被 vlib_node_add_next_with_slot 去重，必须走改邻接）
#   step 3  跨 VRF ping 3/3
#   step 4  出口加密：shim -> wg4-output-tun -> adj-midchain-tx 增量 = 发包数，
#           两侧 mpls-output encapsulated 增量对称
#   step 5  入口解密：对端 sr-wg4-post -> mpls-input 增量 = 发包数，
#           mpls-input/mpls-lookup decapsulated 增量、MPLS DROP DPO 增量必须为 0
#   step 6  标签开销后的 MTU：inner 1328B（size 1300）必须通
#
# 用法:
#   test/scripts/test-mpls.sh [--keep] [--skip-build]
#
#     --keep        测试结束不销毁容器（便于人工排查）
#     --skip-build  跳过插件编译（复用 build/ 里已有的 sr_wg_plugin.so）
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
COMPOSE_FILE="${ROOT}/test/docker/docker-compose.yml"
E2E="${ROOT}/test/scripts/test-e2e.sh"

C_NODE1=srwg-node1
C_NODE2=srwg-node2
ALL_CONTAINERS="${C_NODE1} srwg-transit ${C_NODE2}"

N1_LOOP=10.10.0.1
N2_LOOP=10.20.0.1
N1_WG=192.168.99.1
N2_WG=192.168.99.2
LABEL_TO_N1=100      # node2 发往 node1 的标签
LABEL_TO_N2=200      # node1 发往 node2 的标签
VRF=100

KEEP=0
SKIP_BUILD=0
for a in "$@"; do
    case "$a" in
      --keep)       KEEP=1 ;;
      --skip-build) SKIP_BUILD=1 ;;
      -h|--help)    awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 0 ;;
      *)            echo "unknown option: $a" >&2; exit 2 ;;
    esac
done

PASS=0
FAIL=0
ok()   { PASS=$((PASS+1)); echo "  [PASS] $*"; }
bad()  { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "== $*"; }

vc() { local c=$1; shift; docker exec "${c}" vppctl "$@" 2>&1; }

# show node 里某条 next 的累计 Vectors：node_vec <container> <node> <next-name>
node_vec() {
    vc "$1" show node "$2" |
        awk -v n="$3" '$3==n {print $4}' | tail -1
}

# show errors 里某个节点+错误文本的计数：err_count <container> <node> <substr>
err_count() {
    vc "$1" show errors |
        awk -v n="$2" -v t="$3" 'index($0,t) && $2==n {print $1}' | tail -1
}

num_or_zero() { [ -n "${1:-}" ] && echo "$1" || echo 0; }

cleanup() {
    [ "${KEEP}" = "1" ] && { info "--keep: containers left running"; return 0; }
    info "tearing down"
    (cd "${ROOT}/test/docker" && docker compose -f "${COMPOSE_FILE}" down -v --remove-orphans >/dev/null 2>&1)
}

# ==================================================================
info "step 1: 环境准备（复用 test-e2e --up-only）"
# ==================================================================
e2e_args="--up-only --keep"
[ "${SKIP_BUILD}" = "1" ] && e2e_args="${e2e_args} --skip-build"
bash "${E2E}" ${e2e_args} || { echo "test-e2e --up-only failed" >&2; cleanup; exit 1; }

# ==================================================================
info "step 2: node1/node2 建 VRF ${VRF} + overlay /32 + 本地标签 + 带标签路由"
# ==================================================================
mpls_up() {   # $1=container $2=own-lo $3=peer-lo $4=peer-wg $5=out-label $6=in-label
    vc "$1" ip table add "${VRF}" >/dev/null
    local lo
    lo=$(vc "$1" create loopback interface | tr -d '\r')
    vc "$1" set interface state "${lo}" up >/dev/null
    vc "$1" set interface ip table "${lo}" "${VRF}" >/dev/null
    vc "$1" set interface ip address "${lo}" "$2/32" >/dev/null
    vc "$1" mpls local-label add "$6" eos via ip4-lookup-in-table "${VRF}" >/dev/null
    vc "$1" ip route add "$3/32" table "${VRF}" via "$4" wg0 out-labels "$5" >/dev/null
}
mpls_up "${C_NODE1}" "${N1_LOOP}" "${N2_LOOP}" "${N2_WG}" "${LABEL_TO_N2}" "${LABEL_TO_N1}"
mpls_up "${C_NODE2}" "${N2_LOOP}" "${N1_LOOP}" "${N1_WG}" "${LABEL_TO_N1}" "${LABEL_TO_N2}"

# 看门狗 100ms 内改向；显式触发一次让断言立刻稳定
for c in "${C_NODE1}" "${C_NODE2}"; do
    vc "$c" sr-wg mpls-egress >/dev/null
done
sleep 1

for c in "${C_NODE1}" "${C_NODE2}"; do
    # 邻接要指向 sr-wg4-mpls-out；比较邻接 next 与 shim 在 mpls-midchain 表里的槽
    tbl=$(vc "$c" show node mpls-midchain)
    shim_slot=$(echo "${tbl}" | awk '$3=="sr-wg4-mpls-out" {print $1}')
    adj_line=$(vc "$c" show adj | grep "mpls via .* wg0" | head -1)
    adj_next=$(echo "${adj_line}" | sed -n 's/.*next:\([0-9]*\).*/\1/p')
    if [ -n "${shim_slot}" ] && [ "${adj_next}" = "${shim_slot}" ]; then
        ok "$c: wg0 MPLS 邻接 next=${adj_next} 指向 sr-wg4-mpls-out"
    else
        bad "$c: 邻接 next=${adj_next:-?} 未指向 shim（shim 槽=${shim_slot:-?}）"
    fi
done

# 幂等：再触发一次不应再改（返回 0）
again=$(vc "${C_NODE1}" sr-wg mpls-egress | grep repointed | awk '{print $2}')
if [ "${again}" = "0" ]; then
    ok "sr-wg mpls-egress 幂等（第二次 0 条改动）"
else
    bad "sr-wg mpls-egress 非幂等（第二次改动 ${again:-?} 条）"
fi

# ==================================================================
info "step 3: 跨 VRF ping（${N1_LOOP} -> ${N2_LOOP}，source loop0）"
# ==================================================================
ping_out=$(vc "${C_NODE1}" ping "${N2_LOOP}" source loop0 repeat 3 interval 1)
echo "${ping_out}" | sed 's/^/    /'
if echo "${ping_out}" | grep -q "0% packet loss" && echo "${ping_out}" | grep -q "3 sent, 3 received"; then
    ok "MPLS over WG 跨 VRF 双向 3/3"
else
    bad "跨 VRF ping 未全通"
fi

# ==================================================================
info "step 4: 出口加密路径计数（发包 3 个 + 回程 3 个）"
# ==================================================================
s_shim=$(num_or_zero "$(node_vec "${C_NODE1}" sr-wg4-mpls-out wg4-output-tun)")
s_wg=$(num_or_zero "$(node_vec "${C_NODE1}" wg4-output-tun adj-midchain-tx)")
s_drop=$(num_or_zero "$(node_vec "${C_NODE1}" sr-wg4-mpls-out error-drop)")
echo "    node1: shim->wg4-output-tun=${s_shim}  wg4-output-tun->adj-midchain-tx=${s_wg}  shim->drop=${s_drop}"
if [ "${s_shim}" -ge 3 ] && [ "${s_wg}" -ge "${s_shim}" ] && [ "${s_drop}" = "0" ]; then
    ok "node1 出口全部走加密路径，无落 drop"
else
    bad "node1 出口加密路径计数不对（shim=${s_shim} wg=${s_wg} drop=${s_drop}）"
fi

r_shim=$(num_or_zero "$(node_vec "${C_NODE2}" sr-wg4-mpls-out wg4-output-tun)")
r_wg=$(num_or_zero "$(node_vec "${C_NODE2}" wg4-output-tun adj-midchain-tx)")
r_drop=$(num_or_zero "$(node_vec "${C_NODE2}" sr-wg4-mpls-out error-drop)")
echo "    node2: shim->wg4-output-tun=${r_shim}  wg4-output-tun->adj-midchain-tx=${r_wg}  shim->drop=${r_drop}"
if [ "${r_shim}" -ge 3 ] && [ "${r_wg}" -ge "${r_shim}" ] && [ "${r_drop}" = "0" ]; then
    ok "node2 回程也走加密路径，无落 drop"
else
    bad "node2 回程加密路径计数不对（shim=${r_shim} wg=${r_wg} drop=${r_drop}）"
fi

# ==================================================================
info "step 5: 入口解密 + 标签弹栈（对端 3 个请求 + 本端 3 个回程）"
# ==================================================================
p_post=$(num_or_zero "$(node_vec "${C_NODE2}" sr-wg4-post mpls-input)")
p_in=$(num_or_zero "$(err_count "${C_NODE2}" mpls-input "decapsulated")")
p_drop=$(num_or_zero "$(err_count "${C_NODE2}" mpls-input "MPLS DROP DPO")")
echo "    node2: sr-wg4-post->mpls-input=${p_post}  mpls-input decapsulated=${p_in}  DROP DPO=${p_drop}"
if [ "${p_post}" -ge 3 ] && [ "${p_in}" -ge 3 ] && [ "${p_drop}" = "0" ]; then
    ok "node2 收侧：识别 MPLS -> 解密 -> 弹栈，无 DROP DPO"
else
    bad "node2 收侧不对（post=${p_post} decap=${p_in} dropdpo=${p_drop}）"
fi

q_post=$(num_or_zero "$(node_vec "${C_NODE1}" sr-wg4-post mpls-input)")
q_drop=$(num_or_zero "$(err_count "${C_NODE1}" mpls-input "MPLS DROP DPO")")
echo "    node1: sr-wg4-post->mpls-input=${q_post}  mpls-input DROP DPO=${q_drop}"
if [ "${q_post}" -ge 3 ] && [ "${q_drop}" = "0" ]; then
    ok "node1 收侧回程弹栈正常（local label ${LABEL_TO_N1}）"
else
    bad "node1 收侧不对（post=${q_post} dropdpo=${q_drop}）"
fi

# ==================================================================
info "step 6: 标签后的 MTU（inner ${N1_LOOP} 1328B，wg0 MPLS MTU 1360）"
# ==================================================================
mtu_out=$(vc "${C_NODE1}" ping "${N2_LOOP}" source loop0 repeat 2 size 1300)
if echo "${mtu_out}" | grep -q "0% packet loss"; then
    ok "inner 1328B 通（1328 + 4B 标签 <= 1360）"
else
    bad "inner 1328B 不通，标签开销或 MTU 预算有问题"
    echo "${mtu_out}" | sed 's/^/    /'
fi

# ==================================================================
info "结果: PASS=${PASS} FAIL=${FAIL}"
# ==================================================================
if [ "${FAIL}" != "0" ]; then
    for c in ${ALL_CONTAINERS}; do
        echo "################ ${c} ################"
        vc "$c" show errors | awk '$1+0 > 0' | head -20
    done
fi
cleanup
[ "${FAIL}" = "0" ] || exit 1
echo "== MPLS over WireGuard: ALL PASS"
