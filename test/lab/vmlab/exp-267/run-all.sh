#!/bin/bash
# 问题5（#267）受控对照：三臂各两轮，交替跑，跑完复原并出报表。
#   V0 = 完全没绑核（VPP 放开到两核 + 中断放开 + sr-* 放开）
#   V1 = 现状形状（VPP 单核，WAN 中断与 VPP 同核）
#   V2 = 绑核方案（VPP 单核，WAN 中断挪到另一个核）
# 全部只改运行时亲和性：不重启进程、不写配置、不动磁盘；虚拟机重启即复原。
# 命题 3 那臂（V3）不在这里，单独补两块： bash block.sh V3 90 rep1 / rep2
# 任何一块摆臂失败就中止出数，但复原一定执行——不能把盒子留在摆过的形状里。
EXP=$(cd "$(dirname "$0")" && pwd)
SEC=${1:-90}
fail=""
for rep in 1 2; do
  for arm in V0 V1 V2; do
    bash "$EXP/block.sh" "$arm" "$SEC" "rep$rep" || { fail="arm=$arm rep=$rep"; break 2; }
    echo
  done
done
echo "=== 复原 VPP 到它原来的核 ==="
bash "$EXP/arm.sh" Z
if [ -n "$fail" ]; then
  echo "FATAL: $fail 那块作废（VPP 已复原），本次不出报表"
  exit 1
fi
echo
python3 "$EXP/report.py"
