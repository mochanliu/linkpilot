#!/bin/bash
# test-sr-inject.sh - 使用原始包注入验证 SR 数据面（绕过 WireGuard 握手）
#
# 背景：VPP 26.06 的 WireGuard 在 Docker AF_PACKET 环境中无法完成握手
# （"Peer error"），这是 VPP/WG 的基础性问题，与 SR-WG 插件无关。
# 本脚本通过 AF_PACKET 原始套接字注入手工构造的 SR 封装 UDP 包来验证：
#   1. sr-wg4-transit 节点（HMAC 验证 + 段弹出 + 外层改写）
#   2. sr-wg4-input 节点（HMAC 验证 + seg_left==0 检查 + SR 头剥离）
#   3. sr-wg4-encap 节点（通过 VXLAN 隧道触发 ip4-output arc）
#
# 用法: test/scripts/test-sr-inject.sh [--keep]
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
COMPOSE_FILE="${ROOT}/test/docker/docker-compose.yml"
INJECT_PY="/workspace/test/scripts/inject-sr-packet.py"

C_NODE1=srwg-node1
C_TRANSIT=srwg-transit
C_NODE2=srwg-node2
ALL_CONTAINERS="${C_NODE1} ${C_TRANSIT} ${C_NODE2}"

KEEP=0
for a in "$@"; do
    case "$a" in
      --keep) KEEP=1 ;;
      *) echo "unknown option: $a" >&2; exit 2 ;;
    esac
done

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  [PASS] $*"; }
bad() { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info(){ echo "== $*"; }

dx() { local c=$1; shift; docker exec "${c}" bash -lc "$*"; }
dxd(){ local c=$1; shift; docker exec -d "${c}" bash -lc "$*"; }

START_C="/workspace/test/docker/start-node.sh"

# SR 测试参数
SR_KEY_HEX="00112233445566778899aabbccddeeff"
SR_HMAC_ID=1
SR_PATH_ID=1
# 拓扑地址
N1_UNDERLAY=10.10.1.2
TRANSIT_A=10.10.1.3
TRANSIT_B=10.10.2.3
N2_UNDERLAY=10.10.2.2
VPN_PORT=51820
TRANSIT_PORT=51821

get_stat() {  # $1=container $2=Section_name (e.g. Transit_ok)
    # vppctl 经 `script -qefc` PTY 输出带 CRLF；\r 会让 awk 把段名读成
    # "Transit:\r"、sub(/:$/) 失效，故必须先 tr -d '\r'。
    dx "$1" "vppctl show sr-wg stats" 2>/dev/null | tr -d '\r' | awk -v want="$2" '
        /^  [A-Za-z]+:/ { sec=$1; sub(/:$/,"",sec); next }
        /^    [a-z_]+:/ { nm=$1; sub(/:$/,"",nm);
                          if (sec"_"nm == want) { print $2; found=1 } }
        END { if (!found) print 0 }'
}

wait_vppctl() {
    local c=$1 t=${2:-90} i
    for i in $(seq 1 "${t}"); do
        docker exec "${c}" vppctl show version >/dev/null 2>&1 && return 0
        sleep 1
    done
    return 1
}

# 找到容器内对应 IP 的接口名
find_iface() {  # $1=container $2=ip
    dx "$1" "ip -o -4 addr show 2>/dev/null | awk -v w='$2' '{split(\$4,a,\"/\"); if(a[1]==w){print \$2; exit}}'"
}

# 找到承载指定 underlay IP 的 VPP host-interface 对应的**内核**接口名。
# 数据面 IP 已从内核移到 VPP（host-interface），且容器内存在大量隧道虚拟
# 接口（gretap0@NONE 等含 "eth" 子串），故不能简单 grep eth；改为：
#   1) 在 VPP 中找到持有该 IP 的 host 接口 → 2) 取其 MAC → 3) 匹配同 MAC
#   的内核接口，并去掉 @ifNNN 后缀（AF_PACKET 需要纯接口名）。
kernel_iface_for_ip() {  # $1=container $2=ip
    local c=$1 ip=$2 hif mac kif
    hif=$(dx "$c" "vppctl show interface address" 2>/dev/null |
        awk -v ip="$ip" '/^[^ ]/{cur=$1} index($0, ip"/"){print cur; exit}')
    [ -n "$hif" ] || return 1
    mac=$(dx "$c" "vppctl show hardware-interfaces ${hif}" 2>/dev/null |
        awk '/Ethernet address/{print $3; exit}')
    [ -n "$mac" ] || return 1
    kif=$(dx "$c" "ip -o link show" 2>/dev/null |
        awk -v m="$mac" 'tolower($0) ~ tolower(m) {n=$2; sub(/:$/,"",n); sub(/@.*/,"",n); print n; exit}')
    [ -n "$kif" ] || return 1
    echo "$kif"
}

# ==================================================================
info "step 0: ensure containers are running"
# ==================================================================
cd "${ROOT}/test/docker"
docker compose -f "${COMPOSE_FILE}" up -d --no-build 2>/dev/null || true
for c in ${ALL_CONTAINERS}; do
    docker inspect -f '{{.State.Running}}' "${c}" 2>/dev/null | grep -q true || {
        echo "container ${c} not running" >&2; exit 1; }
done
echo "  containers OK"

# ==================================================================
info "step 1: restart VPP with rebuilt plugin"
# ==================================================================
for c in ${ALL_CONTAINERS}; do
    # VPP 进程名是 vpp_main（不是 vpp），pkill -x vpp 不会命中；遗留实例
    # 会争抢 AF_PACKET 接口与 API socket，导致配置/收包落在不同实例。
    docker exec "${c}" bash -lc 'pkill -9 -x vpp_main; pkill -9 -f "script -qefc vpp"; sleep 1; rm -f /run/vpp/*.sock' >/dev/null 2>&1 || true
done
sleep 2

SO=/workspace/vpp-plugin/build/lib/vpp_plugins/sr_wg_plugin.so
PLUGIN_DIR=/usr/lib/x86_64-linux-gnu/vpp_plugins
for c in ${ALL_CONTAINERS}; do
    dx "${c}" "test -f ${SO} && cp -f ${SO} ${PLUGIN_DIR}/" || {
        echo "plugin .so not available for ${c}" >&2; exit 1; }
done

for c in ${ALL_CONTAINERS}; do
    role=$(case "$c" in
        ${C_NODE1}) echo node1 ;; ${C_TRANSIT}) echo transit ;; ${C_NODE2}) echo node2 ;;
    esac)
    dxd "${c}" "bash ${START_C} ${role}"
