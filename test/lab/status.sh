#!/bin/bash
# status.sh - 打印 lab 的当前状态快照（只读，不做任何修改）
#
# 排障时先看这里：哪一层没起来、隧道绑定可用几条、RIB 里有什么、盒内 FRR
# 和客户路由器各自看到什么。深入某一层再去对应容器里敲命令（login 方式见
# README.md）。
#
# 每台 lab-cpeN 是一整台盒子：VPP 转发全部数据（WAN 口与客户口都在 VPP 里），
# FRR 只算路由（BGP 跑在 linux-cp 送出的内核 TAP lcp-x0 上）。盒内核不转发。
# 客户路由器是独立的 lab-custN。
set -u

VPN=0            # 客户 VPN ID，直接当 VPP 表号
OVERLAY=250      # 隧道与 overlay 下一跳所在的表
FRR_TABLE=1250   # FRR 的远端前缀停车场（镜像进 VPP 后全是 dpo-drop）
LCP_IF=lcp-x0
ROUTE_STATUS=( "" 19001 19002 19003 )
RR_STATUS=18447
CTRL_PORT=8444

vppc(){ local c=$1; shift; docker exec "$c" vppctl "$@" 2>&1 | tr -d '\r'; }
vtysh(){ local c=$1; shift; docker exec "$c" vtysh "$@" 2>&1 | tr -d '\r'; }
hr() { echo "------------------------------------------------------------"; }

hr
echo "容器"
docker ps --filter 'name=lab-' --format '  {{.Names}}\t{{.Status}}' | sort
echo
hr
echo "VPP / sr-wg 端口"
for c in lab-cpe1 lab-cpe2 lab-cpe3 lab-transit; do
    ver=$(vppc "$c" show version 2>/dev/null | head -1)
    [ -n "$ver" ] || { echo "  $c: VPP 不可用"; continue; }
    echo "  $c: $ver"
    vppc "$c" show sr-wg ports | sed 's/^/    /'
done
echo
hr
echo "隧道绑定（show sr-wg tunnels）"
for c in lab-cpe1 lab-cpe2 lab-cpe3; do
    echo "  $c:"
    vppc "$c" show sr-wg tunnels | sed 's/^/    /'
    echo "    peers: $(vppc "$c" show wireguard peer | grep -c 'flags: 2') established / $(vppc "$c" show wireguard peer | grep -c '^\[[0-9]*\] endpoint:') total"
done
echo
hr
echo "盒子接缝（linux-cp：FRR 在 $LCP_IF 上跑路由，VPP 转发全部数据）"
for n in 1 2 3; do
    c=lab-cpe$n
    echo "  $c:"
    vppc "$c" show lcp | sed 's/^/    /'
    docker exec "$c" sh -c 'printf "    ip_forward=%s\n" "$(cat /proc/sys/net/ipv4/ip_forward)"' 2>/dev/null
    docker exec "$c" ip -o -4 addr show dev "$LCP_IF" 2>/dev/null |
        awk '{print "    '"$LCP_IF"': "$4}'
    echo "    内核主表（linux-nl 的输入）："
    docker exec "$c" ip -4 route 2>/dev/null | sed 's/^/      /'
done
echo
hr
echo "控制面"
echo "  sr-ctrl : https://10.99.0.200:$CTRL_PORT"
docker exec lab-rr curl -sf "http://127.0.0.1:$RR_STATUS/status" 2>/dev/null |
    sed 's/^/  sr-rr   : /' | head -c 1200; echo
for n in 1 2 3; do
    echo "  lab-cpe$n /status:"
    docker exec "lab-cpe$n" curl -sf "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" 2>/dev/null |
        sed 's/^/    /' | head -c 1200; echo
done
echo
hr
echo "VPP FIB：table $VPN（客户 VPN + WAN/客户口）· table $OVERLAY（隧道/overlay）"
echo "          · table $FRR_TABLE（FRR 停车场镜像，全 dpo-drop）"
for n in 1 2 3; do
    echo "  lab-cpe$n:"
    vppc "lab-cpe$n" show ip fib table "$VPN" | head -16 | sed 's/^/    '"$VPN"': /'
    vppc "lab-cpe$n" show ip fib table "$OVERLAY" | head -6 | sed "s/^/    $OVERLAY: /"
    vppc "lab-cpe$n" show ip fib table "$FRR_TABLE" | head -6 | sed "s/^/    $FRR_TABLE: /"
done
echo
hr
echo "盒内 FRR（lab-cpeN：BGP 跑在 $LCP_IF 上；正向导出 table $FRR_TABLE / 反向吃 tag 1001）"
for n in 1 2 3; do
    c=lab-cpe$n
    echo "  $c:"
    vtysh "$c" -c "show ip route table $FRR_TABLE" 2>/dev/null |
        grep -E '^[TBCSKL][>*]' | sed 's/^/    table '"$FRR_TABLE"': /'
    vtysh "$c" -c 'show bgp summary' 2>/dev/null |
        awk '/^Neighbor/{h=1;next} h&&NF{print "    bgp: "$0}'
done
echo
hr
echo "客户路由器（lab-custN：只跑 BGP 和一条默认路由）"
for n in 1 2 3; do
    c=lab-cust$n
    echo "  $c:"
    vtysh "$c" -c 'show bgp summary' 2>/dev/null |
        awk '/^Neighbor/{h=1;next} h&&NF{print "    bgp: "$0}'
    vtysh "$c" -c 'show ip route' 2>/dev/null |
        grep -E '^(B|S)\*' | sed 's/^/    /'
done
echo
hr
echo "sr-wg 计数器"
for c in lab-cpe1 lab-cpe2 lab-cpe3 lab-transit; do
    echo "  $c:"
    vppc "$c" show sr-wg stats 2>/dev/null | sed 's/^/    /'
done
