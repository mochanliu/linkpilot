#!/bin/bash
# perf-rig.sh - 在既有三节点 SR-WG lab 之上追加「终端-CPE-中转-CPE-终端」测带宽台
#
# 前置：先跑 test/scripts/test-e2e.sh --skip-build --up-only（WG 接口、SR 路径、
#       绑定、encap 都由它配好）；本脚本只做加法，不重建已有容器和网络。
#
# 加出来的拓扑（全部通过 docker network create/connect，不动 net_a/net_b/net_ctrl）:
#
#   term-a ── srwg_edge_a ──► node1 ══ net_a ══ transit ══ net_b ══ node2 ── srwg_edge_b ── term-b
#  10.20.1.3    10.20.1.0/24  10.20.1.2          中转             10.20.2.2   10.20.2.0/24  10.20.2.3
#
# node1/node2 的 edge 口交给 VPP，终端之间的包是「被转发」的，因此会真的走过
# sr-wg4-encap -> transit -> sr-wg4-input 全链（终端自己不参与隧道）。
#
# 另外还会把两台终端挂到一条不经 VPP 的直连桥 srwg_direct（10.30.0.0/24）上，
# 供 perf-bench.sh 的控制组用：同终端、同载荷、隧道不掺和，先量出台子的余量，
# 才能说「跑不上去」不是 iperf3/终端的锅。
#
# 两个必须写死的坑（本轮实测踩到）：
#   1. `ip route add <net> via wg0` 会得到 dpo-drop（WireGuard 的 adj 解析不出来），
#      必须写 `via <对端 overlay IP> <wg 接口>`，例如 `via 192.168.99.2 wg0`。
#   2. peer 的 allowed-ip 只填 overlay /32 时，转发的 10.20.x 流量落在 drop 上；
#      按定案把 allowed-ip 恒置 0.0.0.0/0。
#      2026-10-24 定到根因：allowed-ips 不参与转发（不装路由），但 wg4-input
#      拿它过滤**解密后的内层源地址**，不匹配就 error-punt 到 null-node
#      （show errors 只涨 null-node blackholed）。隧道自测 ping 的源恰好就是对端
#      /32，所以自测全绿、终端之间不通。setup-node.sh 已改成常量。
#   3. 终端网卡默认开着 checksum offload + GSO，包经 af-packet 交给 VPP 转发后
#      内层 TCP 校验和没算好，对端静默丢弃（ICMP 不受影响，TCP 全丢）。
#      `ethtool -K eth1 tx off rx off tso off gso off gro off` 之后才通。
#   4. 隧道路径 MTU 预算：外层 = 内层 + WG(80) + SR(16+6*seg+8)。underlay veth 是
#      1500，VPP 侧接口却报 9000，不会分片、直接被内核丢。所以给终端的路由加
#      `mtu ${PATH_MTU}`（默认 1360）把内层压到预算内。
#   5. af-packet 的 TX 楔死：show errors 里出现 `host-*-tx tx sendto fatal/temporary
#      failure`。症状很误导人——VPP 主动 ping 终端是通的（RX 还活着），但转发方向
#      全丢，终端只收到「Destination Host Unreachable」。自愈办法就是本脚本第 4 步
#      的 delete + create host-interface，所以自检会把这条报错单独打印出来。
#
# 用法:
#   test/scripts/perf-rig.sh            # 搭台 + 连通性自检
#   test/scripts/perf-rig.sh --pin      # 额外把两台 CPE 锁到 2 vCPU / 2 GB
#   test/scripts/perf-rig.sh --teardown # 删终端与 edge 网段（不碰主 lab）
set -u

EDGE_A_NET=srwg_edge_a;  EDGE_A_SUBNET=10.20.1.0/24
EDGE_B_NET=srwg_edge_b;  EDGE_B_SUBNET=10.20.2.0/24
CPE_A_EDGE_IP=10.20.1.2; TERM_A_IP=10.20.1.3
CPE_B_EDGE_IP=10.20.2.2; TERM_B_IP=10.20.2.3
CPE_A=srwg-node1; CPE_B=srwg-node2
TERM_A=srwg-term-a; TERM_B=srwg-term-b
# 直连控制桥：只把两台终端连起来、不经 VPP，给 perf-bench.sh 的控制组用。
# Docker 把子网的 .1 保留成网关，所以 term-b 只能往后面挑（实测 10.30.0.2）。
DIRECT_NET=srwg_direct; DIRECT_SUBNET=10.30.0.0/24
DIRECT_A_IP=10.30.0.11; DIRECT_B_IP=10.30.0.2
OVERLAY_A=192.168.99.1; OVERLAY_B=192.168.99.2
PATH_MTU=${PATH_MTU:-1360}
CPE_CPUS=${CPE_CPUS:-2}; CPE_MEM=${CPE_MEM:-2g}
PIN=0; TEARDOWN=0
for a in "$@"; do
    [ "$a" = "--pin" ] && PIN=1
    [ "$a" = "--teardown" ] && TEARDOWN=1
