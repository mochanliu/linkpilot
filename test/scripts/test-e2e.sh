#!/bin/bash
# test-e2e.sh - SR-WG 三节点端到端测试（在宿主机执行）
#
# 拓扑（见 test/docker/docker-compose.yml）:
#
#            net_a 10.10.1.0/24              net_b 10.10.2.0/24
#   node1 ─────────────────────── transit ─────────────────────── node2
#  10.10.1.2                     10.10.1.3 / 10.10.2.3          10.10.2.2
#   wg0 192.168.99.1                                              wg0 192.168.99.2
#
# 正向数据流:
#   node1 ping 192.168.99.2
#     -> ip4-lookup -> ip4-midchain(wg0) -> wg4-output-tun (WG 加密 + 外层 IP4/UDP)
#     -> sr-wg4-encap (插 SR Header, 外层 dst 改成 transit:51821, seg_left=1)
#     -> ip4-lookup -> host-eth1 (underlay 出口)
#     -> transit: udp4-local -> sr-wg4-transit (验 HMAC, seg_left 1->0,
#                 外层 dst 改成 node2:51820) -> ip4-lookup -> host-eth(net_b)
#     -> node2: udp4-local -> sr-wg4-input (验 HMAC, seg_left==0, 剥 SR 头)
#                 -> wg4-input (WG 解密)
#
# 反向数据流（node2 未绑定 SR path）:
#   node2 -> 普通 WG 直发 -> transit 纯 IP 转发 -> node1
#     -> sr-wg4-input 见 magic != 0x53 -> 旁路到 wg4-input（decap_bypass++）
#   这条路径专门用来验证「未配置 SR path 时 WireGuard 直连模式不受影响」。
#
# 覆盖的断言：
#   step 1-3  插件加载 / ip4-output arc 挂点 / encap 绑定可用
#   step 4    encap -> transit -> decap 计数器 + 反向 WG 直连旁路 + overlay 连通
#   step 5    HMAC 篡改（换错密钥后必须全部拒绝）
#   step 6    SR 头部格式负向（magic/ver/len 分类计数）+ transit disable 门控
#             —— 用 AF_PACKET 原始包注入，真实 WG 流量造不出畸形头
#   step 7    中转绝不改写外层源（控制面无 local-ip 开关 + transit 自身 trace 佐证）
#   step 8    密钥轮换（key set 刷新路径里的 P-Key 副本）后隧道必须仍然可用
#   step 9    外层源观测表（vBOND 地址发现）：三处采集点的证据 + verified 分档
#             + clear 生效
#   观测自证  两条"观测会不会骗人"的断言（都在计数器断言之后跑）：
#             #24 node2 trace 解出的 hmac_id/path_id/seg_left 必须等于
#                `show sr-wg paths` 的真值 —— 转发计数全对而 trace 是垃圾，曾经真发生过；
#             #25 在 loopback 上开 encap 再删掉那张口，挂点表里不许留 DELETED(n)，
#                而真出口 host-eth0 必须还在（两头都判，防误清）。
#             #26 用非 overlay 源（tap 后的 192.168.50.1）经隧道打到 node2，必须拿到
#                回包且 node2 的 null-node blackholed 不增长 —— peer 的 allowed-ip
#                写对端 /32 会把转发流量在内层源过滤上 punt 掉，隧道自测却全绿。
#
# 用法:
#   test/scripts/test-e2e.sh [--keep] [--skip-build] [--no-tamper] [--up-only]
#
#     --keep        测试结束不销毁容器（便于人工排查）
#     --skip-build  跳过插件编译（复用 build/ 里已有的 sr_wg_plugin.so）
#     --no-tamper   跳过 HMAC 篡改负向测试
#     --up-only     只做环境准备（编译 + 起 VPP + 配置），不跑断言
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
COMPOSE_FILE="${ROOT}/test/docker/docker-compose.yml"
# 容器内路径（docker-compose 把 ../../test 挂到 /workspace/test）
SETUP_C="/workspace/test/scripts/setup-node.sh"
BUILD_C="/workspace/test/docker/build-plugin.sh"
START_C="/workspace/test/docker/start-node.sh"
INJECT_C="/workspace/test/scripts/inject-sr-packet.py"

C_NODE1=srwg-node1
C_TRANSIT=srwg-transit
C_NODE2=srwg-node2
ALL_CONTAINERS="${C_NODE1} ${C_TRANSIT} ${C_NODE2}"

# node1 的 underlay 地址（与 setup-node.sh 默认值一致）。
# encap 挂在承载该地址的 underlay 出口接口上，不是 wg0。
N1_UNDERLAY=${N1_UNDERLAY:-10.10.1.2}
TRANSIT_A=${TRANSIT_A:-10.10.1.3}
TRANSIT_B=${TRANSIT_B:-10.10.2.3}
N2_UNDERLAY=${N2_UNDERLAY:-10.10.2.2}
VPN_PORT=${VPN_PORT:-51820}
TRANSIT_PORT=${TRANSIT_PORT:-51821}
SR_KEY_HEX=${SR_KEY_HEX:-00112233445566778899aabbccddeeff}

KEEP=0
SKIP_BUILD=0
DO_TAMPER=1
UP_ONLY=0
for a in "$@"; do
    case "$a" in
      --keep)        KEEP=1 ;;
      --skip-build)  SKIP_BUILD=1 ;;
      --no-tamper)   DO_TAMPER=0 ;;
      --up-only)     UP_ONLY=1 ;;
      -h|--help)     awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 0 ;;
      *)             echo "unknown option: $a" >&2; exit 2 ;;
    esac
done

PASS=0
FAIL=0
ok()   { PASS=$((PASS+1)); echo "  [PASS] $*"; }
bad()  { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "== $*"; }

# docker compose 里三个服务的镜像都是 srwg-dev:latest，容器名固定
dx()   { local c=$1; shift; docker exec "${c}" bash -lc "$*"; }
dxd()  { local c=$1; shift; docker exec -d "${c}" bash -lc "$*"; }
setup_node() {
    local role="$1"; local c=$(container_of "${role}"); shift
    docker exec "${c}" bash "${SETUP_C}" "${role}" "$@"
}
container_of() {
    case "$1" in
      node1)   echo "${C_NODE1}" ;;
      transit) echo "${C_TRANSIT}" ;;
      node2)   echo "${C_NODE2}" ;;
      *)       echo "$1" ;;
    esac
}

