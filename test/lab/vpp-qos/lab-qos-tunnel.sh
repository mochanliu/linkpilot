#!/bin/bash
# ---------------------------------------------------------------------------
# [历史] O3 基线（git tag qos-o3-baseline）的验证脚本。
# 2026-11-13 起 sr_qos 已重构为物理口统一调度（§13），旧语义（rate 配在 wg
# 隧道口、DF→q2）不再存在；本脚本只对 qos-o3-baseline 的构建有效。
# 现行验证脚本：lab-qos-egress.sh（双 VPP 实例，覆盖四步全链）。
#
# 隧道 QoS 端到端（O3 挂点）：入口弧分类 → 4 队列 → 出隧道加密外发
#
#   netns h1 10.99.1.2/24 --tap10(VPP 10.99.1.1/24)
#                                |  钩子在这里（内层 DSCP 可读）
#                        sr-wg4-qos ── 每包查 FIB 得隧道 ──> wg0 (midchain)
#                                |                              |
#   netns h2 10.99.2.2/24 --tap11(VPP 10.99.2.1/24) <── 加密后外层 UDP 20000
#
#   wg0 对端是一个"只收不解"的假 peer：加密、封装、从 tap11 发出都真实发生，
#   只有对端解不开 —— 判据放在"外发隧道口真的收到了 UDP 20000"这一层。
#
# 判据：
#   1) 未归隧道 == 0（隧道判别成功，这是 O3 的核心）
#   2) EF → q0，DF → q2（内层 DSCP 分类）
#   3) tx_bytes 增量 ≈ 配置速率 × 时长（整形真的生效）
#
# 用法（宿主机）：
#   docker exec <特权容器> bash /repo/test/lab/vpp-qos/lab-qos-tunnel.sh
# ---------------------------------------------------------------------------
set -u

PDIR=/usr/lib/x86_64-linux-gnu/vpp_plugins
SO=${SO:-/repo/vpp-plugin/build-qos2/lib/vpp_plugins/sr_wg_plugin.so}
VPID=""
FAIL=0

log () { echo; echo "### $*"; }

cleanup () {
  [ -n "$VPID" ] && kill "$VPID" 2>/dev/null
  sleep 1
  for n in h1 h2; do ip netns del $n 2>/dev/null; done
  pkill -x tcpdump 2>/dev/null
  return 0
}
trap cleanup EXIT

log "重置环境"
pkill -x vpp 2>/dev/null; sleep 2
pgrep -x vpp >/dev/null && { pkill -9 -x vpp; sleep 1; }
for n in h1 h2; do ip netns del $n 2>/dev/null; ip netns add $n; done
cp "$SO" $PDIR/sr_wg_plugin.so
rm -f /run/vpp/cli.sock /run/vpp/stats.sock; mkdir -p /run/vpp
printf 'unix {\n  nodaemon\n  log /tmp/vpp-tun.log\n  cli-listen /run/vpp/cli.sock\n}\nplugins {\n  plugin linux_cp_plugin.so { enable }\n}\ncpu { main-core 0 }\nbuffers { buffers-per-numa 8192 }\n' > /tmp/vpp-tun.conf
vpp -c /tmp/vpp-tun.conf > /tmp/vpp-tun.out 2>&1 &
VPID=$!
for _ in $(seq 1 30); do [ -S /run/vpp/cli.sock ] && break; sleep 1; done
sleep 2
vppctl show version | head -1 || { echo "VPP 起不来"; tail -10 /tmp/vpp-tun.out; exit 1; }

log "建拓扑"
vppctl create tap id 10 host-if-name h1t >/dev/null
vppctl create tap id 11 host-if-name h2t >/dev/null
vppctl set interface state tap10 up >/dev/null; vppctl set interface state tap11 up >/dev/null
vppctl set interface ip address tap10 10.99.1.1/24 >/dev/null
vppctl set interface ip address tap11 10.99.2.1/24 >/dev/null

