#!/bin/bash
# ---------------------------------------------------------------------------
# 中转节点 + 隧道末端 DSCP 恢复（§13.1 第 4 步"逐跳恢复必须"的判别性验证）
# 三 VPP 实例：A(encap) ──> T(transit) ──> B(decap)，中间夹用户态 mangler
#
#   netns h1 10.99.1.2 ──tap10── VPP-A (10.99.1.1)
#       标记器 sr-wg4-qos 挂 tap10
#       wg0(20000,K1,src 10.99.2.1) peer=B(10.99.3.3:20000)
#       SR path 7: via 10.99.2.2:51821 （唯一中转 = T）
#       tap11 (10.99.2.1) ── wanbr1{a-wan,m0} ──mangler── wanbr2{t-wan1,m1} ── tap12 (VPP-T wan1 10.99.2.2)
#                                          VPP-T: key + transit enable 51821
#                                          下一跳在 SR 头 segments 里（= B）
#                                          tap14 (10.99.3.2) ── wanbr3{t-wan2,m2} ──mangler── wanbr4{b-wan,m3} ── tap13 (VPP-B wan 10.99.3.3)
#       VPP-B: key + port add vpn 20000 + wg0 解密 ── tap15 (10.99.8.1)
#                 ▼
#   netns h2 10.99.8.2
#
# 运营商重标记模拟（判别性的关键，见 lab-qos-mangler.py）：
#   mangler 在用户态重写外层 ToS 并重算校验和（tc pedit 不修正校验和，
#   包会被 VPP ip4-input 的 bad ip4 checksum 丢掉——实测教训）。
#   Phase 1（mode=1）：A->T 的 SR 包 ToS 清成 0。
#            断言 wanbr3（T 出向，未被改写）抓到 tos 0xb8 —— 只有 Step 9.5
#            （从 action 恢复 + 重断言 ToS）能解释，纯透传是 0x00。
#   Phase 2（mode=2）：T->B 也清成 0。
#            断言 B LAN 出向 q0 仍收下 EF 流 —— 只有 Step 4.5（decap 从
#            action 恢复）能解释；没有它，按收到的 ToS=0 会全进 q3。
#
# 用法（宿主机）：docker exec <特权容器> bash /workspace/test/lab/vpp-qos/lab-qos-transit.sh
# ---------------------------------------------------------------------------
set -u

PDIR=/usr/lib/x86_64-linux-gnu/vpp_plugins
SO=${SO:-/workspace/vpp-plugin/build-qos2/lib/vpp_plugins/sr_wg_plugin.so}
FAIL=0
PID_A=""; PID_T=""; PID_B=""; PID_M=""

log () { echo; echo "### $*"; }

cleanup () {
  for p in "$PID_A" "$PID_T" "$PID_B" "$PID_M"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
  sleep 1
  pkill -9 -x vpp 2>/dev/null
  for n in h1 h2; do ip netns del $n 2>/dev/null; done
  for b in wanbr1 wanbr2 wanbr3 wanbr4; do ip link del $b 2>/dev/null; done
  for t in m0 m1 m2 m3; do ip link del $t 2>/dev/null; done
  pkill -x tcpdump 2>/dev/null
  return 0
}
trap cleanup EXIT

ifname_idx () { # $1=cli sock $2=vpp if name -> sw_if_index
  vppctl -s "$1" show interface 2>/dev/null | tr -d '\r' \
    | awk -v n="$2" '$1==n && $2 ~ /^[0-9]+$/ {print $2; exit}'
}

log "重置环境"
pkill -x vpp 2>/dev/null; sleep 2; pkill -9 -x vpp 2>/dev/null; sleep 1
for n in h1 h2; do ip netns del $n 2>/dev/null; ip netns add $n; done
for b in wanbr1 wanbr2 wanbr3 wanbr4; do
  ip link del $b 2>/dev/null
  ip link add $b type bridge stp_state 0 forward_delay 0; ip link set $b up
done
cp "$SO" $PDIR/sr_wg_plugin.so
rm -rf /run/vpp; mkdir -p /run/vpp

