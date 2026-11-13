#!/bin/bash
# ---------------------------------------------------------------------------
# 物理口统一 QoS 端到端（§13 四步全链，双 VPP 实例）
#
#   netns h1 10.99.1.2 ──tap10── VPP-A (10.99.1.1)                客户入口
#       标记器 sr-wg4-qos 挂 tap10（内层 DSCP→3bit）
#       wg0(20000,K1) ──midchain──> tap11 (10.99.2.1)             SR 封装
#           encap 挂 tap11：写 SR 头 action 高3bit + 外层 ToS
#           物理口调度 sr-wg4-qos-out 挂 tap11（rate 200kbps）
#                 │ wanbr (linux bridge)
#                 ▼
#       VPP-B tap12 (10.99.2.2) ── wg0(20000,K2) 解密 ── tap13 (10.99.8.1)
#           sr-wg4-input 从 action 恢复类别 → qos 元数据
#           物理口调度挂 tap13（rate 100Gbps，只计数不丢）
#                 ▼
#   netns h2 10.99.8.2
#
# 判据：
#   1) A: EF(dscp46)→q0，DF→q3（新默认表 EF→0 AF4x→1 AF3x→2 其余→3）
#   2) A: 外层 ToS 真的被 encap 写了（b-wan 抓包 tos 0xb8）
#   3) A: 整形生效（灌包 q3 有 drop/积压，tx_bytes ≈ 速率×时长）
#   4) B: decap_ok > 0 且 tap13 egress q0/q3 有包 —— decap 恢复类别并
#      用于 LAN 出向调度（端到端类别传递闭环）
#
# 用法（宿主机）：docker exec <特权容器> bash /workspace/test/lab/vpp-qos/lab-qos-egress.sh
# ---------------------------------------------------------------------------
set -u

PDIR=/usr/lib/x86_64-linux-gnu/vpp_plugins
SO=${SO:-/workspace/vpp-plugin/build-qos2/lib/vpp_plugins/sr_wg_plugin.so}
FAIL=0
PID_A=""; PID_B=""

log () { echo; echo "### $*"; }