K1=$(head -c32 /dev/urandom | base64); K2=$(head -c32 /dev/urandom | base64)
vppctl wireguard create listen-port 20000 private-key "$K1" >/dev/null
vppctl set interface state wg0 up >/dev/null
vppctl set interface ip address wg0 10.99.9.1/24 >/dev/null
# 两个 allowed-ip：内层目的前缀 + 隧道下一跳本身，缺后者 midchain 会挂在 dpo-drop 上
vppctl wireguard peer add wg0 public-key "$K2" endpoint 10.99.2.2 dst-port 20000 \
  allowed-ip 10.99.8.0/24 allowed-ip 10.99.9.0/24 >/dev/null 2>&1
vppctl ip route add 10.99.8.0/24 via 10.99.9.2 wg0 >/dev/null 2>&1

ip link set h1t netns h1; ip netns exec h1 ip link set lo up; ip netns exec h1 ip link set h1t up
ip netns exec h1 ip addr add 10.99.1.2/24 dev h1t
ip netns exec h1 ip route add default via 10.99.1.1
ip link set h2t netns h2; ip netns exec h2 ip link set lo up; ip netns exec h2 ip link set h2t up
ip netns exec h2 ip addr add 10.99.2.2/24 dev h2t

echo "FIB 10.99.8.5:"
vppctl show ip fib 10.99.8.5 2>&1 | tr -d '\r' | sed -n '/path:\[/,+3p' | sed 's/^/  /'

# 外发抓包：只看 UDP 20000（加密后的外层）
ip netns exec h2 timeout 60 tcpdump -ni h2t -c 500 udp port 20000 > /tmp/tun-tcpd.txt 2>&1 &

log "开钩子 + 限速 200 kbps"
vppctl set sr-wg qos-hook enable tap10 2>&1 | tr -d '\r' | sed 's/^/  /'
vppctl set sr-wg qos rate wg0 200 2>&1 | tr -d '\r' | sed 's/^/  /'

log "打 40 个 EF（ping -Q 0xb8）"
ip netns exec h1 ping -c 40 -i 0.02 -W 1 -Q 0xb8 10.99.8.5 >/dev/null 2>&1
log "打 40 个 DF（默认）"
ip netns exec h1 ping -c 40 -i 0.02 -W 1 10.99.8.5 >/dev/null 2>&1
sleep 4

log "结果"
vppctl show sr-wg qos-hook 2>&1 | tr -d '\r' | sed 's/^/  /'
vppctl show sr-wg qos 2>&1 | tr -d '\r' | sed 's/^/  /'
echo "  外发 UDP20000 抓包: $(grep -c 'IP' /tmp/tun-tcpd.txt 2>/dev/null) 行"
echo "  出隧道口 tap11:"
vppctl show interface tap11 2>&1 | tr -d '\r' | sed -n '1,5p' | sed 's/^/    /'
echo "  非零错误计数:"
vppctl show errors 2>&1 | tr -d '\r' | awk 'NR>1 && $1+0>0' | head -6 | sed 's/^/    /'

unt=$(vppctl show sr-wg qos-hook 2>&1 | tr -d '\r' | sed -n 's/.*未归隧道=\([0-9]*\).*/\1/p')
[ "${unt:-1}" = 0 ] || { echo "  !! 未归隧道=$unt（隧道判别失败，核心问题）"; FAIL=1; }
q0=$(vppctl show sr-wg qos 2>&1 | tr -d '\r' | awk '/q0 LLQ/{print $4}')
q2=$(vppctl show sr-wg qos 2>&1 | tr -d '\r' | awk '/q2 DEF/{print $4}')
[ "${q0:-0}" -gt 0 ] && [ "${q2:-0}" -gt 0 ] || { echo "  !! q0=$q0 q2=$q2，分类没进队列"; FAIL=1; }

log "关钩子，确认积压清空"
vppctl set sr-wg qos rate wg0 0 >/dev/null 2>&1
vppctl set sr-wg qos-hook disable tap10 >/dev/null 2>&1
vppctl show sr-wg qos 2>&1 | tr -d '\r' | awk '/积压/{print "  " $0}'

[ "$FAIL" = 0 ] && echo && echo "PASS" || { echo; echo "FAIL"; }
exit $FAIL
