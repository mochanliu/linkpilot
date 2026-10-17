#!/bin/bash
# setup-node.sh - SR-WG 端到端测试的**单节点**配置脚本
#
# 在容器内部执行（由 test-e2e.sh 通过 `docker exec` 调用），全程只用 vppctl，
# 不依赖 python / vat2 / 任何外部 API 客户端。
#
# 用法:
#   setup-node.sh <role> <command> [args...]
#
#   role    : node1 | transit | node2
#   command :
#     init                  发现承载 underlay 的宿主网卡 -> VPP host-interface
#                           + 配 IP + 配 underlay 路由 + 下发 SR P-Key
#     wg-create             创建 wg0（generate-key）并配置 overlay 地址；
#                           stdout 最后一行输出本机 WireGuard public key
#     peer <peer_pubkey>    添加 WG peer + 配置 SR path + 绑定隧道 + 启用 encap
#     transit               启用 SR transit（transit 角色专用；中转不改外层源）
#     stats                 以 key=value 形式打印 SR-WG 计数器（供宿主机断言）
#     clear-stats           清零 SR-WG 计数器
#     dump                  打印排障信息
#
# 拓扑（与 test/docker/docker-compose.yml 一致）:
#
#            net_a 10.10.1.0/24              net_b 10.10.2.0/24
#   node1 ─────────────────────── transit ─────────────────────── node2
#  10.10.1.2                     10.10.1.3 / 10.10.2.3          10.10.2.2
#
#   overlay: node1 wg0 = 192.168.99.1/24   <->   node2 wg0 = 192.168.99.2/24
#
# SR 路径（node1 视角，段列表顺序 = 「最终目的 → 第一跳」）:
#   segments[0] = 10.10.2.2:51820   (node2，最终目的)
#   segments[1] = 10.10.1.3:51821   (transit，第一跳，"首跳免费"由外层承载)
#   encap  -> 外层 dst 改写为 10.10.1.3:51821，seg_left = seg_count-1 = 1
#   transit-> next_hop = segments[0] = 10.10.2.2:51820，seg_left 1 -> 0
#   node2  -> sr-wg4-input 见 seg_left==0，剥 SR 头后交给 wg4-input 解密
#
# ==================== 手工等价配置（想自己敲 vppctl 复现时照抄） ====================
# 前提：容器已由 test/docker/docker-compose.yml 起好，VPP 由
# test/docker/start-node.sh 起好（VPP 没有 PTY 会静默退出，别直接 nohup vpp）。
# underlay 口交给 VPP 时必须「先建 VIF+配 IP+同步 MAC，再摘掉内核上的同一个 IP」，
# 否则内核和 VPP 都收包，内核会抢答 ARP 并回 ICMP port-unreachable。
#
#   # ---- underlay（node1 为例；transit 两口、node2 同理换地址） ----
#   ip addr add 10.10.1.2/24 dev eth1                  # 若内核上已被摘掉
#   vppctl create host-interface name eth1
#   vppctl set interface mac address host-eth1 $(ip link show eth1 | awk '/link\/ether/{print $2}')
#   vppctl set interface state host-eth1 up
#   vppctl set interface ip address host-eth1 10.10.1.2/24
#   ip addr del 10.10.1.2/24 dev eth1
#
#   # ---- 三台都要：下发 SR 路径密钥（P-Key） ----
#   vppctl sr-wg key set 1 00112233445566778899aabbccddeeff
#
#   # ---- node1：encap 侧 ----
#   vppctl ip route add 10.10.2.0/24 via 10.10.1.3
#   vppctl wireguard create listen-port 51820 generate-key src 10.10.1.2
#   vppctl set interface ip address wg0 192.168.99.1/24
#   vppctl set interface state wg0 up
#   vppctl set sr-wg rebind-ports        # 必须在 wg-create 之后：建 WG 接口会
#                                        # 把 51820 重新注册给 wg4-input
#   vppctl sr-wg path add 1 via 10.10.2.2:51820 via 10.10.1.3:51821 hmac-id 1
#   vppctl set sr-wg tunnel 2 path 1 endpoint 10.10.2.2   # 2 = wg0 的 sw_if_index
#   vppctl set sr-wg encap enable 1                         # 1 = host-eth1
#   # ↑ SR 三件套必须排在 peer add 之前：peer 一加 wireguard 立刻发
#   # handshake-init，encap 还没挂上时这个包会走 docker 网桥被 SNAT，
#   # 回包外层源变成网关地址，把 peer 的 endpoint 污染掉（见 cmd_peer 注释）。
#   vppctl wireguard peer add wg0 public-key <node2_pub> endpoint 10.10.2.2 \
#          allowed-ip 192.168.99.2/32 dst-port 51820 persistent-keepalive 5
#   vppctl ip route add 192.168.99.2/32 via wg0        # WG 不会自动下发路由
#   vppctl ping 192.168.99.2 source wg0 repeat 5            # 期望 0% loss；首轮可能全丢（握手要跑满一个 keepalive 周期）
#   vppctl show sr-wg stats ; vppctl show wireguard peer    # peer flags:2 = ESTABLISHED
#
#   # ---- transit：只转发不解密 ----
#   host-eth1 10.10.1.3/24 + host-eth2 10.10.2.3/24，两口 up
#   vppctl set sr-wg transit enable port 51821
#
#   # ---- node2：不配 path/tunnel/encap，用来验证 WG 直连旁路 ----
#   同 node1 的 underlay + wg-create + peer，收到 SR 包由 sr-wg4-input 剥头交给 wg4-input。
set -u

