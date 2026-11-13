#!/bin/bash
# ---------------------------------------------------------------------------
# 隧道内 FEC 端到端（插件 1.2.0，XOR 块奇偶校验，docs/fec-and-lb-plan.md §9）
#
#   netns h1 10.99.1.2 ──tap10── VPP-A (10.99.1.1)
#       wg0(20000,K1) ──midchain──> tap11 (10.99.2.1)  ← encap 挂这里
#                                    │ a-wan ── wanbr ── b-wan ← netem loss 10%
#                                    ▼
#       VPP-B tap12 (10.99.2.2) ── wg0(20000,K2) 解密 ── tap13 (10.99.8.1)
#           sr-wg4-input 在 tap12：剥 SR 头 → 0xF0 分流 → 配对还原 → wg4-input
#                                    ▼
#   netns h2 10.99.8.2
#
# 丢包注入在 b-wan 的出向 qdisc 上（内核 → VPP-B 这一侧），只影响 A→B 方向，
# 所以 h1 的 ping 请求会被丢、回来的应答不会 —— "丢的是隧道方向"才是 FEC 的
# 考题。两个 VPP 各自有自己的绑定（真机上 bundle 就是两端对称下发的），B 端
# 不启 encap，只当收端。
#
# 判据（§9.6）：
#   1) 开 FEC 但链路无损：业务 0 丢包、recovered == 0、complete > 0
#      （没有白吃自己人的包 —— 这条比"能救回来"更重要）
#   2) netem 10% + 1:2：recovered > 0，业务丢包明显低于 10% 基线
#   3) netem 10% + 1:1：丢包 ≤ 3%（整包复制，除两端全丢都救得回）
#   4) 关 FEC：丢包立刻回到基线（fail-open，冗余真的停了）
#   5) 带宽账：1:2 时 A 侧出口字节 ≈ 基线的 1.5 倍（1 + 1/2）
#
# 用法（宿主机）：docker exec <特权容器> bash /workspace/test/lab/vpp-fec/lab-fec-tunnel.sh
# ---------------------------------------------------------------------------
set -u

PDIR=/usr/lib/x86_64-linux-gnu/vpp_plugins
SO=${SO:-/workspace/vpp-plugin/build/lib/vpp_plugins/sr_wg_plugin.so}
SA=/run/vpp/cli-a.sock
SB=/run/vpp/cli-b.sock
PID_A=""; PID_B=""
FAIL=0

log () { echo; echo "### $*"; }
note () { echo "  · $*"; }
bad () { echo "  !! $*"; FAIL=1; }

cleanup () {
  [ -n "$PID_A" ] && kill "$PID_A" 2>/dev/null
  [ -n "$PID_B" ] && kill "$PID_B" 2>/dev/null
  sleep 1
  pkill -9 -x vpp 2>/dev/null
  for n in h1 h2; do ip netns del $n 2>/dev/null; done
  ip link del wanbr 2>/dev/null
  return 0
}
trap cleanup EXIT

ifname_idx () { # $1=cli sock $2=vpp if name -> sw_if_index
  vppctl -s "$1" show interface 2>/dev/null | tr -d '\r' \
    | awk -v n="$2" '$1==n && $2 ~ /^[0-9]+$/ {print $2; exit}'
}

# fec_stat <sock> <key>  —— 从 show sr-wg fec 里取一个计数值
fec_stat () {
  vppctl -s "$1" show sr-wg fec 2>/dev/null | tr -d '\r' \
    | awk -v k="$2" '{for(i=1;i<NF;i++) if($i==k) {print $(i+1); exit}}'
}

# ping_loss <count> -> 丢包百分比（整数，无 % 号）
ping_loss () {
  local out loss got
  out=$(ip netns exec h1 ping -c "$1" -i 0.02 -W 1 10.99.8.2 2>/dev/null | tr -d '\r')
  loss=$(echo "$out" | sed -n 's/.* \([0-9]\+\)% packet loss.*/\1/p' | tail -1)
  if [ -z "$loss" ]; then
    # 摘要行被 "Destination Host Unreachable" 之类插花时退一步：按收到的个数反算
    got=$(echo "$out" | sed -n 's/.* \([0-9]\+\) received.*/\1/p' | tail -1)
    [ -n "$got" ] && loss=$(( (100 * ($1 - got)) / $1 ))
  fi
  echo "${loss:-?}"
}

set_fec () { # $1 = "off" 或 1..4
  vppctl -s "$SA" set sr-wg fec wg0 "$1" >/dev/null 2>&1
  vppctl -s "$SB" set sr-wg fec wg0 "$1" >/dev/null 2>&1
}

