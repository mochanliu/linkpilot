#!/bin/bash
# status.sh - 打印 lab 的当前状态快照（只读，不做任何修改）
#
# 排障时先看这里：哪一层没起来、隧道绑定可用几条、RIB 里有什么、盒内 FRR
# 和客户路由器各自看到什么。深入某一层再去对应容器里敲命令（login 方式见
# README.md）。
#
# 每台 lab-cpeN 是一整台盒子：VPP + 盒内 FRR + sr-route/sr-fib/sr-frr，
# 客户路由器是独立的 lab-custN。
set -u

VPN=0            # 客户 VPN ID，直接当 VPP 表号
OVERLAY=250      # 隧道与 overlay 下一跳所在的表
FRR_TABLE=1250
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
echo "VPP FIB table $VPN（客户 VPN）+ table $OVERLAY（overlay 隧道/下一跳）"
for n in 1 2 3; do
    echo "  lab-cpe$n:"
    vppc "lab-cpe$n" show ip fib table "$VPN" | head -12 | sed 's/^/    0: /'
    vppc "lab-cpe$n" show ip fib table "$OVERLAY" | head -6 | sed "s/^/    $OVERLAY: /"
done
echo
hr
echo "盒内 FRR（lab-cpeN：正向导出 table $FRR_TABLE / 反向吃 tag 1001）"
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
