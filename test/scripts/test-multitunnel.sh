#!/bin/bash
# test-multitunnel.sh - 同一对 CPE 之间开**两条** SR-over-WG 隧道（每隧道一个监听端口）
#
# 这条脚本证明的是一句话：**"两条隧道只要端口不同就能共存"**（定案 2）在数据面
# 到底成不成立，以及它成立的**前提**是什么。前提是三件事，缺一件都会退化成
# "第二条静默旁路"这种最难查的故障：
#   (1) 隧道自己的 vpn 端口要进插件的 vpn 端口集合，否则没人给它加 SR 头；
#   (2) 中转侧要有它自己的 transit 端口，否则中转不认它；
#   (3) 绑定键要带 dst-port，否则两条隧道的 (endpoint) 撞在同一个身份上。
#
# 前置：**必须先跑过 test-e2e.sh --keep**（单隧道 wg0@51820 已通、三个容器在跑、
# 插件是最新的）。本脚本只往上加第二条隧道，跑完自己拆干净，把环境还原成
# 交给它的样子；`--keep` 则保留第二条隧道供人工排查。
#
# 用法:
#   test/scripts/test-multitunnel.sh [--keep]
#
# 拓扑（在 e2e 之上加一条，underlay/overlay 见 setup-node.sh 头部）:
#   node1 wg0 192.168.99.1  <->  node2 wg0 192.168.99.2   vpn 51820 / transit 51821
#   node1 wg1 192.168.100.1 <->  node2 wg1 192.168.100.2  vpn 51830 / transit 51831
#
# 覆盖的断言：
#   A  隧道 1 本来就通（环境前置）
#   B  隧道 1 的绑定从"端口通配"收紧成显式 51820
#   C  两个 vpn 端口 + 两个 transit 端口都在插件集合里（show sr-wg ports）
#   D  两条绑定在同一 endpoint 上共存，各自一个 dst-port，都不再是通配
#   E  两条隧道各自都通
#   F  每条隧道真的带着自己的 SR 头：收包侧 Decap ok/bypass + 外层端口对 + 中转端口
#   G  抽掉隧道 2 的 transit 端口：只有隧道 2 断，隧道 1 不受影响；加回来即恢复
#   H  抢同一个隧道身份 (endpoint, dst-port) 必须被拒，且现有绑定不受损
#   I  清理后环境回到交给我之前的样子（1 条绑定、vpn 只有 51820、无 wg1、隧道 1 仍通）
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
SETUP_C="/workspace/test/scripts/setup-node.sh"

C_NODE1=srwg-node1
C_TRANSIT=srwg-transit
C_NODE2=srwg-node2
ALL_CONTAINERS="${C_NODE1} ${C_TRANSIT} ${C_NODE2}"

# ---- 隧道 1：沿用 e2e 的配置 ----
N1_UNDERLAY=${N1_UNDERLAY:-10.10.1.2}
TRANSIT_A=${TRANSIT_A:-10.10.1.3}
N2_UNDERLAY=${N2_UNDERLAY:-10.10.2.2}
VPN_PORT=${VPN_PORT:-51820}
TRANSIT_PORT=${TRANSIT_PORT:-51821}
OVERLAY_N1=${OVERLAY_N1:-192.168.99.1}
OVERLAY_N2=${OVERLAY_N2:-192.168.99.2}
PATH1=${PATH1:-1}

# ---- 隧道 2：本脚本新建 ----
VPN2_PORT=${VPN2_PORT:-51830}
TRANSIT2_PORT=${TRANSIT2_PORT:-51831}
OVERLAY2_N1=${OVERLAY2_N1:-192.168.100.1}
OVERLAY2_N2=${OVERLAY2_N2:-192.168.100.2}
PATH2=${PATH2:-2}
IF2=${IF2:-wg1}

TRACE=${TRACE:-/tmp/srwg-mt-trace.txt}

KEEP=0
for a in "$@"; do
    case "$a" in
      --keep)    KEEP=1 ;;
      -h|--help) awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 0 ;;
      *)         echo "unknown option: $a" >&2; exit 2 ;;
    esac
done

PASS=0
FAIL=0
ok()   { PASS=$((PASS+1)); echo "  [PASS] $*"; }
bad()  { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "== $*"; }
die()  { echo "FATAL: $*" >&2; exit 1; }