PKEY=$(head -c16 /dev/urandom | xxd -p | tr -d '\n')      # SR P-Key：128-bit SipHash
K1=$(head -c32 /dev/urandom | base64); K2=$(head -c32 /dev/urandom | base64)
wgpub () { vppctl -s "$1" show wireguard interface 2>/dev/null | tr -d '\r' \
  | head -1 | sed -n 's/.*public-key:\([^ ]*\).*/\1/p'; }

vppconf () { # $1 = tag
  printf 'unix {\n nodaemon\n log /tmp/vpp-%s.log\n cli-listen /run/vpp/cli-%s.sock\n}\napi-segment { prefix srqt%s }\nstatseg { socket-name /run/vpp/stats-%s.sock }\nplugins {\n plugin linux_cp_plugin.so { enable }\n}\ncpu { main-core 0 }\nbuffers { buffers-per-numa 8192 }\n' "$1" "$1" "$1" "$1" > /tmp/vpp-$1.conf
}
for t in a t b; do vppconf $t; done
vpp -c /tmp/vpp-a.conf > /tmp/vpp-a.out 2>&1 & PID_A=$!
vpp -c /tmp/vpp-t.conf > /tmp/vpp-t.out 2>&1 & PID_T=$!
vpp -c /tmp/vpp-b.conf > /tmp/vpp-b.out 2>&1 & PID_B=$!

log "起 mangler（创建 m0-m3 tap）"
rm -f /tmp/mangler-mode; echo 1 > /tmp/mangler-mode
python3 /workspace/test/lab/vpp-qos/lab-qos-mangler.py > /tmp/mangler.log 2>&1 & PID_M=$!
for _ in $(seq 1 20); do grep -q "mangler up" /tmp/mangler.log 2>/dev/null && break; sleep 0.5; done
grep -q "mangler up" /tmp/mangler.log || { echo "mangler 起不来"; cat /tmp/mangler.log; exit 1; }
for i in 0 1 2 3; do
  ip link set m$i up
  ip link set m$i mtu 9500 2>/dev/null
done
ip link set m0 master wanbr1; ip link set m1 master wanbr2
ip link set m2 master wanbr3; ip link set m3 master wanbr4

for _ in $(seq 1 30); do
  [ -S /run/vpp/cli-a.sock ] && [ -S /run/vpp/cli-t.sock ] && [ -S /run/vpp/cli-b.sock ] && break; sleep 1
done
sleep 2
for t in a t b; do
  vppctl -s /run/vpp/cli-$t.sock show version | head -1 \
    || { echo "VPP-$t 起不来"; tail -5 /tmp/vpp-$t.out; exit 1; }
done

CLI_A="vppctl -s /run/vpp/cli-a.sock"
CLI_T="vppctl -s /run/vpp/cli-t.sock"
CLI_B="vppctl -s /run/vpp/cli-b.sock"

log "建拓扑"
# ---- A：客户口 + WAN 口
$CLI_A create tap id 10 host-if-name a-t10 >/dev/null
$CLI_A create tap id 11 host-if-name a-wan >/dev/null
$CLI_A set interface state tap10 up >/dev/null; $CLI_A set interface state tap11 up >/dev/null
$CLI_A set interface ip address tap10 10.99.1.1/24 >/dev/null
$CLI_A set interface ip address tap11 10.99.2.1/24 >/dev/null
# ---- T：两个 WAN 口（中转不参与 WireGuard、无客户口）
$CLI_T create tap id 12 host-if-name t-wan1 >/dev/null
$CLI_T create tap id 14 host-if-name t-wan2 >/dev/null
$CLI_T set interface state tap12 up >/dev/null; $CLI_T set interface state tap14 up >/dev/null
$CLI_T set interface ip address tap12 10.99.2.2/24 >/dev/null
$CLI_T set interface ip address tap14 10.99.3.2/24 >/dev/null
# ---- B：WAN 口 + LAN 口
$CLI_B create tap id 13 host-if-name b-wan >/dev/null
$CLI_B create tap id 15 host-if-name b-lan >/dev/null
$CLI_B set interface state tap13 up >/dev/null; $CLI_B set interface state tap15 up >/dev/null
$CLI_B set interface ip address tap13 10.99.3.3/24 >/dev/null
$CLI_B set interface ip address tap15 10.99.8.1/24 >/dev/null
# ---- WAN 桥接（mangler 已在桥上）
ip link set a-wan master wanbr1 2>/dev/null
ip link set t-wan1 master wanbr2 2>/dev/null
ip link set t-wan2 master wanbr3 2>/dev/null
ip link set b-wan master wanbr4 2>/dev/null
for i in a-wan t-wan1 t-wan2 b-wan; do ip link set $i up; done

