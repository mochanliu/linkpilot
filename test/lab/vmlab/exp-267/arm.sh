#!/bin/bash
# 摆臂：只改运行时 CPU 亲和性（VPP 线程 / 四个 sr-* 进程 / 网卡硬中断），
# 不重启任何进程、不写任何配置文件、不改内核参数。重启虚拟机即全部复原。
#
#   V0  完全没绑核：VPP 主线程放开到 0-1，四个 sr-* 放开到 0-1，WAN 中断放开到 0-1
#   V1  WAN 网卡中断和 VPP 主线程同核（= 现在 CPE1/2/4 的天然形状，强制铺到四台）
#   V2  WAN 网卡中断挪到另一个核，sr-* 进程仍可跑两核
#   V3  WAN 网卡中断在另一核 + 四个 sr-* 进程全钉到另一核（VPP 核只剩 VPP）
#   Z   复原：VPP 回它原来的核（第一次进本脚本时记在 state/ 里），其余全放开
ARM=${1:?V0|V1|V2|V3|Z}
BOXES=${BOXES:-"CPE1 CPE2 CPE3 CPE4"}
EXP=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$EXP/state"

for b in $BOXES; do
  ssh -o ConnectTimeout=6 "$b" ARM="$ARM" 'bash -s' <<'REMOTE'
vp=$(pgrep -x vpp_main | head -1)
if [ -z "$vp" ]; then echo "ERR: vpp_main 不在跑"; exit 1; fi
orig=$(cat /root/exp-267-vppcore 2>/dev/null)
if [ -z "$orig" ]; then orig=$(taskset -cp "$vp" | sed 's/.*: //'); echo "$orig" > /root/exp-267-vppcore; fi
# 参考核用「VPP 原来独占的那个核」(orig)，不能用当前掩码：V0 臂会把掩码放开成 0,1，
# 若从当前掩码反推，V0 之后每一臂都会失败并让整块测成 V0 的形状（2026-11-05 20:47 踩过）。
case "$orig" in
  0) vbit=1; obit=2; oth=1 ;;
  1) vbit=2; obit=1; oth=0 ;;
  *) echo "ERR: 记录的原核不是单核（orig=$orig），本脚本只处理 2 vCPU 盒"; exit 1 ;;
esac
# WAN 网卡 = enp0s9 = virtio1，它的 IRQ 号要从 /proc/interrupts 现查，不能写死
irq=$(awk '/virtio1/ {gsub(":","",$1); print $1; exit}' /proc/interrupts)
[ -n "$irq" ] || { echo "ERR: 找不到 virtio1 的 IRQ"; exit 1; }

case "$ARM" in
  V0) taskset -pc 0-1 "$vp" >/dev/null; echo 3 > "/proc/irq/$irq/smp_affinity"; tm=0-1 ;;
  V1) taskset -pc "$orig" "$vp" >/dev/null; echo "$vbit" > "/proc/irq/$irq/smp_affinity"; tm=0-1 ;;
  V2) taskset -pc "$orig" "$vp" >/dev/null; echo "$obit" > "/proc/irq/$irq/smp_affinity"; tm=0-1 ;;
  V3) taskset -pc "$orig" "$vp" >/dev/null; echo "$obit" > "/proc/irq/$irq/smp_affinity"; tm="$oth" ;;
  Z)  taskset -pc "$orig" "$vp" >/dev/null; echo 3 > "/proc/irq/$irq/smp_affinity"; tm=0-1 ;;
  *)  echo "ERR: 未知臂 $ARM"; exit 1 ;;
esac

for n in sr-agent sr-route sr-fib sr-frr; do
  p=$(pgrep -x "$n" | head -1)
  if [ -n "$p" ]; then taskset -pc "$tm" "$p" >/dev/null; else echo "WARN: $n 没在跑"; fi
done

# 落账：把实际生效的值读回来（写入成功不等于生效）
sleep 1
printf "arm=%s box=%s vpp_pid=%s vpp_mask=%s vpp_orig=%s irq=%s irq_aff=%s cpu0=%s cpu1=%s" \
  "$ARM" "$(hostname)" "$vp" "$(taskset -cp "$vp" | sed 's/.*: //')" "$orig" "$irq" \
  "$(cat /proc/irq/$irq/smp_affinity_list)" \
  "$(awk '/virtio1/{print $2}' /proc/interrupts)" "$(awk '/virtio1/{print $3}' /proc/interrupts)"
for n in sr-agent sr-route sr-fib sr-frr; do
  p=$(pgrep -x "$n" | head -1)
  [ -n "$p" ] && printf " %s=%s" "$n" "$(taskset -cp "$p" | sed 's/.*: //')"
done
echo
REMOTE
  # VPP 原始核同时在 Mac 侧记一份，防止 /root 下的那份被清
  [ -f "$EXP/state/vppcore.$b" ] || ssh -o ConnectTimeout=6 "$b" 'cat /root/exp-267-vppcore' 2>/dev/null | tr -d '\r\n' > "$EXP/state/vppcore.$b"
done

