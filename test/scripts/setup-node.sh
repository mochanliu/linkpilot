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
#     transit [local-ip]    启用 SR transit（transit 角色专用）
#     stats                 以 key=value 形式打印 SR-WG 计数器（供宿主机断言）
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
set -u

ROLE=${1:-}
CMD=${2:-}
shift 2 2>/dev/null || true

[ -n "${ROLE}" ] && [ -n "${CMD}" ] || {
    sed -n '2,32p' "$0"
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

# 幂等地把宿主网卡交给 VPP 并配好 IP，回显 VPP 接口名
# 注意：vppctl show interface <name> 即使接口不存在也返回 exit 0，
#       必须 grep 输出内容来判断接口是否存在。
ensure_host_interface() {
    local want_ip=$1 pfx=$2 hif vif kmac
    hif=$(ifname_for_ip "${want_ip}")
    [ -n "${hif}" ] || die "no kernel interface holds ${want_ip}"
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
    echo "${vif}"
}

sw_if_index_of() {
    vppctl show interface "$1" 2>/dev/null |
        awk -v n="$1" '$1==n {print $2; exit}'
}

wg_public_key() {
    vppctl show wireguard interface 2>/dev/null |
        awk '{for(i=1;i<=NF;i++) if ($i ~ /^public-key:/)
              {sub(/^public-key:/,"",$i); print $i; exit}}'
}

srwg_stat() {   # $1 = <section>_<name>，例如 Encap_ok
    vppctl show sr-wg stats 2>/dev/null | awk -v want="$1" '
        /^  [A-Za-z]+:/ { sec=$1; sub(/:$/,"",sec); next }
        /^    [a-z_]+:/ { nm=$1; sub(/:$/,"",nm);
                          if (sec"_"nm == want) { print $2; found=1 } }
        END { if (!found) print 0 }'
}

# ==================== 命令实现 ====================

cmd_init() {
    log "installing SR P-Key (hmac_id=${SR_HMAC_ID})"
    vppctl sr-wg key set "${SR_HMAC_ID}" "${SR_KEY_HEX}" || die "sr-wg key set failed"

    case "${ROLE}" in
      node1)
        VIF=$(ensure_host_interface "${SELF_UNDERLAY}" "${NET_A_PFX}")
        log "underlay ${VIF} = ${SELF_UNDERLAY}/${NET_A_PFX}"
        # node1 与 node2 不同二层，去 node2 的 underlay 要经过 transit
        vppctl ip route add "${N2_UNDERLAY}/32" via "${TRANSIT_A}" || true
        vppctl ip route add 10.10.2.0/"${NET_B_PFX}" via "${TRANSIT_A}" || true
        ;;
      transit)
        VIF_A=$(ensure_host_interface "${TRANSIT_A}" "${NET_A_PFX}")
        VIF_B=$(ensure_host_interface "${TRANSIT_B}" "${NET_B_PFX}")
        log "underlay ${VIF_A} = ${TRANSIT_A}, ${VIF_B} = ${TRANSIT_B}"
        ;;
      node2)
        VIF=$(ensure_host_interface "${SELF_UNDERLAY}" "${NET_B_PFX}")
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

    vppctl show wireguard peer 2>/dev/null | grep -q "${peer_pub}" ||
        vppctl wireguard peer add "${WG_IF}" \
               public-key "${peer_pub}" \
               endpoint "${PEER_UNDERLAY}" \
               allowed-ip "${PEER_OVERLAY}/32" \
               dst-port "${VPN_PORT}" \
               persistent-keepalive "${KEEPALIVE}" || die "wireguard peer add failed"
    log "wg peer added: endpoint=${PEER_UNDERLAY}:${VPN_PORT} allowed-ip=${PEER_OVERLAY}/32"

    # VPP WireGuard 不会自动为 allowed-ip 安装 FIB 路由，需要手动添加
    vppctl ip route add "${PEER_OVERLAY}/32" via "${WG_IF}" 2>/dev/null || true

    # 如果传入 nosr，跳过 SR 路径绑定（用于 WG 直连模式测试）
    if [ "${no_sr}" = "nosr" ]; then
        log "skipping SR path setup (nosr mode)"
        return 0
    fi

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
    log "bound ${WG_IF}(sw_if_index=${swidx}) -> path ${SR_PATH_ID}"

    # 校验 feature 顺序：ip4-output arc 上 sr-wg4-encap 必须排在 wg4-output-tun 之后
    log "ip4-output features on ${WG_IF}:"
    vppctl show interface features "${WG_IF}" 2>/dev/null |
        awk '/^ip4-output:/{f=1;next} f&&/^$/{exit} f{print "    "$0}' >&2
}

cmd_transit() {
    local local_ip=${1:-}
    if [ -n "${local_ip}" ]; then
        vppctl set sr-wg transit enable port "${TRANSIT_PORT}" \
               local-ip "${local_ip}" || die "set sr-wg transit failed"
    else
        # 不设 local-ip：transit 保留原始 src IP（node1 的 underlay 地址），
        # 这样对端看到的 src 仍是配置好的 peer endpoint，WireGuard 握手不会
        # 因为「源地址漂移」而回错方向。
        vppctl set sr-wg transit enable port "${TRANSIT_PORT}" \
               || die "set sr-wg transit failed"
    fi
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