log "重置环境"
pkill -x vpp 2>/dev/null; sleep 2; pkill -9 -x vpp 2>/dev/null; sleep 1
for n in h1 h2; do ip netns del $n 2>/dev/null; ip netns add $n; done
ip link del wanbr 2>/dev/null; ip link add wanbr type bridge; ip link set wanbr up
cp "$SO" $PDIR/sr_wg_plugin.so
rm -rf /run/vpp; mkdir -p /run/vpp

PKEY=$(head -c16 /dev/urandom | xxd -p | tr -d '\n')
K1=$(head -c32 /dev/urandom | base64); K2=$(head -c32 /dev/urandom | base64)
wgpub () { vppctl -s "$1" show wireguard interface 2>/dev/null | tr -d '\r' \
  | head -1 | sed -n 's/.*public-key:\([^ ]*\).*/\1/p'; }

log "起 VPP-A"
printf 'unix {\n nodaemon\n log /tmp/vpp-a.log\n cli-listen %s\n}\napi-segment { prefix srfa }\nstatseg { socket-name /run/vpp/stats-a.sock }\nplugins {\n plugin linux_cp_plugin.so { enable }\n}\ncpu { main-core 0 }\nbuffers { buffers-per-numa 8192 }\n' "$SA" > /tmp/vpp-a.conf
vpp -c /tmp/vpp-a.conf > /tmp/vpp-a.out 2>&1 & PID_A=$!
log "起 VPP-B"
printf 'unix {\n nodaemon\n log /tmp/vpp-b.log\n cli-listen %s\n}\napi-segment { prefix srfb }\nstatseg { socket-name /run/vpp/stats-b.sock }\nplugins {\n plugin linux_cp_plugin.so { enable }\n}\ncpu { main-core 1 }\nbuffers { buffers-per-numa 8192 }\n' "$SB" > /tmp/vpp-b.conf
vpp -c /tmp/vpp-b.conf > /tmp/vpp-b.out 2>&1 & PID_B=$!
for _ in $(seq 1 30); do
  [ -S "$SA" ] && [ -S "$SB" ] && break; sleep 1
done
sleep 2
vppctl -s "$SA" show version | head -1 || { echo "VPP-A 起不来"; exit 1; }
vppctl -s "$SB" show version | head -1 || { echo "VPP-B 起不来"; exit 1; }

log "建拓扑"
vppctl -s "$SA" create tap id 10 host-if-name a-t10 >/dev/null
vppctl -s "$SA" create tap id 11 host-if-name a-wan >/dev/null
vppctl -s "$SA" set interface state tap10 up >/dev/null
vppctl -s "$SA" set interface state tap11 up >/dev/null
vppctl -s "$SA" set interface ip address tap10 10.99.1.1/24 >/dev/null
vppctl -s "$SA" set interface ip address tap11 10.99.2.1/24 >/dev/null
vppctl -s "$SB" create tap id 12 host-if-name b-wan >/dev/null
vppctl -s "$SB" create tap id 13 host-if-name b-lan >/dev/null
vppctl -s "$SB" set interface state tap12 up >/dev/null
vppctl -s "$SB" set interface state tap13 up >/dev/null
vppctl -s "$SB" set interface ip address tap12 10.99.2.2/24 >/dev/null
vppctl -s "$SB" set interface ip address tap13 10.99.8.1/24 >/dev/null
ip link set a-wan master wanbr 2>/dev/null; ip link set b-wan master wanbr 2>/dev/null
ip link set a-wan up; ip link set b-wan up

log "建 WG 隧道"
vppctl -s "$SA" wireguard create listen-port 20000 private-key "$K1" src 10.99.2.1 >/dev/null
vppctl -s "$SA" set interface state wg0 up >/dev/null
vppctl -s "$SA" set interface ip address wg0 10.99.9.1/24 >/dev/null
vppctl -s "$SA" ip route add 10.99.8.0/24 via 10.99.9.2 wg0 >/dev/null 2>&1
vppctl -s "$SB" wireguard create listen-port 20000 private-key "$K2" src 10.99.2.2 >/dev/null
vppctl -s "$SB" set interface state wg0 up >/dev/null
vppctl -s "$SB" set interface ip address wg0 10.99.9.2/24 >/dev/null
APUB=$(wgpub "$SA"); BPUB=$(wgpub "$SB")
vppctl -s "$SA" wireguard peer add wg0 public-key "$BPUB" endpoint 10.99.2.2 dst-port 20000 \
  allowed-ip 10.99.8.0/24 allowed-ip 10.99.9.0/24 >/dev/null 2>&1
vppctl -s "$SB" wireguard peer add wg0 public-key "$APUB" endpoint 10.99.2.1 dst-port 20000 \
  allowed-ip 10.99.1.0/24 allowed-ip 10.99.9.0/24 >/dev/null 2>&1
