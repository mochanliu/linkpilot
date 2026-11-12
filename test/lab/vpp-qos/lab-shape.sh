#!/bin/bash
# ---------------------------------------------------------------------------
# 隧道 QoS 4 队列整形的自测（在 srwg-dev 容器里跑，不碰 CPE4）
#
# 拓扑：
#
#   kernel 10.99.1.2/24 -- q1 ~~ veth ~~ q0 -- host-q0 (VPP)
#                                              |
#                                        ip4 转发 / ip4-output 弧
#                                              |
#   netns far 10.99.2.2/24 -- q3 ~~ veth ~~ q2 -- host-q2 (VPP)   ← 整形挂这里
#
# 对端必须放在独立 netns：同一个 netns 时内核走本地投递，包根本不进 VPP
# （第一版就是这么错的：基线"通"，但 VPP 计数全 0）。
#
# 判据说明：容器的 `ping` 统计不可信（-c 2 会报 "10 transmitted"），所以整形
# 速率一律用 `show sr-wg qos` 的 tx_bytes **增量**来量，不看 ping 的 rtt。
#
# 用法（宿主机，仓库根目录）：
#   docker run --rm --privileged -v "$PWD":/repo -w /repo srwg-dev:latest \
#     bash test/lab/vpp-qos/lab-shape.sh
# ---------------------------------------------------------------------------
set -u

PLUGIN=${PLUGIN:-/repo/vpp-plugin/build-qos/lib/vpp_plugins/sr_wg_plugin.so}
PDIR=/usr/lib/x86_64-linux-gnu/vpp_plugins
DST=10.99.2.2
SRC=10.99.1.2
VPID=""

log () { echo; echo "### $*"; }

cleanup () {
  [ -n "$VPID" ] && kill "$VPID" 2>/dev/null
  sleep 1
  ip link del q0 2>/dev/null
  ip link del q2 2>/dev/null
  ip netns del far 2>/dev/null
  return 0
}
trap cleanup EXIT

# show sr-wg qos 的队列行：
#   q2 DEF   tx 135 包 / 125750 B   drop 0   backlog 0/128
#    $1        $3 $4  $5  $6  $7     $9  $10   $11    $12
q_tx_bytes () { vppctl show sr-wg qos 2>/dev/null | awk -v q="q$1" '$1==q {print $7; exit}'; }
q_backlog ()  { vppctl show sr-wg qos 2>/dev/null | awk -v q="q$1" '$1==q {print $12; exit}' | cut -d/ -f1; }
q_drop ()     { vppctl show sr-wg qos 2>/dev/null | awk -v q="q$1" '$1==q {print $10; exit}'; }

# 注入 count 个 size 字节的包（VPP 自己发，计数准）
inject () {
  vppctl ping "$DST" repeat "$1" interval 0.001 size "$2" source host-q2 >/dev/null 2>&1
}
# 注入 DSCP 标记的包（走内核；VPP ping 不能设 tos）
inject_dscp () {
  ping -c "$1" -i 0.001 -s "$2" -Q "$3" -w 2 "$DST" >/dev/null 2>&1
}

log "准备 VPP + veth（对端在独立 netns）"
cp "$PLUGIN" "$PDIR"/ || exit 1
mkdir -p /run/vpp
printf 'unix {\n nodaemon\n cli-listen /run/vpp/cli.sock\n}\n' > /tmp/vpp.conf

ip netns del far 2>/dev/null
ip netns add far || exit 1
ip link add q0 type veth peer name q1 || exit 1
ip link add q2 type veth peer name q3 || exit 1
ip link set q3 netns far
for i in q0 q1 q2; do ip link set "$i" up; done
ip netns exec far ip link set q3 up
ip netns exec far ip addr add $DST/24 dev q3
ip netns exec far ip route add 10.99.1.0/24 via 10.99.2.1 dev q3
ip netns exec far sysctl -qw net.ipv4.conf.all.rp_filter=0
ip addr add $SRC/24 dev q1
ip route add 10.99.2.0/24 via 10.99.1.1 dev q1