ROLE=${1:-}
CMD=${2:-}
shift 2 2>/dev/null || true

[ -n "${ROLE}" ] && [ -n "${CMD}" ] || {
    # 打印到「手工等价配置」小节之前的用法+拓扑说明，行号变化无需再改这里
    awk 'NR > 1 { if ($0 ~ /^# =+ 手工等价配置/) exit; print }' "$0"
    echo
    echo "手工复现（自己敲 vppctl）的完整命令序列见本文件头部「手工等价配置」一节。"
    exit 2
}

# ==================== 可覆盖参数 ====================
N1_UNDERLAY=${N1_UNDERLAY:-10.10.1.2}
TRANSIT_A=${TRANSIT_A:-10.10.1.3}
TRANSIT_B=${TRANSIT_B:-10.10.2.3}
N2_UNDERLAY=${N2_UNDERLAY:-10.10.2.2}
NET_A_PFX=${NET_A_PFX:-24}
NET_B_PFX=${NET_B_PFX:-24}

OVERLAY_N1=${OVERLAY_N1:-192.168.99.1}
OVERLAY_N2=${OVERLAY_N2:-192.168.99.2}
OVERLAY_PFX=${OVERLAY_PFX:-24}

VPN_PORT=${VPN_PORT:-51820}
TRANSIT_PORT=${TRANSIT_PORT:-51821}

SR_KEY_HEX=${SR_KEY_HEX:-00112233445566778899aabbccddeeff}
SR_PATH_ID=${SR_PATH_ID:-1}
SR_HMAC_ID=${SR_HMAC_ID:-1}
WG_IF=${WG_IF:-wg0}
KEEPALIVE=${KEEPALIVE:-5}

log() { echo "[setup:${ROLE}] $*" >&2; }
die() { echo "[setup:${ROLE}] FATAL: $*" >&2; exit 1; }

# ==================== 角色 -> 地址 ====================
case "${ROLE}" in
  node1)   SELF_UNDERLAY=${N1_UNDERLAY}; PEER_UNDERLAY=${N2_UNDERLAY}
           SELF_OVERLAY=${OVERLAY_N1};   PEER_OVERLAY=${OVERLAY_N2}
           FIRST_HOP=${TRANSIT_A} ;;
  node2)   SELF_UNDERLAY=${N2_UNDERLAY}; PEER_UNDERLAY=${N1_UNDERLAY}
           SELF_OVERLAY=${OVERLAY_N2};   PEER_OVERLAY=${OVERLAY_N1}
           FIRST_HOP=${TRANSIT_B} ;;
  transit) SELF_UNDERLAY=${TRANSIT_A};   PEER_UNDERLAY=""
           SELF_OVERLAY="";              PEER_OVERLAY=""
           FIRST_HOP="" ;;
  *) die "unknown role '${ROLE}' (expect node1|transit|node2)" ;;
esac

# ==================== 工具函数 ====================

# 按 IPv4 找宿主网卡名（不依赖 eth0/eth1 的顺序，compose 的接口顺序不稳定）
ifname_for_ip() {
    ip -o -4 addr show 2>/dev/null |
        awk -v want="$1" '{split($4,a,"/"); if (a[1]==want) {print $2; exit}}'
}

