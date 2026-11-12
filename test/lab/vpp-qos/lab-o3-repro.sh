#!/bin/bash
# ---------------------------------------------------------------------------
# O3 挂点复现台：把 CPE4 的形状搬进特权容器，回答一个问题
#   —— 「把 feature 注册到 ip4-unicast 弧」到底会不会打死数据面？
#
# 形状（对照 CPE4：转发口 + 一个 LCP 对当管理面）：
#
#   netns h1 10.99.1.2/24 -- h1t ~~ tap10 (VPP 10.99.1.1/24)
#                                    |
#                             ip4-unicast 弧 ← 被测 feature 挂在这里
#                                    |
#   netns h2 10.99.2.2/24 -- h2t ~~ tap11 (VPP 10.99.2.1/24)
#
#   netns h3 10.250.0.9/24 -- lcpAt ~~ tap12 (VPP 10.250.0.1/24)
#                                        └── LCP 对 ── 内核 lcpA 10.250.0.1/24
#   h3 ping 10.250.0.1 = 模拟"管理面"：包必须走完 ip4-unicast 弧、被
#   linux-cp-xc-ip4 抓去内核、再由内核回。CPE4 那次事故的现象正是
#   「VPP 活着但管理面不响」，所以这条路径必须单独量。
#
# 为什么用 tap 不用 host-interface+veth：后者在本容器里 l3 mac mismatch，
# 基线就不通（veth 两头 MAC 与 VPP 认的不一致），量不出挂点的锅。
#
# 用法（宿主机，仓库根目录）：
#   docker exec <特权容器> bash /repo/test/lab/vpp-qos/lab-o3-repro.sh
# ---------------------------------------------------------------------------
set -u

PDIR=/usr/lib/x86_64-linux-gnu/vpp_plugins
NEW=${NEW:-/repo/vpp-plugin/build-qos2/lib/vpp_plugins/sr_wg_plugin.so}
OLD=${OLD:-/repo/vpp-plugin/build-qos/lib/vpp_plugins/sr_wg_plugin.so}
VPID=""
FAIL=0

log () { echo; echo "### $*"; }

cleanup () {
  [ -n "$VPID" ] && kill "$VPID" 2>/dev/null
  sleep 1
  for n in h1 h2 h3; do ip netns del $n 2>/dev/null; done
  rm -f /tmp/vpp-o3.out /tmp/vpp-o3.log /run/vpp/cli.sock /run/vpp/stats.sock
  return 0
}
trap cleanup EXIT

start_vpp () {
  # 必须等**旧进程真的没了**：SIGTERM 之后 VPP 还要拆一遍图，这期间 vppctl 会连到
  # 旧实例，而旧实例的 tap 已经跟着 fd 一起死了 —— 现象就是"配置命令全返回成功、
  # ping 一个都不通、错误计数里全是 null-node blackholed"（第一版脚本就栽在这）。
  if [ -n "$VPID" ]; then kill "$VPID" 2>/dev/null; fi
  for _ in $(seq 1 30); do
    pgrep -x vpp >/dev/null 2>&1 || break
    sleep 0.5
  done
  pgrep -x vpp >/dev/null 2>&1 && { pkill -9 -x vpp; sleep 1; }
  VPID=""
  # stats.sock 残留会让新实例 clib_socket_init 直接 abort（"Active listener"），
  # cli.sock 残留会让 vppctl 连到上一个尸体 —— 都要在启动前清掉。
  rm -f /run/vpp/cli.sock /run/vpp/stats.sock
  mkdir -p /run/vpp
  vpp -c /tmp/vpp-o3.conf > /tmp/vpp-o3.out 2>&1 &
  VPID=$!
  for _ in $(seq 1 30); do
    [ -S /run/vpp/cli.sock ] && break
    sleep 1
  done
  sleep 2
  if ! vppctl show version >/dev/null 2>&1; then
    echo "   !! VPP 不可用，输出尾部："
    tail -12 /tmp/vpp-o3.out 2>/dev/null | sed 's/^/      /'
    return 1
  fi
  return 0
}