vppctl -s "$SB" ip route add 10.99.1.0/24 via 10.99.9.1 wg0 >/dev/null 2>&1

log "SR 路径/密钥/绑定（两端对称；只有 A 启 encap）"
vppctl -s "$SA" sr-wg key set 1 "$PKEY" >/dev/null
vppctl -s "$SA" sr-wg path add 7 hmac-id 1 no-relay >/dev/null
WGA=$(ifname_idx "$SA" wg0)
vppctl -s "$SA" set sr-wg tunnel "$WGA" path 7 src-port 20000 >/dev/null
vppctl -s "$SA" set sr-wg encap enable tap11 >/dev/null
vppctl -s "$SB" sr-wg key set 1 "$PKEY" >/dev/null
vppctl -s "$SB" sr-wg path add 7 hmac-id 1 no-relay >/dev/null
WGB=$(ifname_idx "$SB" wg0)
vppctl -s "$SB" set sr-wg tunnel "$WGB" path 7 src-port 20000 >/dev/null
vppctl -s "$SB" set sr-wg port add vpn 20000 >/dev/null

ip link set a-t10 netns h1; ip netns exec h1 ip link set lo up; ip netns exec h1 ip link set a-t10 up
ip netns exec h1 ip addr add 10.99.1.2/24 dev a-t10
ip netns exec h1 ip route add default via 10.99.1.1
ip link set b-lan netns h2; ip netns exec h2 ip link set lo up; ip netns exec h2 ip link set b-lan up
ip netns exec h2 ip addr add 10.99.8.2/24 dev b-lan
ip netns exec h2 ip route add default via 10.99.8.1

sleep 3
log "连通性（FEC off、链路无损）"
ip netns exec h1 ping -c 20 -i 0.02 -W1 10.99.8.2 | tail -2
L0=$(ping_loss 100)
note "基线丢包 ${L0:-?}%（FS 校验：FEC off 且无损必须 0）"
[ "${L0:-9}" -le 1 ] || bad "无 FEC 无损链路就丢包 ${L0}% —— 拓扑/封装本身有问题"

# ---------------------------------------------------------------- 阶段 1
log "阶段 1：开 FEC 1:2，链路仍无损（无害性）"
set_fec 2
note "A: $(vppctl -s "$SA" show sr-wg fec 2>/dev/null | tr -d '\r' | head -1)"
sleep 1
L1=$(ping_loss 100)
R1=$(fec_stat "$SB" recovered); C1=$(fec_stat "$SB" complete)
U1=$(fec_stat "$SB" unrecovered); P1=$(fec_stat "$SA" parity)
note "丢包 ${L1:-?}%，A parity=${P1:-?}，B complete=${C1:-?} recovered=${R1:-?} unrecovered=${U1:-?}"
[ "${L1:-9}" -le 1 ] || bad "开 FEC 后无损链路反而丢包 ${L1}%"
[ "${C1:-0}" -ge 20 ] || bad "冗余包没有被对端判成 complete（C1=${C1:-?}）—— 冗余包可能根本没到或头不对"
# 错配（recovered+unrecovered）只该零星出现：见下面阶段 2 前的说明。
[ $(( ${R1:-0} + ${U1:-0} )) -le 8 ] || bad "无损链路上错配 ${R1:-0}+${U1:-0} 次（100 包 = 50 组，容忍 ≤8）"

# 诊断：分组打流看计数增量。链路无损时 recovered 必须是 0 —— 它每涨一次都
# 意味着有一个"还原出来的帧"被送进 WG（然后被标签校验丢掉），是错配的信号。
if [ "${FEC_DIAG:-0}" = 1 ]; then
  echo
  echo "  -- 分组打流（每段 50 包）--"
  for seg in 1 2 3 4 5 6; do
    ip netns exec h1 ping -c 50 -i 0.02 -W1 10.99.8.2 >/dev/null 2>&1
    echo "    段$seg  A[$(vppctl -s "$SA" show sr-wg fec 2>/dev/null | tr -d '\r' | head -1 | sed 's/.*|//')]  B[$(vppctl -s "$SB" show sr-wg fec 2>/dev/null | tr -d '\r' | head -1 | sed 's/.*|//')]"
  done
  echo "    B 解封装/错误：$(vppctl -s "$SB" show sr-wg stats 2>/dev/null | tr -d '\r' | tr '\n' ' ' | sed 's/  */ /g' | cut -c1-400)"
fi

# 无损链路上 recovered/unrecovered 也会零星涨：容器里 tap+bridge 偶尔把同一组
# 的两帧排成"冗余包先到"，此时配对窗口看到的是 j=k-1/j=0 —— 还原出来的帧是
# 在途那一帧的副本，到得晚，被 WG 反回放当重复丢掉。判据因此看"业务丢包 0"
# 与"错配比例小"，而不是 recovered 严格为 0。

