#!/bin/bash
# bringup.sh - 三站点 SD-WAN 复杂 lab 的一键搭建（宿主机执行）
#
# 这个 lab 的目的：把已经落地的每一层放进同一张网里，并且让**盒子里同时跑**
# 自研路由栈和 FRR——两条导入方向都要能测：
#
#   正向：sr-route RIB → sr-frr → FRR table 1250 → BGP → 客户路由器
#   反向：客户路由器 BGP → FRR（打 tag）→ sr-frr 反向导入 → sr-route 动态
#         local prefixes → RR 反射 → 对端 CPE 的 RIB/FIB
#
# 它**不开发控制器**：按 2026-10-25 的指示，凡是由控制器下发的东西（每隧道
# 端口、overlay /32、隧道计划、VRF/RT、FRR 旁路表号）这里都手写。sr-ctrl
# 只做它现在已经做好的那件事——入网签发设备证书（CPE 拿它跑 sr-route）。
#
# 拓扑（站点 N = 1..3）：
#
#   客户终端        客户路由器            盒子（单容器 lab-cpeN）
#   10.68.N.10 ──── 10.68.N.2             VPP：wan 10.88.1.1N（table 0）
#                   xconn 10.79.N.3 ────  VPP host-eth1 10.79.N.2（table 0）
#                                         │ linux-cp 把它镜像成内核 TAP lcp-x0
#                                         │ 盒内 FRR/zebra：只在 TAP 上跑 BGP
#                                         │ 客户 VPN 0 = table 0 / 标签 1000
#                                         │ overlay（wg 接口）在 table 250
#                                         └── SR-over-WG（经 lab-transit 两跳）
#
# **数据面只有 VPP 转发**：盒内核 ip_forward 与每个接口的 forwarding 全为 0，
# 客户口的 IP 在 VPP 里。内核之所以还能跑 FRR，是因为 linux-cp 把客户口镜像成
# 一条 TAP，VPP 只把"发给盒子自己"的 L3 本地流量（BGP/ARP）punt 到 TAP。
# FRR 把客户路由装进内核主表，linux-nl 插件按 netlink 把主表镜像成 VPP table 0
# —— 这就是"FRR 算路由、VPP 转发"。sr-fib 只管 overlay 学到的前缀（压标签走
# wg）；两个写者不碰同一批前缀。
#
#   CPE 全互联（每对一条隧道，每隧道一个端口，一对 overlay /32）：
#     T12  cpe1 wg0 51820 192.168.99.1  <-> cpe2 wg0 51820 192.168.99.2   transit 51821
#     T13  cpe1 wg1 51840 192.168.100.1 <-> cpe3 wg0 51840 192.168.100.2  transit 51841
#     T23  cpe2 wg1 51860 192.168.101.1 <-> cpe3 wg1 51860 192.168.101.2  transit 51861
#
#   路由：underlay 与客户侧都在 VPP table 0（global）。客户 VPN 0 = table 0，
#   VPN 标签 1000（= 标签基址 1000 + VPN ID 0）。wg 接口与 overlay 下一跳在
#   table 250 —— 250 是 SD-WAN 路由域/传输表，不是 VPN；多 VPN 时 VPN N 用 table N
#   （250 保留给 overlay）。
#   本站 LAN 与客户 loopback 都由客户路由器通过 eBGP 宣告：盒内 FRR 用 route-map
#   给客户路由打 tag，sr-frr 只把带 tag 的 BGP 路由反向导入 sr-route 的 local
#   prefixes；我方导出到 table 1250 再进 BGP 的路由 tag=0，不会被反向吸回（防环）。
#
# 管理网 10.99.0.0/24 承载控制面（RR 的 mTLS、sr-route 的 /status、控制器 8444）
# 与各容器的第二跳，不开在数据面上。数据面的 WAN / 客户口都在 VPP 里。
#
# 用法:
#   test/lab/bringup.sh                 # 全量重建并配置
#   test/lab/bringup.sh --stage vpp     # 只跑某一阶段（containers|vpp|enroll|
#                                       #   dataplane|control|frr|terms|all）
#   test/lab/bringup.sh --bfd           # 额外建隧道内多跳 BFD 会话（默认不建）
#   test/lab/bringup.sh --keep          # 不先 down 现有容器（调试用，慎用）
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
COMPOSE="${HERE}/lab-compose.yml"
PROJECT=sdwan-lab
RUN="${HERE}/run"
BIN="${RUN}/bin"
IMAGE=srwg-lab:latest
CUST_IMAGE=quay.io/frrouting/frr:10.7.1

STAGE=all
WITH_BFD=0
KEEP=0
while [ $# -gt 0 ]; do
    case "$1" in
      --stage)    STAGE=${2:-}; shift 2 ;;
      --bfd)      WITH_BFD=1; shift ;;
      --keep)     KEEP=1; shift ;;
      -h|--help)  awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 0 ;;
      *)          echo "unknown option: $1" >&2; exit 2 ;;
    esac
done
case "$STAGE" in containers|vpp|enroll|dataplane|control|frr|terms|all) ;; *)
    echo "unknown stage: $STAGE" >&2; exit 2 ;;
esac

# ---------------- 手写的"控制器下发值" ----------------
WAN_IP=(  "" 10.88.1.11 10.88.1.12 10.88.1.13 )
XCONN_IP=( "" 10.79.1.2 10.79.2.2 10.79.3.2 )    # 盒子的客户口（IP 在 VPP 里）
CUST_X_IP=( "" 10.79.1.3 10.79.2.3 10.79.3.3 )   # 客户路由器对盒子
MGMT_IP=( "" 10.99.0.11 10.99.0.12 10.99.0.13 )
CUST_MGMT=( "" 10.99.0.21 10.99.0.22 10.99.0.23 )
LAN_PFX=( "" 10.68.1.0/24 10.68.2.0/24 10.68.3.0/24 )
LOOP_IP=( "" 172.16.1.1 172.16.2.1 172.16.3.1 )  # 客户路由器 loopback（/32）
BOX_AS=(  "" 65101 65102 65103 )
CUST_AS=( "" 65001 65002 65003 )
BGP_TAG=1001
FRR_TABLE=1250
TRANSIT_WAN=10.88.1.3
VPN=0             # 客户 VPN ID，直接当 VPP 表号（0 = global；避开 250）
OVERLAY=250       # 隧道与 overlay 下一跳所在的表，不是 VPN
LABEL=$((1000 + VPN))  # VPN 标签 = 基址 1000 + VPN ID（16 是 IPv4 显式空标签）
LCP_IF=lcp-x0     # 客户口在盒内核里的镜像 TAP 名（每台盒子自己的 netns，不冲突）
ROUTE_STATUS=( "" 19001 19002 19003 )
SR_KEY=00112233445566778899aabbccddeeff
TUNNEL_MTU=1360
MSS=$((TUNNEL_MTU - 44))

RR_ADDR=10.99.0.100
RR_PORT=18446
RR_STATUS=18447
CTRL_ADDR=10.99.0.200
CTRL_PORT=8444
TENANT=tenant-a

# a b aif aport a_overlay bif bport b_overlay path_a path_b transit_port
TUNNELS="1 2 wg0 51820 192.168.99.1  wg0 51820 192.168.99.2  1 3 51821
1 3 wg1 51840 192.168.100.1 wg0 51840 192.168.100.2 2 5 51841
2 3 wg1 51860 192.168.101.1 wg1 51860 192.168.101.2 4 6 51861"

log()  { echo "== $*"; }
die()  { echo "FATAL: $*" >&2; exit 2; }
note() { echo "   $*"; }

dx()  { docker exec "$@"; }
vpp() { local c=$1; shift; docker exec "$c" vppctl "$@" 2>&1 | tr -d '\r'; }
vtysh() { local c=$1; shift; docker exec "$c" vtysh "$@" 2>&1 | tr -d '\r'; }