dx() { local c=$1; shift; docker exec "${c}" bash -lc "$*"; }
role_of() {
    case "$1" in
      ${C_NODE1})   echo node1 ;;
      ${C_TRANSIT}) echo transit ;;
      ${C_NODE2})   echo node2 ;;
    esac
}
container_of() {
    case "$1" in
      node1) echo ${C_NODE1} ;;  transit) echo ${C_TRANSIT} ;;
      node2) echo ${C_NODE2} ;;  *)       echo "$1" ;;
    esac
}
# setup_node <role> <KEY=VAL...> -- <子命令 ...>
# 环境变量只喂给这一次 setup-node.sh 调用：隧道 2 靠它们改接口名/端口/地址，
# 但不能把后续隧道 1 的操作也带跑偏。
setup_node() {
    local role="$1" c envs=""
    shift
    while [ "$1" != "--" ]; do envs+="$1 "; shift; done
    shift
    c=$(container_of "${role}")
    docker exec "${c}" bash -lc "env ${envs} bash ${SETUP_C} ${role} $*"
}

vpp() { dx "$1" "vppctl $2" 2>&1 | tr -d '\r'; }

swidx_of() {   # <container> <iface> -> sw_if_index（空=接口不存在）
    vpp "$1" "show interface $2" | awk -v n="$2" '$1==n {print $2; exit}'
}

# 插件端口集合的成员列表，如 "51820,51830"。
# 只取冒号后的那一段：transit 行的末尾还跟着"(已启用)"，用行尾锚点会漏判。
port_set() {   # <container> <vpn|transit>
    vpp "$1" "show sr-wg ports" | awk -v pat="^$2" '
        $0 ~ pat { i = index($0, ": "); s = substr($0, i + 2);
                   sub(/ .*/, "", s); print s; exit }'
}

# 绑定条数。`show sr-wg tunnels` 的头部还会列出启用了 encap 的接口，那些行也含
# "sw_if_index="，所以只数绑定行自己的 "[N] sw_if_index="（环境里实测留着一条
# sw_if_index=3 的 DELETED 僵尸项，它就是头部那种行）。
bind_lines() { vpp "$1" "show sr-wg tunnels" | grep -E "^ +\[[0-9]+\] sw_if_index="; }

# VPP 的统计行是 "N sent, M received"；冷启动首轮可能全丢（握手要跑完一个
# keepalive 周期），所以重试 3 轮，任一轮拿到回包即算通。
ping_ok() {   # <container> <dst> <iface> -> 0=通
    local c=$1 dst=$2 ifn=$3 out i
    for i in 1 2 3; do
        out=$(dx "${c}" "vppctl ping ${dst} source ${ifn} repeat 8" 2>&1)
        echo "${out}" | grep -qE "[0-9]+ sent, [1-9][0-9]* received" && return 0
        sleep 4
    done
    echo "${out}" | tail -2 | sed 's/^/      /' >&2
    return 1
}

dump_all() {
    for c in ${ALL_CONTAINERS}; do
        echo "################ ${c} ################"
        vpp "${c}" "show sr-wg ports"
        vpp "${c}" "show sr-wg tunnels"
        vpp "${c}" "show sr-wg paths"
        vpp "${c}" "show wireguard interface"
        vpp "${c}" "show sr-wg stats"
    done
}

