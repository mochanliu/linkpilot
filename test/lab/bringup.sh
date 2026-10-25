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
#   10.68.N.10 ──── 10.68.N.2             VPP：wan 10.88.1.1N
#                   xconn 10.79.N.3 ────  kernel 10.79.N.2
#                                         │ veth vti0/vti1（盒内两半）
#                                         │ kernel 10.78.N.20 <-> VPP 10.78.N.2
#                                         │ VPP table 250 / 标签 266
#                                         └── SR-over-WG（经 lab-transit 两跳）
#
#   CPE 全互联（每对一条隧道，每隧道一个端口，一对 overlay /32）：
#     T12  cpe1 wg0 51820 192.168.99.1  <-> cpe2 wg0 51820 192.168.99.2   transit 51821
#     T13  cpe1 wg1 51840 192.168.100.1 <-> cpe3 wg0 51840 192.168.100.2  transit 51841
#     T23  cpe2 wg1 51860 192.168.101.1 <-> cpe3 wg1 51860 192.168.101.2  transit 51861
#
#   路由：每台 CPE 的 inside 口在 VPP table 250（VRF 250，标签 16+250=266）。
#   静态 local prefix 是本站客户 LAN 10.68.N.0/24；客户路由器再通过 eBGP 宣告
#   自己的 LAN 和 loopback 172.16.N.1/32。盒内 FRR 用 route-map 给客户路由打
#   tag，sr-frr 只把带 tag 的 BGP 路由反向导入 sr-route 的 local prefixes；我方
#   导出到 table 1250 再进 BGP 的路由 tag=0，不会被反向吸回（防环）。
#
# 管理网 10.99.0.0/24 承载控制面（RR 的 mTLS、sr-route 的 /status、控制器 8444）
# 与各容器的第二跳，不进货数据面。数据面的 WAN/inside/xconn 都在容器内核或 VPP。
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
INSIDE_IP=( "" 10.78.1.2 10.78.2.2 10.78.3.2 )   # VPP 侧 veth（table 250）
FRR_IP=(  "" 10.78.1.20 10.78.2.20 10.78.3.20 )  # 内核侧 veth（盒内 FRR）
XCONN_IP=( "" 10.79.1.2 10.79.2.2 10.79.3.2 )    # 盒子 kernel 对客户路由器
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
VRF=250
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
    vpp_if_exists "$c" "$vif" ||
        docker exec "$c" vppctl create host-interface name "$ifn" >/dev/null ||
        die "$c: create host-interface $ifn 失败"
    kmac=$(docker exec "$c" cat "/sys/class/net/$ifn/address")
    vpp "$c" set interface mac address "$vif" "$kmac" >/dev/null
    vpp "$c" set interface state "$vif" up >/dev/null
    mtu=$(docker exec "$c" cat "/sys/class/net/$ifn/mtu")
    vpp "$c" set interface mtu packet "$mtu" "$vif" >/dev/null
    echo "$vif"
}