vpp -c /tmp/vpp.conf > /tmp/vpp.out 2>&1 &
VPID=$!
for _ in $(seq 1 40); do vppctl show version >/dev/null 2>&1 && break; sleep 0.5; done
vppctl show version | head -1 || { echo "VPP 没起来"; tail -5 /tmp/vpp.out; exit 1; }

vppctl create host-interface name q0 >/dev/null
vppctl create host-interface name q2 >/dev/null
vppctl set interface state host-q0 up >/dev/null
vppctl set interface state host-q2 up >/dev/null
vppctl set interface ip address host-q0 10.99.1.1/24 >/dev/null
vppctl set interface ip address host-q2 10.99.2.1/24 >/dev/null
sleep 1

log "T0 连通性（钩子未开）"
vppctl ping "$DST" repeat 5 interval 0.05 size 200 source host-q2 2>&1 | tail -1

vppctl set sr-wg qos-hook enable host-q2 >/dev/null

log "T1 rate=1000→0：关掉整形后必须完全直通（tx 计数不再动）"
vppctl set sr-wg qos rate host-q2 1000 >/dev/null
vppctl set sr-wg qos rate host-q2 0 >/dev/null
a=$(q_tx_bytes 2)
inject 100 1000
sleep 0.5
echo "  q2 排出 $(( $(q_tx_bytes 2) - a )) B（直通时不该动） 积压=$(q_backlog 2)"

log "T2 rate=1000 kbps（125 KB/s）：105 KB 应在 ~0.9s 内排空"
vppctl set sr-wg qos rate host-q2 1000 >/dev/null
a=$(q_tx_bytes 2)
inject 100 1000
sleep 1.2
echo "  q2 排出 $(( $(q_tx_bytes 2) - a )) B（≈105000） 积压=$(q_backlog 2)  drop=$(q_drop 2)"

log "T3 rate=100 kbps（12.5 KB/s）：1s 只该排出 ~12.5 KB，其余仍积压"
vppctl set sr-wg qos rate host-q2 100 >/dev/null
a=$(q_tx_bytes 2)
inject 100 1000
sleep 1
echo "  q2 排出 $(( $(q_tx_bytes 2) - a )) B（≈12500） 积压=$(q_backlog 2)"

log "T4 rate=0：立刻回直通，积压清空（注意：积压的包是被丢掉的）"
vppctl set sr-wg qos rate host-q2 0 >/dev/null
sleep 0.3
echo "  积压=$(q_backlog 2)"

log "T5 DSCP 分类：EF(46) 进 q0，默认(0) 进 q2"
vppctl set sr-wg qos rate host-q2 1000 >/dev/null
inject_dscp 30 200 184
inject 20 200
sleep 0.5
vppctl show sr-wg qos-hook | head -5
vppctl show sr-wg qos | sed -n "1,6p"

log "T6 LLQ 上限 20%：rate 1000 ⇒ EF 只走 25 KB/s，且不饿死默认类"
vppctl set sr-wg qos llq host-q2 percent 20 >/dev/null
e0=$(q_tx_bytes 0); d0=$(q_tx_bytes 2)
inject_dscp 60 1000 184
sleep 1
e1=$(q_tx_bytes 0); d1=$(q_tx_bytes 2)
echo "  EF 1s 排出 $((e1 - e0)) B（期望 ≈25000）  q0 积压=$(q_backlog 0)"
inject 20 1000
sleep 1
echo "  并发默认流后：EF 再排 $(( $(q_tx_bytes 0) - e1 )) B  默认类再排 $(( $(q_tx_bytes 2) - d1 )) B"

log "T7 尾丢：depth 32 + rate 1000，注入 100 包"
vppctl set sr-wg qos rate host-q2 0 >/dev/null
vppctl set sr-wg qos queue 2 depth 32
vppctl set sr-wg qos rate host-q2 1000 >/dev/null
inject 100 1000
sleep 0.3
vppctl show sr-wg qos | sed -n "1,6p"

log "T8 摘钩子：积压必须还回缓冲池"
vppctl set sr-wg qos-hook disable host-q2
sleep 0.3
vppctl show sr-wg qos | sed -n "1,6p"
vppctl show buffers | head -3

log "完成"