vpp_if_exists() { vpp "$1" show interface | awk -v n="$2" '$1==n {f=1} END{exit !f}'; }
vpp_swidx()     { vpp "$1" show interface "$2" | awk -v n="$2" '$1==n {print $2; exit}'; }
vpp_addr_has()  { vpp "$1" show interface addr | grep -q "L3 $2/"; }
vpp_fib_has()   { vpp "$1" show ip fib "$2" | grep -q "^$2"; }
vpp_ifname_for_ip() {
    vpp "$1" show interface addr |
        awk -v want="$2" '
            /:[[:space:]]*$/     { n=$1; next }
            index($0, "L3 "want"/") { print n; exit }'
}
wg_pubkey() {
    vpp "$1" show wireguard interface |
        awk -v ifn="$2" '
            $2==ifn { for(i=1;i<=NF;i++) if ($i ~ /^public-key:/)
                      { sub(/^public-key:/,"",$i); print $i; exit } }'
}
wg_peer_has() { vpp "$1" show wireguard peer | grep -q "$2"; }

kernel_if_for_ip() { docker exec "$1" ip -o -4 addr show | awk -v w="$2" '{split($4,a,"/"); if (a[1]==w) {print $2; exit}}'; }
hint_if_for_ip()   { docker exec "$1" awk -v w="$2" '$1==w {print $2; exit}' /tmp/lab-ifstate 2>/dev/null; }

# 幂等：把内核网卡交给 VPP，回显 VPP 接口名。IP 由 take_ip 单独搬。
ensure_host_if() {
    local c=$1 ip=$2 pfx=$3 ifn vif kmac mtu
    ifn=$(kernel_if_for_ip "$c" "$ip")
    if [ -z "$ifn" ]; then
        ifn=$(hint_if_for_ip "$c" "$ip")
        [ -n "$ifn" ] || die "$c: 内核上没有 $ip，也没有 /tmp/lab-ifstate 记录——重跑 --stage containers"
        docker exec "$c" ip addr add "$ip/$pfx" dev "$ifn" ||
            die "$c: 恢复 $ip/$pfx 到 $ifn 失败"
    fi
    vif="host-$ifn"
    # TPACKET_V2，不用默认的 V3：VPP 的 V3 路径把 tp_retire_blk_tov 写死 1ms，
    # 稀疏报文（ping / BFD 探针）在每个接口每方向都要多等 0~1ms；V2 没有块退休
    # 定时器。实测同一跳 RTT 0.75ms -> 0.27ms（2026-10-26）。
    vpp_if_exists "$c" "$vif" ||
        docker exec "$c" vppctl create host-interface name "$ifn" v2 >/dev/null ||
        die "$c: create host-interface $ifn 失败"
    kmac=$(docker exec "$c" cat "/sys/class/net/$ifn/address")
    vpp "$c" set interface mac address "$vif" "$kmac" >/dev/null
    vpp "$c" set interface state "$vif" up >/dev/null
    mtu=$(docker exec "$c" cat "/sys/class/net/$ifn/mtu")
    vpp "$c" set interface mtu packet "$mtu" "$vif" >/dev/null
    echo "$vif"
}