# ensure_host_interface 会把 underlay IP 从内核摘掉，于是下一轮重跑时
# ifname_for_ip 再也找不到这块网卡 —— `--keep` 之后重复跑 e2e 会直接死在
# "no kernel interface holds 10.10.1.2"。把 IP->网卡 记在容器内 /tmp：它的
# 生命周期恰好等于"内核 IP 已被摘走"的周期（容器重建会带回内核 IP）。
UNDERLAY_STATE=${UNDERLAY_STATE:-/tmp/srwg-underlay.state}

underlay_hint() {   # $1=want_ip -> 打印上次记录的内核网卡名（无记录则空）
    [ -f "${UNDERLAY_STATE}" ] &&
        awk -v w="$1" '$1==w {print $2; exit}' "${UNDERLAY_STATE}"
}

underlay_record() { # $1=want_ip $2=ifname $3=pfx
    grep -q "^$1 " "${UNDERLAY_STATE}" 2>/dev/null ||
        printf '%s %s %s\n' "$1" "$2" "$3" >> "${UNDERLAY_STATE}"
}

# 幂等地把宿主网卡交给 VPP 并配好 IP，回显 VPP 接口名
# 注意：vppctl show interface <name> 即使接口不存在也返回 exit 0，
#       必须 grep 输出内容来判断接口是否存在。
ensure_host_interface() {
    local want_ip=$1 pfx=$2 hif vif kmac
    hif=$(ifname_for_ip "${want_ip}")
    if [ -z "${hif}" ]; then
        hif=$(underlay_hint "${want_ip}")
        [ -n "${hif}" ] ||
            die "no kernel interface holds ${want_ip} (且 ${UNDERLAY_STATE} 无记录)"
        ip addr add "${want_ip}/${pfx}" dev "${hif}" 2>/dev/null ||
            die "cannot restore ${want_ip}/${pfx} on ${hif}"
        log "restored ${want_ip}/${pfx} on ${hif} (上一轮已摘)"
    fi
    vif="host-${hif}"
    if ! vppctl show interface 2>/dev/null | grep -q "${vif}"; then
        vppctl create host-interface name "${hif}" >/dev/null ||
            die "create host-interface name ${hif} failed"
    fi
    # 同步 MAC 地址：VPP 的 host-interface 会生成自己的 MAC，但内核仍然
    # 用原始 MAC 回复 ARP，导致对端发来的包 dst MAC 与 VPP MAC 不匹配
    # （ethernet-input 报 l3 mac mismatch）。把 VPP 的 MAC 设为内核的即可。
    kmac=$(ip link show "${hif}" 2>/dev/null | awk '/link\/ether/{print $2}')
    if [ -n "${kmac}" ]; then
        vppctl set interface mac address "${vif}" "${kmac}" 2>/dev/null || true
    fi
    vppctl set interface state "${vif}" up
    if ! vppctl show interface "${vif}" 2>/dev/null | grep -q "${want_ip}/"; then
        vppctl set interface ip address "${vif}" "${want_ip}/${pfx}"
    fi
    # 删除内核上同一 IP，避免内核抢先响应 ARP 和发送 ICMP port-unreachable
    # （AF_PACKET 会让内核和 VPP 同时收到包，内核无对应 socket 时会回 ICMP 错误）
    ip addr del "${want_ip}/${pfx}" dev "${hif}" 2>/dev/null || true
    underlay_record "${want_ip}" "${hif}" "${pfx}"
    echo "${vif}"
}

sw_if_index_of() {
    vppctl show interface "$1" 2>/dev/null |
        awk -v n="$1" '$1==n {print $2; exit}'
}

# 按 VPP 侧配置的 IPv4 反查接口名。
# 不能用宿主的 `ip addr` —— cmd_init 已把 underlay IP 从内核摘掉（避免内核
# 抢答 ARP），此时只有 VPP 知道这个地址挂在哪张口上。
vpp_ifname_for_ip() {
    vppctl show interface addr 2>/dev/null |
        awk -v want="$1" '
            /:[[:space:]]*$/      { n=$1; next }
            index($0, "L3 "want"/") { print n; exit }'
}

wg_public_key() {
    vppctl show wireguard interface 2>/dev/null |
        awk '{for(i=1;i<=NF;i++) if ($i ~ /^public-key:/)
              {sub(/^public-key:/,"",$i); print $i; exit}}'
}

# peer 的动态 endpoint，即 "endpoint:[本端->对端]" 里的对端
wg_peer_endpoint() {
    vppctl show wireguard peer 2>/dev/null | tr -d '\r' |
        sed -n 's/.*->\([0-9.]*:[0-9]*\)\].*/\1/p' | head -1
}

