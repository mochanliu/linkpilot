#!/bin/bash
# 问题6（#270）：把 `cpu { main-core 0 }` 铺到盒子上并让它生效。
#
# 为什么这条操作要在页面上点名：main-core 是 VPP 的**启动期**配置，改它必须 systemctl restart vpp，
# 那是必然中断客户流量的操作。CPE4 实测（docs/vmlab-rebuild-plan.md §9.18）：
#   判据A sr-fib 重连 2s；判据B 装表齐 13s；客户可见中断 15.3s（双向各 15 条 LOSS，丢包率 0.2%）。
# 每台重启前都在盒内挂一个看门狗：60s 后 VPP 还没起来就退回旧配置再重启一次
# （管理 IP 在 VPP 的 vpp0 上，配置写坏会让 ssh 一起没了，回滚不能靠我这条连接）。
#
# 用法（在 Mac 上跑，盒子走 ~/.ssh/config 别名）：
#   BOXES="CPE1 CPE2 CPE3" bash deploy-main-core.sh
set -uo pipefail

BOXES=${BOXES:-}
CORE=${CORE:-0}
[ -n "$BOXES" ] || { echo "BOXES 必填，例：BOXES=\"CPE1\""; exit 2; }

stanza="cpu { main-core $CORE }"

for box in $BOXES; do
  echo "=============== $box ==============="
  ssh "$box" "bash -s" <<EOS
set -e
cd /root/vmlab
if grep -qE '^cpu \{ main-core' startup-vmlab.conf; then
  echo "配置里已有 cpu 段：\$(grep -E '^cpu \{' startup-vmlab.conf)（本脚本不重复插）"
else
  [ -f startup-vmlab.conf.270.bak ] || cp -p startup-vmlab.conf startup-vmlab.conf.270.bak
  awk '/^unix \{/{print "$stanza"; print ""} {print}' startup-vmlab.conf > startup-vmlab.conf.new
  mv -f startup-vmlab.conf.new startup-vmlab.conf
  echo "插入：$stanza（备份 startup-vmlab.conf.270.bak）"
fi
# 看门狗：脱离 ssh 跑，回滚动作在盒子里完成
setsid nohup bash -c 'sleep 60; if ! pgrep -x vpp_main >/dev/null; then
  cp -p /root/vmlab/startup-vmlab.conf.270.bak /root/vmlab/startup-vmlab.conf
  systemctl restart vpp; date -u +%FT%TZ >> /root/vmlab/rollback-270.log; fi' >/dev/null 2>&1 </dev/null &
echo "看门狗 pid=\$!"
EOS

  # 客户侧连续探针：两条路径都过被重启的那台（12 侧出口可能是 CPE1 或 CPE2）
  here=$(cd "$(dirname "$0")" && pwd)
  for p in "cust4:172.16.4.1:172.16.3.1" "cust12:172.16.12.1:172.16.3.1"; do
    h=${p%%:*}; rest=${p#*:}; src=${rest%%:*}; dst=${rest##*:}
    scp -q "$here/../exp-268/ping-probe.sh" "$h:/tmp/ping-probe.sh" 2>/dev/null
    ssh "$h" "rm -f /tmp/dep270.$h.out; setsid nohup bash /tmp/ping-probe.sh $src $dst 120 /tmp/dep270.$h.out >/dev/null 2>&1 </dev/null & " >/dev/null 2>&1
  done
  # 主机侧管理口探针（带时间戳，用来量管理通道自己的断口）：0.2s × 550 ≈ 110s
  ip=$(ssh "$box" 'ip -4 -br addr show vpp0 2>/dev/null | awk "{print \$3}" | cut -d/ -f1')
  ( ping -i 0.2 -c 550 "$ip" 2>/dev/null | while read -r l; do printf "%s %s\n" "$(date +%s.%N)" "$l"; done > "/tmp/mgmt-$box.out" ) &
  mgpid=$!
  sleep 5
  t0=$(date +%s)
  ssh "$box" 'systemctl restart vpp' 2>/dev/null
  echo "  restart 发出：$(date +%H:%M:%S)"
  vpp_at=""; fib_at=""; core=""
  for i in $(seq 1 90); do
    if [ -z "$vpp_at" ] && ssh "$box" 'vppctl show version >/dev/null 2>&1'; then vpp_at=$(( $(date +%s) - t0 )); fi
    if [ -n "$vpp_at" ] && [ -z "$core" ]; then
      core=$(ssh "$box" 'vppctl "show threads" 2>/dev/null | tr -d "\r" | awk "\$2==\"vpp_main\"{print \$7}"')
    fi
    if [ -n "$core" ] && [ -z "$fib_at" ] && ssh "$box" 'curl -s --max-time 2 http://127.0.0.1:19011/status' 2>/dev/null \
        | python3 -c 'import json,sys; sys.exit(0 if (json.load(sys.stdin).get("last") or {}).get("converged") else 1)' 2>/dev/null; then
      fib_at=$(( $(date +%s) - t0 )); break
    fi
    sleep 1
  done
  kill $mgpid 2>/dev/null; wait $mgpid 2>/dev/null
  echo "  VPP 可用 = ${vpp_at:-未恢复}s ｜ 装表收敛 = ${fib_at:-未恢复}s ｜ vpp_main 落核 = ${core:-读不到}（期望 $CORE）"
  ssh "$box" 'printf "  盒内读数：pid=%s mask=%s 隧道=%s r0/t250=%s/%s\n" \
    "$(pgrep -x vpp_main)" "$(taskset -cp $(pgrep -x vpp_main) | cut -d: -f2 | tr -d " ")" \
    "$(curl -s --max-time 2 http://127.0.0.1:19011/status | python3 -c "import json,sys;print(len(json.load(sys.stdin).get(\"tunnels\") or []))" 2>/dev/null)" \
    "$(vppctl "show ip fib" 2>/dev/null | grep -cE "^172\.16\.(3|12)\.1/32")" \
    "$(vppctl "show ip fib table 250" 2>/dev/null | grep -cE "via 10\.250\.0\.[0-9]+ wg")"' 2>/dev/null
  for h in cust4 cust12; do
    scp -q "$h:/tmp/dep270.$h.out" "/tmp/dep270-$h-$box.out" 2>/dev/null
    printf "  客户探针 %s→cust3：" "$h"
    awk '/LOSS|unreachable|timeout/{n++} END{printf "中断行数=%d 总行数=%d\n", n+0, NR}' "/tmp/dep270-$h-$box.out" 2>/dev/null
  done
  echo "  回滚日志（应为空）：$(ssh "$box" 'cat /root/vmlab/rollback-270.log 2>/dev/null | tr "\n" " "')"
done