# 把内核上的 ip 搬进 VPP（可选放某张 table），并从内核摘掉。table 0 是 VPP 的
# 默认表，不用也不该显式设置。
take_ip() {
    local c=$1 vif=$2 ip=$3 pfx=$4 table=${5:-} kif
    kif=${vif#host-}
    if [ -n "$table" ] && [ "$table" != "0" ]; then
        vpp "$c" set interface ip table "$vif" "$table" >/dev/null
    fi
    vpp_addr_has "$c" "$ip" || vpp "$c" set interface ip address "$vif" "$ip/$pfx" >/dev/null
    docker exec "$c" ip addr del "$ip/$pfx" dev "$kif" 2>/dev/null || true
    docker exec "$c" sh -c "grep -q '^$ip ' /tmp/lab-ifstate 2>/dev/null ||
                            echo '$ip $kif $pfx' >> /tmp/lab-ifstate"
}

# 把内核网卡交给 VPP 并回显 VPP 接口名。IP 由 take_ip 单独搬。重复执行时
# 已经在 VPP 里就什么都不做（不然会把 linux-cp 给配对接口换过的 MAC 又改回去）。
host_if_for_ip() {   # <cpe> <ip> <pfx> <table>
    local c=$1 ip=$2 pfx=$3 table=${4:-} vif
    vif=$(vpp_ifname_for_ip "$c" "$ip")
    if [ -n "$vif" ]; then
        echo "$vif"
        return 0
    fi
    vif=$(ensure_host_if "$c" "$ip" "$pfx") || return 1
    take_ip "$c" "$vif" "$ip" "$pfx" "$table"
    echo "$vif"
}

# 把内核上的 ip 搬进 VPP（可选放某张 table），并从内核摘掉。table 0 是 VPP 的
# 默认表，不用也不该显式设置。
take_ip() {
    local c=$1 vif=$2 ip=$3 pfx=$4 table=${5:-} kif
    kif=${vif#host-}
    if [ -n "$table" ] && [ "$table" != "0" ]; then
        vpp "$c" set interface ip table "$vif" "$table" >/dev/null
    fi
    vpp_addr_has "$c" "$ip" || vpp "$c" set interface ip address "$vif" "$ip/$pfx" >/dev/null
    docker exec "$c" ip addr del "$ip/$pfx" dev "$kif" 2>/dev/null || true
    docker exec "$c" sh -c "grep -q '^$ip ' /tmp/lab-ifstate 2>/dev/null ||
                            echo '$ip $kif $pfx' >> /tmp/lab-ifstate"
}

# 盒内核不转发**任何**包：数据面全在 VPP。start-node.sh 已经把全局开关清零，
# 这里把每个接口的 forwarding 也清零（per-interface 会覆盖全局），并重新清一遍
# 默认路由 —— 内核不是路由器，没有"下一跳"这件事。
kernel_no_forwarding() {   # <cpe>
    local c=$1
    docker exec "$c" bash -lc '
        set -e
        sysctl -qw net.ipv4.ip_forward=0 net.ipv4.conf.all.forwarding=0 net.ipv4.conf.all.rp_filter=0
        for i in /sys/class/net/*; do
            [ -d "$i" ] || continue          # bonding_masters 是文件，不是网卡
            i=$(basename "$i"); [ "$i" = lo ] && continue
            sysctl -qw net.ipv4.conf."$i".forwarding=0 net.ipv4.conf."$i".rp_filter=0
        done
        # 反过来，交给 VPP 的那几张网卡要关死：AF_PACKET 只是旁路，内核照样收到
        # 同一帧；地址又在 lcp TAP 上、同样算 local，弱主机模型下内核会替这个口
        # 再回一次 ICMP，终端看到「每个包两个回包」。严格 rp_filter 让内核把从
        # VPP 独占口进来的包全部丢掉。控制面（BGP 等）走 VPP punt → lcp TAP，
        # TAP 的 rp_filter 仍是 0，不受影响。
        if [ -r /tmp/lab-ifstate ]; then
            while read -r _ ifn _; do
                [ -n "$ifn" ] && [ -d "/sys/class/net/$ifn" ] || continue
                sysctl -qw net.ipv4.conf."$ifn".rp_filter=1
            done < /tmp/lab-ifstate
        fi
    ' || die "$c: 关内核转发失败"
}

# AF_PACKET 让内核仍收到一份网卡拷贝。地址已经在 VPP 里，内核不该再拿同一个
# IP 抢答 ARP（weak host model 会在 eth1 上用 eth1 的 MAC 回答），否则客户路由器
# 会把包发给内核那份 eth1，VPP 在 ethernet-input 按 l3 mac mismatch 丢掉——
# 表现为"BGP 通、数据不通"。arp_ignore=1：只允许持有该地址的接口回答。
kernel_arp_owner_only() {   # <cpe>
    local c=$1
    docker exec "$c" bash -lc '
        set -e
        for i in /sys/class/net/*; do
            [ -d "$i" ] || continue
            i=$(basename "$i"); [ "$i" = lo ] && continue
            sysctl -qw net.ipv4.conf."$i".arp_ignore=1
        done' || die "$c: 设置 arp_ignore 失败"
}

# 客户口交给 VPP 之后，linux-cp 把它镜像成内核 TAP：FRR 只在这条 TAP 上跑 BGP，
# 转发全部由 VPP 完成。lcp-sync 把 VPP 的地址（和以后的新地址）复制到 TAP。
lcp_pair() {   # <cpe> <vpp-if>
    local c=$1 vif=$2
    if ! vpp "$c" show lcp | grep -q " $vif "; then
        # 上一次 VPP 被 SIGKILL 时 TAP 可能残留，先清掉再配
        docker exec "$c" ip link del "$LCP_IF" 2>/dev/null || true
        vpp "$c" lcp create "$vif" host-if "$LCP_IF" >/dev/null ||
            die "$c: lcp create $vif 失败"
    fi
    vpp "$c" lcp lcp-sync on >/dev/null
}

wait_lcp_addr() {   # <cpe> <ip> <pfx>
    local c=$1 ip=$2 pfx=$3 i
    for i in $(seq 1 40); do
        docker exec "$c" ip -o -4 addr show dev "$LCP_IF" 2>/dev/null |
            grep -q "inet $ip/$pfx" && return 0
        sleep 0.25
    done
    die "$c: $LCP_IF 没有拿到 $ip/$pfx（lcp-sync 没生效：vppctl show lcp / show interface addr）"
}

# docker 塞的默认路由指向管理网桥。盒子不转发、也不做出口路由器，删掉它，
# 让"内核没有默认路由"成为一个可断言的事实（数据走 VPP 的 FIB）。
drop_kernel_default() {   # <container>
    docker exec "$1" sh -c 'while ip route del default 2>/dev/null; do :; done' || true
}

wait_default_via() {   # <container> <nexthop> <label>
    local c=$1 nh=$2 what=$3 i
    for i in $(seq 1 40); do
        docker exec "$c" ip route show default 2>/dev/null | grep -q "via $nh" && return 0
        sleep 0.5
    done
    die "$c: 内核默认路由没有指向 $nh（$what；看 /var/log/frr/ 与 vtysh show ip route 0.0.0.0/0）"
}

# WG 接口住在 overlay 表（250）。VPP 不允许带地址改表：先摘地址、换表、再放回。
# 重复执行时已经在 250 就什么都不做。
wg_to_overlay() {   # <cpe> <if> <overlay/32>
    local c=$1 ifn=$2 ov=$3
    vpp "$c" show interface address |
        grep -A1 "^$ifn (" | grep -q "ip4 table-id $OVERLAY " && return 0
    vpp "$c" set interface ip address del "$ifn" "$ov/32" >/dev/null 2>&1 || true
    vpp "$c" set interface ip table "$ifn" "$OVERLAY" >/dev/null ||
        die "$c: 把 $ifn 放进 overlay 表 $OVERLAY 失败"
    vpp "$c" set interface ip address "$ifn" "$ov/32" >/dev/null ||
        die "$c: $ifn 重新配 $ov/32 失败"
}

wg_create() {   # <cpe> <if> <listen-port> <src-ip> <overlay/32>
    local c=$1 ifn=$2 port=$3 src=$4 ov=$5
    vpp_if_exists "$c" "$ifn" ||
        docker exec "$c" vppctl wireguard create listen-port "$port" generate-key src "$src" >/dev/null ||
        die "$c: wireguard create $ifn 失败"
    wg_to_overlay "$c" "$ifn" "$ov"
    vpp_addr_has "$c" "$ov" || vpp "$c" set interface ip address "$ifn" "$ov/32" >/dev/null
    vpp "$c" set interface state "$ifn" up >/dev/null
    vpp "$c" set interface mpls "$ifn" enable >/dev/null
    vpp "$c" set interface mtu packet "$TUNNEL_MTU" "$ifn" >/dev/null
    vpp "$c" set interface tcp-mss-clamp "$ifn" ip4 rx ip4-mss "$MSS" ip6 disable >/dev/null
}

tunnel_rows() { printf '%s\n' "$TUNNELS" | while read -r a b aif aport aov bif bport bov pa pb tport; do
    [ -z "${a:-}" ] && continue
    echo "$a $b $aif $aport $aov $bif $bport $bov $pa $pb $tport"
done; }

# 停一个自研二进制并等它真的退出。sr-frr 收到 SIGTERM 会撤表、清空反向导入
# 集合；如果紧接着起新进程，旧的清理会在新进程推送之后到达，把刚刚导入的
# 前缀又清掉（实测：动态 prefixes 2 → 0 同秒发生）。按进程名（-x）匹配，
# 避免 pgrep -f 把执行这段脚本的 shell 自己也匹配上。
stop_proc() {   # <container> <process-name>
    docker exec "$1" bash -lc "pkill -x $2 2>/dev/null || true
        for i in \$(seq 1 40); do pgrep -x $2 >/dev/null 2>&1 || exit 0; sleep 0.25; done
        pkill -9 -x $2 2>/dev/null || true"
}

# ==================================================================
stage_containers() {
    log "containers: 生成盒内 FRR / 客户路由器配置并重建 ${PROJECT}"
    docker image inspect "$IMAGE" >/dev/null 2>&1 ||
        die "缺少镜像 $IMAGE（cd test/lab && docker build -t $IMAGE .）"
    docker image inspect "$CUST_IMAGE" >/dev/null 2>&1 || die "缺少镜像 $CUST_IMAGE：docker pull $CUST_IMAGE"

    mkdir -p "$RUN" "$BIN"
    local n
    for n in 1 2 3; do
        # 盒内 FRR：与自研控制面同盒子。table-direct 1250 把 sr-frr 导出的
        # 远端前缀交给 BGP；inbound route-map 给客户路由打 tag，sr-frr 只认
        # 这个 tag，把我方导出的 tag=0 路由挡在反向导入之外（防环）。
        cat > "$RUN/cpe$n.frr.conf" <<EOF
frr defaults traditional
hostname lab-cpe$n
log file /var/log/frr/frr.log
service integrated-vtysh-config
!
route-map CUST-IN permit 10
 set tag $BGP_TAG
!
router bgp ${BOX_AS[$n]}
 bgp router-id ${MGMT_IP[$n]}
 no bgp ebgp-requires-policy
 neighbor ${CUST_X_IP[$n]} remote-as ${CUST_AS[$n]}
 neighbor ${CUST_X_IP[$n]} description lab-cust$n
 neighbor ${CUST_X_IP[$n]} route-map CUST-IN in
 address-family ipv4 unicast
  redistribute table-direct $FRR_TABLE
 exit-address-family
!
! 盒内核不做转发，所以这里**没有**默认路由：客户侧流量的明细路由由 BGP
! 装进内核主表，linux-nl 再把它镜像成 VPP table 0；远端前缀压标签走隧道，
! 由 sr-fib 直接写进同一张 VPP 表。
EOF
        # 客户路由器：只宣告自己的 LAN 与 loopback。两个前缀都只能经 BGP 学到
        # （盒内没有静态 local_prefixes）——这是"FRR 算路由"的前提。
        cat > "$RUN/cust$n.frr.conf" <<EOF
frr defaults traditional
hostname lab-cust$n
log stdout
service integrated-vtysh-config
!
interface lo
 ip address ${LOOP_IP[$n]}/32
!
router bgp ${CUST_AS[$n]}
 bgp router-id ${CUST_MGMT[$n]}
 no bgp ebgp-requires-policy
 neighbor ${XCONN_IP[$n]} remote-as ${BOX_AS[$n]}
 neighbor ${XCONN_IP[$n]} description lab-cpe$n
 address-family ipv4 unicast
  network ${LAN_PFX[$n]}
  network ${LOOP_IP[$n]}/32
 exit-address-family
!
ip route 0.0.0.0/0 ${XCONN_IP[$n]}
!
EOF
    done

    if [ "$KEEP" != "1" ]; then
        docker compose -f "$COMPOSE" -p "$PROJECT" down -v --remove-orphans >/dev/null 2>&1 || true
    fi
    docker compose -f "$COMPOSE" -p "$PROJECT" up -d --no-build || die "docker compose up 失败"
    local c
    for c in lab-cpe1 lab-cpe2 lab-cpe3 lab-cust1 lab-cust2 lab-cust3 \
             lab-transit lab-rr lab-ctrl lab-term1 lab-term2 lab-term3; do
        docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null | grep -q true || die "$c 没起来"
    done

    for n in 1 2 3; do
        c=lab-cpe$n
        # 内核既不当路由器也不当主机：转发全关、默认路由删掉。客户口的 IP
        # 在 stage dataplane 里搬进 VPP（那时 FRR 的 BGP 会话会短暂断掉，
        # stage frr 里重启 FRR 让它从 TAP 重新建会话）。
        kernel_no_forwarding "$c"
        drop_kernel_default "$c"
        docker cp "$RUN/cpe$n.frr.conf" "$c:/etc/frr/frr.conf" >/dev/null
        docker exec "$c" bash -lc 'touch /etc/frr/vtysh.conf; chown -R frr:frr /etc/frr'
        docker exec "$c" bash -lc '/usr/lib/frr/frrinit.sh restart >/dev/null 2>&1 ||
                                   /usr/lib/frr/frrinit.sh start >/dev/null 2>&1' ||
            die "$c: FRR 起不来（docker exec $c tail -40 /var/log/frr/frr.log）"
        local i
        for i in $(seq 1 40); do
            vtysh "$c" -c 'show version' >/dev/null 2>&1 && break
            sleep 0.5
        done
        vtysh "$c" -c 'show running-config' | grep -q "redistribute table-direct $FRR_TABLE" ||
            die "$c: frr.conf 没应用（docker exec $c vtysh -c 'show running-config'）"
        drop_kernel_default "$c"
        docker exec "$c" ip route show default | grep -q . &&
            die "$c: 盒内核还有默认路由（内核不转发，不该有）"
        note "$c: FRR 10.7.1 + VPP 同盒，BGP AS ${BOX_AS[$n]}；内核转发全关、无默认路由"
    done

    for n in 1 2 3; do
        c=lab-cust$n
        docker cp "$RUN/cust$n.frr.conf" "$c:/etc/frr/frr.conf" >/dev/null
        docker exec "$c" sh -c 'sed -i "s/^bgpd=no/bgpd=yes/" /etc/frr/daemons
            touch /etc/frr/vtysh.conf; chown -R frr:frr /etc/frr' ||
            die "$c: 写 FRR 配置失败"
        docker restart "$c" >/dev/null || die "$c: restart 失败"
        local i
        for i in $(seq 1 40); do
            vtysh "$c" -c 'show version' >/dev/null 2>&1 &&
                vtysh "$c" -c 'show running-config' | grep -q "remote-as ${BOX_AS[$n]}" && break
            sleep 0.5
        done
        vtysh "$c" -c 'show running-config' | grep -q "remote-as ${BOX_AS[$n]}" ||
            die "$c: 客户路由器配置没应用"
        docker exec "$c" ip -4 addr show lo | grep -q "${LOOP_IP[$n]}/32" ||
            die "$c: loopback ${LOOP_IP[$n]}/32 没配上"
        drop_kernel_default "$c"
        wait_default_via "$c" "${XCONN_IP[$n]}" "客户路由器默认路由应指向盒子"
        note "$c: 客户路由器 AS ${CUST_AS[$n]}，loopback ${LOOP_IP[$n]}/32，默认路由 → ${XCONN_IP[$n]}"
    done
}

# ==================================================================
stage_vpp() {
    log "vpp: 启动 4 个数据面实例（3 CPE + transit）"
    [ -f "$ROOT/vpp-plugin/build/lib/vpp_plugins/sr_wg_plugin.so" ] ||
        die "缺 sr_wg_plugin.so：先跑一次 test/scripts/test-e2e.sh 或在容器里编译插件"
    local c
    for c in lab-cpe1 lab-cpe2 lab-cpe3 lab-transit; do
        docker exec "$c" bash -lc 'pkill -9 -x vpp_main; pkill -9 -f "script -qefc vpp"; sleep 1
            rm -f /run/vpp/*.sock; ip link del '"$LCP_IF"' 2>/dev/null' >/dev/null 2>&1
        # 必须用工作区里那份启动配置：镜像自带的没有开 linux-cp/linux-nl，
        # 而这两个插件是"FRR 算路由、VPP 转发"的接缝。
        docker exec -d "$c" bash -lc \
            "VPP_CONF=/workspace/test/docker/vpp-startup.conf bash /workspace/test/docker/start-node.sh ${c}"
    done
    for c in lab-cpe1 lab-cpe2 lab-cpe3 lab-transit; do
        local i
        for i in $(seq 1 90); do
            docker exec "$c" vppctl show version >/dev/null 2>&1 && break
            sleep 1
        done
        docker exec "$c" vppctl show version >/dev/null 2>&1 ||
            die "$c: VPP 没起来（docker exec $c tail -40 /var/log/vpp/vpp.log）"
        docker exec "$c" vppctl show plugins | grep -q sr_wg_plugin.so ||
            die "$c: sr_wg 插件没加载"
        docker exec "$c" vppctl show plugins | grep -q linux_cp_plugin.so ||
            die "$c: linux-cp 插件没加载（检查 /workspace/test/docker/vpp-startup.conf 的 plugins 段，必须带 .so）"
        docker exec "$c" vppctl show plugins | grep -q linux_nl_plugin.so ||
            die "$c: linux-nl 插件没加载（检查 /workspace/test/docker/vpp-startup.conf 的 plugins 段，必须带 .so）"
        note "$c: VPP + sr_wg + linux-cp/linux-nl OK"
    done
    # 内核一个包都不转发：数据面全在 VPP。
    local n
    for n in 1 2 3; do kernel_no_forwarding "lab-cpe$n"; done
}

# ==================================================================
stage_enroll() {
    log "enroll: 编译二进制、起控制器、三台 CPE 入网"
    docker exec lab-cpe1 bash -lc '
        set -e; cd /workspace/controller; mkdir -p /tmp/lab-bin
        for b in sr-rr sr-route sr-fib sr-frr sr-ctrl; do
            CGO_ENABLED=0 go build -o /tmp/lab-bin/$b ./cmd/$b
        done' || die "编译失败"
    local b n
    for b in sr-rr sr-route sr-fib sr-frr sr-ctrl; do
        docker cp "lab-cpe1:/tmp/lab-bin/$b" "$BIN/$b" >/dev/null || die "取 $b 失败"
    done
    docker cp "$BIN/sr-rr"   lab-rr:/usr/local/bin/sr-rr
    docker cp "$BIN/sr-ctrl" lab-ctrl:/usr/local/bin/sr-ctrl
    for n in 1 2 3; do
        docker cp "$BIN/sr-route" "lab-cpe$n:/usr/local/bin/sr-route"
        docker cp "$BIN/sr-fib"   "lab-cpe$n:/usr/local/bin/sr-fib"
        docker cp "$BIN/sr-frr"   "lab-cpe$n:/usr/local/bin/sr-frr"
    done

    log "enroll: 启动 sr-ctrl（CA + 8444/mTLS + 控制台 8080）"
    stop_proc lab-ctrl sr-ctrl
    # 控制台 token 落盘在容器里（重启沿用同一个，不然每次 bringup 都要重新登录）。
    docker exec lab-ctrl sh -c 'mkdir -p /var/lib/sr-ctrl
        [ -s /var/lib/sr-ctrl/console-token ] || \
            head -c 18 /dev/urandom | base64 | tr -d "=+/\n" > /var/lib/sr-ctrl/console-token'
    CTRL_TOKEN=$(docker exec lab-ctrl cat /var/lib/sr-ctrl/console-token)
    # 入网应答只有身份：控制器不再需要知道 RR 地址、端口、p-key 环。RR 列表现在是
    # 节点计划的一部分（客户在控制台配置设备时才下发），lab 里仍由 bringup 手写。
    docker exec -d lab-ctrl sh -c "mkdir -p /var/lib/sr-ctrl && /usr/local/bin/sr-ctrl \
        -data /var/lib/sr-ctrl -listen 0.0.0.0:${CTRL_PORT} \
        -tls-san ${CTRL_ADDR},10.88.1.200,lab-ctrl \
        -console 0.0.0.0:8080 -admin-token ${CTRL_TOKEN} \
        > /var/log/sr-ctrl.log 2>&1"
    local i
    for i in $(seq 1 30); do
        docker exec lab-ctrl test -f /var/lib/sr-ctrl/ca.pem && break
        sleep 0.5
    done
    docker exec lab-ctrl test -f /var/lib/sr-ctrl/ca.pem || die "sr-ctrl 没生成 CA（看 /var/log/sr-ctrl.log）"
    docker cp lab-ctrl:/var/lib/sr-ctrl/ca.pem "$RUN/ca.pem" >/dev/null
    docker cp lab-ctrl:/var/lib/sr-ctrl/ca.key "$RUN/ca.key" >/dev/null
    note "控制器 CA 就绪（$RUN/ca.pem）"
    printf '%s' "$CTRL_TOKEN" > "$RUN/console-token"
    note "控制台：http://127.0.0.1:8088/  （token: $CTRL_TOKEN）"

    for n in 1 2 3; do
        local c=lab-cpe$n
        docker cp "$RUN/ca.pem" "$c:/opt/vbond/ca.pem" >/dev/null 2>&1 || {
            docker exec "$c" mkdir -p /opt/vbond
            docker cp "$RUN/ca.pem" "$c:/opt/vbond/ca.pem" >/dev/null
        }
        docker exec -e VBOND_URL="https://${CTRL_ADDR}:${CTRL_PORT}" -e VBOND_CA=/opt/vbond/ca.pem \
            "$c" bash -lc "/workspace/controller/test/scripts/vbond-client.sh enroll $c claim-a $c" >/dev/null ||
            die "$c: 入网失败"
        local node_id
        node_id=$(docker exec "$c" python3 -c \
            'import json;print(json.load(open("/opt/vbond/'"$c"'.enroll.json"))["node_id"])' 2>/dev/null)
        [ -n "$node_id" ] || die "$c: 入网响应里没有 node_id"
        docker exec "$c" bash -lc "set -e
            mkdir -p /etc/sdwan/identity
            install -m600 /opt/vbond/$c.key /etc/sdwan/identity/device_ed25519.key
            install -m644 /opt/vbond/$c.pem /etc/sdwan/identity/node_cert.pem
            install -m644 /opt/vbond/ca.pem /etc/sdwan/identity/ca_cert.pem
            printf '{\"serial\":\"%s\",\"key_gen\":0,\"tenant_id\":\"%s\",\"node_id\":%s}' \
                $c $TENANT $node_id > /etc/sdwan/identity/state.json" || die "$c: 写身份目录失败"
        note "$c: tenant=${TENANT} node=${node_id}"
    done

    log "enroll: 用控制器 CA 给 RR 签服务端证书（SAN 10.99.0.100 / 10.88.1.100 / lab-rr）"
    docker exec lab-cpe1 bash -lc 'rm -rf /tmp/rr-pki && mkdir -p /tmp/rr-pki'
    docker cp "$RUN/ca.pem" lab-cpe1:/tmp/rr-pki/ca.pem >/dev/null
    docker cp "$RUN/ca.key" lab-cpe1:/tmp/rr-pki/ca.key >/dev/null
    docker exec lab-cpe1 bash -lc '
        set -e; cd /tmp/rr-pki
        openssl genpkey -algorithm ED25519 -out rr.key 2>/dev/null
        openssl req -new -key rr.key -subj "/CN=lab-rr" -out rr.csr 2>/dev/null
        cat > server.ext <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=IP:10.99.0.100,IP:10.88.1.100,DNS:lab-rr
EOF
        openssl x509 -req -in rr.csr -CA ca.pem -CAkey ca.key -CAcreateserial \
            -days 30 -extfile server.ext -out rr.crt 2>/dev/null' || die "RR 证书签发失败"
    docker exec lab-rr mkdir -p /etc/sr
    docker cp lab-cpe1:/tmp/rr-pki/rr.crt "$RUN/rr.crt" >/dev/null
    docker cp lab-cpe1:/tmp/rr-pki/rr.key "$RUN/rr.key" >/dev/null
    docker cp "$RUN/rr.crt" lab-rr:/etc/sr/rr.crt >/dev/null
    docker cp "$RUN/rr.key" lab-rr:/etc/sr/rr.key >/dev/null
    docker cp "$RUN/ca.pem" lab-rr:/etc/sr/ca.pem >/dev/null

    cat > "$RUN/rr.json" <<EOF
{
  "rr_id": "rr-a",
  "tenant": "$TENANT",
  "hold_sec": 12,
  "listen": "0.0.0.0:${RR_PORT}",
  "status_addr": "0.0.0.0:${RR_STATUS}",
  "tls": {"cert_file": "/etc/sr/rr.crt", "key_file": "/etc/sr/rr.key", "ca_file": "/etc/sr/ca.pem"}
}
EOF
    docker cp "$RUN/rr.json" lab-rr:/etc/sr/rr.json >/dev/null
    stop_proc lab-rr sr-rr
    docker exec -d lab-rr sh -c '/usr/local/bin/sr-rr -config /etc/sr/rr.json > /var/log/sr-rr.log 2>&1'
    for i in $(seq 1 20); do
        docker exec lab-rr curl -sf "http://127.0.0.1:${RR_STATUS}/status" >/dev/null 2>&1 && break
        sleep 0.5
    done
    docker exec lab-rr curl -sf "http://127.0.0.1:${RR_STATUS}/status" >/dev/null 2>&1 ||
        die "sr-rr 没起来（docker exec lab-rr cat /var/log/sr-rr.log）"
    note "rr-a 在 ${RR_ADDR}:${RR_PORT}（状态 ${RR_STATUS}）"
}

# ==================================================================
stage_dataplane() {
    log "dataplane: CPE 的 underlay / 客户口（linux-cp）/ 隧道（每隧道一个端口 + overlay /32）"
    local n c wan xc
    for n in 1 2 3; do
        c=lab-cpe$n
        kernel_arp_owner_only "$c"
        vpp "$c" sr-wg key set 1 "$SR_KEY" >/dev/null
        vpp "$c" mpls table add 0 >/dev/null
        vpp "$c" ip table add "$OVERLAY" >/dev/null
        wan=$(host_if_for_ip "$c" "${WAN_IP[$n]}" 24 0)
        xc=$(host_if_for_ip "$c" "${XCONN_IP[$n]}" 24 0)
        [ -n "$xc" ] || die "$c: 客户口没交给 VPP"
        lcp_pair "$c" "$xc"
        wait_lcp_addr "$c" "${XCONN_IP[$n]}" 24
        drop_kernel_default "$c"
        # 放在搬完口之后：它要按 /tmp/lab-ifstate（take_ip 写的）把 VPP 独占口
        # 的 rp_filter 反过来设成严格，TAP 也是这一步才存在。
        kernel_no_forwarding "$c"
        # 客户路由器在 stage containers 里可能已经把 10.79.N.2 解析成内核 eth1
        # 的 MAC（那时地址还在内核）。地址搬进 VPP 之后必须让它重新 ARP，否则
        # 数据包会被 VPP 按 l3 mac mismatch 丢掉。
        local cif
        cif=$(kernel_if_for_ip "lab-cust$n" "${CUST_X_IP[$n]}")
        [ -n "$cif" ] && docker exec "lab-cust$n" ip neigh flush dev "$cif" 2>/dev/null || true
        note "$c: wan=$wan(${WAN_IP[$n]}, table 0) cust=$xc(${XCONN_IP[$n]}, table 0) → 内核 TAP $LCP_IF（BGP 走 TAP，转发走 VPP）"
    done

    # 先建所有 WG 接口（每个接口自己 generate-key；同对端的多条隧道不能用同一把
    # 远端公钥，VPP 的 peer 池跨接口去重）。
    local a b aif aport aov bif bport bov pa pb tport
    while read -r a b aif aport aov bif bport bov pa pb tport; do
        wg_create "lab-cpe$a" "$aif" "$aport" "${WAN_IP[$a]}" "$aov"
        wg_create "lab-cpe$b" "$bif" "$bport" "${WAN_IP[$b]}" "$bov"
    done < <(tunnel_rows) || die "建 WG 接口失败"

    # 端口登记必须排在 wireguard create 之后：建接口会把 listen-port 抢回 wg4-input。
    for n in 1 2 3; do
        c=lab-cpe$n
        while read -r a b aif aport aov bif bport bov pa pb tport; do
            [ "$a" = "$n" ] && vpp "$c" set sr-wg port add vpn "$aport" >/dev/null
            [ "$b" = "$n" ] && vpp "$c" set sr-wg port add vpn "$bport" >/dev/null
        done < <(tunnel_rows)
        vpp "$c" set sr-wg rebind-ports >/dev/null
        note "$c: vpn 端口集合 = $(vpp "$c" show sr-wg ports | awk '/^vpn/{i=index($0,": "); s=substr($0,i+2); sub(/ .*/,"",s); print s}')"
    done

    # SR path / 按 (endpoint,dst-port) 的绑定 / underlay 出口的 encap 挂点。
    while read -r a b aif aport aov bif bport bov pa pb tport; do
        local ca=lab-cpe$a cb=lab-cpe$b sw
        vpp "$ca" sr-wg path del "$pa" >/dev/null 2>&1
        vpp "$ca" sr-wg path add "$pa" via "${WAN_IP[$b]}:$bport" via "${TRANSIT_WAN}:$tport" hmac-id 1 >/dev/null ||
            die "$ca: path $pa 下发失败"
        sw=$(vpp_swidx "$ca" "$aif")
        [ -n "$sw" ] || die "$ca: 取不到 $aif 的 sw_if_index"
        vpp "$ca" set sr-wg tunnel "$sw" path "$pa" endpoint "${WAN_IP[$b]}" dst-port "$bport" >/dev/null ||
            die "$ca: 绑定 $aif 失败"
        vpp "$cb" sr-wg path del "$pb" >/dev/null 2>&1
        vpp "$cb" sr-wg path add "$pb" via "${WAN_IP[$a]}:$aport" via "${TRANSIT_WAN}:$tport" hmac-id 1 >/dev/null ||
            die "$cb: path $pb 下发失败"
        sw=$(vpp_swidx "$cb" "$bif")
        [ -n "$sw" ] || die "$cb: 取不到 $bif 的 sw_if_index"
        vpp "$cb" set sr-wg tunnel "$sw" path "$pb" endpoint "${WAN_IP[$a]}" dst-port "$aport" >/dev/null ||
            die "$cb: 绑定 $bif 失败"
    done < <(tunnel_rows)
    for n in 1 2 3; do
        c=lab-cpe$n
        local uvif uidx
        uvif=$(vpp_ifname_for_ip "$c" "${WAN_IP[$n]}")
        [ -n "$uvif" ] || die "$c: 找不到持有 ${WAN_IP[$n]} 的 VPP 接口"
        uidx=$(vpp_swidx "$c" "$uvif")
        vpp "$c" set sr-wg encap enable "$uidx" >/dev/null
        note "$c: encap 挂在 $uvif(sw_if_index=$uidx)"
    done

    # peer 一定最后加：peer 一加 WG 立刻发 handshake，encap 必须先挂好。
    while read -r a b aif aport aov bif bport bov pa pb tport; do
        local ca=lab-cpe$a cb=lab-cpe$b puba pubb
        pubb=$(wg_pubkey "$cb" "$bif")
        puba=$(wg_pubkey "$ca" "$aif")
        [ -n "$pubb" ] && [ -n "$puba" ] || die "取不到 $aif/$bif 的 WG 公钥"
        wg_peer_has "$ca" "$pubb" ||
            vpp "$ca" wireguard peer add "$aif" public-key "$pubb" endpoint "${WAN_IP[$b]}" \
                allowed-ip 0.0.0.0/0 allowed-ip ::/0 dst-port "$bport" persistent-keepalive 5 >/dev/null ||
            die "$ca: 加 peer 失败"
        wg_peer_has "$cb" "$puba" ||
            vpp "$cb" wireguard peer add "$bif" public-key "$puba" endpoint "${WAN_IP[$a]}" \
                allowed-ip 0.0.0.0/0 allowed-ip ::/0 dst-port "$aport" persistent-keepalive 5 >/dev/null ||
            die "$cb: 加 peer 失败"
        # 对端 overlay /32 落在 overlay 表（wg 接口也在那里）。下一条 hop 的
        # 解析只认显式接口，这张 /32 是给 BFD/探针和排障用的直连条目。
        vpp "$ca" show ip fib table "$OVERLAY" | grep -q "^$bov/32" ||
            vpp "$ca" ip route add table "$OVERLAY" "$bov/32" via "$aif" >/dev/null
        vpp "$cb" show ip fib table "$OVERLAY" | grep -q "^$aov/32" ||
            vpp "$cb" ip route add table "$OVERLAY" "$aov/32" via "$bif" >/dev/null
    done < <(tunnel_rows)

    log "dataplane: transit（三个 per-tunnel transit 端口）"
    vpp lab-transit sr-wg key set 1 "$SR_KEY" >/dev/null
    vpp lab-transit mpls table add 0 >/dev/null
    wan=$(ensure_host_if lab-transit "$TRANSIT_WAN" 24)
    take_ip lab-transit "$wan" "$TRANSIT_WAN" 24
    vpp lab-transit set sr-wg transit enable port 51821 >/dev/null
    vpp lab-transit set sr-wg port add transit 51841 >/dev/null
    vpp lab-transit set sr-wg port add transit 51861 >/dev/null
    vpp lab-transit set sr-wg rebind-ports >/dev/null
    note "lab-transit: $(vpp lab-transit show sr-wg ports | awk '/^transit/{i=index($0,": "); s=substr($0,i+2); sub(/ .*/,"",s); print s}')"
}