# 按公钥找回 peer 的 [N] 索引（`wireguard peer remove <N>` 用）
wg_peer_index_of() {
    vppctl show wireguard peer 2>/dev/null | tr -d '\r' |
        awk -v k="$1" '
            /^\[[0-9]+\]/ { idx=$1; gsub(/[][]/,"",idx); next }
            /public-key:/ { if (index($0, k)) { print idx; exit } }'
}

wg_add_peer() {
    local peer_pub=$1
    vppctl show wireguard peer 2>/dev/null | grep -q "${peer_pub}" ||
        vppctl wireguard peer add "${WG_IF}" \
               public-key "${peer_pub}" \
               endpoint "${PEER_UNDERLAY}" \
               allowed-ip "${PEER_OVERLAY}/32" \
               dst-port "${VPN_PORT}" \
               persistent-keepalive "${KEEPALIVE}" || die "wireguard peer add failed"
    log "wg peer added: endpoint=${PEER_UNDERLAY}:${VPN_PORT} allowed-ip=${PEER_OVERLAY}/32"
}

# WireGuard 有 endpoint roaming：对端只要从另一个地址发来**签名有效**的报文，
# 静态配的 endpoint 就会被改写。本实验环境里 docker 网桥会 SNAT 一部分跨网段
# 流量，实测把 node1 的 peer endpoint 改成了 10.10.1.1:<随机端口>，之后 encap
# 的外层 dst 永远不等于绑定端点 → 整条 SR 路径静默旁路（Encap ok=0）。
# add 带「已存在就跳过」守卫，救不回被污染的老 peer，所以这里显式校验，
# 污染了就摘掉重建一次 —— 调用点已保证 encap 先配好，重建后的 handshake
# 第一跳就走 SR 路径，不会再被网桥改写。
wg_ensure_endpoint() {
    local peer_pub=$1 ep idx want="${PEER_UNDERLAY}:${VPN_PORT}"
    ep=$(wg_peer_endpoint)
    if [ "${ep}" = "${want}" ]; then
        log "wg peer endpoint ok: ${ep}"
        return 0
    fi
    log "WARNING: wg peer endpoint drifted: got '${ep}', want '${want}' -> rebuild peer"
    idx=$(wg_peer_index_of "${peer_pub}")
    if [ -z "${idx}" ]; then
        log "         cannot locate peer [N] for ${peer_pub}, skip rebuild"
        return 1
    fi
    vppctl wireguard peer remove "${idx}" >/dev/null 2>&1 || true
    sleep 1
    wg_add_peer "${peer_pub}"
    sleep 3
    ep=$(wg_peer_endpoint)
    if [ "${ep}" = "${want}" ]; then
        log "wg peer endpoint repaired: ${ep}"
    else
        log "WARNING: wg peer endpoint still '${ep}' (want '${want}')"
        log "         manual repair: vppctl wireguard peer remove ${idx},"
        log "                        then $0 ${ROLE} peer <peer_pubkey>"
    fi
}

srwg_stat() {   # $1 = <section>_<name>，例如 Encap_ok
    # vppctl 输出行尾是 \r\n。不脱掉 \r 的话 $1 会是 "Encap:\r"，下面的
    # sub(/:$/,"") 匹配不上，每个计数都会被当成 0 —— 断言全部假阴性，
    # 而"期望为 0"的断言还会假阳性通过。
    vppctl show sr-wg stats 2>/dev/null | tr -d '\r' | awk -v want="$1" '
        /^  [A-Za-z]+:/ { sec=$1; sub(/:$/,"",sec); next }
        /^    [a-z_]+:/ { nm=$1; sub(/:$/,"",nm);
                          if (sec"_"nm == want) { print $2; found=1 } }
        END { if (!found) print 0 }'
}

# ==================== 命令实现 ====================

# die/exit 在命令替换的子 shell 里只能杀掉子 shell，父进程只会拿到空串，
# 所以 ensure_host_interface 的每个调用点都必须自己验空。
host_if_or_die() {
    local vif
    vif=$(ensure_host_interface "$1" "$2")
    [ -n "${vif}" ] || die "ensure_host_interface($1) failed"
    echo "${vif}"
}