config () { # $1=1 表示建 LCP 对
  vppctl create tap id 10 host-if-name h1t >/dev/null 2>&1
  vppctl create tap id 11 host-if-name h2t >/dev/null 2>&1
  vppctl set interface state tap10 up >/dev/null 2>&1
  vppctl set interface state tap11 up >/dev/null 2>&1
  vppctl set interface ip address tap10 10.99.1.1/24 >/dev/null 2>&1
  vppctl set interface ip address tap11 10.99.2.1/24 >/dev/null 2>&1

  if [ "${1:-0}" = 1 ]; then
    # LCP 对：把 linux-cp-xc-ip4 拉上 ip4-unicast 弧（CPE4 管理面同形状）。
    # 语法是 `host-if`，不是 `host-if-name`（v26.06 的 lcp_cli.c 只认 host-if）。
    vppctl create tap id 12 host-if-name lcpAt >/dev/null 2>&1
    vppctl set interface state tap12 up >/dev/null 2>&1
    vppctl set interface ip address tap12 10.250.0.1/24 >/dev/null 2>&1
    vppctl lcp create tap12 host-if lcpA >/dev/null 2>&1
    ip addr add 10.250.0.1/24 dev lcpA 2>/dev/null
    ip link set lcpA up 2>/dev/null
    echo "   LCP: $(vppctl show lcp 2>&1 | tr -d '\r' | grep -c 'itf-pair') 对, 内核 $(ip -br addr show lcpA 2>/dev/null | awk '{print $3}')"
  fi

  ip link set h1t netns h1 2>/dev/null
  ip netns exec h1 ip link set lo up
  ip netns exec h1 ip link set h1t up
  ip netns exec h1 ip addr add 10.99.1.2/24 dev h1t 2>/dev/null
  ip netns exec h1 ip route add default via 10.99.1.1 2>/dev/null
  ip link set h2t netns h2 2>/dev/null
  ip netns exec h2 ip link set lo up
  ip netns exec h2 ip link set h2t up
  ip netns exec h2 ip addr add 10.99.2.2/24 dev h2t 2>/dev/null
  ip netns exec h2 ip route add default via 10.99.2.1 2>/dev/null
  if [ "${1:-0}" = 1 ]; then
    ip link set lcpAt netns h3 2>/dev/null
    ip netns exec h3 ip link set lo up
    ip netns exec h3 ip link set lcpAt up
    ip netns exec h3 ip addr add 10.250.0.9/24 dev lcpAt 2>/dev/null
  fi
}

nrecv () { # <netns> <dst> -> 收到几个
  ip netns exec "$1" ping -c 3 -W 1 -q "$2" 2>/dev/null | \
    sed -n 's/.* \([0-9]\+\) received.*/\1/p'
}

# run_case <标签> <so|none> <0|1 建LCP> [钩子口]
run_case () {
  local label=$1 so=$2 lcp=$3 hook=${4:-}
  log "CASE: $label"

  if [ "$so" = none ]; then
    rm -f $PDIR/sr_wg_plugin.so
  else
    [ -f "$so" ] || { echo "   !! 缺 $so"; FAIL=1; return; }
    cp "$so" $PDIR/sr_wg_plugin.so
  fi

  start_vpp || { FAIL=1; return; }
  config "$lcp"

  echo "   插件=$(vppctl show plugins 2>/dev/null | tr -d '\r' | grep -c sr_wg_plugin) 弧内前 4: $(vppctl show features 2>/dev/null | tr -d '\r' | awk '/^ip4-unicast:/{f=1;next} /^[a-z0-9-]+:$/{f=0} f' | head -4 | tr '\n' ' ')"

  if [ -n "$hook" ]; then
    echo "   钩子: $(vppctl set sr-wg qos-hook enable "$hook" 2>&1 | tr -d '\r' | cut -c1-60)"
  fi

  local fwd mgmt ok=0
  fwd=$(nrecv h1 10.99.2.2); fwd=${fwd:-0}
  mgmt=$([ "$lcp" = 1 ] && { n=$(nrecv h3 10.250.0.1); echo "${n:-0}"; } || echo "-")

  echo "   转发 h1→h2: ${fwd}/3    管理面 h3→VPP(lcp): ${mgmt}"
  [ "$fwd" -ge 2 ] 2>/dev/null && ok=1
  [ "$mgmt" != "-" ] && [ "$mgmt" -lt 1 ] 2>/dev/null && ok=0

  if [ "$ok" = 1 ]; then
    echo "   => PASS"
  else
    echo "   => FAIL"
    FAIL=1
    vppctl show errors 2>/dev/null | tr -d '\r' | awk 'NR>1 && $1+0>0' | head -6 | sed 's/^/      /'
    vppctl show node counters 2>/dev/null | tr -d '\r' | awk '$3+0>0' | head -6 | sed 's/^/      /'
  fi
}

# ---- 环境 ----------------------------------------------------------------
pkill -x vpp 2>/dev/null
sleep 1
for n in h1 h2 h3; do ip netns del $n 2>/dev/null; ip netns add $n; done
mv $PDIR/dpdk_plugin.so /tmp/dpdk_plugin.so.hidden 2>/dev/null
# linux_cp / linux_nl 不在默认加载集里，不显式写就没有 lcp 命令
printf 'unix {\n  nodaemon\n  log /tmp/vpp-o3.log\n  cli-listen /run/vpp/cli.sock\n}\nplugins {\n  plugin linux_cp_plugin.so { enable }\n  plugin linux_nl_plugin.so { enable }\n}\ncpu { main-core 0 }\nbuffers { buffers-per-numa 8192 }\n' > /tmp/vpp-o3.conf

# ---- 用例 ----------------------------------------------------------------
run_case "① 无插件、无 LCP（基线）"          none 0
run_case "② 无插件、有 LCP"                 none 1
run_case "③ O3 插件、无 LCP、无钩子"         "$NEW" 0
run_case "④ O3 插件、有 LCP、无钩子"         "$NEW" 1
run_case "⑤ O3 插件、有 LCP、tap10 开钩子"   "$NEW" 1 tap10
[ -f "$OLD" ] && run_case "⑥ 旧挂点(ip4-output)、有 LCP" "$OLD" 1

log "结论"
if [ "$FAIL" -eq 0 ]; then
  echo "全通：O3 注册本身不打死数据面（转发与管理面都在）"
else
  echo "有用例失败；对照 ①..⑥，看是插件、LCP 还是钩子引入的"
fi
exit $FAIL