# ---- WG（A 发端 / B 收端；握手 A<->B 走 T 的普通转发，T 不参与 WG）
$CLI_A wireguard create listen-port 20000 private-key "$K1" src 10.99.2.1 >/dev/null
$CLI_A set interface state wg0 up >/dev/null
$CLI_A set interface ip address wg0 10.99.9.1/24 >/dev/null
$CLI_B wireguard create listen-port 20000 private-key "$K2" src 10.99.3.3 >/dev/null
$CLI_B set interface state wg0 up >/dev/null
$CLI_B set interface ip address wg0 10.99.9.2/24 >/dev/null
APUB=$(wgpub /run/vpp/cli-a.sock); BPUB=$(wgpub /run/vpp/cli-b.sock)
$CLI_A wireguard peer add wg0 public-key "$BPUB" endpoint 10.99.3.3 dst-port 20000 \
  allowed-ip 10.99.8.0/24 allowed-ip 10.99.9.0/24 >/dev/null 2>&1
$CLI_B wireguard peer add wg0 public-key "$APUB" endpoint 10.99.2.1 dst-port 20000 \
  allowed-ip 10.99.1.0/24 allowed-ip 10.99.9.0/24 >/dev/null 2>&1
# 握手/回程要过 T 的普通转发（T 是两段 WAN 的路由器）
$CLI_A ip route add 10.99.3.0/24 via 10.99.2.2 tap11 >/dev/null 2>&1
$CLI_B ip route add 10.99.2.0/24 via 10.99.3.2 tap13 >/dev/null 2>&1
$CLI_A ip route add 10.99.8.0/24 via 10.99.9.2 wg0 >/dev/null 2>&1
$CLI_B ip route add 10.99.1.0/24 via 10.99.9.1 wg0 >/dev/null 2>&1

# ---- SR：A 发端（带一跳中转 T），T 中转（只要 PKEY），B 收端
$CLI_A sr-wg key set 1 "$PKEY" >/dev/null
$CLI_A sr-wg path add 7 hmac-id 1 via 10.99.2.2:51821 >/dev/null
WGA=$(ifname_idx /run/vpp/cli-a.sock wg0)
$CLI_A set sr-wg tunnel "$WGA" path 7 src-port 20000 >/dev/null
$CLI_A set sr-wg encap enable tap11 >/dev/null
$CLI_T sr-wg key set 1 "$PKEY" >/dev/null
$CLI_T set sr-wg transit enable port 51821 2>&1 | tr -d '\r' | sed 's/^/  T: /'
$CLI_B sr-wg key set 1 "$PKEY" >/dev/null
$CLI_B set sr-wg port add vpn 20000 2>&1 | tr -d '\r' | sed 's/^/  B: /'

# ---- netns 侧
ip link set a-t10 netns h1; ip netns exec h1 ip link set lo up; ip netns exec h1 ip link set a-t10 up
ip netns exec h1 ip addr add 10.99.1.2/24 dev a-t10
ip netns exec h1 ip route add default via 10.99.1.1
ip link set b-lan netns h2; ip netns exec h2 ip link set lo up; ip netns exec h2 ip link set b-lan up
ip netns exec h2 ip addr add 10.99.8.2/24 dev b-lan
ip netns exec h2 ip route add default via 10.99.8.1

# ---- T / B 出向调度器：大速率只做分类计数，不整形
$CLI_T set sr-wg qos rate tap14 100000000 2>&1 | tr -d '\r' | sed 's/^/  T: /'
$CLI_B set sr-wg qos rate tap15 100000000 2>&1 | tr -d '\r' | sed 's/^/  B: /'
# ---- A 标记器（内层 DSCP -> 3bit）
$CLI_A set sr-wg qos-hook enable tap10 2>&1 | tr -d '\r' | sed 's/^/  A: /'