done

log() { printf '[perf-rig] %s\n' "$*"; }
die() { printf '[perf-rig] FATAL: %s\n' "$*" >&2; exit 1; }

if [ "${TEARDOWN}" = "1" ]; then
    docker rm -f "${TERM_A}" "${TERM_B}" >/dev/null 2>&1 || true
    docker network rm "${EDGE_A_NET}" "${EDGE_B_NET}" "${DIRECT_NET}" >/dev/null 2>&1 || true
    log "teardown done (主 lab 未动)"
    exit 0
fi

# ---- 0. 前置检查：主 lab 的 SR 隧道必须已经配好 ----
docker exec "${CPE_A}" vppctl show sr-wg tunnels 2>/dev/null | grep -q 'usable' ||
    die "node1 上没有可用的 SR 绑定，先跑 test-e2e.sh --skip-build --up-only"

# ---- 1. edge 网段（关 masquerade，理由见 docker-compose.yml 注释）----
docker network inspect "${EDGE_A_NET}" >/dev/null 2>&1 ||
    docker network create --driver bridge --subnet "${EDGE_A_SUBNET}" \
        -o com.docker.network.bridge.enable_ip_masquerade=false "${EDGE_A_NET}" >/dev/null
docker network inspect "${EDGE_B_NET}" >/dev/null 2>&1 ||
    docker network create --driver bridge --subnet "${EDGE_B_SUBNET}" \
        -o com.docker.network.bridge.enable_ip_masquerade=false "${EDGE_B_NET}" >/dev/null

NET_CTRL=$(docker network ls --format '{{.Name}}' | grep -E 'net_ctrl$' | head -1)
[ -n "${NET_CTRL}" ] || die "找不到控制面网段（容器要装 iperf3/ethtool）"

# ---- 2. 终端容器 ----
ensure_term() { # $1=name $2=edge net $3=ip
    docker inspect "$1" >/dev/null 2>&1 ||
        docker run -d --name "$1" --hostname "term-$1" --privileged \
            --cap-add NET_ADMIN --network "${NET_CTRL}" srwg-dev:latest sleep infinity >/dev/null
    docker network ls --format '{{.Name}}' | grep -qx "$2" || die "网段 $2 不存在"
    docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{"\n"}}{{end}}' "$1" |
        grep -qx "$2" || docker network connect --ip "$3" "$2" "$1"
}
ensure_term "${TERM_A}" "${EDGE_A_NET}" "${TERM_A_IP}"
ensure_term "${TERM_B}" "${EDGE_B_NET}" "${TERM_B_IP}"

# ---- 2b. 直连控制桥（不经 VPP 的那一条，perf-bench.sh 控制组要用）----
# 没有它，控制组只能走 edge 网段——那条路一定经过 node1 的 af-packet，
# 「终端不是瓶颈」就没法证明了。
docker network inspect "${DIRECT_NET}" >/dev/null 2>&1 ||
    docker network create --driver bridge --subnet "${DIRECT_SUBNET}" "${DIRECT_NET}" >/dev/null
attach_direct() { # $1=container $2=ip
    docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{"\n"}}{{end}}' "$1" |
        grep -qx "${DIRECT_NET}" || docker network connect --ip "$2" "${DIRECT_NET}" "$1"
}
attach_direct "${TERM_A}" "${DIRECT_A_IP}"
attach_direct "${TERM_B}" "${DIRECT_B_IP}"

# CPE 也要接上 edge 网段。docker network connect 是「运行时附加」，不在
# docker-compose.yml 里，容器 restart / compose 重建后就掉了；更坑的是 Docker 的
# 「已附加」视图可能还在、容器里的网卡却没有地址。统一交给 deliver_edge 判断。

# 终端的 edge 口在容器里叫什么：按 IP 反查，别写死 eth1
term_edge_if() { # $1=container $2=ip
    docker exec "$1" bash -c "ip -o -4 addr show | awk '/$2/ {print \$2; exit}'"
}
A_IF=$(term_edge_if "${TERM_A}" "${TERM_A_IP}")
B_IF=$(term_edge_if "${TERM_B}" "${TERM_B_IP}")
[ -n "${A_IF}" ] && [ -n "${B_IF}" ] || die "终端上找不到 edge 网卡"