# ==================================================================
stage_control() {
    log "control: sr-route（local 全部来自客户 BGP）+ sr-fib（VPN table $VPN，标签 $LABEL）"
    local n c node_id a b aif aport aov bif bport bov pa pb tport tun route_tun fib_tun
    for n in 1 2 3; do
        c=lab-cpe$n
        node_id=$(docker exec "$c" sed -n 's/.*"node_id":\([0-9]*\).*/\1/p' /etc/sdwan/identity/state.json | head -1)
        [ -n "$node_id" ] || die "$c: 没有身份（先跑 --stage enroll）"
        route_tun=""; fib_tun=""
        while read -r a b aif aport aov bif bport bov pa pb tport; do
            [ -z "${a:-}" ] && continue
            if [ "$a" = "$n" ]; then
                route_tun+="{\"peer_device_id\": $b, \"next_hop\": \"$bov\"},"
                fib_tun+="{\"next_hop\": \"$bov\", \"interface\": \"$aif\"},"
            fi
            if [ "$b" = "$n" ]; then
                route_tun+="{\"peer_device_id\": $a, \"next_hop\": \"$aov\"},"
                fib_tun+="{\"next_hop\": \"$aov\", \"interface\": \"$bif\"},"
            fi
        done <<< "$TUNNELS"
        route_tun=${route_tun%,}; fib_tun=${fib_tun%,}

        cat > "$RUN/cpe$n.route.json" <<EOF
{
  "tenant": "$TENANT",
  "device_id": $node_id,
  "identity_dir": "/etc/sdwan/identity",
  "serial": "lab-cpe$n",
  "hold_sec": 12,
  "tls_server_name": "$RR_ADDR",
  "rrs": [{"id": "rr-a", "addr": "$RR_ADDR:$RR_PORT"}],
  "tunnels": [$route_tun],
  "vrfs": [{"id": $VPN, "export_rts": ["65000:$VPN"], "import_rts": ["65000:$VPN"]}],
  "status_addr": "0.0.0.0:${ROUTE_STATUS[$n]}"
}
EOF
        # sr-fib 只装 overlay 学到的前缀（压标签走 wg）。本站/客户前缀归盒内
        # FRR：FRR 把它们装进内核主表，linux-nl 再镜像进 VPP table 0。
        cat > "$RUN/cpe$n.fib.json" <<EOF
{
  "status_url": "http://127.0.0.1:${ROUTE_STATUS[$n]}/status",
  "vpp_socket": "/run/vpp/api.sock",
  "poll_sec": 1,
  "vrfs": [$VPN],
  "tunnels": [$fib_tun]
}
EOF
        docker cp "$RUN/cpe$n.route.json" "$c:/etc/sdwan/route.json" >/dev/null
        docker cp "$RUN/cpe$n.fib.json" "$c:/etc/sdwan/fib.json" >/dev/null
        stop_proc "$c" sr-route
        stop_proc "$c" sr-fib
        docker exec -d "$c" sh -c '/usr/local/bin/sr-route -config /etc/sdwan/route.json > /var/log/sr-route.log 2>&1'
    done

    local i
    for n in 1 2 3; do
        c=lab-cpe$n
        for i in $(seq 1 40); do
            docker exec "$c" curl -sf --max-time 2 "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" >/dev/null 2>&1 && break
            sleep 0.5
        done
        docker exec "$c" curl -sf --max-time 2 "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" >/dev/null 2>&1 ||
            die "$c: sr-route 状态口没起来（cat /var/log/sr-route.log）"
        for i in $(seq 1 40); do
            docker exec "$c" curl -sf "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" |
                grep -Eq "\"rib_ready\":[[:space:]]*true" && break
            sleep 0.5
        done
        docker exec "$c" curl -sf "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" |
            grep -Eq "\"rib_ready\":[[:space:]]*true" ||
            die "$c: 没有和 RR 同步（cat /var/log/sr-route.log）"
    done

    for n in 1 2 3; do
        c=lab-cpe$n
        docker exec -d "$c" sh -c '/usr/local/bin/sr-fib -config /etc/sdwan/fib.json > /var/log/sr-fib.log 2>&1'
    done
    for n in 1 2 3; do
        c=lab-cpe$n
        for i in $(seq 1 20); do
            docker exec "$c" pgrep -x sr-fib >/dev/null 2>&1 && break
            sleep 0.25
        done
        docker exec "$c" pgrep -x sr-fib >/dev/null 2>&1 ||
            die "$c: sr-fib 没起来（cat /var/log/sr-fib.log）"
        # 此刻 RIB 还是空的：local 全部来自客户 BGP，要等 stage frr 把
        # sr-frr 的 tag 1001 反向导入接上。远端前缀下 FIB 的断言也在那里。
        note "$c: sr-route/sr-fib 在跑（RIB 要等客户 BGP 反向上报才有前缀）"
    done
}