cleanup() {
    [ "${KEEP}" = "1" ] && { info "--keep: containers left running"; return 0; }
    info "tearing down"
    (cd "${ROOT}/test/docker" && docker compose -f "${COMPOSE_FILE}" down -v --remove-orphans >/dev/null 2>&1)
}

dump_all() {
    for c in ${ALL_CONTAINERS}; do
        echo "################ ${c} ################"
        setup_node "$(role_of "${c}")" dump 2>&1
        echo "---------------- ${c} vpp.log tail ----------------"
        docker exec "${c}" tail -40 /var/log/vpp/vpp.log 2>&1
    done
}

role_of() {
    case "$1" in
      ${C_NODE1})   echo node1 ;;
      ${C_TRANSIT}) echo transit ;;
      ${C_NODE2})   echo node2 ;;
    esac
}

# 读取某节点的某个计数器：counter <container> <Section>_<name>
counter() {
    setup_node "$(role_of "$1")" stats 2>/dev/null |
        awk -F= -v k="$2" '$1==k {print $2; f=1} END{if(!f) print 0}'
}

wait_vppctl() {   # $1=container $2=timeout_sec
    local c=$1 t=${2:-120} i
    for i in $(seq 1 "${t}"); do
        if docker exec "${c}" vppctl show version >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# ==================================================================
info "step 0: docker compose up"
# ==================================================================
cd "${ROOT}/test/docker"
docker compose -f "${COMPOSE_FILE}" up -d --no-build || {
    echo "docker compose up failed" >&2; exit 1;
}
for c in ${ALL_CONTAINERS}; do
    docker inspect -f '{{.State.Running}}' "${c}" 2>/dev/null | grep -q true || {
        echo "container ${c} is not running" >&2; exit 1;
    }
done
echo "  containers: ${ALL_CONTAINERS}"

# NOTE: 不设 trap ERR —— 管道中 grep 无匹配返回 1 会误触发

# ==================================================================
info "step 1: build sr_wg plugin (in ${C_NODE1})"
# ==================================================================
if [ "${SKIP_BUILD}" = "1" ]; then
    echo "  --skip-build: reusing existing build artifacts"
else
    # vppapigen 解析 .api 需要 python 的 ply 模块；srwg-dev 镜像里
    # python3-ply 可能缺失，先按需补装，否则编译会卡在 "Generating API header"
    dx "${C_NODE1}" 'python3 -c "import ply" 2>/dev/null ||
                     pip3 install --quiet ply 2>/dev/null ||
                     (apt-get update -qq && apt-get install -y -qq python3-ply)' \
        >/dev/null 2>&1 || true
    dx "${C_NODE1}" "cd /workspace/vpp-plugin && rm -rf build && bash ${BUILD_C}" \
        > /tmp/srwg-build.log 2>&1 || {
        echo "plugin build failed, last 60 lines:" >&2
        tail -60 /tmp/srwg-build.log >&2
        cleanup; exit 1;
    }
    tail -6 /tmp/srwg-build.log | sed 's/^/  /'
fi

SO=/workspace/vpp-plugin/build/lib/vpp_plugins/sr_wg_plugin.so
dx "${C_NODE1}" "test -f ${SO}" || { echo "plugin .so not found: ${SO}" >&2; cleanup; exit 1; }
echo "  artifact: $(dx "${C_NODE1}" "ls -l ${SO}" | awk '{print $5" bytes"}')"

# ==================================================================
info "step 2: start VPP on all nodes"
# ==================================================================
for c in ${ALL_CONTAINERS}; do
    # VPP 进程名是 vpp_main（不是 vpp），pkill -x vpp 不会命中；遗留实例
    # 会争抢 AF_PACKET 接口与 API socket，导致配置/收包落在不同实例上。
    docker exec "${c}" bash -lc 'pkill -9 -x vpp_main; pkill -9 -f "script -qefc vpp"; sleep 1; rm -f /run/vpp/*.sock' >/dev/null 2>&1
    sleep 1
    dxd "${c}" "bash ${START_C} $(role_of "${c}")"
done
for c in ${ALL_CONTAINERS}; do
    if wait_vppctl "${c}" 120; then
        echo "  ${c}: vppctl ready"
    else
        echo "  ${c}: VPP did not come up" >&2
        docker exec "${c}" tail -40 /var/log/vpp/vpp.log >&2
        cleanup; exit 1
    fi
done

echo
info "ASSERT: sr_wg plugin loaded on every node"
for c in ${ALL_CONTAINERS}; do
    if dx "${c}" 'vppctl show plugins' | grep -q sr_wg_plugin.so; then
        ok "${c}: sr_wg_plugin.so loaded"
    else
        bad "${c}: sr_wg_plugin.so NOT loaded"; dump_all; cleanup; exit 1
    fi
done

echo
info "ASSERT: sr-wg4-encap is registered on the ip4-output arc"
dx "${C_NODE1}" 'vppctl show features' | tr -d '\r' |
    awk '/^ip4-output:/{f=1;next} f&&/^[^ ]/{exit} f&&NF{print}' |
    grep -n . > /tmp/srwg-arc.txt
cat /tmp/srwg-arc.txt | sed 's/^/    /'
ENCN=$(grep -c "sr-wg4-encap" /tmp/srwg-arc.txt || true)
[ "${ENCN}" = "1" ] && ok "sr-wg4-encap present on ip4-output arc" ||
    bad "sr-wg4-encap missing from ip4-output arc"
# 挂点是 underlay 出口接口（wg4-output-tun 不进 feature 链），
# 所以不再断言它排在 wg4-output-tun 之后，改断言启用在正确的接口上（step 3 之后）。

# ==================================================================
info "step 3: configure nodes"
# ==================================================================
for c in ${C_NODE1} ${C_TRANSIT} ${C_NODE2}; do
    echo "  -- $(role_of "${c}"): init"
    setup_node "$(role_of "${c}")" init >&2 || { echo "init failed on ${c}" >&2; dump_all; cleanup; exit 1; }
done

echo "  -- transit: enable SR transit"
setup_node transit transit >&2

# WG 公钥交换（容器内没有 cryptography/nacl，用 VPP 自己 generate-key）
echo "  -- node1: create wg0"
PUB1=$(setup_node node1 wg-create 2>/dev/null | tail -1)
echo "     node1 public-key = ${PUB1}"
echo "  -- node2: create wg0"
PUB2=$(setup_node node2 wg-create 2>/dev/null | tail -1)
echo "     node2 public-key = ${PUB2}"
[ -n "${PUB1}" ] && [ -n "${PUB2}" ] || { echo "failed to read WG public keys" >&2; dump_all; cleanup; exit 1; }

echo "  -- node1: peer + SR path + encap bind"
setup_node node1 peer "${PUB2}" >&2 || { echo "node1 peer failed" >&2; dump_all; cleanup; exit 1; }
echo "  -- node2: peer (no SR path => plain WireGuard direct mode)"
setup_node node2 peer "${PUB1}" nosr >&2 || { echo "node2 peer failed" >&2; dump_all; cleanup; exit 1; }

echo
info "ASSERT: encap feature is enabled on node1's UNDERLAY egress interface"
# 挂点必须在承载 underlay IP 的物理口上：VPP 26.06 的 wg4-output-tun 把
# next 硬编码成 adj-midchain-tx，从不走 feature 链，挂 wg0 收不到任何包。
UVIF=$(dx "${C_NODE1}" 'vppctl show interface addr' | tr -d '\r' |
       awk -v want="${N1_UNDERLAY}" '
           /:[[:space:]]*$/ { n=$1; next }
           index($0, "L3 "want"/") { print n; exit }')
[ -n "${UVIF}" ] && ok "underlay egress interface = ${UVIF}" ||
    bad "cannot find the VPP interface holding ${N1_UNDERLAY}"
FEAT=$(dx "${C_NODE1}" "vppctl show interface features ${UVIF}" | tr -d '\r' |
       awk '/^ip4-output:/{f=1;next} f&&/^[^ ]/{exit} f&&NF{print}')
echo "${FEAT}" | sed 's/^/    /'
echo "${FEAT}" | grep -q "sr-wg4-encap" &&
    ok "sr-wg4-encap enabled on ${UVIF} (ip4-output arc)" ||
    bad "sr-wg4-encap NOT enabled on ${UVIF}"
dx "${C_NODE1}" 'vppctl show sr-wg tunnels' | grep -q ", 1 usable" &&
    ok "binding is usable (endpoint set + encap interface enabled)" ||
    bad "binding is NOT usable: $(dx "${C_NODE1}" 'vppctl show sr-wg tunnels' |
                                  grep -E 'WARNING|usable' | tr '\n' ' ')"

echo
info "ASSERT: node1 bound wg0 to SR path 1"
dx "${C_NODE1}" 'vppctl show sr-wg tunnels' | sed 's/^/    /'
dx "${C_NODE1}" 'vppctl show sr-wg tunnels' | grep -q "path_id=1" &&
    ok "node1 tunnel binding present" || bad "node1 tunnel binding missing"
dx "${C_NODE2}" 'vppctl show sr-wg tunnels' | grep -q "0 tunnel binding" &&
    ok "node2 has no binding (WG direct mode preserved)" ||
    bad "node2 unexpectedly has a binding"

if [ "${UP_ONLY}" = "1" ]; then
    info "--up-only: environment configured, skipping dataplane assertions"
    exit 0
fi

# ==================================================================
info "step 4: generate traffic"
# ==================================================================
for c in ${ALL_CONTAINERS}; do setup_node "$(role_of "${c}")" clear-stats >/dev/null 2>&1; done

# 让 wireguard 有充足时间握手，然后打数据
for round in 1 2 3; do
    echo "  round ${round}: node1 ping 192.168.99.2 (overlay)"
    dx "${C_NODE1}" 'vppctl ping 192.168.99.2 source wg0 repeat 5' 2>&1 |
        tail -3 | sed 's/^/    /'
    sleep 4
done

# 抓一份 trace 作为证据（不影响断言）。
# 必须 trace **af-packet-input**：VPP 只在输入节点给 buffer 打
# VLIB_BUFFER_IS_TRACED，只 `trace add sr-wg4-*` 永远不会有任何包进
# trace buffer（早期版本踩过这个坑，三个 trace 文件全是空的）。
trace_to() {   # trace_to <container> <outfile> <extra-node>
    local c=$1 out=$2 node=$3
    dx "${c}" "vppctl \"clear trace\";
               vppctl \"trace add af-packet-input 60\";
               vppctl \"trace add ${node} 60\"" >/dev/null 2>&1
    dx "${C_NODE1}" 'vppctl ping 192.168.99.2 source wg0 repeat 2 >/dev/null 2>&1'
    sleep 4
    dx "${c}" 'vppctl "show trace max 200"' > "${out}" 2>&1
    dx "${c}" 'vppctl "clear trace"' >/dev/null 2>&1
}
echo "  -- capturing a trace on node1"
trace_to "${C_NODE1}"   /tmp/srwg-trace-node1.txt   sr-wg4-encap
echo "  -- capturing a trace on transit"
trace_to "${C_TRANSIT}" /tmp/srwg-trace-transit.txt sr-wg4-transit
echo "  -- capturing a trace on node2"
trace_to "${C_NODE2}"   /tmp/srwg-trace-node2.txt   sr-wg4-input

# ==================================================================
echo
info "ASSERT: SR dataplane counters"
# ==================================================================
for c in ${ALL_CONTAINERS}; do
    echo "  -- $(role_of "${c}")"
    setup_node "$(role_of "${c}")" stats 2>/dev/null | sed 's/^/     /'
done

E_OK=$(counter "${C_NODE1}"   Encap_ok)
E_BP=$(counter "${C_NODE1}"   Encap_bypass)
E_NP=$(counter "${C_NODE1}"   Encap_no_path)
E_HM=$(counter "${C_NODE1}"   Encap_hmac_fail)
E_NH=$(counter "${C_NODE1}"   Encap_no_headroom)
T_OK=$(counter "${C_TRANSIT}" Transit_ok)
T_HM=$(counter "${C_TRANSIT}" Transit_hmac_fail)
T_SE=$(counter "${C_TRANSIT}" Transit_seg_exhausted)
T_BM=$(counter "${C_TRANSIT}" Transit_bad_magic)
D_OK=$(counter "${C_NODE2}"   Decap_ok)
D_HM=$(counter "${C_NODE2}"   Decap_hmac_fail)
D_SN=$(counter "${C_NODE2}"   Decap_seg_not_zero)
D_BP=$(counter "${C_NODE2}"   Decap_bypass)
DROP=$(counter "${C_NODE1}"   General_drop_total)

echo
[ "${E_OK}" -gt 0 ] && ok "node1   encap_ok=${E_OK} (>0)"        || bad "node1   encap_ok=${E_OK} (expected >0)"
[ "${T_OK}" -gt 0 ] && ok "transit transit_ok=${T_OK} (>0)"      || bad "transit transit_ok=${T_OK} (expected >0)"
[ "${D_OK}" -gt 0 ] && ok "node2   decap_ok=${D_OK} (>0)"        || bad "node2   decap_ok=${D_OK} (expected >0)"

[ "${E_NP}" = "0" ] && ok "node1   encap_no_path=0"              || bad "node1   encap_no_path=${E_NP} (path lookup broken)"
[ "${E_HM}" = "0" ] && ok "node1   encap_hmac_fail=0"            || bad "node1   encap_hmac_fail=${E_HM}"
[ "${E_NH}" = "0" ] && ok "node1   encap_no_headroom=0 (buffer headroom sufficient)" ||
    bad "node1   encap_no_headroom=${E_NH} (sr_len exceeds headroom)"
[ "${T_HM}" = "0" ] && ok "transit transit_hmac_fail=0 (HMAC verified)" ||
    bad "transit transit_hmac_fail=${T_HM} (key mismatch or header corrupted)"
[ "${T_SE}" = "0" ] && ok "transit transit_seg_exhausted=0 (seg_left chain correct)" ||
    bad "transit transit_seg_exhausted=${T_SE} (seg_left already 0 on arrival)"
[ "${T_BM}" = "0" ] && ok "transit transit_bad_magic=0"          || bad "transit transit_bad_magic=${T_BM}"
[ "${D_HM}" = "0" ] && ok "node2   decap_hmac_fail=0"            || bad "node2   decap_hmac_fail=${D_HM}"
[ "${D_SN}" = "0" ] && ok "node2   decap_seg_not_zero=0 (transit popped the last segment)" ||
    bad "node2   decap_seg_not_zero=${D_SN} (transit did not decrement seg_left to 0)"
[ "${DROP}" = "0" ] && ok "node1   drop_total=0"                 || bad "node1   drop_total=${DROP}"

echo
info "ASSERT: sr-wg4-input 的 trace 解码必须等于路径表里的真值（#24）"
# 这条断言盯的是"观测在骗人"这一类故障：转发计数、外层 UDP 端口对、WG 解封后的
# Length: 可以全对，而 trace 里的 hmac_id/path_id/seg_left 是别的字节。这里真的坏过：
# Step 5 用 memmove 把 WG 报文覆盖到 SR 头的位置（那是设计目标，wg4-input 从
# current_data 往回取 UDP 头），而 trace 块在 done: 之后才回读 srh —— 解出来的是 WG
# 报文的小端字段（实测 hmac_id=5120/1024/1280，seg_count=87='W'/101='e'），同一时刻
# 真值是 hmac_id=1 path_id=1 seg_count=2。
# 真值不写死，从 node1 的路径表读：trace 和这张表必须说同一件事。
P_TBL=$(dx "${C_NODE1}" 'vppctl "show sr-wg paths"' 2>/dev/null | head -4)
R_PID=$(echo "${P_TBL}" | sed -n 's/.*path_id=\([0-9][0-9]*\).*/\1/p'  | head -1)
R_HID=$(echo "${P_TBL}" | sed -n 's/.*hmac_id=\([0-9][0-9]*\).*/\1/p'   | head -1)
R_SEG=$(echo "${P_TBL}" | sed -n 's/.*seg_count=\([0-9][0-9]*\).*/\1/p'  | head -1)
TR_HIT=$(grep -c "SR-WG-INPUT: hmac_id=${R_HID} path_id=${R_PID} seg_left=0/${R_SEG} err=0" \
         /tmp/srwg-trace-node2.txt 2>/dev/null)
if [ -z "${R_PID}" ] || [ -z "${R_HID}" ] || [ -z "${R_SEG}" ]; then
    bad "读不到 node1 的路径表（path=${R_PID} hmac=${R_HID} seg=${R_SEG}），这条断言无法判定"
elif [ "${TR_HIT:-0}" -gt 0 ]; then
    ok "node2 trace 解出 hmac_id=${R_HID} path_id=${R_PID} seg_left=0/${R_SEG}，与 show sr-wg paths 一致（${TR_HIT} 行）"
else
    bad "node2 trace 里没有一行等于路径表真值（期望 hmac_id=${R_HID} path_id=${R_PID} seg_left=0/${R_SEG}）；实际解到：$(grep -o 'SR-WG-INPUT:.*' /tmp/srwg-trace-node2.txt 2>/dev/null | sort -u | head -3 | tr '\n' ' ')"
fi

echo
info "ASSERT: encap 挂点表不留僵尸条目（#25）"
# 建一张 loopback、在它上面开 encap、然后删掉这张口。接口删除时插件的
# encap_enabled_ifs 没有任何清理钩子，而 `set sr-wg encap disable <已删的号>` 也会被
# 同一处"接口必须存在"的校验挡回 rv=-2（2026-10-23 在旧插件上实测复现：
# `DELETED (3)` 一直留到 VPP 重启）。后果不是转发，而是 encap_enabled_count 虚高 ——
# 隧道的 is_effective 和 CLI 的 WARNING 都读这个数，于是"看起来开了其实没开"。
# 判据两头都要有：僵尸必须消失，真出口必须还在（防 reconcile 读错 VPP 登记把有效条目一起清掉）。
LB_NAME=$(dx "${C_NODE1}" 'vppctl "create loopback interface"' 2>/dev/null |
          sed -n 's/.*\(loop[0-9][0-9]*\).*/\1/p' | head -1)
if [ -z "${LB_NAME}" ]; then
    bad "建不出 loopback，僵尸条目这条无法判定"
else
    dx "${C_NODE1}" "vppctl 'set sr-wg encap enable ${LB_NAME}'" >/dev/null 2>&1
    dx "${C_NODE1}" "vppctl 'delete loopback interface intfc ${LB_NAME}'" >/dev/null 2>&1
    TUN_OUT=$(dx "${C_NODE1}" 'vppctl "show sr-wg tunnels"' 2>&1)
    if echo "${TUN_OUT}" | grep -q 'DELETED'; then
        bad "删掉接口之后挂点表里还有 DELETED（$(echo "${TUN_OUT}" | grep -c DELETED) 条），encap_enabled_count 虚高"
    else
        ok "接口删除后 ${LB_NAME} 那条已从挂点表里消失（表按 VPP 的真实登记重算过）"
    fi
    if echo "${TUN_OUT}" | grep -q 'host-eth0'; then
        ok "真出口 host-eth0 仍在挂点表里（reconcile 没有误伤有效条目）"
    else
        bad "挂点表里连 host-eth0 都没了，reconcile 判据读错了 VPP 的登记：$(echo "${TUN_OUT}" | head -3 | tr '\n' ' ')"
    fi
fi

echo
info "SOFT: WireGuard direct-mode compatibility (node1 received plain WG from node2)"
if [ "${D_BP}" -gt 0 ] || [ "$(counter "${C_NODE1}" Decap_bypass)" -gt 0 ]; then
    ok "plain-WG packets bypassed sr-wg4-input (decap_bypass node2=${D_BP}, node1=$(counter "${C_NODE1}" Decap_bypass))"
else
    echo "     [INFO] decap_bypass=0 on both ends - node2 sent no plain WG traffic"
    echo "            (only relevant if the reverse direction carries packets)"
fi

echo
info "ASSERT: overlay connectivity (node1 -> 192.168.99.2 over WireGuard)"
PING_OUT=$(dx "${C_NODE1}" 'vppctl ping 192.168.99.2 source wg0 repeat 8' 2>&1)
echo "${PING_OUT}" | tail -3 | sed 's/^/    /'
# VPP 的统计行格式是 "N sent, M received"，不是 "sent N, received M"，
# 原来的匹配串永不命中，把真的通了的说成没通。
# 冷启动时首轮 ping 可能全丢（WireGuard 握手要跑完一个 keepalive 周期），
# 所以最多重试 3 次，任一轮拿到回包即判定隧道可用。
OVERLAY_OK=0
for _try in 1 2 3; do
    if echo "${PING_OUT}" | grep -qE "[0-9]+ sent, [1-9][0-9]* received"; then
        OVERLAY_OK=1; break
    fi
    sleep 4
    PING_OUT=$(dx "${C_NODE1}" 'vppctl ping 192.168.99.2 source wg0 repeat 8' 2>&1)
    echo "${PING_OUT}" | tail -2 | sed 's/^/    /'
done
if [ "${OVERLAY_OK}" = "1" ]; then
    ok "overlay ping got replies through the SR tunnel"
else
    bad "overlay ping got 0 replies - WireGuard session never established"
fi

echo
info "ASSERT: 转发来的内层包不能在收端被 punt（#26，终端到终端）"
# wg4-input 会拿 peer 的 allowed-ip 过滤**解密后的内层源地址**。peer 写对端
# overlay /32 时，隧道自测 ping（源恰好就是那个 /32）正常，而本机转发的终端
# 流量（源是 LAN 地址）在收端被 `error-punt -> null-node` 黑洞掉，`show errors`
# 只涨 null-node blackholed。这就是"终端到终端不通、但隧道自测全绿"的真因
# （2026-10-24 实测：/32 时 3 发 0 收 + punt+3；0.0.0.0/0 时 3 发 3 收、punt 不涨）。
# 这里用一张 tap 造一个非 overlay 源，经 SR 隧道打到 node2；回程走 node2 的
# 普通 WG 直连（它没有 SR path）。
FWD_TAP=tap7
FWD_NET=192.168.50
punt_count() { dx "${C_NODE2}" 'vppctl "show errors"' 2>/dev/null |
               awk '$2=="null-node"{print $1; exit}'; }
PUNT_BEFORE=$(punt_count)
dx "${C_NODE1}" "vppctl 'create tap if-name ${FWD_TAP} host-if-name ${FWD_TAP} \
                  host-ip4-addr ${FWD_NET}.1/24 host-mtu-size 1500'" >/dev/null 2>&1
dx "${C_NODE1}" "vppctl 'set interface state ${FWD_TAP} up'; \
                 vppctl 'set interface mtu packet 1500 ${FWD_TAP}'; \
                 vppctl 'set interface ip address ${FWD_TAP} ${FWD_NET}.2/24'" >/dev/null 2>&1
dx "${C_NODE1}" "ip route add 192.168.99.2/32 via ${FWD_NET}.2 dev ${FWD_TAP}" >/dev/null 2>&1
# 回程路由必须显式下发：allowed-ips 不装 FIB 路由。
dx "${C_NODE2}" "vppctl 'ip route add ${FWD_NET}.0/24 via 192.168.99.1'" >/dev/null 2>&1
FWD_OUT=$(dx "${C_NODE1}" "ping -c 4 -W 2 -M do -s 1272 -I ${FWD_NET}.1 192.168.99.2" 2>&1)
echo "${FWD_OUT}" | tail -2 | sed 's/^/    /'
PUNT_AFTER=$(punt_count)
dx "${C_NODE2}" "vppctl 'ip route del ${FWD_NET}.0/24 via 192.168.99.1'" >/dev/null 2>&1
dx "${C_NODE1}" "ip route del 192.168.99.2/32 via ${FWD_NET}.2 dev ${FWD_TAP}" >/dev/null 2>&1
dx "${C_NODE1}" "vppctl 'delete tap ${FWD_TAP}'" >/dev/null 2>&1

if echo "${FWD_OUT}" | grep -qE "[1-9][0-9]* received"; then
    ok "转发流量穿过隧道拿到回包（内层源 ${FWD_NET}.1，不是 overlay 地址）"
else
    bad "转发流量 0 回包：$(echo "${FWD_OUT}" | tail -3 | tr '\n' ' ')"
fi
if [ "${PUNT_AFTER:-0}" -gt "${PUNT_BEFORE:-0}" ] 2>/dev/null; then
    bad "node2 null-node blackholed ${PUNT_BEFORE} -> ${PUNT_AFTER}：内层源被 allowed-ips 过滤（peer 必须写 0.0.0.0/0）"
else
    ok "node2 null-node blackholed 未增长（${PUNT_BEFORE} -> ${PUNT_AFTER}）"
fi

# ==================================================================
if [ "${DO_TAMPER}" = "1" ]; then
    echo
    info "step 5: HMAC tamper test (node2 switches to a wrong P-Key)"
    dx "${C_NODE2}" 'vppctl sr-wg key set 1 ffffffffffffffffffffffffffffffff'
    for c in ${ALL_CONTAINERS}; do setup_node "$(role_of "${c}")" clear-stats >/dev/null 2>&1; done
    dx "${C_NODE1}" 'vppctl ping 192.168.99.2 source wg0 repeat 5' >/dev/null 2>&1
    sleep 6

    T_HM2=$(counter "${C_TRANSIT}" Transit_hmac_fail)
    D_HM2=$(counter "${C_NODE2}"   Decap_hmac_fail)
    D_OK2=$(counter "${C_NODE2}"   Decap_ok)
    echo "     transit transit_hmac_fail=${T_HM2}, node2 decap_hmac_fail=${D_HM2}, node2 decap_ok=${D_OK2}"
    if [ "${D_HM2}" -gt 0 ] && [ "${D_OK2}" = "0" ]; then
        ok "node2 rejected every SR packet after the key change (decap_hmac_fail=${D_HM2}, decap_ok=0)"
    elif [ "${T_HM2}" -gt 0 ]; then
        ok "transit rejected the SR packets (transit_hmac_fail=${T_HM2})"
    else
        bad "tampered SR packets were not rejected (decap_hmac_fail=${D_HM2}, decap_ok=${D_OK2})"
    fi

    echo "  -- restoring the correct key on node2"
    dx "${C_NODE2}" 'vppctl sr-wg key set 1 00112233445566778899aabbccddeeff'
fi

# ==================================================================
# 原始包注入：不经 WireGuard，直接喂给 sr-wg4-transit。
# 头部格式校验（magic/ver/len）与 transit_enabled 门控只能这样测——
# 真实 WG 流量永远由 encap 生成合法头部，造不出畸形包。
# ==================================================================
echo
info "step 6: SR header format negatives (raw injection -> transit classification)"

KIF=${UVIF#host-}                     # host-eth1 -> eth1（内核侧注入用）
N1_MAC=$(dx "${C_NODE1}" "vppctl show hardware-interfaces ${UVIF}" | tr -d '\r' |
         awk '/Ethernet address/{print $3; exit}')
TRANSIT_MAC=$(dx "${C_NODE1}" "vppctl show ip neighbor" | tr -d '\r' |
              awk -v ip="${TRANSIT_A}" '$0 ~ ip {
                  for (i=1;i<=NF;i++) if ($i ~ /^[0-9a-f][0-9a-f]:/) {print $i; exit}}')
echo "     inject via ${KIF}: src-mac=${N1_MAC:-?} dst-mac=${TRANSIT_MAC:-?}"

inject_transit() {   # inject_transit <corrupt-kind|''> <count>
    local corr=""
    [ -n "$1" ] && corr="--corrupt $1"
    dx "${C_NODE1}" "python3 ${INJECT_C} --send --iface ${KIF} --mode transit \
        --src-ip ${N1_UNDERLAY} --dst-ip ${TRANSIT_A} --dst-port ${TRANSIT_PORT} \
        --key-hex ${SR_KEY_HEX} --hmac-id 1 --path-id 1 \
        --seg-count 2 --seg-left 1 \
        --segments '${N2_UNDERLAY}:${VPN_PORT},${TRANSIT_A}:${TRANSIT_PORT}' \
        --src-mac ${N1_MAC} --dst-mac ${TRANSIT_MAC} \
        --count $2 --delay 0.3 ${corr}" >/dev/null 2>&1
}

if [ -z "${N1_MAC}" ] || [ -z "${TRANSIT_MAC}" ]; then
    bad "cannot resolve injection MACs; skipping header-negative tests"
else
    for kind in magic ver len; do
        setup_node transit clear-stats >/dev/null 2>&1
        inject_transit "${kind}" 4
        sleep 2
        CTR=$(counter "${C_TRANSIT}" "Transit_bad_${kind}")
        DRP=$(counter "${C_TRANSIT}" "General_drop_total")
        if [ "${CTR}" -ge 4 ] 2>/dev/null; then
            ok "corrupt-${kind}: Transit_bad_${kind}=${CTR} (>=4), drop_total=${DRP}"
        else
            bad "corrupt-${kind}: Transit_bad_${kind}=${CTR} (expected >=4)"
        fi
    done

    # ---- transit_enabled 门控：disable 之后必须一个包都不处理 ----
    # 这一步是确定性的：关掉中转后连 WG keepalive 的 SR 包也走同一条
    # error 分支，所以 Transit_ok 必须恒为 0（没有别的来源能把它们计成 ok）。
    info "step 6b: set sr-wg transit disable must stop all transit processing"
    dx "${C_TRANSIT}" 'vppctl set sr-wg transit disable' >/dev/null 2>&1
    setup_node transit clear-stats >/dev/null 2>&1
    inject_transit "" 3
    dx "${C_NODE1}" 'vppctl ping 192.168.99.2 source wg0 repeat 2 >/dev/null 2>&1'
    sleep 3
    T_OK_OFF=$(counter "${C_TRANSIT}" Transit_ok)
    T_DROP_OFF=$(counter "${C_TRANSIT}" General_drop_total)
    ERR_OFF=$(dx "${C_TRANSIT}" 'vppctl show errors' | tr -d '\r' |
              grep -c "sr-wg transit is not enabled")
    if [ "${T_OK_OFF}" = "0" ] && [ "${T_DROP_OFF}" -ge 3 ] 2>/dev/null; then
        ok "transit disabled: Transit_ok=0, drop_total=${T_DROP_OFF}"
    else
        bad "transit disabled but Transit_ok=${T_OK_OFF}, drop_total=${T_DROP_OFF}"
    fi
    if [ "${ERR_OFF}" -ge 1 ] 2>/dev/null; then
        ok "transit disabled: error reason 'sr-wg transit is not enabled' counted ${ERR_OFF}x"
    else
        bad "transit disabled but no 'not enabled' error shown"
    fi

    echo "  -- re-enabling transit"
    dx "${C_TRANSIT}" "vppctl set sr-wg transit enable port ${TRANSIT_PORT}" >/dev/null 2>&1
    dx "${C_TRANSIT}" 'vppctl set sr-wg rebind-ports' >/dev/null 2>&1
    setup_node transit clear-stats >/dev/null 2>&1
    inject_transit "" 3
    sleep 2
    T_OK_ON=$(counter "${C_TRANSIT}" Transit_ok)
    if [ "${T_OK_ON}" -ge 3 ] 2>/dev/null; then
        ok "transit re-enabled: Transit_ok=${T_OK_ON} (>=3)"
    else
        bad "transit re-enabled but Transit_ok=${T_OK_ON}"
    fi
fi

# ==================================================================
echo
info "step 7: 中转绝不改写外层源（IP 与端口）"
# ==================================================================
# (a) 控制面已经没有任何「改源」开关：local-ip 关键字必须被 CLI 拒绝。
LI_OUT=$(dx "${C_TRANSIT}" "vppctl set sr-wg transit enable port ${TRANSIT_PORT} local-ip ${TRANSIT_B}" 2>&1)
if echo "${LI_OUT}" | grep -qiE "unknown input|error|not found"; then
    ok "transit enable 不再接受 local-ip（改源开关已删除）"
else
    bad "transit enable 仍接受 local-ip，外层源可能被改写: ${LI_OUT}"
fi

# (b) 数据面证据：看 transit 自己的 trace —— 转发后的包在 host-*-tx 上
#     外层源仍是 node1 的 underlay IP，目的才改成 node2。
#     为什么不用 node2 侧 tcpdump：docker 网桥对跨网段的帧会做 SNAT
#     （实测 src 变成 10.10.2.1），在线上抓到的包已不是 transit 发出的原样。
trace_to "${C_TRANSIT}" /tmp/srwg-transit-src.txt sr-wg4-transit
if ! grep -q "sr-wg4-transit" /tmp/srwg-transit-src.txt; then
    bad "transit trace 为空（trace add 未生效），无法证明外层源"
else
    PRESERVED=$(grep -c "UDP: ${N1_UNDERLAY} -> ${N2_UNDERLAY}" /tmp/srwg-transit-src.txt)
    REWRITTEN=$(grep -c "UDP: ${TRANSIT_A} -> ${N2_UNDERLAY}\|UDP: ${TRANSIT_B} -> ${N2_UNDERLAY}" \
                 /tmp/srwg-transit-src.txt)
    if [ "${PRESERVED}" -ge 1 ] && [ "${REWRITTEN}" = "0" ]; then
        ok "外层源保持 ${N1_UNDERLAY}（tx 侧 ${PRESERVED} 条），无改源痕迹"
    else
        bad "外层源异常：preserved=${PRESERVED}, rewritten=${REWRITTEN}"
        grep -E "UDP: 10\.10\.|host-eth[0-9]-tx" /tmp/srwg-transit-src.txt | head -8 | sed 's/^/    /'
    fi
fi

# ==================================================================
echo
info "step 8: 密钥轮换不能断隧道（key set 必须刷新路径里的 P-Key 副本）"
# ==================================================================
# 路径表存的是 P-Key 的**副本**，encap 签名走 sr_path_fill_header(cfg) 只读
# 副本。若 `key set` 只更新密钥表，node1 会继续用旧签名而对端已换新密钥，
# 现象就是「一换密钥隧道就断」。这里用真 WG 流量验证副本被同步刷新。
NEW_KEY=aabbccddeeff00112233445566778899
rotate_key() { for c in ${ALL_CONTAINERS}; do dx "${c}" "vppctl sr-wg key set 1 $1" >/dev/null 2>&1; done; }

rotate_key "${NEW_KEY}"
# 换钥瞬间在途的旧签名包会被对端拒掉，先把这段噪声放干净再开始计量
sleep 3
for c in ${ALL_CONTAINERS}; do setup_node "$(role_of "${c}")" clear-stats >/dev/null 2>&1; done
PING8=$(dx "${C_NODE1}" 'vppctl ping 192.168.99.2 source wg0 repeat 5' 2>&1)
sleep 4
R_EH=$(counter "${C_NODE1}"   Encap_hmac_fail)
R_TH=$(counter "${C_TRANSIT}" Transit_hmac_fail)
R_DH=$(counter "${C_NODE2}"   Decap_hmac_fail)
R_DOK=$(counter "${C_NODE2}"  Decap_ok)
echo "     encap_hmac_fail=${R_EH} transit_hmac_fail=${R_TH} decap_hmac_fail=${R_DH} decap_ok=${R_DOK}"
if [ "${R_EH}" = "0" ] && [ "${R_TH}" = "0" ] && [ "${R_DH}" = "0" ] &&
   [ "${R_DOK}" -gt 0 ] 2>/dev/null &&
   echo "${PING8}" | grep -qE "[0-9]+ sent, [1-9][0-9]* received"; then
    ok "轮换密钥后隧道仍通（路径里的 P-Key 副本已刷新）"
else
    bad "密钥轮换破坏了隧道 (encap_hm=${R_EH} transit_hm=${R_TH} decap_hm=${R_DH} decap_ok=${R_DOK})"
    echo "${PING8}" | tail -2 | sed 's/^/    /'
fi

echo "  -- restoring the lab key"
rotate_key "${SR_KEY_HEX}"

# ==================================================================
echo
info "step 9: 外层源观测表（vBOND 地址发现的数据面证据）"
# ==================================================================
# 定案 1：探针不再另开 socket，观测点就是数据面自己 —— 谁收到包，谁才知道
# NAT 把对方的 socket 改成了什么。这一步断言三处采集点各自的行为，尤其是
# verified 的分档：只有过了 SR HMAC 的包才算认证过的外层源。
obs_of() { dx "$1" 'vppctl show sr-wg observed' | tr -d '\r'; }

for c in ${ALL_CONTAINERS}; do
    dx "${c}" 'vppctl clear sr-wg observed' >/dev/null 2>&1
done
OBS_EMPTY=$(obs_of "${C_TRANSIT}" | grep -c "0 observed tuple(s)")
if [ "${OBS_EMPTY}" = "1" ]; then
    ok "clear sr-wg observed 生效（transit 表已空）"
else
    bad "clear 后 transit 观测表非空: $(obs_of "${C_TRANSIT}" | tail -2 | tr '\n' ' ')"
fi

# 造一轮真实流量：node1 -> (SR/WG) -> transit -> node2，回程 node2 直发 WG
dx "${C_NODE1}" 'vppctl ping 192.168.99.2 source wg0 repeat 5' >/dev/null 2>&1
sleep 4

OBS_T=$(obs_of "${C_TRANSIT}")
OBS_N2=$(obs_of "${C_NODE2}")
OBS_N1=$(obs_of "${C_NODE1}")
echo "${OBS_T}" | sed 's/^/     transit: /'
echo "${OBS_N2}" | sed 's/^/     node2:   /'
echo "${OBS_N1}" | sed 's/^/     node1:   /'

# (a) transit 必须看到 node1 的真实外层源，且落在 transit_port 上、verified=1
if echo "${OBS_T}" | grep -qE "^ *${N1_UNDERLAY}:[0-9]+ -> :${TRANSIT_PORT} .*verified=1$"; then
    ok "transit 观测到 ${N1_UNDERLAY} 打在 transit_port=${TRANSIT_PORT}，verified=1（过了 SR HMAC）"
else
    bad "transit 未观测到 ${N1_UNDERLAY}:${TRANSIT_PORT} 的已认证条目"
fi

# (b) 终点 CPE 也要看到打在自己 vpn_port 上的已认证条目。
#     不断言具体源 IP：docker 网桥对跨网段帧做 SNAT（见 step 7 同款陷阱），
#     node2 实际看到的 src 是 net_b 的网关而不是 10.10.1.2，这是 lab 的现象，
#     不是数据面的错。观测本来就是"我看到的源"，lab 里就得按看到的记。
if echo "${OBS_N2}" | grep -qE " -> :${VPN_PORT} .*verified=1$"; then
    ok "node2 观测到 vpn_port=${VPN_PORT} 上的已认证外层源（解封装成功路径）"
else
    bad "node2 观测表里没有 verified=1 的 vpn_port 条目"
fi

# (c) node1 收到的是 node2 的普通 WG 直发包（无 SR 头）=> 必须有 verified=0
if echo "${OBS_N1}" | grep -qE " -> :${VPN_PORT} .*verified=0$"; then
    ok "node1 把旁路的普通 WG 包记成 verified=0（外层源未认证，只能当候选映射）"
else
    bad "node1 未记录 verified=0 的旁路观测（反向 WG 直连流量没进观测表）"
fi

# (d) 正常规模下不该互相淘汰：本 lab 每台只有一个 peer
OBS_EV=$(echo "${OBS_T}" | awk -F'evictions=' '/evictions=/{print $2+0; exit}')
if [ "${OBS_EV}" = "0" ]; then
    ok "transit evictions=0（单 peer 场景下没有槽冲突）"
else
    bad "transit evictions=${OBS_EV}（正常规模就淘汰，哈希或表长有问题）"
fi

# (e) hits/age 可用：hits 至少随 keepalive/数据增长，age 是个位数秒
OBS_HITS=$(echo "${OBS_T}" | grep -oE "hits=[0-9]+" | head -1 | cut -d= -f2)
OBS_AGE=$(echo "${OBS_T}" | grep -oE "age=[0-9]+ s" | head -1 | cut -d= -f2 | cut -d' ' -f1)
if [ -n "${OBS_HITS}" ] && [ "${OBS_HITS}" -gt 1 ] 2>/dev/null &&
   [ -n "${OBS_AGE}" ] && [ "${OBS_AGE}" -lt 60 ] 2>/dev/null; then
    ok "观测条目带 hits=${OBS_HITS} / age=${OBS_AGE}s（可用作映射新鲜度）"
else
    bad "观测条目计数异常: hits=${OBS_HITS} age=${OBS_AGE}"
fi

# (f) 清空后重新观测：证明表不是只写不删的死结构
dx "${C_NODE1}" 'vppctl clear sr-wg observed' >/dev/null 2>&1
if obs_of "${C_NODE1}" | grep -q "0 observed tuple(s)"; then
    ok "clear 后 node1 表空"
else
    bad "clear 后 node1 仍有残留条目"
fi
dx "${C_NODE1}" 'vppctl ping 192.168.99.2 source wg0 repeat 3' >/dev/null 2>&1
sleep 3
if obs_of "${C_NODE1}" | grep -qE "^ *[0-9.]+:[0-9]+ -> :${VPN_PORT}"; then
    ok "清空后继续观测（反向 WG 包重新入表）"
else
    bad "清空后没有重新观测到任何 vpn_port 条目"
fi

# ==================================================================
echo
echo "=========================================================="
echo " traces: /tmp/srwg-trace-node1.txt /tmp/srwg-trace-transit.txt /tmp/srwg-trace-node2.txt"
echo "         /tmp/srwg-transit-src.txt (外层源保持的证据)"
echo " build log: /tmp/srwg-build.log"
echo " RESULT: ${PASS} passed, ${FAIL} failed"
echo "=========================================================="

if [ "${FAIL}" != "0" ]; then
    echo
    info "dumping diagnostics (FAIL)"
    dump_all
fi

cleanup

[ "${FAIL}" = "0" ]