# ---- 清理：把本脚本加的东西全部拆掉 ----
# 顺序有三处讲究：
#  (a) 解绑必须排在删接口之前。绑定按 sw_if_index 索引，接口没了之后
#      sr_wg_tunnel_set_path 的接口存在性校验会让解绑失败，绑定永久留在池里
#      变成 "DELETED" 僵尸项（本仓库的旧环境实测留下过一条）。
#  (b) vpn 端口交回 wg4-input 要排在删接口之前，这样"端口归谁"和"socket 归谁"
#      在同一时刻一致，不会留一个 sr-wg4-input 持有已消失 socket 的端口。
#  (c) 最后 rebind-ports：把集合里剩下的端口（51820）确实在 sr-wg4-input 上。
teardown() {   # $1=force 时无视 --keep（用来收拾上一轮的残留）
    if [ "${KEEP}" = "1" ] && [ "${1:-}" != "force" ]; then
        info "--keep: 隧道 2 保留，未清理"
        return 0
    fi
    info "清理：拆掉隧道 2"
    local sw
    sw=$(swidx_of "${C_NODE1}" "${IF2}")
    [ -n "${sw}" ] && vpp "${C_NODE1}" "set sr-wg tunnel ${sw} del" >/dev/null 2>&1
    vpp "${C_NODE1}" "sr-wg path del ${PATH2}"            >/dev/null 2>&1
    vpp "${C_NODE1}" "set sr-wg port del vpn ${VPN2_PORT}" >/dev/null 2>&1
    vpp "${C_NODE2}" "set sr-wg port del vpn ${VPN2_PORT}" >/dev/null 2>&1
    vpp "${C_TRANSIT}" "set sr-wg port del transit ${TRANSIT2_PORT}" >/dev/null 2>&1
    vpp "${C_NODE1}" "wireguard delete ${IF2}"            >/dev/null 2>&1
    vpp "${C_NODE2}" "wireguard delete ${IF2}"            >/dev/null 2>&1
    # 隧道 1 恢复成 e2e 留下的样子（通配绑定），下一个跑 e2e 的人看到同样的表
    [ -n "${TUN1_SWIDX:-}" ] &&
        vpp "${C_NODE1}" "set sr-wg tunnel ${TUN1_SWIDX} path ${PATH1} endpoint ${N2_UNDERLAY}" >/dev/null 2>&1
    for c in ${ALL_CONTAINERS}; do vpp "${c}" "set sr-wg rebind-ports" >/dev/null 2>&1; done
}

# ==================================================================
info "step 0: 前置检查（需要 test-e2e.sh --keep 的环境）"
# ==================================================================
for c in ${ALL_CONTAINERS}; do
    docker inspect -f '{{.State.Running}}' "${c}" 2>/dev/null | grep -q true ||
        die "容器 ${c} 没在跑，请先 bash test/scripts/test-e2e.sh --keep"
    vpp "${c}" "show version" | grep -q vpp || die "${c}: vppctl 不通（VPP 没起来）"
    vpp "${c}" "show plugins" | grep -q sr_wg_plugin.so ||
        die "${c}: sr_wg 插件没加载（.so 不是最新的？重跑 e2e）"
done
echo "  三个容器 + 插件 OK"

TUN1_SWIDX=$(swidx_of "${C_NODE1}" wg0)
[ -n "${TUN1_SWIDX}" ] || die "node1 上没有 wg0：隧道 1 还没配，先跑 e2e"
if [ -n "$(swidx_of "${C_NODE1}" ${IF2})" ] || [ -n "$(swidx_of "${C_NODE2}" ${IF2})" ]; then
    info "发现上一轮残留的 ${IF2}，先拆一遍再开始"
    teardown force
    sleep 1
    [ -z "$(swidx_of "${C_NODE1}" ${IF2})" ] || die "残留的 ${IF2} 拆不掉，请手工检查"
fi

# ==================================================================
info "ASSERT A: 隧道 1（${OVERLAY_N2} via wg0）本来就通"
# ==================================================================
ping_ok "${C_NODE1}" "${OVERLAY_N2}" wg0 &&
    ok "隧道 1 通：环境可用" ||
    die "隧道 1 不通 —— 本脚本的前提不成立，请先跑通 e2e 再来"

# ==================================================================
info "ASSERT B: 隧道 1 的绑定从『端口通配』收紧成显式 ${VPN_PORT}"
# ==================================================================
# 通配键 (endpoint,0) 命中"这个 IP 上不分行数全走本路径"，第二条隧道来了就
# 会被它一起拽走（精确键优先级更高，所以隧道 2 不受影响，但表里留个通配项迟早
# 踩坑）。这里显式收紧，顺便验证"原地改端口"不会留下指向老键的僵尸项。
vpp "${C_NODE1}" "set sr-wg tunnel ${TUN1_SWIDX} path ${PATH1} endpoint ${N2_UNDERLAY} dst-port ${VPN_PORT}" |
    sed 's/^/    /'