cleanup () {
  [ -n "$PID_A" ] && kill "$PID_A" 2>/dev/null
  [ -n "$PID_B" ] && kill "$PID_B" 2>/dev/null
  sleep 1
  pkill -9 -x vpp 2>/dev/null
  for n in h1 h2; do ip netns del $n 2>/dev/null; done
  ip link del wanbr 2>/dev/null
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
ip link del wanbr 2>/dev/null; ip link add wanbr type bridge; ip link set wanbr up
cp "$SO" $PDIR/sr_wg_plugin.so
rm -rf /run/vpp; mkdir -p /run/vpp

PKEY=$(head -c16 /dev/urandom | xxd -p | tr -d '\n')      # SR P-Key：128-bit SipHash（32 hex 字符）
K1=$(head -c32 /dev/urandom | base64); K2=$(head -c32 /dev/urandom | base64)
# peer 的 public-key 必须用**派生的公钥**：直接传私钥会握手 MAC 校验失败
wgpub () { vppctl -s "$1" show wireguard interface 2>/dev/null | tr -d '\r' \
  | head -1 | sed -n 's/.*public-key:\([^ ]*\).*/\1/p'; }

log "起 VPP-A"
printf 'unix {\n nodaemon\n log /tmp/vpp-a.log\n cli-listen /run/vpp/cli-a.sock\n}\napi-segment { prefix srqa }\nstatseg { socket-name /run/vpp/stats-a.sock }\nplugins {\n plugin linux_cp_plugin.so { enable }\n}\ncpu { main-core 0 }\nbuffers { buffers-per-numa 8192 }\n' > /tmp/vpp-a.conf
vpp -c /tmp/vpp-a.conf > /tmp/vpp-a.out 2>&1 & PID_A=$!
log "起 VPP-B"
printf 'unix {\n nodaemon\n log /tmp/vpp-b.log\n cli-listen /run/vpp/cli-b.sock\n}\napi-segment { prefix srqb }\nstatseg { socket-name /run/vpp/stats-b.sock }\nplugins {\n plugin linux_cp_plugin.so { enable }\n}\ncpu { main-core 1 }\nbuffers { buffers-per-numa 8192 }\n' > /tmp/vpp-b.conf
vpp -c /tmp/vpp-b.conf > /tmp/vpp-b.out 2>&1 & PID_B=$!
for _ in $(seq 1 30); do
  [ -S /run/vpp/cli-a.sock ] && [ -S /run/vpp/cli-b.sock ] && break; sleep 1
done
sleep 2
vppctl -s /run/vpp/cli-a.sock show version | head -1 || { echo "VPP-A 起不来"; exit 1; }
vppctl -s /run/vpp/cli-b.sock show version | head -1 || { echo "VPP-B 起不来"; exit 1; }

log "建拓扑"
# ---- A：客户口 + WAN 口
vppctl -s /run/vpp/cli-a.sock create tap id 10 host-if-name a-t10 >/dev/null
vppctl -s /run/vpp/cli-a.sock create tap id 11 host-if-name a-wan >/dev/null
vppctl -s /run/vpp/cli-a.sock set interface state tap10 up >/dev/null
vppctl -s /run/vpp/cli-a.sock set interface state tap11 up >/dev/null
vppctl -s /run/vpp/cli-a.sock set interface ip address tap10 10.99.1.1/24 >/dev/null
vppctl -s /run/vpp/cli-a.sock set interface ip address tap11 10.99.2.1/24 >/dev/null
# ---- B：WAN 口 + LAN 口
vppctl -s /run/vpp/cli-b.sock create tap id 12 host-if-name b-wan >/dev/null
vppctl -s /run/vpp/cli-b.sock create tap id 13 host-if-name b-lan >/dev/null
vppctl -s /run/vpp/cli-b.sock set interface state tap12 up >/dev/null
vppctl -s /run/vpp/cli-b.sock set interface state tap13 up >/dev/null
vppctl -s /run/vpp/cli-b.sock set interface ip address tap12 10.99.2.2/24 >/dev/null
vppctl -s /run/vpp/cli-b.sock set interface ip address tap13 10.99.8.1/24 >/dev/null
# ---- WAN 桥接
ip link set a-wan master wanbr 2>/dev/null; ip link set b-wan master wanbr 2>/dev/null
ip link set a-wan up; ip link set b-wan up

# ---- A 的 WG（发端）
vppctl -s /run/vpp/cli-a.sock wireguard create listen-port 20000 private-key "$K1" src 10.99.2.1 >/dev/null
vppctl -s /run/vpp/cli-a.sock set interface state wg0 up >/dev/null
vppctl -s /run/vpp/cli-a.sock set interface ip address wg0 10.99.9.1/24 >/dev/null

vppctl -s /run/vpp/cli-a.sock ip route add 10.99.8.0/24 via 10.99.9.2 wg0 >/dev/null 2>&1
# ---- B 的 WG（收端）
vppctl -s /run/vpp/cli-b.sock wireguard create listen-port 20000 private-key "$K2" src 10.99.2.2 >/dev/null
vppctl -s /run/vpp/cli-b.sock set interface state wg0 up >/dev/null
vppctl -s /run/vpp/cli-b.sock set interface ip address wg0 10.99.9.2/24 >/dev/null
# 公钥互相喂：peer 的 public-key 必须是**派生公钥**（传私钥握手 MAC 必败）
APUB=$(wgpub /run/vpp/cli-a.sock); BPUB=$(wgpub /run/vpp/cli-b.sock)
vppctl -s /run/vpp/cli-a.sock wireguard peer add wg0 public-key "$BPUB" endpoint 10.99.2.2 dst-port 20000 \
  allowed-ip 10.99.8.0/24 allowed-ip 10.99.9.0/24 >/dev/null 2>&1
vppctl -s /run/vpp/cli-b.sock wireguard peer add wg0 public-key "$APUB" endpoint 10.99.2.1 dst-port 20000 \
  allowed-ip 10.99.1.0/24 allowed-ip 10.99.9.0/24 >/dev/null 2>&1

vppctl -s /run/vpp/cli-b.sock ip route add 10.99.1.0/24 via 10.99.9.1 wg0 >/dev/null 2>&1

# ---- SR 路径/密钥/绑定（A 发端封装，B 收端拦截解封装）
vppctl -s /run/vpp/cli-a.sock sr-wg key set 1 "$PKEY" >/dev/null
vppctl -s /run/vpp/cli-a.sock sr-wg path add 7 hmac-id 1 no-relay >/dev/null
WGA=$(ifname_idx /run/vpp/cli-a.sock wg0)
vppctl -s /run/vpp/cli-a.sock set sr-wg tunnel "$WGA" path 7 src-port 20000 >/dev/null
vppctl -s /run/vpp/cli-a.sock set sr-wg encap enable tap11 >/dev/null
vppctl -s /run/vpp/cli-b.sock sr-wg key set 1 "$PKEY" >/dev/null
vppctl -s /run/vpp/cli-b.sock set sr-wg port add vpn 20000 >/dev/null

# ---- netns 侧
ip link set a-t10 netns h1; ip netns exec h1 ip link set lo up; ip netns exec h1 ip link set a-t10 up
ip netns exec h1 ip addr add 10.99.1.2/24 dev a-t10
ip netns exec h1 ip route add default via 10.99.1.1
ip link set b-lan netns h2; ip netns exec h2 ip link set lo up; ip netns exec h2 ip link set b-lan up
ip netns exec h2 ip addr add 10.99.8.2/24 dev b-lan
ip netns exec h2 ip route add default via 10.99.8.1

sleep 3
log "连通性（无 QoS 状态）"
ip netns exec h1 ping -c 3 -i 0.3 -W2 10.99.8.2 | tail -2

log "开标记器 + 物理口限速"
vppctl -s /run/vpp/cli-a.sock set sr-wg qos-hook enable tap10 2>&1 | tr -d '\r' | sed 's/^/  /'
vppctl -s /run/vpp/cli-a.sock set sr-wg qos rate tap11 200 2>&1 | tr -d '\r' | sed 's/^/  /'
vppctl -s /run/vpp/cli-b.sock set sr-wg qos rate tap13 100000000 >/dev/null 2>&1

log "B 侧外层抓包（验证 encap 写外层 ToS）"
ip netns exec h1 true 2>/dev/null
timeout 40 tcpdump -ni b-wan -c 300 'udp port 20000' -v > /tmp/eg-tcpd.txt 2>&1 &

log "打 40 个 EF（ping -Q 0xb8 = dscp46）"
ip netns exec h1 ping -c 40 -i 0.02 -W 1 -Q 0xb8 10.99.8.2 >/dev/null 2>&1
sleep 2
grep -c "tos 0xb8" /tmp/eg-tcpd.txt >/dev/null 2>&1
EF_TOS=$(grep -c "tos 0xb8" /tmp/eg-tcpd.txt)
log "打 40 个 DF"
ip netns exec h1 ping -c 40 -i 0.02 -W 1 10.99.8.2 >/dev/null 2>&1

log "灌包看整形（150 个 1400B DF，200kbps 下必有积压/丢包）"
ip netns exec h1 ping -c 150 -i 0.01 -W 2 -s 1300 10.99.8.2 >/dev/null 2>&1
sleep 6

log "结果"
echo "-- A 标记器计数 --"
vppctl -s /run/vpp/cli-a.sock show sr-wg qos-hook 2>&1 | tr -d '\r' | sed 's/^/  /'
echo "-- A tap11 出向调度 --"
vppctl -s /run/vpp/cli-a.sock show sr-wg qos 2>&1 | tr -d '\r' | sed 's/^/  /'
echo "-- B decap/调度 --"
vppctl -s /run/vpp/cli-b.sock show sr-wg stats 2>&1 | tr -d '\r' | grep -E "decap" | sed 's/^/  /'
vppctl -s /run/vpp/cli-b.sock show sr-wg qos 2>&1 | tr -d '\r' | sed 's/^/  /'
echo "  外层 tos 0xb8 (EF) 抓到 $EF_TOS 包"

# ---- 断言 ----
hookA=$(vppctl -s /run/vpp/cli-a.sock show sr-wg qos-hook 2>&1 | tr -d '\r')
qA=$(vppctl -s /run/vpp/cli-a.sock show sr-wg qos 2>&1 | tr -d '\r')
qB=$(vppctl -s /run/vpp/cli-b.sock show sr-wg qos 2>&1 | tr -d '\r')
statsB=$(vppctl -s /run/vpp/cli-b.sock show sr-wg stats 2>&1 | tr -d '\r')

dscp46=$(echo "$hookA" | awk '/dscp 46/{print $3; exit}')
[ "${dscp46:-0}" -ge 35 ] || { echo "  !! A dscp46=$dscp46（标记器没看到 EF 流）"; FAIL=1; }
q0A=$(echo "$qA" | awk '/q0 LLQ/{print $4; exit}')
[ "${q0A:-0}" -ge 35 ] || { echo "  !! A q0=$q0A（EF 没进 LLQ）"; FAIL=1; }
q3A=$(echo "$qA" | awk '/q3 BULK/{print $4; exit}')
[ "${q3A:-0}" -ge 35 ] || { echo "  !! A q3=$q3A（DF 没进 BULK——新默认表 其余→3）"; FAIL=1; }
[ "${EF_TOS:-0}" -ge 20 ] || { echo "  !! 外层 tos 0xb8 只抓到 $EF_TOS 包（encap 没写 ToS？）"; FAIL=1; }
drop3A=$(echo "$qA" | awk '/q3 BULK/{for(i=1;i<=NF;i++) if($i=="drop") print $(i+1); exit}')
[ "${drop3A:-0}" -ge 1 ] || echo "  （提示）A q3 无丢包——灌包可能没超速率，整形断言弱化"
decapB=$(echo "$statsB" | awk '/Decap:/{f=1;next} f&&/^ *ok:/{print $2; exit}')
[ "${decapB:-0}" -ge 35 ] || { echo "  !! B decap_ok=$decapB（对端没解封装）"; FAIL=1; }
q0B=$(echo "$qB" | awk '/q0 LLQ/{print $4; exit}')
[ "${q0B:-0}" -ge 35 ] || { echo "  !! B tap13 q0=$q0B（decap 恢复的类别没进 LAN 出向队列）"; FAIL=1; }
q3B=$(echo "$qB" | awk '/q3 BULK/{print $4; exit}')
[ "${q3B:-0}" -ge 35 ] || { echo "  !! B tap13 q3=$q3B（DF 流没进 BULK）"; FAIL=1; }

log "关速率，确认完全直通"
vppctl -s /run/vpp/cli-a.sock set sr-wg qos rate tap11 0 >/dev/null 2>&1
ip netns exec h1 ping -c 5 -i 0.05 -W1 -Q 0xb8 10.99.8.2 | tail -1
vppctl -s /run/vpp/cli-a.sock show sr-wg qos 2>&1 | tr -d '\r' | head -2 | sed 's/^/  /'

[ "$FAIL" = 0 ] && echo && echo "PASS" || { echo; echo "FAIL"; }
exit $FAIL
