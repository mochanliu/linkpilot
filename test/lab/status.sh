#!/bin/bash
# status.sh - 打印 lab 的当前状态快照（只读，不做任何修改）
#
# 排障时先看这里：哪一层没起来、隧道绑定可用几条、RIB 里有什么、客户侧看到
# 什么。深入某一层再去对应容器里敲命令（login 方式见 README.md）。
set -u

VRF=250
ROUTE_STATUS=( "" 19001 19002 19003 )
RR_STATUS=18447
CTRL_PORT=8444

vppc(){ local c=$1; shift; docker exec "$c" vppctl "$@" 2>&1 | tr -d '\r'; }
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
echo "VPP FIB table $VRF"
for n in 1 2 3; do
    echo "  lab-cpe$n:"
    vppc "lab-cpe$n" show ip fib table "$VRF" | head -12 | sed 's/^/    /'
done
echo
hr
echo "FRR 客户侧"
for n in 1 2 3; do
    echo "  lab-frr$n:"
    docker exec "lab-frr$n" vtysh -c 'show ip route table 1250' 2>/dev/null |
        grep -E '10\.68\.' | sed 's/^/    /'
    echo "    BGP: $(docker exec "lab-frr$n" vtysh -c 'show bgp ipv4 unicast' 2>/dev/null | grep -c '\*>') 条"
done
echo
hr
echo "sr-wg 计数器"
for c in lab-cpe1 lab-cpe2 lab-cpe3 lab-transit; do
    echo "  $c:"
    vppc "$c" show sr-wg stats 2>/dev/null | sed 's/^/    /'
done