TUNNELS=$(vpp "${C_NODE1}" "show sr-wg tunnels")
echo "${TUNNELS}" | grep -q "endpoint=${N2_UNDERLAY}:${VPN_PORT}" &&
    ok "隧道 1 绑定已带显式端口 ${VPN_PORT}" ||
    bad "隧道 1 绑定没带端口：$(bind_lines "${C_NODE1}" | grep "sw_if_index=${TUN1_SWIDX}")"
echo "${TUNNELS}" | grep -q "端口通配" &&
    bad "表里仍有端口通配的绑定" || ok "已无端口通配绑定"

# ==================================================================
info "ASSERT C: 端口注册（node1/node2 的 vpn ${VPN2_PORT}、transit 的 transit ${TRANSIT2_PORT}）"
# ==================================================================
# 建 WG 接口和登记端口这对顺序**不能反**：`wireguard create` 会把 listen-port
# 重新注册给 wg4-input，先登记就被抢走。登记之后 51830 才有人给它加 SR 头
# —— 没登记不报错，只是 encap_bypass，这是最阴的一种坏。
PUB1=$(setup_node node1 "WG_IF=${IF2} VPN_PORT=${VPN2_PORT} OVERLAY_N1=${OVERLAY2_N1}" -- wg-create | tail -1)
PUB2=$(setup_node node2 "WG_IF=${IF2} VPN_PORT=${VPN2_PORT} OVERLAY_N2=${OVERLAY2_N2}" -- wg-create | tail -1)
echo "${PUB1}" | grep -qE '^[A-Za-z0-9+/]{43}=$' || die "没拿到 node1 ${IF2} 的公钥: '${PUB1}'"
echo "${PUB2}" | grep -qE '^[A-Za-z0-9+/]{43}=$' || die "没拿到 node2 ${IF2} 的公钥: '${PUB2}'"
echo "  ${IF2} 公钥: node1=${PUB1:0:12}... node2=${PUB2:0:12}..."

setup_node node1 -- port add vpn "${VPN2_PORT}" >&2 || die "node1: 登记 vpn ${VPN2_PORT} 失败"
setup_node node2 -- port add vpn "${VPN2_PORT}" >&2 || die "node2: 登记 vpn ${VPN2_PORT} 失败"
# transit 的启用标志是全局的：上一轮 e2e 设过就还在，没设过这里补一次，
# 否则 `port add transit` 只注册端口，收到包仍按"未启用"丢弃。
vpp "${C_TRANSIT}" "show sr-wg ports" | grep -q "已启用" || setup_node transit -- transit >&2
setup_node transit -- port add transit "${TRANSIT2_PORT}" >&2 ||
    die "transit: 登记 transit ${TRANSIT2_PORT} 失败"

for c in ${C_NODE1} ${C_NODE2}; do
    [ "$(port_set "${c}" vpn)" = "${VPN_PORT},${VPN2_PORT}" ] &&
        ok "$(role_of "${c}"): vpn 集合 = ${VPN_PORT},${VPN2_PORT}" ||
        bad "$(role_of "${c}"): vpn 集合不对 -> 实际 '$(port_set "${c}" vpn)'"
done
[ "$(port_set "${C_TRANSIT}" transit)" = "${TRANSIT_PORT},${TRANSIT2_PORT}" ] &&
    ok "transit: transit 集合 = ${TRANSIT_PORT},${TRANSIT2_PORT}" ||
    bad "transit 集合不对 -> 实际 '$(port_set "${C_TRANSIT}" transit)'"

# ==================================================================
info "ASSERT D: 隧道 2 建好（path ${PATH2} + 按端口绑定 + WG peer）"
# ==================================================================
# peer 一定排在最后：cmd_peer 内部先把 SR 三件套（path / 按端口绑定 / encap）
# 配好再加 peer，否则 peer 一加 wireguard 立刻发 handshake-init，那时还没加 SR
# 头，这个包会被 docker 网桥 SNAT，回包外层源变成网关地址，endpoint roaming
# 把静态 endpoint 污染掉 → 整条路径静默旁路（e2e 实测踩过）。
setup_node node1 "WG_IF=${IF2} VPN_PORT=${VPN2_PORT} TRANSIT_PORT=${TRANSIT2_PORT} SR_PATH_ID=${PATH2} OVERLAY_N1=${OVERLAY2_N1} OVERLAY_N2=${OVERLAY2_N2} TUN_DST_PORT=${VPN2_PORT}" -- peer "${PUB2}" >&2 ||
    die "node1: 隧道 2 发送侧配置失败"