# ---- 3. 终端装 iperf3/ethtool + 关 offload + 指路由 ----
prep_term() { # $1=container $2=if $3=via $4=remote net
    docker exec "$1" bash -c "
        command -v iperf3 >/dev/null || { apt-get update -q >/dev/null 2>&1; apt-get install -y -q iperf3 ethtool >/dev/null 2>&1; }
        command -v ethtool >/dev/null || { apt-get update -q >/dev/null 2>&1; apt-get install -y -q ethtool >/dev/null 2>&1; }
        ethtool -K $2 tx off rx off tso off gso off gro off >/dev/null 2>&1 || true
        ip route replace $4 via $3 dev $2 mtu ${PATH_MTU}
    " || die "终端 $1 准备失败"
}
prep_term "${TERM_A}" "${A_IF}" "${CPE_A_EDGE_IP}" "${EDGE_B_SUBNET}"
prep_term "${TERM_B}" "${B_IF}" "${CPE_B_EDGE_IP}" "${EDGE_A_SUBNET}"

# 控制组的 iperf3 服务端常驻在 term-b（要等上一步装好 iperf3）。--daemon 重复起会
# 报错，所以先探活；perf-bench.sh 里还有一份同样的兜底。
docker exec "${TERM_B}" sh -c 'pgrep -x iperf3 >/dev/null || iperf3 -s -p 5201 --daemon' \
    >/dev/null 2>&1 || true

