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

C_NODE1=srwg-node1
C_TRANSIT=srwg-transit
C_NODE2=srwg-node2
ALL_CONTAINERS="${C_NODE1} ${C_TRANSIT} ${C_NODE2}"

# node1 的 underlay 地址（与 setup-node.sh 默认值一致）。
# encap 挂在承载该地址的 underlay 出口接口上，不是 wg0。
N1_UNDERLAY=${N1_UNDERLAY:-10.10.1.2}

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
      -h|--help)     sed -n '2,40p' "$0"; exit 0 ;;
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

# 抓一份 trace 作为证据（不影响断言）
echo "  -- capturing a trace on node1"
dx "${C_NODE1}" 'vppctl clear trace; vppctl trace add sr-wg4-encap 16;
                 vppctl ping 192.168.99.2 source wg0 repeat 3 >/dev/null 2>&1;
                 sleep 1; vppctl show trace max 3' > /tmp/srwg-trace-node1.txt 2>&1
echo "  -- capturing a trace on transit"
dx "${C_TRANSIT}" 'vppctl clear trace; vppctl trace add sr-wg4-transit 16;
                   sleep 6; vppctl show trace max 3' > /tmp/srwg-trace-transit.txt 2>&1
echo "  -- capturing a trace on node2"
dx "${C_NODE2}" 'vppctl clear trace; vppctl trace add sr-wg4-input 16;
                 sleep 6; vppctl show trace max 3' > /tmp/srwg-trace-node2.txt 2>&1

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
echo
echo "=========================================================="
echo " traces: /tmp/srwg-trace-node1.txt /tmp/srwg-trace-transit.txt /tmp/srwg-trace-node2.txt"
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