setup_node node2 "WG_IF=${IF2} VPN_PORT=${VPN2_PORT} OVERLAY_N1=${OVERLAY2_N1} OVERLAY_N2=${OVERLAY2_N2}" -- peer "${PUB1}" nosr >&2 ||
    die "node2: 隧道 2 接收侧配置失败"

TUNNELS=$(vpp "${C_NODE1}" "show sr-wg tunnels")
echo "${TUNNELS}" | sed 's/^/    /'
NBIND=$(bind_lines "${C_NODE1}" | wc -l | tr -d ' ')
[ "${NBIND}" = "2" ] && ok "同一对 CPE 上两条绑定共存" ||
    bad "绑定数=${NBIND}，期望 2"
echo "${TUNNELS}" | grep -q "endpoint=${N2_UNDERLAY}:${VPN_PORT}"   &&
    echo "${TUNNELS}" | grep -q "endpoint=${N2_UNDERLAY}:${VPN2_PORT}" &&
    ok "两条绑定各占一个显式 dst-port（${VPN_PORT} / ${VPN2_PORT}）" ||
    bad "绑定表里没看到两个不同的 dst-port"
echo "${TUNNELS}" | grep -q "端口通配" && bad "表里又出现端口通配" || ok "无通配绑定"
PATHS=$(vpp "${C_NODE1}" "show sr-wg paths")
echo "${PATHS}" | grep -qE "path_id=${PATH2} " &&
    ok "path ${PATH2} 已下发" || bad "path ${PATH2} 没下发：$(echo "${PATHS}" | grep path_id)"
echo "${PATHS}" | grep -q "seg\[1\]: ${TRANSIT_A}:${TRANSIT2_PORT}" &&
    ok "path ${PATH2} 的第一跳是隧道 2 自己的中转端口 ${TRANSIT2_PORT}" ||
    bad "path ${PATH2} 的段列表不对：$(echo "${PATHS}" | sed -n '/path_id=2/,+3p' | tr '\n' ' ')"

# ==================================================================
info "ASSERT E: 两条隧道各自都通"
# ==================================================================
ping_ok "${C_NODE1}" "${OVERLAY_N2}"  wg0 && ok "隧道 1 通：${OVERLAY_N1} -> ${OVERLAY_N2}" || bad "隧道 1 不通了（被隧道 2 抢走？）"
ping_ok "${C_NODE1}" "${OVERLAY2_N2}" ${IF2} && ok "隧道 2 通：${OVERLAY2_N1} -> ${OVERLAY2_N2}" || bad "隧道 2 不通"