cmd_init() {
    log "installing SR P-Key (hmac_id=${SR_HMAC_ID})"
    vppctl sr-wg key set "${SR_HMAC_ID}" "${SR_KEY_HEX}" || die "sr-wg key set failed"

    case "${ROLE}" in
      node1)
        VIF=$(host_if_or_die "${SELF_UNDERLAY}" "${NET_A_PFX}")
        log "underlay ${VIF} = ${SELF_UNDERLAY}/${NET_A_PFX}"
        # node1 与 node2 不同二层，去 node2 的 underlay 要经过 transit
        vppctl ip route add "${N2_UNDERLAY}/32" via "${TRANSIT_A}" || true
        vppctl ip route add 10.10.2.0/"${NET_B_PFX}" via "${TRANSIT_A}" || true
        ;;
      transit)
        VIF_A=$(host_if_or_die "${TRANSIT_A}" "${NET_A_PFX}")
        VIF_B=$(host_if_or_die "${TRANSIT_B}" "${NET_B_PFX}")
        log "underlay ${VIF_A} = ${TRANSIT_A}, ${VIF_B} = ${TRANSIT_B}"
        ;;
      node2)
        VIF=$(host_if_or_die "${SELF_UNDERLAY}" "${NET_B_PFX}")
        log "underlay ${VIF} = ${SELF_UNDERLAY}/${NET_B_PFX}"
        vppctl ip route add "${N1_UNDERLAY}/32" via "${TRANSIT_B}" || true
        vppctl ip route add 10.10.1.0/"${NET_A_PFX}" via "${TRANSIT_B}" || true
        ;;
    esac

    log "underlay reachable?"
    case "${ROLE}" in
      node1)   vppctl ping "${TRANSIT_A}" repeat 2 || true ;;
      node2)   vppctl ping "${TRANSIT_B}" repeat 2 || true ;;
      transit) vppctl ping "${N1_UNDERLAY}" repeat 2 || true
               vppctl ping "${N2_UNDERLAY}" repeat 2 || true ;;
    esac
    log "init done"
}

cmd_wg_create() {
    if ! vppctl show interface 2>/dev/null | grep -q "${WG_IF}"; then
        vppctl wireguard create listen-port "${VPN_PORT}" generate-key \
               src "${SELF_UNDERLAY}" >/dev/null ||
            die "wireguard create failed"
    fi
    vppctl set interface ip address "${WG_IF}" "${SELF_OVERLAY}/${OVERLAY_PFX}" 2>/dev/null || true
    vppctl set interface state "${WG_IF}" up
    # WireGuard 创建接口时会重新注册 listen-port，覆盖 SR-WG 的 51820 注册。
    # 必须在 WG 接口创建后重新绑定端口，让 sr-wg4-input 接管 51820 流量。
    vppctl set sr-wg rebind-ports 2>/dev/null || true
    # public key 必须是 stdout 的最后一行，test-e2e.sh 靠它交换密钥
    wg_public_key
}

cmd_peer() {
    local peer_pub=${1:-}
    local no_sr=${2:-}
    [ -n "${peer_pub}" ] || die "usage: setup-node.sh ${ROLE} peer <peer_pubkey> [nosr]"

    # VPP WireGuard 不会自动为 allowed-ip 安装 FIB 路由，需要手动添加
    vppctl ip route add "${PEER_OVERLAY}/32" via "${WG_IF}" 2>/dev/null || true

    # 顺序很重要：先配 SR（path / tunnel / encap），**最后**才加 peer。
    # peer 一加，wireguard 立刻发 handshake-init；那时 encap 还没挂上，这个包
    # 走的是 docker 网桥而不是 transit，被网桥 SNAT 之后对端回包的外层源就成了
    # 网关地址，endpoint roaming 把本端静态 endpoint 改掉 → encap 永远不命中 →
    # 整条 SR 路径静默旁路（Encap ok=0，overlay 0 回包）。实测踩过。
    if [ "${no_sr}" = "nosr" ]; then
        log "nosr mode: skip SR path setup (WG direct endpoint)"
    else
        # SR 路径：segments[0]=最终目的(peer:51820)，segments[1]=第一跳(transit:51821)
        vppctl sr-wg path add "${SR_PATH_ID}" \
               via "${PEER_UNDERLAY}:${VPN_PORT}" \
               via "${FIRST_HOP}:${TRANSIT_PORT}" \
               hmac-id "${SR_HMAC_ID}" || die "sr-wg path add failed"

        local swidx
        swidx=$(sw_if_index_of "${WG_IF}")
        [ -n "${swidx}" ] || die "cannot resolve sw_if_index of ${WG_IF}"
        vppctl set sr-wg tunnel "${swidx}" path "${SR_PATH_ID}" \
               endpoint "${PEER_UNDERLAY}" || die "set sr-wg tunnel failed"
        log "bound ${WG_IF}(sw_if_index=${swidx}) -> path ${SR_PATH_ID} endpoint=${PEER_UNDERLAY}"

        # encap 的挂点是 **underlay 出口接口**，不是 wg0：VPP 26.06 的
        # wg4-output-tun 把 next 硬编码为 adj-midchain-tx，从不走 feature 链，
        # 挂在 wg0 上 0 vectors（已实测）。
        local uvif uidx
        uvif=$(vpp_ifname_for_ip "${SELF_UNDERLAY}")
        [ -n "${uvif}" ] || die "cannot find VPP interface holding ${SELF_UNDERLAY}"
        uidx=$(sw_if_index_of "${uvif}")
        [ -n "${uidx}" ] || die "cannot resolve sw_if_index of ${uvif}"
        vppctl set sr-wg encap enable "${uidx}" >/dev/null ||
            die "set sr-wg encap enable ${uidx} (${uvif}) failed"
        log "sr-wg4-encap enabled on underlay ${uvif}(sw_if_index=${uidx})"
    fi

    wg_add_peer "${peer_pub}"

    if [ "${no_sr}" != "nosr" ]; then
        wg_ensure_endpoint "${peer_pub}"

        # 校验：feature 确实挂在 underlay 口的 ip4-output arc 上，且绑定可命中
        log "ip4-output features on $(vpp_ifname_for_ip "${SELF_UNDERLAY}"):"
        vppctl show interface features "$(vpp_ifname_for_ip "${SELF_UNDERLAY}")" 2>/dev/null |
            awk '/^ip4-output:/{f=1;next} f&&/^$/{exit} f{print "    "$0}' >&2
        vppctl show sr-wg tunnels 2>/dev/null | grep -E "WARNING|usable" >&2
    fi
}