log "握手预热（WG 会话要先建好，A↔B 握手过 T 的普通转发）"
ok=0
for i in $(seq 1 10); do
  if ip netns exec h1 ping -c 3 -i 0.3 -W2 10.99.8.2 | grep -q " 0% packet loss"; then ok=1; break; fi
  sleep 2
done
[ "$ok" = 1 ] || { echo "  !! WG 会话建不起来，端到端不通"; exit 1; }
echo "  会话就绪"

log "Phase 1：mangler mode=1（A->T 的 SR 包外层 ToS 清成 0，校验和已修正）"
log "打 40 个 EF（ping -Q 0xb8 = dscp46）+ 40 个 DF"
# T 转发后的外层抓包：wanbr3 桥面（T 出向、mangler 之前）——Phase 1 的判据点
timeout 30 tcpdump -ni wanbr3 -c 300 'udp port 20000' -v > /tmp/tr-tcpd-tx.txt 2>&1 &
# T 收到侧抓包：wanbr2 桥面（mangler 之后）——验证 mangler 生效
timeout 30 tcpdump -ni wanbr2 -c 300 'udp port 51821' -v > /tmp/tr-tcpd-rx.txt 2>&1 &
# A 发出侧抓包：a-wan（AF_PACKET 在 mangler 之前）——验证 A 确实写了 0xb8
timeout 30 tcpdump -ni a-wan -c 300 'udp port 51821' -v > /tmp/tr-tcpd-src.txt 2>&1 &

ip netns exec h1 ping -c 40 -i 0.02 -W 1 -Q 0xb8 10.99.8.2 >/dev/null 2>&1
ip netns exec h1 ping -c 40 -i 0.02 -W 1 10.99.8.2 >/dev/null 2>&1
sleep 3
SRC_B8=$(grep -c "tos 0xb8" /tmp/tr-tcpd-src.txt 2>/dev/null)
RX_B8=$(grep -c "tos 0xb8" /tmp/tr-tcpd-rx.txt 2>/dev/null)
RX_0=$(grep -c "tos 0x0," /tmp/tr-tcpd-rx.txt 2>/dev/null)
TX_B8=$(grep -c "tos 0xb8" /tmp/tr-tcpd-tx.txt 2>/dev/null)
echo "  a-wan(A 发出): tos 0xb8 = $SRC_B8"
echo "  wanbr2(T 收到，mangler 后): tos 0xb8 = $RX_B8, tos 0x0 = $RX_0   （全 0 → mangler 生效）"
echo "  wanbr3(T 转发后，mangler 前): tos 0xb8 = $TX_B8   （≥20 → Step 9.5 恢复并重断言）"

log "Phase 1 结果（T 的恢复与出向分类）"
echo "-- T 完整 stats --"
$CLI_T show sr-wg stats 2>&1 | tr -d '\r' | sed 's/^/  /'
echo "-- T 错误计数（非零项）--"
$CLI_T show errors 2>&1 | tr -d '\r' | awk '$1+0>0' | head -10 | sed 's/^/  /'
echo "-- T tap14 出向分类（应来自恢复后的元数据）--"
$CLI_T show sr-wg qos 2>&1 | tr -d '\r' | sed 's/^/  /'

log "Phase 2：mangler mode=2（T->B 也清成 0，B 收到的外层 ToS=0）"
echo 2 > /tmp/mangler-mode; sleep 1.5

log "再打 40 个 EF + 40 个 DF（B 侧 tos=0，只能靠 action 恢复）"
ip netns exec h1 ping -c 40 -i 0.02 -W 1 -Q 0xb8 10.99.8.2 >/dev/null 2>&1
ip netns exec h1 ping -c 40 -i 0.02 -W 1 10.99.8.2 >/dev/null 2>&1
sleep 3