# ==================================================================
info "ASSERT F: 每条隧道真的带着自己的 SR 头，且外层端口对是它自己的"
# ==================================================================
# **ping 通不能证明加了 SR 头**：未命中绑定的包按 bypass 直发 peer，隧道照样通，
# 只是源路由没了（Encap bypass 上涨但没有任何报错）。
# 证据取在**收包侧 node2**：
#   - node1 的包是本机生成的，VPP 只在**输入节点**给 buffer 打 IS_TRACED，
#     所以在 node1 上 trace 不到自己的 encap（实测 trace add ip4-output 连节点名
#     都不认）；node2 是收包方，af-packet-input 一打全链都有。
#   - Decap ok 只在 sr-wg4-input 认到 magic 0x53 时才增长 → 证明"确实加了 SR 头"；
#     Decap bypass 保持 0 → 证明"没有任何包是裸 WG 直发过来的"。
#   - 剥头前的外层 UDP 端口对（<vpn_port> -> <vpn_port>）就是隧道身份：
#     51820 的那条属隧道 1，51830 的那条属隧道 2。
stat_of() {   # <container> <Section>_<name> -> 计数
    setup_node "$(role_of "$1")" -- stats 2>/dev/null |
        awk -F= -v k="$2" '$1==k {print $2; f=1} END{if(!f) print 0}'
}
f_probe() {   # <dst> <iface> <期望端口> <标签>
    local dst=$1 ifn=$2 port=$3 tag=$4 ok_n by ep
    setup_node node2 -- clear-stats >/dev/null 2>&1
    vpp "${C_NODE2}" "clear trace" >/dev/null
    vpp "${C_NODE2}" "trace add af-packet-input 100" >/dev/null
    dx "${C_NODE1}" "vppctl ping ${dst} source ${ifn} repeat 6 >/dev/null 2>&1"
    vpp "${C_NODE2}" "show trace max 300" >> "${TRACE}" 2>&1
    vpp "${C_NODE2}" "clear trace" >/dev/null
    ok_n=$(stat_of "${C_NODE2}" Decap_ok)
    by=$(stat_of "${C_NODE2}" Decap_bypass)
    ep=$(grep -c "UDP: ${port} -> ${port}" "${TRACE}" || true)
    echo "      ${tag}: Decap ok=${ok_n} bypass=${by}, 外层端口对 ${port}->${port} 命中 ${ep} 条"
    [ "${ok_n}" -ge 4 ] && ok "${tag} 收到的包都带 SR 头（Decap ok=${ok_n}）" ||
        bad "${tag} 只有 ${ok_n} 个包解封装：多数没走 SR，可能被旁路"
    [ "${by}" = "0" ] && ok "${tag} 没有任何裸 WG 直发包（Decap bypass=0）" ||
        bad "${tag} 收到 ${by} 个未封装的 WG 包：这条隧道的绑定没生效"
    [ "${ep}" -ge 1 ] && ok "${tag} 外层端口对是 ${port}（它自己的隧道身份）" ||
        bad "${tag} 的收包里没有 ${port}->${port} 端口对：包不是从这条隧道进来的"
}
: > "${TRACE}"
f_probe "${OVERLAY_N2}"  wg0    "${VPN_PORT}"   "隧道 1"
f_probe "${OVERLAY2_N2}" "${IF2}" "${VPN2_PORT}" "隧道 2"

# 中转侧再钉一次"第一跳也各走各的端口"：中转是收包方，trace 打得起来。
vpp "${C_TRANSIT}" "clear trace" >/dev/null
vpp "${C_TRANSIT}" "trace add af-packet-input 100" >/dev/null
vpp "${C_TRANSIT}" "trace add sr-wg4-transit 100" >/dev/null
dx "${C_NODE1}" "vppctl ping ${OVERLAY2_N2} source ${IF2} repeat 3 >/dev/null 2>&1"
vpp "${C_TRANSIT}" "show trace max 300" > /tmp/srwg-mt-transit-trace.txt 2>&1
vpp "${C_TRANSIT}" "clear trace" >/dev/null
grep -q "dst-port ${TRANSIT2_PORT}\|-> ${TRANSIT2_PORT}" /tmp/srwg-mt-transit-trace.txt &&
    ok "中转侧看到隧道 2 的包是从它自己的 ${TRANSIT2_PORT} 进来的" ||
    bad "中转 trace 里没有 ${TRANSIT2_PORT}（隧道 2 的下一跳不是它自己的中转端口）"

# ==================================================================
info "ASSERT G: 抽掉隧道 2 的 transit 端口，只有隧道 2 断"
# ==================================================================
# 端口集合就是"这条隧道在数据面登记过"的凭证：摘掉 ${TRANSIT2_PORT} 之后，
# 中转收到这个端口的包不再交给 sr-wg4-transit（中转上没有 WG socket，最终是
# ICMP port-unreachable），而 ${TRANSIT_PORT} 那条完全不受影响。
vpp "${C_TRANSIT}" "set sr-wg port del transit ${TRANSIT2_PORT}" | sed 's/^/    /'
sleep 1
ping_ok "${C_NODE1}" "${OVERLAY_N2}" wg0 &&
    ok "摘掉隧道 2 的端口期间，隧道 1 仍通" || bad "隧道 1 被牵连：端口集合不是按端口隔离的"
if ping_ok "${C_NODE1}" "${OVERLAY2_N2}" ${IF2}; then
    bad "摘掉 transit ${TRANSIT2_PORT} 后隧道 2 居然还通 —— 它的包没走中转？"
else
    ok "隧道 2 如期中断（它的路径确实要经过 ${TRANSIT2_PORT}）"