# ==================================================================
stage_frr() {
    log "frr: 重启 FRR（客户口已是 TAP）+ sr-frr 正向导出 table $FRR_TABLE / 反向导入 tag $BGP_TAG"
    local n c i remote have xc st

    # 客户口的地址在 stage dataplane 里从内核 eth1 搬到了 VPP，再由 lcp-sync
    # 复制到 TAP。FRR 之前在 eth1 上建过会话，eBGP 的 ConnectRetryTimer 是
    # 120s —— 重启一次，让它立刻从 TAP（源地址 10.79.N.2）重新建会话。
    for n in 1 2 3; do
        c=lab-cpe$n
        stop_proc "$c" sr-frr
        docker exec "$c" bash -lc '/usr/lib/frr/frrinit.sh restart >/dev/null 2>&1 ||
                                   /usr/lib/frr/frrinit.sh start >/dev/null 2>&1' ||
            die "$c: FRR 重启失败（docker exec $c tail -40 /var/log/frr/frr.log）"
        for i in $(seq 1 40); do
            vtysh "$c" -c 'show version' >/dev/null 2>&1 && break
            sleep 0.5
        done
        docker exec "$c" ip -o -4 addr show dev "$LCP_IF" |
            grep -q "inet ${XCONN_IP[$n]}/24" ||
            die "$c: $LCP_IF 上没有 ${XCONN_IP[$n]}/24——FRR 无法从 TAP 建 BGP"
    done

    for n in 1 2 3; do
        c=lab-cpe$n
        cat > "$RUN/cpe$n.sr-frr.json" <<EOF
{
  "status_url": "http://127.0.0.1:${ROUTE_STATUS[$n]}/status",
  "zserv_socket": "/run/frr/zserv.api",
  "state_file": "/var/lib/sr-frr/state.json",
  "poll_sec": 1,
  "distance": 1,
  "metric": 20,
  "vrfs": [{"vrf_id": $VPN, "table_id": $FRR_TABLE}],
  "import": {
    "vrf_id": $VPN,
    "tag": $BGP_TAG,
    "prefixes_url": "http://127.0.0.1:${ROUTE_STATUS[$n]}/local-prefixes"
  }
}
EOF
        docker cp "$RUN/cpe$n.sr-frr.json" "$c:/etc/sr-frr.json" >/dev/null
        docker exec "$c" mkdir -p /var/lib/sr-frr
        stop_proc "$c" sr-frr
        docker exec -d "$c" sh -c '/usr/local/bin/sr-frr -config /etc/sr-frr.json > /var/log/sr-frr.log 2>&1'
    done

    # 1) 正向导入的边界：BGP 与客户路由器 Established（会话跑在 lcp TAP 上）。
    for n in 1 2 3; do
        c=lab-cpe$n
        for i in $(seq 1 60); do
            vtysh "$c" -c "show bgp neighbors ${CUST_X_IP[$n]}" |
                grep -q 'BGP state = Established' && break
            sleep 0.5
        done
        vtysh "$c" -c "show bgp neighbors ${CUST_X_IP[$n]}" |
            grep -q 'BGP state = Established' ||
            die "$c: 与客户路由器 ${CUST_X_IP[$n]} 的 BGP 没建立（vtysh -c 'show bgp summary'）"
    done
    note "三台盒子与客户路由器的 BGP 都 Established（客户口是 VPP，会话经 linux-cp 的 TAP）"

    # 2) 反向上报：客户宣告的 LAN 与 loopback（tag $BGP_TAG）进 sr-route 的 local。
    for n in 1 2 3; do
        c=lab-cpe$n
        for i in $(seq 1 60); do
            st=$(docker exec "$c" curl -sf "http://127.0.0.1:${ROUTE_STATUS[$n]}/status")
            echo "$st" | grep -Eq "\"prefix\":[[:space:]]*\"${LAN_PFX[$n]}\"" &&
                echo "$st" | grep -Eq "\"prefix\":[[:space:]]*\"${LOOP_IP[$n]}/32\"" && break
            sleep 0.5
        done
        st=$(docker exec "$c" curl -sf "http://127.0.0.1:${ROUTE_STATUS[$n]}/status")
        echo "$st" | grep -Eq "\"prefix\":[[:space:]]*\"${LAN_PFX[$n]}\"" ||
            die "$c: 客户 LAN ${LAN_PFX[$n]} 没有反向导入 sr-route（cat /var/log/sr-frr.log）"
        echo "$st" | grep -Eq "\"prefix\":[[:space:]]*\"${LOOP_IP[$n]}/32\"" ||
            die "$c: 客户 loopback ${LOOP_IP[$n]}/32 没有反向导入 sr-route（cat /var/log/sr-frr.log）"
        note "$c: sr-route local 已有 ${LAN_PFX[$n]} 与 ${LOOP_IP[$n]}/32（来源是客户 BGP）"
    done

    # 3) RR 反射：每台都收到另外两个站点的 LAN + loopback。
    for n in 1 2 3; do
        c=lab-cpe$n
        for i in $(seq 1 60); do
            have=0
            for remote in 1 2 3; do
                [ "$remote" = "$n" ] && continue
                st=$(docker exec "$c" curl -sf "http://127.0.0.1:${ROUTE_STATUS[$n]}/status")
                echo "$st" | grep -Eq "\"prefix\":[[:space:]]*\"${LAN_PFX[$remote]}\"" && have=$((have + 1))
                echo "$st" | grep -Eq "\"prefix\":[[:space:]]*\"${LOOP_IP[$remote]}/32\"" && have=$((have + 1))
            done
            [ "$have" = "4" ] && break
            sleep 0.5
        done
        [ "$have" = "4" ] || die "$c: RIB 没收到另两个站点的 LAN+loopback（cat /var/log/sr-route.log）"
    done
    note "三台的 RIB 都收齐了对方两个站点的 LAN + loopback"

    # 4) 正向：sr-frr 把远端前缀停进 FRR table $FRR_TABLE，BGP 再发给客户路由器。
    for n in 1 2 3; do
        c=lab-cpe$n
        for i in $(seq 1 60); do
            have=0
            for remote in 1 2 3; do
                [ "$remote" = "$n" ] && continue
                vtysh "$c" -c "show ip route table $FRR_TABLE" |
                    grep -q "${LAN_PFX[$remote]}" && have=$((have + 1))
            done
            [ "$have" = "2" ] && break
            sleep 0.5
        done
        [ "$have" = "2" ] || die "$c: table $FRR_TABLE 没收到两个远端前缀（cat /var/log/sr-frr.log）"
        note "$c: table $FRR_TABLE 有另两个站点的前缀"
    done

    # 5) 数据面：同一张 VPP table $VPN 里，两个方向的出口对得上。
    for n in 1 2 3; do
        c=lab-cpe$n
        xc=$(vpp_ifname_for_ip "$c" "${XCONN_IP[$n]}")
        [ -n "$xc" ] || die "$c: 找不到客户口的 VPP 接口"
        # 远端：sr-fib 写进来的、压标签走 wg
        for remote in 1 2 3; do
            [ "$remote" = "$n" ] && continue
            for pfx in "${LAN_PFX[$remote]}" "${LOOP_IP[$remote]}/32"; do
                for i in $(seq 1 60); do
                    vpp "$c" show ip fib table "$VPN" "$pfx" |
                        grep -q "mpls-label\[@[0-9]*\]:\[$LABEL:" && break
                    sleep 0.5
                done
                one=$(vpp "$c" show ip fib table "$VPN" "$pfx")
                echo "$one" | grep -q "^$pfx" ||
                    die "$c: table $VPN 里没有远端 $pfx（cat /var/log/sr-fib.log）"
                echo "$one" | grep -q "mpls-label\[@[0-9]*\]:\[$LABEL:" ||
                    die "$c: 远端 $pfx 没压标签 $LABEL（cat /var/log/sr-fib.log）"
            done
        done
        # 本地：FRR → 内核主表 → linux-nl → VPP，下一跳是客户路由器、不压标签。
        # 这条路径就是"FRR 算路由、VPP 转发"的直接证据。
        for pfx in "${LAN_PFX[$n]}" "${LOOP_IP[$n]}/32"; do
            for i in $(seq 1 60); do
                vpp "$c" show ip fib table "$VPN" "$pfx" | grep -q "via ${CUST_X_IP[$n]} " && break
                sleep 0.5
            done
            one=$(vpp "$c" show ip fib table "$VPN" "$pfx")
            echo "$one" | grep -q "via ${CUST_X_IP[$n]} $xc" ||
                die "$c: 本地 $pfx 没有经客户口 $xc 转发到 ${CUST_X_IP[$n]}（linux-nl 没同步？看 vppctl show lcp 与 /var/log/vpp/vpp.log）"
            echo "$one" | grep -q "mpls-label" &&
                die "$c: 本地 $pfx 被压了标签（本地前缀不该走标签）"
            docker exec "$c" ip route show "$pfx" | grep -q "via ${CUST_X_IP[$n]} dev $LCP_IF" ||
                die "$c: 内核主表里没有 $pfx via ${CUST_X_IP[$n]} dev $LCP_IF（zebra 没装进去？）"
        done
        note "$c: VPP table $VPN —— 远端压 $LABEL 走隧道；本地 ${LAN_PFX[$n]} / ${LOOP_IP[$n]}/32 经 $xc 交给客户路由器"
    done
}