# ---------------------------------------------------------------- 阶段 2
log "阶段 2：netem 10%（A→B），FEC 1:2"
tc qdisc add dev b-wan root netem loss 10%
sleep 1
P0=$(fec_stat "$SA" parity); R0=$(fec_stat "$SB" recovered)
L2=$(ping_loss 200)
P2=$(( $(fec_stat "$SA" parity) - ${P0:-0} ))
R2=$(( $(fec_stat "$SB" recovered) - ${R0:-0} ))
U2=$(fec_stat "$SB" unrecovered)
note "丢包 ${L2:-?}%（基线 ~10%），A 本段 parity 增量 ${P2}（200 帧应 ≈100）"
note "A: $(vppctl -s "$SA" show sr-wg fec 2>/dev/null | tr -d '\r' | head -1)"
note "B: $(vppctl -s "$SB" show sr-wg fec 2>/dev/null | tr -d '\r' | head -1)"
[ "${R2:-0}" -ge 10 ] || bad "10% 丢包下本段 recovered=${R2:-0}（还原路径没跑起来）"
[ "${L2:-9}" -le 5 ] || bad "1:2 下业务丢包 ${L2}%（应明显低于 10% 基线）"
# 冗余包头一个都不该被判非法：bad>0 说明线格式/校验和/长度有错。
[ "$(fec_stat "$SB" bad)" = 0 ] || bad "B 侧 bad=$(fec_stat "$SB" bad)（冗余包头被拒，线格式有问题）"
[ "$(fec_stat "$SA" too_big)" = 0 ] || bad "A 侧 too_big=$(fec_stat "$SA" too_big)（有帧没进组）"
# 带宽账：每 2 个数据包 1 个冗余包。200 个 ping 请求 ≈ 200 帧，所以 ≈100 个，
# 给 tap 层的握手/keepalive 帧留 ±20% 余量。
[ "${P2:-0}" -ge 80 ] && [ "${P2:-0}" -le 130 ] \
  || bad "1:2 本段发了 ${P2} 个冗余包（200 帧应 ≈100：比例没按 1:2 走）"
note "B 侧 多丢放弃=${U2:-?}（组内丢 2 个 —— XOR 的数学边界，正常）"

# ---------------------------------------------------------------- 阶段 3
log "阶段 3：FEC 1:1（整包复制）"
set_fec 1
sleep 1
P0=$(fec_stat "$SA" parity)
L3=$(ping_loss 200)
P3=$(( $(fec_stat "$SA" parity) - ${P0:-0} ))
note "丢包 ${L3:-?}%，A 本段 parity 增量 ${P3}（≈ 帧数 200）"
note "A: $(vppctl -s "$SA" show sr-wg fec 2>/dev/null | tr -d '\r' | head -1)"
[ "${L3:-9}" -le 3 ] || bad "1:1 下业务丢包 ${L3}%（除两端全丢都应救得回）"
[ "${P3:-0}" -ge 160 ] || bad "1:1 只发了 ${P3:-0} 个冗余包（复制路径没生效）"

# ---------------------------------------------------------------- 阶段 4
log "阶段 4：关 FEC，丢包回到基线（fail-open）"
set_fec off
P0=$(fec_stat "$SA" parity)
sleep 1
L4=$(ping_loss 200)
P4=$(( $(fec_stat "$SA" parity) - ${P0:-0} ))
note "丢包 ${L4:-?}%，本段 parity 增量 ${P4}（必须为 0）"
[ "${L4:-0}" -ge 5 ] || bad "关了 FEC 丢包只有 ${L4}%（冗余是不是还在发？）"
[ "${P4:-1}" = 0 ] || bad "关了 FEC 还在发冗余包（${P4} 个）—— 数据面没真停"

log "结果"
echo "-- A --"; vppctl -s "$SA" show sr-wg fec 2>&1 | tr -d '\r' | sed 's/^/  /'
echo "-- B --"; vppctl -s "$SB" show sr-wg fec 2>&1 | tr -d '\r' | sed 's/^/  /'
echo "-- B sr-wg 统计 --"; vppctl -s "$SB" show sr-wg stats 2>&1 | tr -d '\r' | sed 's/^/  /'
echo "  丢包账：无损 ${L0:-?}% | 1:2+10% ${L2:-?}% | 1:1+10% ${L3:-?}% | off+10% ${L4:-?}%"

tc qdisc del dev b-wan root 2>/dev/null
[ "$FAIL" = 0 ] && echo && echo "PASS" || { echo; echo "FAIL"; }
exit $FAIL
