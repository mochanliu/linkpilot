#!/bin/bash
# 问题6（#270）取样：一臂一把尺子不够，这里同时用三把独立仪器——
#   1) 主机 → 盒子管理口（192.168.1.10x，落在 VPP 的 vpp0 上，是 punt 到内核的路径）
#   2) 客户 VM → 客户 VM（进隧道、跨两台 CPE，owner 真正关心的那条）
#   3) 插件自己的探针（vppctl show sr-wg probe，纯数据面 CPE↔CPE，不经客户 VM、不经内核 ping）
# 三把同时降下来才敢说"客户确实快了"。ROUNDS 轮是为了压宿主调度漂移，反序复现靠多次跑本脚本。
#
# 用法（在 Mac 上跑）：
#   LABEL=F-全cpu0 bash sample.sh                 # 默认量四台管理口 + cust4<->cust3 + CPE3/4 探针
#   LABEL=G-全cpu1 HOSTS="192.168.1.102" bash sample.sh
set -uo pipefail
LABEL=${LABEL:-arm}
ROUNDS=${ROUNDS:-3}
N=${N:-40}
HOSTS=${HOSTS:-"192.168.1.101 192.168.1.102 192.168.1.103 192.168.1.104"}
PROBE_BOXES=${PROBE_BOXES:-"3 4"}
OVL=${OVL:-"cust4:172.16.4.1:172.16.3.1 cust3:172.16.3.1:172.16.4.1"}

for r in $(seq 1 "$ROUNDS"); do
  for ip in $HOSTS; do
    printf "%s 管理口 r%s %s " "$LABEL" "$r" "$ip"
    ping -c "$N" -i 0.2 "$ip" 2>/dev/null | tail -1
  done
done
for side in $OVL; do
  h=$(echo "$side" | cut -d: -f1); src=$(echo "$side" | cut -d: -f2); dst=$(echo "$side" | cut -d: -f3)
  for r in $(seq 1 "$ROUNDS"); do
    printf "%s overlay r%s %s->%s " "$LABEL" "$r" "$src" "$dst"
    ssh "$h" "ping -c $N -i 0.2 -W 2 -I $src $dst 2>/dev/null | tail -1" 2>/dev/null
  done
done
for b in $PROBE_BOXES; do
  printf "%s CPE%s 探针(300ms 定时，纯隧道内) " "$LABEL" "$b"
  ssh "CPE$b" 'for i in $(seq 1 24); do vppctl "show sr-wg probe" 2>/dev/null | awk "/rtt/{printf \"%s \", \$NF}"; sleep 1; done' 2>/dev/null \
    | tr -d '\r' | awk '{n=0;s=0;mx=0;for(i=1;i<=NF;i++){gsub(/[^0-9.]/,"",$i);if($i=="")continue;n++;s+=$i;if($i>mx)mx=$i}
        printf "n=%d avg=%.2f max=%.2f ms\n", n, s/(n?n:1), mx}'
done