# 把内核上的 ip 搬进 VPP（可选放某张 table），并从内核摘掉。
take_ip() {
    local c=$1 vif=$2 ip=$3 pfx=$4 table=${5:-} kif
    kif=${vif#host-}
    if [ -n "$table" ]; then
        vpp "$c" set interface ip table "$vif" "$table" >/dev/null
    fi
    vpp_addr_has "$c" "$ip" || vpp "$c" set interface ip address "$vif" "$ip/$pfx" >/dev/null
    docker exec "$c" ip addr del "$ip/$pfx" dev "$kif" 2>/dev/null || true
    docker exec "$c" sh -c "grep -q '^$ip ' /tmp/lab-ifstate 2>/dev/null ||
                            echo '$ip $kif $pfx' >> /tmp/lab-ifstate"
}

# 盒内两半之间的 veth：vti0 留内核（10.78.N.20），vti1 给 VPP（10.78.N.2）。
ensure_veth() {   # <cpe> <n>
    local c=$1 n=$2
    docker exec "$c" ip link show vti0 >/dev/null 2>&1 ||
        docker exec "$c" ip link add vti0 type veth peer name vti1 >/dev/null ||
        die "$c: 建 veth 失败"
    docker exec "$c" ip link set vti0 up
    docker exec "$c" ip link set vti1 up
    docker exec "$c" ip -o -4 addr show dev vti0 | grep -q "${FRR_IP[$n]}/" ||
        docker exec "$c" ip addr add "${FRR_IP[$n]}/24" dev vti0 ||
        die "$c: 给 vti0 配 ${FRR_IP[$n]} 失败"
    if ! vpp_addr_has "$c" "${INSIDE_IP[$n]}"; then
        docker exec "$c" ip -o -4 addr show dev vti1 | grep -q "${INSIDE_IP[$n]}/" ||
            docker exec "$c" ip addr add "${INSIDE_IP[$n]}/24" dev vti1 ||
            die "$c: 给 vti1 配 ${INSIDE_IP[$n]} 失败"
    fi
}

# 内核转发只对盒内两半之间的链路开。VPP 的 host-interface 仍在内核里
# （AF_PACKET），WAN 口也会收到一份拷贝；那些口必须关转发，否则同一包会被
# 内核再转发一次（重复包会撞 WG replay window）。start-node.sh 会先把全局
# ip_forward 清零，所以这个函数要在 VPP 起来之后再跑一遍。
kernel_forwarding() {   # <cpe> <n>
    local c=$1 n=$2 xcif
    xcif=$(kernel_if_for_ip "$c" "${XCONN_IP[$n]}")
    [ -n "$xcif" ] || die "$c: 找不到持有 ${XCONN_IP[$n]} 的内核接口"
    docker exec "$c" bash -lc '
        set -e
        xcif='"$xcif"'
        sysctl -qw net.ipv4.ip_forward=1 net.ipv4.conf.all.forwarding=1 net.ipv4.conf.all.rp_filter=0
        for i in /sys/class/net/*; do
            [ -d "$i" ] || continue          # bonding_masters 是文件，不是网卡
            i=$(basename "$i"); [ "$i" = lo ] && continue
            case "$i" in
              "$xcif"|vti0) fwd=1 ;;
              *)            fwd=0 ;;
            esac
            sysctl -qw net.ipv4.conf."$i".forwarding=$fwd net.ipv4.conf."$i".rp_filter=0
        done' || die "$c: 设置内核转发失败"
}

# docker 塞的默认路由（distance 0）会压住 zebra 的 static(distance 1)，
# 客户流量就会跑去管理网桥。先删干净，再让 FRR 把默认路由装回内核。
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

wg_create() {   # <cpe> <if> <listen-port> <src-ip> <overlay/32>
    local c=$1 ifn=$2 port=$3 src=$4 ov=$5
    vpp_if_exists "$c" "$ifn" ||
        docker exec "$c" vppctl wireguard create listen-port "$port" generate-key src "$src" >/dev/null ||
        die "$c: wireguard create $ifn 失败"
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
! 盒内默认路由指向 VPP：客户侧流量的明细路由由 BGP 提供，其余一律进 VPP
! table 250（远端前缀在那里压标签走隧道；本站前缀由 sr-fib 经 FRR 回来）。
ip route 0.0.0.0/0 ${INSIDE_IP[$n]}
!
EOF
        # 客户路由器：只宣告自己的 LAN 与 loopback。loopback /32 是"反向导入"
        # 的探针：它不在 bringup 的 local_prefixes 里，只能经 BGP 学到。
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
        # veth 必须在 FRR 起来之前就位：static 默认路由的下一跳 10.78.N.2
        # 只有 veth 起来才可达，zebra 的 NHT 才会把默认路由装进内核。
        ensure_veth "$c" "$n"
        kernel_forwarding "$c" "$n"
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
        wait_default_via "$c" "${INSIDE_IP[$n]}" "盒内默认路由应指向 VPP"
        note "$c: FRR 10.7.1 + VPP 同盒，BGP AS ${BOX_AS[$n]}，默认路由 → ${INSIDE_IP[$n]}"
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
        docker exec "$c" bash -lc 'pkill -9 -x vpp_main; pkill -9 -f "script -qefc vpp"; sleep 1; rm -f /run/vpp/*.sock' >/dev/null 2>&1
        docker exec -d "$c" bash -lc "bash /workspace/test/docker/start-node.sh ${c}"
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
        note "$c: VPP + sr_wg OK"
    done
    # start-node.sh 为了防重复包把 ip_forward 清零了；盒内两半之间的转发要开回来。
    local n
    for n in 1 2 3; do kernel_forwarding "lab-cpe$n" "$n"; done
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

    log "enroll: 启动 sr-ctrl（CA + 8444/mTLS）"
    stop_proc lab-ctrl sr-ctrl
    docker exec -d lab-ctrl sh -c "mkdir -p /var/lib/sr-ctrl && /usr/local/bin/sr-ctrl \
        -data /var/lib/sr-ctrl -listen 0.0.0.0:${CTRL_PORT} \
        -tls-san ${CTRL_ADDR},10.88.1.200,lab-ctrl -rr rr-a=${RR_ADDR}:${RR_PORT} \
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
    log "dataplane: CPE 的 underlay / inside veth / 隧道（每隧道一个端口 + overlay /32）"
    local n c wan ins
    for n in 1 2 3; do
        c=lab-cpe$n
        ensure_veth "$c" "$n"
        vpp "$c" sr-wg key set 1 "$SR_KEY" >/dev/null
        vpp "$c" mpls table add 0 >/dev/null
        vpp "$c" ip table add "$VRF" >/dev/null
        wan=$(ensure_host_if "$c" "${WAN_IP[$n]}" 24)
        take_ip "$c" "$wan" "${WAN_IP[$n]}" 24
        ins=$(ensure_host_if "$c" "${INSIDE_IP[$n]}" 24)
        take_ip "$c" "$ins" "${INSIDE_IP[$n]}" 24 "$VRF"
        kernel_forwarding "$c" "$n"
        note "$c: wan=$wan(${WAN_IP[$n]}) inside=$ins(${INSIDE_IP[$n]}, table $VRF) kernel↔FRR ${FRR_IP[$n]}"
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
        vpp_fib_has "$ca" "$bov/32" || vpp "$ca" ip route add "$bov/32" via "$aif" >/dev/null
        vpp_fib_has "$cb" "$aov/32" || vpp "$cb" ip route add "$aov/32" via "$bif" >/dev/null
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
    log "control: sr-route（静态 local 10.68.N.0/24）+ sr-fib（table $VRF，标签 266）"
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

        local ins
        ins=$(vpp_ifname_for_ip "$c" "${INSIDE_IP[$n]}")
        [ -n "$ins" ] || die "$c: 找不到持有 ${INSIDE_IP[$n]} 的 VPP 接口"

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
  "vrfs": [{"id": $VRF, "export_rts": ["65000:$VRF"], "import_rts": ["65000:$VRF"]}],
  "local_prefixes": [{"vrf_id": $VRF, "prefix": "${LAN_PFX[$n]}"}],
  "status_addr": "0.0.0.0:${ROUTE_STATUS[$n]}"
}
EOF
        # local 路径是盒内 FRR：这些前缀在 VPP 里没有 overlay 下一跳，也不压
        # 标签——报文交给内核，由 FRR/客户路由器继续转发。
        cat > "$RUN/cpe$n.fib.json" <<EOF
{
  "status_url": "http://127.0.0.1:${ROUTE_STATUS[$n]}/status",
  "vpp_socket": "/run/vpp/api.sock",
  "poll_sec": 1,
  "vrfs": [$VRF],
  "tunnels": [$fib_tun],
  "local": {"next_hop": "${FRR_IP[$n]}", "interface": "$ins"}
}
EOF
        docker cp "$RUN/cpe$n.route.json" "$c:/etc/sdwan/route.json" >/dev/null
        docker cp "$RUN/cpe$n.fib.json" "$c:/etc/sdwan/fib.json" >/dev/null
        stop_proc "$c" sr-route
        stop_proc "$c" sr-fib
        docker exec -d "$c" sh -c '/usr/local/bin/sr-route -config /etc/sdwan/route.json > /var/log/sr-route.log 2>&1'
    done

    local i remote
    for n in 1 2 3; do
        c=lab-cpe$n
        for i in $(seq 1 40); do
            docker exec "$c" curl -sf --max-time 2 "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" >/dev/null 2>&1 && break
            sleep 0.5
        done
        docker exec "$c" curl -sf --max-time 2 "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" >/dev/null 2>&1 ||
            die "$c: sr-route 状态口没起来（cat /var/log/sr-route.log）"
    done
    # 等三台都从 RR 收到另外两个站点。Go 的 encoding/json 是紧凑输出（冒号后没有
    # 空格），所以这里必须容忍 0..n 个空格，别按"好看"的 JSON 写死。
    for n in 1 2 3; do
        c=lab-cpe$n
        for i in $(seq 1 60); do
            local have=0
            for remote in 1 2 3; do
                [ "$remote" = "$n" ] && continue
                docker exec "$c" curl -sf "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" |
                    grep -Eq "\"prefix\":[[:space:]]*\"${LAN_PFX[$remote]}\"" && have=$((have + 1))
            done
            [ "$have" = "2" ] && break
            sleep 0.5
        done
        docker exec "$c" curl -sf "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" |
            grep -Eq "\"active_rr\":[[:space:]]*0" ||
            die "$c: 没有和 rr-a 同步（cat /var/log/sr-route.log）"
        note "$c: RIB 已收齐另两个站点"
    done

    for n in 1 2 3; do
        c=lab-cpe$n
        docker exec -d "$c" sh -c '/usr/local/bin/sr-fib -config /etc/sdwan/fib.json > /var/log/sr-fib.log 2>&1'
    done
    for n in 1 2 3; do
        c=lab-cpe$n
        for i in $(seq 1 40); do
            local got=0
            for remote in 1 2 3; do
                [ "$remote" = "$n" ] && continue
                vpp "$c" show ip fib table "$VRF" | grep -q "^${LAN_PFX[$remote]}" && got=$((got + 1))
            done
            [ "$got" = "2" ] && break
            sleep 0.5
        done
        for remote in 1 2 3; do
            [ "$remote" = "$n" ] && continue
            vpp "$c" show ip fib table "$VRF" | grep -q "^${LAN_PFX[$remote]}" ||
                die "$c: table $VRF 里没有 ${LAN_PFX[$remote]}（cat /var/log/sr-fib.log）"
        done
        # 本站客户 LAN 也必须由 sr-fib 的 local 路径装进 table 250（不压标签、
        # 下一跳是盒内 FRR），否则远端解封装回来的包没有出口。
        vpp "$c" show ip fib table "$VRF" "${LAN_PFX[$n]}" | grep -q "dpo-drop" &&
            die "$c: 本地 ${LAN_PFX[$n]} 解析成 dpo-drop（看 /var/log/sr-fib.log）"
        vpp "$c" show ip fib table "$VRF" "${LAN_PFX[$n]}" | grep -q "${FRR_IP[$n]}" ||
            die "$c: 本地 ${LAN_PFX[$n]} 没有经 ${FRR_IP[$n]} 转发"
        note "$c: FIB 已装好（远端压 266；本地 10.68.$n.0/24 → ${FRR_IP[$n]}）"
    done
}

# ==================================================================
stage_frr() {
    log "frr: sr-frr 正向导出 table $FRR_TABLE，反向把客户 BGP 路由导入 local prefixes"
    local n c i
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
  "vrfs": [{"vrf_id": $VRF, "table_id": $FRR_TABLE}],
  "import": {
    "vrf_id": $VRF,
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

    local remote have
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
        # 正向导入的边界：BGP 与客户路由器必须 Established。
        for i in $(seq 1 40); do
            vtysh "$c" -c "show bgp neighbors ${CUST_X_IP[$n]}" | grep -q 'BGP state = Established' && break
            sleep 0.5
        done
        vtysh "$c" -c "show bgp neighbors ${CUST_X_IP[$n]}" | grep -q 'BGP state = Established' ||
            die "$c: 与客户路由器 ${CUST_X_IP[$n]} 的 BGP 没建立（vtysh -c 'show bgp summary'）"
        # 反向导入的边界：客户宣告的 loopback 已经进 sr-route 的 local 集合。
        for i in $(seq 1 60); do
            docker exec "$c" curl -sf "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" |
                grep -Eq "\"prefix\":[[:space:]]*\"${LOOP_IP[$n]}/32\"" && break
            sleep 0.5
        done
        docker exec "$c" curl -sf "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" |
            grep -Eq "\"prefix\":[[:space:]]*\"${LOOP_IP[$n]}/32\"" ||
            die "$c: 客户 loopback ${LOOP_IP[$n]}/32 没有反向导入 sr-route（cat /var/log/sr-frr.log）"
        note "$c: table $FRR_TABLE 有远端前缀；BGP 与 cust$n Established；已反向导入 ${LOOP_IP[$n]}/32"
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