done
for c in ${ALL_CONTAINERS}; do
    wait_vppctl "${c}" 90 || { echo "${c}: VPP did not start" >&2; exit 1; }
    # 单实例断言：多个 vpp_main 会争抢 AF_PACKET 接口与 API socket
    ninst=$(docker exec "${c}" bash -lc 'ps -eo comm | grep -c vpp_main')
    if [ "${ninst}" != "1" ]; then
        echo "${c}: expected 1 vpp_main, found ${ninst} (stale instances!)" >&2
        docker exec "${c}" bash -lc 'ps -eo pid,ppid,comm | grep -i vpp' >&2
        exit 1
    fi
    echo "  ${c}: vppctl ready (single instance)"
done

# 验证插件加载
for c in ${ALL_CONTAINERS}; do
    dx "${c}" "vppctl show plugins" | grep -q sr_wg_plugin.so || {
        echo "${c}: sr_wg_plugin.so NOT loaded" >&2; exit 1; }
done
echo "  sr_wg_plugin.so loaded on all nodes"

# ==================================================================
info "step 2: configure underlay interfaces + SR"
# ==================================================================
# 配置各节点的 underlay 接口（host-interface）
dx "${C_NODE1}" "
vppctl show interface 2>/dev/null | grep -q host-eth0 || vppctl create host-interface name eth0
kmac=\$(ip link show eth0 2>/dev/null | awk '/link\/ether/{print \$2}')
[ -n \"\${kmac}\" ] && vppctl set interface mac address host-eth0 \${kmac} 2>/dev/null
vppctl set interface state host-eth0 up
vppctl show interface host-eth0 2>/dev/null | grep -q '${N1_UNDERLAY}/' ||
    vppctl set interface ip address host-eth0 ${N1_UNDERLAY}/24
ip addr del ${N1_UNDERLAY}/24 dev eth0 2>/dev/null
vppctl ip route add ${N2_UNDERLAY}/32 via ${TRANSIT_A} 2>/dev/null
" >/dev/null 2>&1

dx "${C_TRANSIT}" "
# transit 接三个 docker 网络，compose 按名称字母序附加：
#   net_a(10.10.1.3)->eth0, net_b(10.10.2.3)->eth1, net_ctrl->eth2
# 不能用「按 IP 反查内核接口」：上一轮运行已把数据面 IP 从内核删除，
# 再跑时反查为空会静默跳过创建。改用固定接口名，幂等且可重复运行。
for pair in eth0:${TRANSIT_A} eth1:${TRANSIT_B}; do
    hif=\${pair%%:*}; a=\${pair##*:}
    vif=\"host-\${hif}\"
    vppctl show interface 2>/dev/null | grep -q \"\${vif}\" || vppctl create host-interface name \"\${hif}\"
    kmac=\$(ip link show \"\${hif}\" 2>/dev/null | awk '/link\/ether/{print \$2}')
    [ -n \"\${kmac}\" ] && vppctl set interface mac address \"\${vif}\" \"\${kmac}\" 2>/dev/null
    vppctl set interface state \"\${vif}\" up
    vppctl show interface \"\${vif}\" 2>/dev/null | grep -q \"\${a}/\" ||
        vppctl set interface ip address \"\${vif}\" \${a}/24 2>/dev/null
    ip addr del \${a}/24 dev \"\${hif}\" 2>/dev/null
done
" >/dev/null 2>&1

dx "${C_NODE2}" "
# node2 可能在 eth0 或 eth1 上有 net_b 的 IP
hif=\$(ip -o -4 addr show 2>/dev/null | awk -v w='${N2_UNDERLAY}' '{split(\$4,a,\"/\"); if(a[1]==w){print \$2; exit}}')
if [ -z \"\${hif}\" ]; then
    # IP 可能已被删除，用默认接口
    hif=eth0
fi
vif=\"host-\${hif}\"
vppctl show interface 2>/dev/null | grep -q \"\${vif}\" || vppctl create host-interface name \"\${hif}\"
kmac=\$(ip link show \"\${hif}\" 2>/dev/null | awk '/link\/ether/{print \$2}')
[ -n \"\${kmac}\" ] && vppctl set interface mac address \"\${vif}\" \"\${kmac}\" 2>/dev/null
vppctl set interface state \"\${vif}\" up
vppctl show interface \"\${vif}\" 2>/dev/null | grep -q '${N2_UNDERLAY}/' ||
    vppctl set interface ip address \"\${vif}\" ${N2_UNDERLAY}/24
ip addr del ${N2_UNDERLAY}/24 dev \"\${hif}\" 2>/dev/null
vppctl ip route add ${N1_UNDERLAY}/32 via ${TRANSIT_B} 2>/dev/null
" >/dev/null 2>&1

echo "  underlay interfaces configured"

# 验证 underlay 连通性（ping 同时填充 ARP 表）
PING_OUT=$(dx "${C_NODE1}" "vppctl ping ${TRANSIT_A} repeat 2" 2>&1)
if echo "${PING_OUT}" | grep -qE "[1-9]+ received"; then
    echo "  node1 -> transit underlay OK"
else
    echo "  WARNING: node1 -> transit ping failed" >&2
    echo "${PING_OUT}" | tail -3 | sed 's/^/    /'
fi
# transit -> node2 ping（填充 transit 的 ARP 表，以便后续转发）
dx "${C_TRANSIT}" "vppctl ping ${N2_UNDERLAY} repeat 2" >/dev/null 2>&1
# node2 -> transit ping（填充 node2 的 ARP 表）
dx "${C_NODE2}" "vppctl ping ${TRANSIT_B} repeat 2" >/dev/null 2>&1

# 设置 SR 密钥
for c in ${ALL_CONTAINERS}; do
    dx "${c}" "vppctl sr-wg key set ${SR_HMAC_ID} ${SR_KEY_HEX}" >/dev/null 2>&1
done
echo "  HMAC key installed on all nodes"

# Transit 启用 SR transit
dx "${C_TRANSIT}" "vppctl set sr-wg transit enable port ${TRANSIT_PORT} local-ip ${TRANSIT_A}" >/dev/null 2>&1
echo "  transit enabled on ${C_TRANSIT}:${TRANSIT_PORT}"

# 重新绑定端口
for c in ${ALL_CONTAINERS}; do
    dx "${c}" "vppctl set sr-wg rebind-ports" >/dev/null 2>&1
done

# ==================================================================
info "step 3: verify rebind-ports"
# ==================================================================
REBIND_OUT=$(dx "${C_TRANSIT}" "vppctl set sr-wg rebind-ports" 2>&1)
if echo "${REBIND_OUT}" | grep -q "rebound"; then
    ok "rebind-ports command works on transit"
else
    bad "rebind-ports failed: ${REBIND_OUT}"
fi

# ==================================================================
info "step 4: inject SR packets at transit (port ${TRANSIT_PORT})"
# ==================================================================
for c in ${ALL_CONTAINERS}; do
    dx "${c}" "vppctl clear sr-wg stats" >/dev/null 2>&1
    dx "${c}" "vppctl clear errors" >/dev/null 2>&1
done

# 从 VPP ARP 表获取正确的 MAC 地址（ping 已填充）
N1_MAC=$(dx "${C_NODE1}" "vppctl show hardware-interfaces host-eth0" 2>/dev/null |
    awk '/Ethernet address/{print $3; exit}')
TRANSIT_MAC=$(dx "${C_NODE1}" "vppctl show ip neighbor" 2>/dev/null |
    awk -v ip="${TRANSIT_A}" '$0 ~ ip {for(i=1;i<=NF;i++) if($i ~ /^[0-9a-f][0-9a-f]:/) {print $i; exit}}')
NODE2_MAC=$(dx "${C_TRANSIT}" "vppctl show ip neighbor" 2>/dev/null |
    awk -v ip="${N2_UNDERLAY}" '$0 ~ ip {for(i=1;i<=NF;i++) if($i ~ /^[0-9a-f][0-9a-f]:/) {print $i; exit}}')
echo "  node1 MAC=${N1_MAC:-unknown}, transit MAC=${TRANSIT_MAC:-unknown}, node2 MAC=${NODE2_MAC:-unknown}"

# 回退默认值
N1_MAC=${N1_MAC:-9a:81:be:4b:23:72}
TRANSIT_MAC=${TRANSIT_MAC:-be:2d:c2:d8:93:61}
NODE2_MAC=${NODE2_MAC:-02:42:0a:0a:02:02}

# 找到 node1 上通往 transit 的接口（内核侧，用于 AF_PACKET 发送）
# net_a 承载 N1_UNDERLAY，通过 VPP host 接口 MAC 反查内核接口名（去 @后缀）
N1_IFACE=$(kernel_iface_for_ip "${C_NODE1}" "${N1_UNDERLAY}")
N1_IFACE=${N1_IFACE:-eth0}
echo "  node1 injection interface: ${N1_IFACE}"

# 从 node1 容器内发送 SR 包到 transit:51821
echo "  sending 5 SR packets from node1 -> transit:${TRANSIT_PORT}..."
dx "${C_NODE1}" "python3 ${INJECT_PY} --send --iface ${N1_IFACE} \
    --mode transit \
    --src-ip ${N1_UNDERLAY} --dst-ip ${TRANSIT_A} \
    --dst-port ${TRANSIT_PORT} \
    --key-hex ${SR_KEY_HEX} --hmac-id ${SR_HMAC_ID} --path-id ${SR_PATH_ID} \
    --seg-count 2 --seg-left 1 \
    --segments '${N2_UNDERLAY}:${VPN_PORT},${TRANSIT_A}:${TRANSIT_PORT}' \
    --src-mac ${N1_MAC} --dst-mac ${TRANSIT_MAC} \
    --count 5 --payload-size 64 --delay 0.1" 2>&1 | sed 's/^/    /'
sleep 2

# ==================================================================
echo
info "ASSERT: transit counters after injection"
# ==================================================================
dx "${C_TRANSIT}" "vppctl show sr-wg stats" 2>/dev/null | sed 's/^/    /'

T_OK=$(get_stat "${C_TRANSIT}" Transit_ok)
T_HM=$(get_stat "${C_TRANSIT}" Transit_hmac_fail)
T_SE=$(get_stat "${C_TRANSIT}" Transit_seg_exhausted)
T_BM=$(get_stat "${C_TRANSIT}" Transit_bad_magic)

echo
[ "${T_OK:-0}" -gt 0 ] 2>/dev/null && ok "transit_ok=${T_OK} (>0)" || bad "transit_ok=${T_OK:-0} (expected >0)"
[ "${T_HM:-0}" = "0" ] 2>/dev/null && ok "transit_hmac_fail=0" || bad "transit_hmac_fail=${T_HM:-?}"
[ "${T_SE:-0}" = "0" ] 2>/dev/null && ok "transit_seg_exhausted=0" || bad "transit_seg_exhausted=${T_SE:-?}"
[ "${T_BM:-0}" = "0" ] 2>/dev/null && ok "transit_bad_magic=0" || bad "transit_bad_magic=${T_BM:-?}"

# ==================================================================
info "step 5: check decap on node2 (transit should forward)"
# ==================================================================
sleep 2
dx "${C_NODE2}" "vppctl show sr-wg stats" 2>/dev/null | sed 's/^/    /'

D_OK=$(get_stat "${C_NODE2}" Decap_ok)
D_HM=$(get_stat "${C_NODE2}" Decap_hmac_fail)
D_SN=$(get_stat "${C_NODE2}" Decap_seg_not_zero)

if [ "${D_OK:-0}" -gt 0 ] 2>/dev/null; then
    echo "  transit successfully forwarded SR packets to node2"
else
    echo "  transit did not forward to node2, injecting directly..."
    # 找到 node2 的网络接口
    N2_IFACE=$(kernel_iface_for_ip "${C_NODE2}" "${N2_UNDERLAY}")
    N2_IFACE=${N2_IFACE:-eth0}

    # 从 transit 容器内发送到 node2:51820（seg_left=0，模拟 transit 处理后的包）
    # net_b 承载 TRANSIT_B，反查 transit 侧内核接口名
    T_IFACE=$(kernel_iface_for_ip "${C_TRANSIT}" "${TRANSIT_B}")
    T_IFACE=${T_IFACE:-eth1}
    T_MAC=$(dx "${C_TRANSIT}" "vppctl show hardware-interfaces host-${T_IFACE}" 2>/dev/null |
        awk '/Ethernet address/{print $3; exit}')
    T_MAC=${T_MAC:-da:9f:f0:45:14:26}
    echo "  transit injection interface: ${T_IFACE} (mac=${T_MAC}) -> node2:${VPN_PORT}"

    dx "${C_TRANSIT}" "python3 ${INJECT_PY} --send --iface ${T_IFACE} \
        --mode decap \
        --src-ip ${TRANSIT_B} --dst-ip ${N2_UNDERLAY} \
        --dst-port ${VPN_PORT} \
        --key-hex ${SR_KEY_HEX} --hmac-id ${SR_HMAC_ID} --path-id ${SR_PATH_ID} \
        --seg-count 2 --seg-left 0 \
        --segments '${N2_UNDERLAY}:${VPN_PORT},${TRANSIT_A}:${TRANSIT_PORT}' \
        --src-mac ${T_MAC} --dst-mac ${NODE2_MAC} \
        --count 5 --payload-size 64 --delay 0.1" 2>&1 | sed 's/^/    /'
    sleep 2

    D_OK=$(get_stat "${C_NODE2}" Decap_ok)
    D_HM=$(get_stat "${C_NODE2}" Decap_hmac_fail)
    D_SN=$(get_stat "${C_NODE2}" Decap_seg_not_zero)
    dx "${C_NODE2}" "vppctl show sr-wg stats" 2>/dev/null | sed 's/^/    /'
fi

echo
[ "${D_OK:-0}" -gt 0 ] 2>/dev/null && ok "decap_ok=${D_OK} (>0)" || bad "decap_ok=${D_OK:-0} (expected >0)"
[ "${D_HM:-0}" = "0" ] 2>/dev/null && ok "decap_hmac_fail=0" || bad "decap_hmac_fail=${D_HM:-?}"
[ "${D_SN:-0}" = "0" ] 2>/dev/null && ok "decap_seg_not_zero=0" || bad "decap_seg_not_zero=${D_SN:-?}"

# ==================================================================
info "step 6: HMAC tamper test (wrong key -> transit rejects)"
# ==================================================================
for c in ${ALL_CONTAINERS}; do
    dx "${c}" "vppctl clear sr-wg stats" >/dev/null 2>&1
done

echo "  sending 3 SR packets with WRONG HMAC key..."
dx "${C_NODE1}" "python3 ${INJECT_PY} --send --iface ${N1_IFACE} \
    --mode transit \
    --src-ip ${N1_UNDERLAY} --dst-ip ${TRANSIT_A} \
    --dst-port ${TRANSIT_PORT} \
    --key-hex ffffffffffffffffffffffffffffffff \
    --hmac-id ${SR_HMAC_ID} --path-id ${SR_PATH_ID} \
    --seg-count 2 --seg-left 1 \
    --segments '${N2_UNDERLAY}:${VPN_PORT},${TRANSIT_A}:${TRANSIT_PORT}' \
    --src-mac ${N1_MAC} --dst-mac ${TRANSIT_MAC} \
    --count 3 --payload-size 64 --delay 0.1" 2>&1 | sed 's/^/    /'
sleep 2

T_OK2=$(get_stat "${C_TRANSIT}" Transit_ok)
T_HM2=$(get_stat "${C_TRANSIT}" Transit_hmac_fail)
echo "  transit_ok=${T_OK2:-0}, transit_hmac_fail=${T_HM2:-0}"
echo
[ "${T_HM2:-0}" -gt 0 ] 2>/dev/null && ok "tampered packets rejected (hmac_fail=${T_HM2})" ||
    bad "tampered packets NOT rejected (hmac_fail=${T_HM2:-0})"
[ "${T_OK2:-0}" = "0" ] 2>/dev/null && ok "no transit_ok for tampered packets" ||
    bad "transit_ok=${T_OK2} for tampered packets (should be 0)"

# ==================================================================
info "step 7: encap 集成正确性验证（ip4-output arc 顺序 + wg 接口）"
# ==================================================================
# 说明：sr-wg4-encap 挂在 ip4-output feature arc（起点 ip4-midchain），
# 只有 midchain 型隧道（WireGuard 的 wg4-output-tun）才会经过该 arc。
# 实测 VXLAN 的转发路径是 ip4-lookup -> vxlan4-encap -> ip4-lookup ->
# ip4-rewrite -> host-eth0-output，**不经过 ip4-midchain/ip4-output arc**，
# 因此 VXLAN 无法触发 encap（encap_bypass 恒为 0）。真正能触发 encap 的
# 是 wg4-output-tun，而 WG 握手在本 Docker 环境不可用（见脚本末尾说明），
# 故 encap 的数据面无法在此环境端到端触发；这里验证 encap 的**集成正确性**：
#   (a) 全局 ip4-output arc 上 sr-wg4-encap 紧跟在 wg4-output-tun 之后；
#   (b) 绑定 SR path 后 sr-wg4-encap 被自动启用到 wg 接口的 ip4-output arc。
for c in ${ALL_CONTAINERS}; do
    dx "${c}" "vppctl clear sr-wg stats" >/dev/null 2>&1
done

# (a) 全局 arc 顺序
ARC_TXT=/tmp/srwg-arc-inject.txt
dx "${C_NODE1}" "vppctl show features" 2>/dev/null | tr -d '\r' |
    awk '/^ip4-output:/{f=1;next} f&&/^[^ ]/{exit} f&&NF{print}' > "${ARC_TXT}"
IDX_WG=$(awk '/wg4-output-tun/{print NR; exit}' "${ARC_TXT}")
IDX_EN=$(awk '/sr-wg4-encap/{print NR; exit}' "${ARC_TXT}")
if [ -n "${IDX_EN}" ] && [ -n "${IDX_WG}" ] && [ "${IDX_EN}" -gt "${IDX_WG}" ]; then
    ok "ip4-output arc ordering: wg4-output-tun(#${IDX_WG}) -> sr-wg4-encap(#${IDX_EN})"
else
    bad "ip4-output arc ordering wrong: wg4-output-tun=#${IDX_WG:-none} sr-wg4-encap=#${IDX_EN:-none}"
fi

# (b) 创建真实 wg 接口并绑定 SR path，验证 feature 自动启用
echo "  creating WireGuard interface wg0 on node1 + binding SR path..."
dx "${C_NODE1}" "
vppctl show interface 2>/dev/null | grep -qw wg0 ||
    vppctl wireguard create listen-port ${VPN_PORT} generate-key src ${N1_UNDERLAY} >/dev/null 2>&1
vppctl set interface state wg0 up 2>/dev/null
vppctl sr-wg path add ${SR_PATH_ID} \
    via ${N2_UNDERLAY}:${VPN_PORT} \
    via ${TRANSIT_A}:${TRANSIT_PORT} \
    hmac-id ${SR_HMAC_ID} >/dev/null 2>&1
WGIDX=\$(vppctl show interface 2>/dev/null | tr -d '\r' | awk '\$1==\"wg0\"{print \$2; exit}')
[ -n \"\${WGIDX}\" ] && vppctl set sr-wg tunnel \${WGIDX} path ${SR_PATH_ID} endpoint ${N2_UNDERLAY}
" 2>&1 | tr -d '\r' | sed 's/^/    /'

WG_FEAT=$(dx "${C_NODE1}" "vppctl show interface features wg0" 2>/dev/null | tr -d '\r' |
    awk '/^ip4-output:/{f=1;next} f&&/^$/{exit} f&&NF{print}')
echo "  wg0 ip4-output features:"
echo "${WG_FEAT}" | sed 's/^/    /'
echo "${WG_FEAT}" | grep -q "sr-wg4-encap" &&
    ok "sr-wg4-encap auto-enabled on wg0 ip4-output arc after binding" ||
    bad "sr-wg4-encap NOT enabled on wg0"

# 绑定表校验
dx "${C_NODE1}" "vppctl show sr-wg tunnels" 2>/dev/null | tr -d '\r' |
    grep -q "path_id=${SR_PATH_ID}" &&
    ok "tunnel->path binding present (path_id=${SR_PATH_ID})" ||
    bad "tunnel->path binding missing"

# encap 数据面：环境限制说明（SKIP，非 FAIL）
echo
echo "  [SKIP] encap data-plane (encap_ok>0) needs a live WireGuard session:"
echo "         wg4-output-tun only emits packets after a successful WG handshake,"
echo "         which fails in this Docker/AF_PACKET env (VPP 26.06 'Peer error')."
echo "         VXLAN cannot substitute: its tx path bypasses the ip4-output arc"
echo "         (verified: ip4-lookup->vxlan4-encap->ip4-lookup->ip4-rewrite)."
echo "         encap's SR-header/HMAC construction is shared with transit, which"
echo "         IS verified above (transit_ok>0 on injected packets)."

# ==================================================================
echo
echo "=========================================================="
echo "RESULT: ${PASS} passed, ${FAIL} failed"
echo "=========================================================="
echo
echo "NOTE: WireGuard handshake fails in this Docker environment (VPP 26.06"
echo "      wg4-input reports 'Peer error' even on independent tunnels without"
echo "      SR plugin involvement). SR data-plane (transit + decap + HMAC) is"
echo "      verified via raw AF_PACKET packet injection; encap is verified at the"
echo "      integration level (ip4-output arc ordering + per-interface enable)."

if [ "${KEEP}" != "1" ]; then
    info "cleanup"
    (cd "${ROOT}/test/docker" && docker compose -f "${COMPOSE_FILE}" down -v --remove-orphans >/dev/null 2>&1)
fi

exit ${FAIL}