log "Phase 2 结果（B 的恢复与 LAN 出向分类）"
echo "-- B 完整 stats --"
$CLI_B show sr-wg stats 2>&1 | tr -d '\r' | sed 's/^/  /'
echo "-- B 错误计数（非零项）--"
$CLI_B show errors 2>&1 | tr -d '\r' | awk '$1+0>0' | head -10 | sed 's/^/  /'
echo "-- B tap15 出向分类（q0 应收下 EF，尽管收到 tos=0）--"
$CLI_B show sr-wg qos 2>&1 | tr -d '\r' | sed 's/^/  /'

# ---- 断言 ----
statsT=$($CLI_T show sr-wg stats 2>&1 | tr -d '\r')
qT=$($CLI_T show sr-wg qos 2>&1 | tr -d '\r')
statsB=$($CLI_B show sr-wg stats 2>&1 | tr -d '\r')
qB=$($CLI_B show sr-wg qos 2>&1 | tr -d '\r')

tok=$(echo "$statsT" | awk '/^ *Transit:/{f=1;next}/^ *Decap:/{f=0}f&&$1=="ok:"{print $2; exit}')
hmacfail=$(echo "$statsT" | awk '/^ *Transit:/{f=1;next}/^ *Decap:/{f=0}f&&$1=="hmac_fail:"{print $2; exit}')
decapB=$(echo "$statsB" | awk '/^ *Decap:/{f=1;next}/^ *General:/{f=0}f&&$1=="ok:"{print $2; exit}')
q0T=$(echo "$qT" | awk '/q0 LLQ/{print $4; exit}')
q3T=$(echo "$qT" | awk '/q3 BULK/{print $4; exit}')
q0B=$(echo "$qB" | awk '/q0 LLQ/{print $4; exit}')
q3B=$(echo "$qB" | awk '/q3 BULK/{print $4; exit}')
echo "  statsT 行数: $(echo "$statsT" | wc -l)"
echo "  解析值: transit_ok=$tok hmac_fail=$hmacfail decap_ok=$decapB T(q0=$q0T q3=$q3T) B(q0=$q0B q3=$q3B)"

[ "${tok:-0}" -ge 60 ] || { echo "  !! T transit_ok=$tok（中转没转够包）"; FAIL=1; }
[ "${hmacfail:-0}" = 0 ] || { echo "  !! T hmac_fail=$hmacfail（验签失败）"; FAIL=1; }
[ "${SRC_B8:-0}" -ge 20 ] || { echo "  !! A 发出的 tos 0xb8 只有 $SRC_B8 个（encap 没写 ToS？）"; FAIL=1; }
[ "${RX_0:-0}" -ge 40 ] || { echo "  !! T 收到的包 tos 0x0 只有 $RX_0 个（mangler 没生效？）"; FAIL=1; }
[ "${TX_B8:-0}" -ge 20 ] || { echo "  !! wanbr3 只抓到 $TX_B8 个 tos 0xb8（T 没有从 action 重断言 ToS）"; FAIL=1; }
[ "${q0T:-0}" -ge 60 ] || { echo "  !! T tap14 q0=$q0T（恢复后的 EF 类别没进 LLQ）"; FAIL=1; }
[ "${q3T:-0}" -ge 60 ] || { echo "  !! T tap14 q3=$q3T（DF 没进 BULK）"; FAIL=1; }
[ "${decapB:-0}" -ge 60 ] || { echo "  !! B decap_ok=$decapB（对端没解封装）"; FAIL=1; }
[ "${q0B:-0}" -ge 35 ] || { echo "  !! B tap15 q0=$q0B（B 收到 tos=0 时 EF 没进 LLQ——Step 4.5 恢复失败）"; FAIL=1; }
[ "${q3B:-0}" -ge 60 ] || { echo "  !! B tap15 q3=$q3B（DF 没进 BULK）"; FAIL=1; }

log "清理 mangler（mode=0 直通）并复测连通"
echo 0 > /tmp/mangler-mode
ip netns exec h1 ping -c 3 -i 0.2 -W2 10.99.8.2 | tail -1

[ "$FAIL" = 0 ] && echo && echo "PASS" || { echo; echo "FAIL"; }
exit $FAIL
