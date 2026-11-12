#!/bin/bash
# 一次性排查：节点索引到底拿没拿到、CPE4 那个桶的 next_node 是哪个节点
# 用法（容器内）：bash dbg-nodes.sh
set -u
PDIR=/usr/lib/x86_64-linux-gnu/vpp_plugins
SO=${SO:-/workspace/vpp-plugin/build-qos2/lib/vpp_plugins/sr_wg_plugin.so}

pkill -x vpp 2>/dev/null; sleep 2
pgrep -x vpp >/dev/null && { pkill -9 -x vpp; sleep 1; }
for n in h1 h2; do ip netns del $n 2>/dev/null; ip netns add $n; done
cp "$SO" $PDIR/sr_wg_plugin.so
rm -f /run/vpp/cli.sock /run/vpp/stats.sock; mkdir -p /run/vpp
printf 'unix {\n  nodaemon\n  log /tmp/vpp-dbg.log\n  cli-listen /run/vpp/cli.sock\n}\ncpu { main-core 0 }\nbuffers { buffers-per-numa 8192 }\n' > /tmp/vpp-dbg.conf
vpp -c /tmp/vpp-dbg.conf > /tmp/vpp-dbg.out 2>&1 &
for _ in $(seq 1 30); do [ -S /run/vpp/cli.sock ] && break; sleep 1; done
sleep 2

vppctl create tap id 10 host-if-name h1t >/dev/null
vppctl create tap id 11 host-if-name h2t >/dev/null
vppctl set interface state tap10 up >/dev/null; vppctl set interface state tap11 up >/dev/null
vppctl set interface ip address tap10 10.99.1.1/24 >/dev/null
vppctl set interface ip address tap11 10.99.2.1/24 >/dev/null
K1=$(head -c32 /dev/urandom | base64); K2=$(head -c32 /dev/urandom | base64)
vppctl wireguard create listen-port 20000 private-key "$K1" >/dev/null
vppctl set interface state wg0 up >/dev/null
vppctl set interface ip address wg0 10.99.9.1/24 >/dev/null
vppctl wireguard peer add wg0 public-key "$K2" endpoint 10.99.2.2 dst-port 20000 \
  allowed-ip 10.99.8.0/24 allowed-ip 10.99.9.0/24 >/dev/null 2>&1
vppctl ip route add 10.99.8.0/24 via 10.99.9.2 wg0 >/dev/null 2>&1

vppctl set sr-wg qos-hook enable tap10 >/dev/null 2>&1
vppctl set sr-wg qos rate wg0 200 >/dev/null 2>&1

echo "=== show node 里的 ip4-midchain / ip4-rewrite"
vppctl show node 2>&1 | tr -d '\r' | grep -E "ip4-midchain|ip4-rewrite" | head -5
echo "=== 节点总数"
vppctl show node 2>&1 | tr -d '\r' | wc -l
echo "=== resolve"
vppctl show sr-wg qos resolve 10.99.8.5 from tap10 2>&1 | tr -d '\r'
echo "=== wg0 邻接（rewrite 字节里的外层端口）"
vppctl show adjacency 2>&1 | tr -d '\r' | grep -i -E "wg0|midchain" | head -6

pkill -x vpp 2>/dev/null
sleep 1