# ==================================================================
stage_terms() {
    log "terms: 终端默认路由（客户侧不用知道 SD-WAN 内部怎么走）"
    local n
    for n in 1 2 3; do
        docker exec "lab-term$n" ip route replace default via 10.68.$n.2
        note "lab-term$n: default via 10.68.$n.2（客户路由器）"
    done
}

# ==================================================================
stage_bfd() {
    log "bfd: 每对 overlay 之间建**多跳**会话（绑接口的会话会把 overlay 路由打成 dpo-drop）"
    local a b aif aport aov bif bport bov pa pb tport
    while read -r a b aif aport aov bif bport bov pa pb tport; do
        local ca=lab-cpe$a cb=lab-cpe$b
        vpp "$ca" bfd udp session add multihop local-addr "$aov" peer-addr "$bov" \
            desired-min-tx 300000 required-min-rx 300000 detect-mult 3 >/dev/null
        vpp "$cb" bfd udp session add multihop local-addr "$bov" peer-addr "$aov" \
            desired-min-tx 300000 required-min-rx 300000 detect-mult 3 >/dev/null
        note "$ca($aov) <-> $cb($bov)"
    done < <(tunnel_rows)
}

# ==================================================================
case "$STAGE" in
  containers) stage_containers ;;
  vpp)        stage_vpp ;;
  enroll)     stage_enroll ;;
  dataplane)  stage_dataplane ;;
  control)    stage_control ;;
  frr)        stage_frr ;;
  terms)      stage_terms ;;
  all)
        stage_containers
        stage_vpp
        stage_enroll
        stage_dataplane
        stage_control
        stage_frr
        stage_terms
        [ "$WITH_BFD" = "1" ] && stage_bfd || true
        ;;
esac

if [ "$STAGE" = "all" ] || [ "$STAGE" = "dataplane" ]; then
    echo
    log "完成。先跑 test/lab/verify.sh 看绿灯；登录方式见 test/lab/README.md"
fi