cmd_transit() {
    # 中转不改写外层源（IP 和端口都不动）：目的端 wg4-input 按 peer endpoint
    # 校验外层源，改源会让对端丢包并把 peer 的动态 endpoint 污染到中转。
    vppctl set sr-wg transit enable port "${TRANSIT_PORT}" \
           || die "set sr-wg transit failed"
    log "transit enabled on port ${TRANSIT_PORT}"
}

cmd_stats() {
    local k
    for k in Encap_ok Encap_no_path Encap_hmac_fail Encap_bypass Encap_no_headroom \
             Transit_ok Transit_hmac_fail Transit_seg_exhausted Transit_bad_magic \
             Transit_bad_ver Transit_bad_len \
             Decap_ok Decap_hmac_fail Decap_seg_not_zero Decap_bypass \
             General_drop_total; do
        echo "${k}=$(srwg_stat "${k}")"
    done
}

cmd_clear_stats() { vppctl clear sr-wg stats; }

cmd_dump() {
    echo "===== [${ROLE}] show interface ====="
    vppctl show interface
    echo "===== [${ROLE}] show ip fib (brief) ====="
    vppctl show ip fib | head -40
    echo "===== [${ROLE}] show wireguard interface ====="
    vppctl show wireguard interface 2>/dev/null
    echo "===== [${ROLE}] show wireguard peer ====="
    vppctl show wireguard peer 2>/dev/null
    echo "===== [${ROLE}] show sr-wg paths ====="
    vppctl show sr-wg paths
    echo "===== [${ROLE}] show sr-wg keys ====="
    vppctl show sr-wg keys
    echo "===== [${ROLE}] show sr-wg tunnels ====="
    vppctl show sr-wg tunnels
    echo "===== [${ROLE}] show sr-wg stats ====="
    vppctl show sr-wg stats
    echo "===== [${ROLE}] show features ip4-output ====="
    vppctl show features | grep -A20 "^ip4-output:"
    echo "===== [${ROLE}] show errors ====="
    vppctl show errors | grep -iE "sr-wg|wg4|ip4-" | head -30
    echo "===== [${ROLE}] show runtime (sr/wg) ====="
    vppctl show runtime | grep -iE "sr-wg|wg4" | head -20
}

case "${CMD}" in
  init)          cmd_init "$@" ;;
  wg-create)     cmd_wg_create "$@" ;;
  peer)          cmd_peer "$@" ;;
  transit)       cmd_transit "$@" ;;
  stats)         cmd_stats "$@" ;;
  clear-stats)   cmd_clear_stats "$@" ;;
  dump)          cmd_dump "$@" ;;
  *)             die "unknown command '${CMD}' (init|wg-create|peer|transit|stats|clear-stats|dump)" ;;
esac