# ---- 4. CPE 的 edge 口交给 VPP ----
# 顺序必须是「先建 VIF + 同步 MAC + 配 IP，再摘掉内核上的同一个 IP」，否则内核和
# VPP 同时持有地址：内核抢答 ARP，VPP 的 host-*-tx 报 tx sendto fatal failure。
# 每轮都 delete + create：docker 重插网卡会换掉 netdev，VPP 里旧 VIF 仍绑着已消失
# 的 netdev，收发全哑。edge 口上没有 SR 绑定，删重建不影响隧道。
deliver_edge_once() { # $1=container $2=edge ip $3=remote net $4=overlay next-hop
    docker exec "$1" bash -c "
        set -e
        HIF=\$(ip -o -4 addr show | awk -v ip='$2' '\$0 ~ ip\"/\" {print \$2; exit}')
        if [ -z \"\${HIF}\" ]; then
            # 内核不持有：多半是上一轮已经交给了 VPP。反查 VPP 用的是哪块网卡，
            # 把地址临时加回内核，再走完整的 delete+create 流程自愈。
            VIF=\$(vppctl show interface address |
                   awk -v ip='$2' '/^host-/{n=\$1} \$0 ~ ip\"/\" {print n; exit}')
            [ -n \"\${VIF}\" ] || { echo 'nobody holds $2' >&2; exit 3; }
            HIF=\${VIF#host-}
            ip link show \"\${HIF}\" >/dev/null 2>&1 || { echo \"netdev \${HIF} gone\" >&2; exit 4; }
            ip addr add $2/24 dev \"\${HIF}\"
        fi
        vppctl delete host-interface name \"\${HIF}\" >/dev/null 2>&1 || true
        vppctl create host-interface name \"\${HIF}\" >/dev/null
        VIF=host-\${HIF}
        KMAC=\$(ip link show \"\${HIF}\" | awk '/link\/ether/{print \$2}')
        vppctl set interface mac address \"\${VIF}\" \"\${KMAC}\" >/dev/null 2>&1 || true
        vppctl set interface state \"\${VIF}\" up
        vppctl set interface ip address \"\${VIF}\" $2/24
        ip addr del $2/24 dev \"\${HIF}\"
        # dpo 必须指向 overlay 下一跳，见文件头坑 1
        vppctl ip route add $3 via $4 wg0 >/dev/null 2>&1 || true
    "
}
deliver_edge() { # $1=container $2=net $3=edge ip $4=remote net $5=overlay next-hop
    deliver_edge_once "$1" "$3" "$4" "$5" && return 0
    # exit 3/4 = 地址谁也不持有或 netdev 已消失：让 docker 重新插一次网卡再来
    docker network disconnect "$2" "$1" >/dev/null 2>&1 || true
    docker network connect --ip "$3" "$2" "$1"
    deliver_edge_once "$1" "$3" "$4" "$5" || die "CPE $1 的 edge 口交付失败"
}
deliver_edge "${CPE_A}" "${EDGE_A_NET}" "${CPE_A_EDGE_IP}" "${EDGE_B_SUBNET}" "${OVERLAY_B}"
deliver_edge "${CPE_B}" "${EDGE_B_NET}" "${CPE_B_EDGE_IP}" "${EDGE_A_SUBNET}" "${OVERLAY_A}"

# ---- 5. peer allowed-ip 恒 0.0.0.0/0（定案，见文件头坑 2）----
# 对端公钥从本机 peer 表里读，并和「本机自己的接口公钥」比对，防止上一轮把
# 自己的键当对端键写进去（本轮实测踩过：写错后 peer flags 永远不到 2）。
force_wildcard_allowed_ip() { # $1=container $2=对端 underlay IP
    docker exec "$1" bash -c "
        OWN=\$(vppctl show wireguard interface | grep -m1 -oE 'public-key:[A-Za-z0-9+/=]+' | cut -d: -f2)
        PEER=\$(vppctl show wireguard peer | grep -m1 -oE 'public key:[A-Za-z0-9+/=]+' | cut -d: -f2)
        if [ -z \"\${PEER}\" ]; then echo 'peer 表为空，交给 test-e2e.sh 重建' >&2; exit 1; fi
        if [ \"\${PEER}\" = \"\${OWN}\" ]; then echo 'peer 里填的是本机公钥' >&2; exit 1; fi
        vppctl show wireguard peer | grep -q 'allowed-ips: 0.0.0.0/0' && exit 0
        vppctl wireguard peer remove 0 >/dev/null 2>&1 || true
        vppctl wireguard peer add wg0 public-key \"\${PEER}\" endpoint $2 \\
            allowed-ip 0.0.0.0/0 dst-port 51820 persistent-keepalive 5
    " || die "CPE $1 的 allowed-ip 改造失败"
}
force_wildcard_allowed_ip "${CPE_A}" 10.10.2.2
force_wildcard_allowed_ip "${CPE_B}" 10.10.1.2
sleep 5

# ---- 6. 自检 ----
# peer 是刚重加的，握手要跑满一个 keepalive 周期；先等 flags:2，再连打两轮
# （第一轮全丢是冷启动的正常现象，不能当故障）。
for c in "${CPE_A}" "${CPE_B}"; do
    for i in 1 2 3 4 5 6; do
        docker exec "$c" vppctl show wireguard peer 2>/dev/null | grep -q 'flags: 2' && break
        sleep 5
    done
    docker exec "$c" vppctl show wireguard peer | grep -q 'flags: 2' ||
        log "警告：$c 的 WG peer 未 ESTABLISHED"
done
RT=$(docker exec "${TERM_A}" bash -c "ping -c 4 -W 2 ${DIRECT_B_IP} 2>&1 | tail -2 | head -1")
log "直连桥（不经 VPP，控制组走这条路）: ${RT}"
docker exec "${TERM_A}" bash -c "ping -c 3 -W 2 ${TERM_B_IP} >/dev/null 2>&1" || true
RT=$(docker exec "${TERM_A}" bash -c "ping -c 4 -W 2 ${TERM_B_IP} 2>&1 | tail -2 | head -1")
log "term-a -> term-b: ${RT}"
echo "${RT}" | grep -q '0% packet loss' ||
    log "注意：第一轮已发过探测包，若仍不通请检查 CPE 上 host-*-tx 的 sendto 报错"
docker exec "${TERM_A}" bash -c "ping -c 2 -W 2 ${TERM_B_IP} >/dev/null 2>&1"
# 见文件头坑 5：TX 楔死时光看 ping 结果不够，必须单独查 sendto 报错
for c in "${CPE_A}" "${CPE_B}"; do
    WEDGE=$(docker exec "$c" vppctl show errors 2>/dev/null | grep -E 'tx sendto' | head -2)
    [ -z "${WEDGE}" ] || log "警告：${c} 的 af-packet TX 有 sendto 报错 -> ${WEDGE}"
done
TA=$(docker exec "${CPE_A}" vppctl show sr-wg stats | awk '/Encap:/{f=1} f && /ok:/{print $2; exit}')
log "node1 Encap ok 累计=${TA}（>0 说明转发流量真的走了 SR 全链）"

if [ "${PIN}" = "1" ]; then
    # --memory 必须和 --memory-swap 一起给，否则 docker 报错
    docker update --cpus="${CPE_CPUS}" --memory="${CPE_MEM}" --memory-swap="${CPE_MEM}" \
        "${CPE_A}" "${CPE_B}" >/dev/null || die "docker update 失败"
    for c in "${CPE_A}" "${CPE_B}"; do
        log "${c} cpu.max=$(docker exec $c cat /sys/fs/cgroup/cpu.max) mem.max=$(docker exec $c cat /sys/fs/cgroup/memory.max)"
    done
fi
log "测带宽台就绪：iperf3 -c ${TERM_B_IP} 请在 ${TERM_A} 上发起"