fi
vpp "${C_TRANSIT}" "set sr-wg port add transit ${TRANSIT2_PORT}" | sed 's/^/    /'
sleep 2
ping_ok "${C_NODE1}" "${OVERLAY2_N2}" ${IF2} &&
    ok "端口加回来，隧道 2 恢复" || bad "端口加回来后隧道 2 没恢复"

# ==================================================================
info "ASSERT H: 抢同一个隧道身份 (endpoint, dst-port) 必须被拒"
# ==================================================================
# 这是"两条隧道同端口"最真实的误配：让 ${IF2} 去绑隧道 1 的身份。真抢成功的话
# 隧道 1 的下一跳会被换成隧道 2 的路径，而**没有任何报错**（前一条只是悄悄旁路），
# 所以数据面必须把这种覆盖挡下来。
SW2=$(swidx_of "${C_NODE1}" ${IF2})
STEAL=$(vpp "${C_NODE1}" "set sr-wg tunnel ${SW2} path ${PATH1} endpoint ${N2_UNDERLAY} dst-port ${VPN_PORT}")
echo "    ${STEAL}"
echo "${STEAL}" | grep -q "已经被另一条隧道的绑定占着" &&
    ok "抢身份被拒绝，且把原因和排查命令说清楚了" ||
    bad "抢身份没被拒（这是回归）：${STEAL}"
TUNNELS=$(vpp "${C_NODE1}" "show sr-wg tunnels")
bind_lines "${C_NODE1}" | grep "sw_if_index=${SW2}" | grep -q "path_id=${PATH2}" &&
    ok "被拒之后 ${IF2} 的绑定没被动过（仍是 path_id=${PATH2}）" ||
    bad "${IF2} 的绑定被改写了：$(bind_lines "${C_NODE1}" | grep "sw_if_index=${SW2}")"
ping_ok "${C_NODE1}" "${OVERLAY_N2}" wg0 && ok "拒绝之后隧道 1 仍通" || bad "拒绝之后隧道 1 不通了"

# ==================================================================
info "ASSERT I: 清理后环境回到交给我之前的样子"
# ==================================================================
if [ "${KEEP}" = "1" ]; then
    info "--keep：跳过清理，也跳过本组断言（隧道 2 留在环境里，人工拆见 teardown 顺序注释）"
else
teardown
sleep 2
NBIND=$(bind_lines "${C_NODE1}" | wc -l | tr -d ' ')
[ "${NBIND}" = "1" ] && ok "绑定表回到 1 条" || bad "绑定表残留（${NBIND} 条）：$(bind_lines "${C_NODE1}")"
for c in ${C_NODE1} ${C_NODE2}; do
    [ "$(port_set "${c}" vpn)" = "${VPN_PORT}" ] &&
        ok "$(role_of "${c}") vpn 集合回到只有 ${VPN_PORT}" ||
        bad "$(role_of "${c}") vpn 集合残留：实际 '$(port_set "${c}" vpn)'"
done
[ "$(port_set "${C_TRANSIT}" transit)" = "${TRANSIT_PORT}" ] &&
    ok "transit 集合回到只有 ${TRANSIT_PORT}" ||
    bad "transit 集合残留：实际 '$(port_set "${C_TRANSIT}" transit)'"
[ -z "$(swidx_of "${C_NODE1}" ${IF2})" ] && [ -z "$(swidx_of "${C_NODE2}" ${IF2})" ] &&
    ok "两侧 ${IF2} 已删除" || bad "${IF2} 没删干净"
vpp "${C_NODE1}" "show sr-wg paths" | grep -qE "path_id=${PATH2} " &&
    bad "path ${PATH2} 残留" || ok "path ${PATH2} 已删除"
ping_ok "${C_NODE1}" "${OVERLAY_N2}" wg0 &&
    ok "清理没伤到隧道 1" || bad "清理之后隧道 1 不通了"
fi

echo
echo "=========================================================="
echo " trace 证据: ${TRACE} /tmp/srwg-mt-transit-trace.txt"
echo " RESULT: ${PASS} passed, ${FAIL} failed"
echo "=========================================================="
[ "${FAIL}" != "0" ] && { echo; info "dumping diagnostics (FAIL)"; dump_all; }
[ "${FAIL}" = "0" ]
