#!/bin/bash
# 一个对照块：摆臂 -> 同时打四条 ping 序列 + 1Hz 读插件探针 + 10s 采盒内每核负载 + 采宿主 CPU -> 汇总
# 用法： block.sh <V0|V1|V2|V3|Z> <秒> <备注>
ARM=${1:?臂}; SEC=${2:-90}; NOTE=${3:-}
EXP=$(cd "$(dirname "$0")" && pwd)
TAG=$(date +%Y%m%d-%H%M%S)
OUT=$EXP/raw/$TAG-$ARM
mkdir -p "$OUT"
echo "block arm=$ARM sec=$SEC note=$NOTE tag=$TAG" | tee "$OUT/block.txt"

# 采样脚本先铺到四台（只读脚本，放 /tmp，重启即消失）
for b in CPE1 CPE2 CPE3 CPE4; do
  ssh -o ConnectTimeout=6 "$b" 'cat > /tmp/cpu-samp.sh' < "$EXP/cpu-samp.sh"
done

echo "--- 摆臂 ---" | tee -a "$OUT/block.txt"
bash "$EXP/arm.sh" "$ARM" 2>&1 | tee "$OUT/arm.txt"
# 摆臂失败绝不让块继续：否则这块测的其实是上一臂的形状，数字会被当成新臂读走。
if grep -q '^ERR' "$OUT/arm.txt"; then
  echo "FATAL: 摆臂失败，本块作废（不出数）" | tee -a "$OUT/block.txt"
  exit 1
fi

PKTS=$((SEC*2))
pingrun() { ssh -o ConnectTimeout=6 "$2" "ping -i 0.5 -c $PKTS -W 1 $4 $3" > "$OUT/ping-$1.txt" 2>&1; }
probe_run() { ssh -o ConnectTimeout=6 "$1" "end=\$((\$(date +%s)+$SEC)); while [ \$(date +%s) -lt \$end ]; do date -u +%H:%M:%S; vppctl show sr-wg probe 2>/dev/null | grep -E 'rtt|wg[0-9]'; sleep 1; done" > "$OUT/probe-$1.txt" 2>&1; }
core_run() { ssh -o ConnectTimeout=6 "$1" "end=\$((\$(date +%s)+$SEC)); while [ \$(date +%s) -lt \$end ]; do date -u +%H:%M:%S; bash /tmp/cpu-samp.sh 10; done" > "$OUT/core-$1.txt" 2>&1; }

pingrun ovl_1_3 cust12 172.16.3.1 "-I 172.16.12.1" &
pingrun ovl_1_4 cust12 172.16.4.1 "-I 172.16.12.1" &
pingrun ovl_3_4 cust3  172.16.4.1 "-I 172.16.3.1"  &
pingrun undl_1_3 CPE1  192.168.1.103 "-I 192.168.1.101" &
probe_run CPE1 &
probe_run CPE3 &
core_run CPE1 &
core_run CPE3 &
host_run() { end=$(( $(date +%s) + SEC )); while [ $(date +%s) -lt $end ]; do
  date -u +%H:%M:%S; sysctl -n vm.loadavg
  top -l 2 -s 3 -n 10 -o cpu -stats pid,command,cpu 2>/dev/null | grep -E 'VBoxHeadless|Virtualization|PID' | tail -12
  sleep 15; done; }
host_run > "$OUT/host.txt" 2>&1 &
wait

echo "--- 块末复核：WAN 中断实际落在哪个核 ---" | tee -a "$OUT/block.txt"
for b in CPE1 CPE2 CPE3 CPE4; do
  ssh -o ConnectTimeout=6 "$b" 'printf "%s irq_aff=%s virtio1_cpu0=%s cpu1=%s vpp_core=%s\n" "$(hostname)" "$(cat /proc/irq/$(awk "/virtio1/{gsub(\":\",\"\",\$1);print \$1;exit}" /proc/interrupts)/smp_affinity_list)" "$(awk "/virtio1/{print \$2}" /proc/interrupts)" "$(awk "/virtio1/{print \$3}" /proc/interrupts)" "$(taskset -cp $(pgrep -x vpp_main|head -1)|sed "s/.*: //")"' 2>&1 | grep -v '^Warning' | tee -a "$OUT/block.txt"
done

echo "--- ping 汇总 ---" | tee -a "$OUT/block.txt"
for f in "$OUT"/ping-*.txt; do
  n=$(basename "$f" .txt); n=${n#ping-}
  printf "%s  %s\n     %s\n" "$n" "$(grep -E 'packet loss' "$f" | tail -1)" "$(grep -E 'rtt min|round-trip' "$f" | tail -1)" | tee -a "$OUT/block.txt"
done
echo "raw=$OUT"
