#!/bin/bash
# 量一次「各主机 epoch - Mac epoch」，给 gap-report.py 对齐时间轴用。
#
# 为什么要单独记：客户 VM 的时钟能和大管子的钟差好几个小时（2026-11-05 实测
# cust4 比 Mac 快 28677s、CPE4 慢 223s）。探针时间戳来自 VM、gate 相位时间戳来自盒子，
# 不先把两套钟钉到同一条轴上，"中断窗口落在哪一阶段"这个问题根本没法答。
#
# 用法：clock-anchor.sh <输出文件> <主机别名...>     （第一个约定写盒子，标签用 box）
set -u
OUT=$1; shift
: > "$OUT"
for h in "$@"; do
  now_mac=$(date +%s)
  now_h=$(ssh -o ConnectTimeout=8 "$h" 'date +%s' 2>/dev/null) || { echo "$h ssh 不通" >&2; continue; }
  # 两次读数之间还夹着 ssh 往返，取中点当同一瞬间
  mid=$(( (now_mac + $(date +%s)) / 2 ))
  printf '%s %s\n' "$h" "$((now_h - mid))" >> "$OUT"
done
cat "$OUT"
