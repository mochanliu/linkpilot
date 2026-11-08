#!/bin/bash
# #276 装路边的高分辨量具：采和 sample-leg.sh 一样的五列，但**每发只 fork 四次**
# （date / curl / tr / vppctl），字符串判断全留在 bash 里。
# 2026-11-08 实测：sample-leg.sh 那版每发多一条 `grep -o … | sed` 的捕获管道，中位间隔 102ms，
# 而 2→3 那段一共就两三发，分辨率直接把结论压扁了。这版中位 53ms。
#
# 列的含义、以及 in/ok 为什么要分开、ours 为什么不是"前缀行在不在" —— 全在 sample-leg.sh 的头部，
# 那份是解释，这份只是把同一套判据采得更快，不重复。
#
# 这里对 /status 的字面形状做了两个假设（改格式时先跑最下面那两条核验命令）：
#   rib 条目     压缩后含 "prefix":"<PFX>","rts":[…],"targets"…   ← 用 targets 认「可装」
#   pending 条目 压缩后含 "prefix":"<PFX>","rts":[…],"reason"…    ← 没有 targets
#   核验：curl -s 127.0.0.1:19001/status | tr -d ' \n' | grep -o '"prefix":"172.16.3.1/32","rts":.\{0,60\}'
#         curl -s 127.0.0.1:19001/status | tr -d ' \n' | grep -o '"reason":"[^"]*"'
# 注意 /status 默认是带缩进的美化 JSON（实测 1767 字节 vs 压缩 1071），所以必须先 tr 再比。
#
# 用法：sample-tight.sh [总秒数=70] [前缀=172.16.3.1/32]
set -u
DUR=${1:-70}
PFX=${2:-172.16.3.1/32}
end=$((SECONDS + DUR))
HEAD="\"prefix\":\"$PFX\""
while [ $SECONDS -lt $end ]; do
  T=$(date +%s.%3N)
  J=$(curl -s --max-time 0.3 http://127.0.0.1:19001/status 2>/dev/null | tr -d ' \n')
  if [ -z "$J" ]; then
    I=ERR; K=ERR; R=na
  else
    case "$J" in *"$HEAD"*) I=Y ;; *) I=N ;; esac
    # 逐个出现点都看一遍：一条前缀同时挂在 rib 和 pending 下时不能只看第一个。
    T2=$J; K=N
    while :; do
      S=${T2#*"$HEAD"}
      [ "$S" = "$T2" ] && break
      case "${S:0:200}" in *'"targets"'*) K=Y; break ;; esac
      T2=$S
    done
    if [ "$I" = Y ] && [ "$K" = N ]; then
      # 只在真的"知道但不可装"时才付一次 fork，把理由抓出来（常态下这段不执行）。
      R=$(printf '%s' "$J" | grep -o "$HEAD[^}]*}" | sed -n 's/.*"reason":"\([^"]*\)".*/\1/p' | head -1)
      [ -z "$R" ] && R=na
    else R=na; fi
  fi
  F=$(timeout 2 vppctl "show ip fib $PFX" 2>/dev/null)
  if [ -z "$F" ]; then O=ERR; L=ERR
  else
    O=0; L=N
    while IFS= read -r ln; do
      case "$ln" in *'mpls via'*) O=$((O + 1)); continue ;; esac
      case "$ln" in "$PFX"*) L=Y ;; esac
    done <<< "$F"
  fi
  printf '%s in=%s ok=%s rsn=%s ours=%s line=%s\n' "$T" "$I" "$K" "$R" "$O" "$L"
  sleep 0.005
done
