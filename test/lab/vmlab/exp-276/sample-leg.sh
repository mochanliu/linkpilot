#!/bin/bash
# #276 切片 B/C 的量具：**在盒子本地**一个循环里、同一个时钟采这条腿的各段。
#
#   in=Y|N|ERR   这条前缀在 sr-route /status 里出现过（rib 段或 pending 段任一）—— 控制面**知道**它
#   ok=Y|N       它出现在带 next_hops 的那一份视图里 —— sr-route 已经认为这条路径**可装**
#   rsn=<理由|->  只在 pending 里时给的理由（no_path / rt_mismatch / …）
#   ours=<n>     VPP 里这条前缀还有几条 `mpls via` 路径（0 = 我们的转发不在）—— 数据面**生效**
#   line=Y|N     这条前缀的 FIB 行还在不在 —— 用来暴露"第二个写手"
#
# 为什么要 in 和 ok 两列（2026-11-08 实测踩到的）：/status 的 `pending` 段里每一条也带
# "prefix" 字段（当前这台 lab 里就有 device 2 的 172.16.12.1/32，reason=no_path），所以
# 只 grep "prefix" 会把"这条前缀我听说过、但下一跳还落不了地"和"这条前缀已经可装"读成同一个信号。
# 装路边因此被算进了一段不属于切片 B 的账：in→ok 是 sr-route 的可达性/隧道门（#262、#275 的地盘），
# ok→ours 才是 sr-route→sr-fib→VPP 这条腿（切片 B 的地盘）。两列分开才能各自给数。
#
# ok 的口径和 AllRIB 一致：/status 的 rib 段就是 viewLocked 渲染出来的那份视图（同一个
# importTargets + 同一份 nextHops），所以 ok=Y 当且仅当 watch 的合并窗口该发出这条 upsert。
#
# 为什么 ours 而不是"前缀行在不在"：盒子里 linux_cp 会把内核路由镜像进 VPP 主表
# （src-flags `lcp-rt-dynamic`，pref=20），而 sr-frr 正向注入的 K 路由就在同一张内核主表里
# ⇒ sr-fib 撤掉自己那条 MPLS 路径之后前缀行还在，只是换了署名。只看行会把这条腿量短。
#
# 为什么必须盒内跑：ssh 一条命令的往返本身 20ms+，Mac↔盒子这条管理链路只有 12KB/s 量级
# （2026-11-08 实测 512KB/39.6s）—— 从外面采等于把链路算进结论里。
# 为什么循环里一个 python3 都不留：每发起一次 python3 会把中位间隔顶到 247ms，
# 比要量的那段还粗；curl+grep+tr、vppctl 只 dump 这一条前缀，中位间隔在几十毫秒。
#
# 用法：sample-leg.sh [总秒数=70] [前缀=172.16.3.1/32]
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
    # 每一条出现都截到它所在对象的右括号：rib 条目以 "next_hops":[…] 收尾，pending 条目以 "reason":"…" 收尾。
    E=$(printf '%s' "$J" | grep -o "$HEAD[^}]*}")
    if [ -z "$E" ]; then I=N; K=N; R=na
    else
      I=Y
      case "$E" in *next_hops*) K=Y ;; *) K=N ;; esac
      R=$(printf '%s\n' "$E" | sed -n 's/.*"reason":"\([^"]*\)".*/\1/p' | head -1)
      [ -z "$R" ] && R=na
    fi
  fi

  F=$(timeout 2 vppctl "show ip fib $PFX" 2>/dev/null)
  if [ -z "$F" ]; then O=ERR; L=ERR
  else
    O=$(printf '%s\n' "$F" | grep -c "mpls via")
    if printf '%s\n' "$F" | grep -q "^$PFX"; then L=Y; else L=N; fi
  fi

  printf '%s in=%s ok=%s rsn=%s ours=%s line=%s\n' "$T" "$I" "$K" "$R" "$O" "$L"
  sleep 0.02
done
