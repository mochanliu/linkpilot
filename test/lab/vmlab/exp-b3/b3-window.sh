#!/bin/bash
# B3 实测驱动：可达性变化（BFD 判 down/up）到 sr-fib 把那条路径从 VPP FIB 里摘掉/装回的那一段。
#
# 为什么必须有这一把：sr-fib 的 convergeLoop 只被 wake 驱动，而 #276 之后 wake 只有两个来源
# —— sr-route 的 SSE 事件、以及启动/重连时那一次全量。隧道不通但路由表一个字没变时，BFD 的
# 结论没人拿去重算计划：摘路那一枪要么等下一发路由事件（=没有期限），要么（老轮询那一版）
# 等下一轮 poll（=秒级封顶）。这一把量的就是这一段。
#
# **注入必须在对端做**（2026-11-09 实测出来的教训）：在本盒 `set interface state wg1010 down`
# 看着像在测 sr-fib，其实不是 —— 口一 down，本机那条 `mpls via` 邻接自己就失效了，VPP 不等
# sr-fib 就把这一腿从转发链里摘掉（CPE1 那一枪：sr-fib 一条日志都没打，mpls via 照样归 0，
# 客户探测 1619 发只 FAIL 1 发）。那种枪测的是 VPP 的接口级反应，B3 改不改都一样。
# 只有「本盒口一直 up、只有 BFD 判 down」才是可达性输入的形状：把对端的隧道口 down 掉，本盒的
# 邻接仍然是 resolved，唯一能摘掉这条路径的就是 sr-fib。采样器因此必须同时盯本机口态（控制变量）。
#
# 口径（三臂只差在被测盒上跑的那一份 sr-fib）：
#   锚点一律取被测盒自己采到的 BFD 状态翻转（不试图跨盒对齐注入命令的时刻：这几台盒钟互差
#   0.8~8.2h，2026-11-09 实测，跨机对齐只会把 48ms 的量测成 480ms）。
#     t_bfd_down = 第一次看出该会话不是 Up；t_bfd_up = 之后第一次看出又是 Up
#     t_ours_gone = t_bfd_down 之后第一次看出 mpls via 条数低于起手值（单路径就是归 0）
#     t_ours_back = t_bfd_up 之后第一次看出条数回到起手值
#   摘路窗口 = t_ours_gone - t_bfd_down（这一段归 B3）；装回窗口 = t_ours_back - t_bfd_up
#   黑洞时长 = 故障窗口里 mpls via 条数仍等于起手值的采样时长（改前那一版的直接症状）
#   客户可见 = 探测机上 ok/FAIL 的分布（自钟自算，只按段读，不与盒上时刻对齐）
#
# 还有一条必须分开的岔路：`mpls via` 变少有两种成因 —— ①sr-fib 因为 BFD 非 Up 摘了这条路径
# （路由还在 rib 里、还带着 next_hops），②对端把这条路由撤了（路由根本没了）。B3 只管①，
# 所以紧采样器之外再挂一个 0.5s 的慢采样器记这条前缀在 sr-route /status 的 rib 里还在不在、
# 可不可装；摘路那一段必须配着它读。
#
# 用法：bash b3-window.sh [BOX] [PFX] [PEER] [PB] [SRC] [PEERBOX] [PEERTUN] [DOWN秒]
#   默认 CPE3 172.16.12.1/32 10.250.0.0 cust3 172.16.3.1 CPE1 wg1009 20
#   BOX=被测盒（采样在这台上）；PEER=这条隧道在对端的 overlay 地址（用来认 BFD 会话）；
#   PFX=被盯前缀（最好是有两条路径的那种，能读出「摘掉死腿=不丢包 / 留着死腿=丢一半且不自愈」）；
#   PB/SRC=客户探测机与其源地址；PEERBOX/PEERTUN=在对面哪台、down 哪个口。
#   时间轴：静默 12s → 对端 down → DOWN 秒 → 对端 up → 再采到采样满 52s。
# **会动状态**：在 PEERBOX 上对 PEERTUN 各一次 down / 一次 up（单发，不循环 —— 2026-11-08 曾因
# 守卫比对了两种拼写把同一个口抖了 40 次）。注入的是被测故障，不是配置生效路径。
set -eu
BOX=${1:-CPE3}
PFX=${2:-172.16.12.1/32}
PEER=${3:-10.250.0.0}
PB=${4:-cust3}
SRC=${5:-172.16.3.1}
PEERBOX=${6:-CPE1}
PEERTUN=${7:-wg1009}
HOLD=${8:-20}
DST=${PFX%/*}
WORK=${WORK:-/tmp/b3}
STATUS_URL=${STATUS_URL:-http://127.0.0.1:19001/status}
# 本机隧道口：从 sr-fib 的活表里按 PEER 反查，不手写（写错就白测一臂）。
LOCALTUN=${LOCALTUN:-$(ssh -n -o BatchMode=yes "$BOX" \
  "curl -s --max-time 3 http://127.0.0.1:19011/status" 2>/dev/null \
  | tr -d ' \n' | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    print("")
    raise SystemExit
for t in d.get("tunnels", []):
    if t.get("next_hop") == "'"$PEER"'":
        print(t["interface"])
        break
' 2>/dev/null || true)}
TAG=$BOX-$PEERTUN
LOG=$WORK/window-$TAG.txt
SAMPLE=$WORK/sample-$TAG.csv
RIBCSV=$WORK/rib-$TAG.csv
PROBE=$WORK/probe-$TAG.raw
mkdir -p "$WORK"

{
  echo "== B3 window: 被测盒=$BOX pfx=$PFX 死腿对端=$PEER（注入 $PEERBOX:$PEERTUN）探测=$PB:$SRC→$DST  $(date -u +%FT%TZ)"
  [ -n "$LOCALTUN" ] || { echo "   !! 没在 $BOX 的 sr-fib 活表里查到 next_hop=$PEER 的隧道口，本机口态这一列没法当控制变量用" >&2; exit 1; }
  # 注意：这里不能加 -n —— ssh -n 把 stdin 换成 /dev/null，heredoc 就送不到远端，
  # 整段 PRE 会一声不响地输出 0 行（2026-11-09 踩过）。
  ssh -o BatchMode=yes "$BOX" bash -s -- "$LOCALTUN" "$PEER" "$PFX" <<'PRE'
set -u
TUN=$1; PEER=$2; PFX=$3
PID=$(pgrep -x sr-fib | head -1 || true)
printf '   被测盒 sr-fib pid=%s 跑=%s 盘=%s\n' "$PID" \
  "$(sha256sum /proc/$PID/exe 2>/dev/null | cut -c1-16)" \
  "$(sha256sum /usr/local/bin/sr-fib | cut -c1-16)"
echo "   本机口 $TUN=$(vppctl "show interface $TUN" 2>/dev/null | awk -v n="$TUN" '$1==n{print $3}')（整枪必须一直是 up，否则这一臂作废）"
echo "   起手：$PFX 的 mpls via=$(vppctl "show ip fib $PFX" 2>/dev/null | grep -c 'mpls via') 条；日志行基线=$(wc -l </var/log/sr-box/fib.log)"
PRE
} | tee "$LOG"

# 客户侧探测：45 秒，速率由链路自己决定（通的时候 ~30-45 发/秒，断的时候 1 发/秒）。
ssh -n -o BatchMode=yes "$PB" \
  'end=$((SECONDS+46)); while [ $SECONDS -lt $end ]; do printf "%s " $EPOCHREALTIME; timeout 1 ping -c1 -W1 -I '"$SRC"' '"$DST"' >/dev/null 2>&1 && echo ok || echo FAIL; done' \
  >"$PROBE" 2>/dev/null &
PROBER=$!

# 紧采样器（被测盒，~50ms 一发）+ 慢采样器（rib 形状，0.5s 一发）
ssh -o BatchMode=yes "$BOX" bash -s -- "$LOCALTUN" "$PEER" "$PFX" <<'REMOTE' >"$SAMPLE" &
set -u
TUN=$1; PEER=$2; PFX=$3
end=$(( ${EPOCHREALTIME%.*} + 52 ))
while :; do
  # 一发三个 fork（vppctl ×3），取时与判定全留在 bash 内建
  b=$(vppctl show bfd sessions 2>/dev/null) || b=""
  st=NA; seen=0
  while IFS= read -r ln; do
    case $ln in
    *"$PEER"*) seen=1 ;;
    *"IPv4 address"*)
      if [ $seen = 0 ]; then continue; fi
      ;;
    esac
    if [ $seen = 1 ] && [ "${ln#*Session state}" != "$ln" ]; then
      set -- $ln
      st=$3
      seen=0
    fi
  done <<<"$b"

  f=$(vppctl "show ip fib $PFX" 2>/dev/null) || f=""
  n=0
  while IFS= read -r ln; do
    [ "${ln#*mpls via}" != "$ln" ] && n=$((n + 1))
  done <<<"$f"

  i=$(vppctl "show interface $TUN" 2>/dev/null) || i=""
  ts=NA
  while IFS= read -r ln; do
    set -- $ln
    [ "$1" = "$TUN" ] && ts=$3
  done <<<"$i"

  printf 'S %s %s %s %s\n' "$EPOCHREALTIME" "$st" "$n" "$ts"
  [ ${EPOCHREALTIME%.*} -ge $end ] && break
  sleep 0.02
done
REMOTE
SAMPLER=$!

ssh -o BatchMode=yes "$BOX" bash -s -- "$PFX" "$STATUS_URL" <<'RIBSAMP' >"$RIBCSV" &
set -u
PFX=$1; URL=$2
end=$(( ${EPOCHREALTIME%.*} + 53 ))
while :; do
  v=$(curl -s --max-time 2 "$URL" | python3 -c '
import json,sys
pfx=sys.argv[1]
try:
    d=json.load(sys.stdin)
except Exception:
    print("ERR ERR"); raise SystemExit
inr=ok=0
for key,rs in (d.get("rib") or {}).items():
    for r in rs:
        if r.get("prefix")==pfx:
            inr=1
            if r.get("next_hops"):
                ok=1
print(inr, ok)' "$PFX" 2>/dev/null) || v="ERR ERR"
  [ -z "$v" ] && v="ERR ERR"
  printf 'R %s %s\n' "$EPOCHREALTIME" "$v"
  [ ${EPOCHREALTIME%.*} -ge $end ] && break
  sleep 0.5
done
RIBSAMP
RIBSAMPER=$!

# 注入：单发 down → 停 HOLD 秒 → 单发 up（在对端，本盒的口全程 up）
sleep 12
{
  echo "   --- 注入（$PEERBOX 的钟，不与被测盒对齐）---"
  # heredoc 是引号形式（'INJ'）：本地变量不会展开，必须按位置参数送进远端，
  # 否则远端 set -u 直接 unbound variable、注入根本没发生（2026-11-09 踩过）。
  ssh -o BatchMode=yes "$PEERBOX" bash -s -- "$PEERTUN" "$HOLD" "$PEERBOX" <<'INJ'
set -u
TUN=$1; HOLD=$2; BX=$3
echo "   down 发出 于 $BX 钟 $(date +%s.%3N) 口态=$(vppctl "show interface $TUN" 2>/dev/null | awk -v n="$TUN" '$1==n{print $3}')"
vppctl "set interface state $TUN down"
echo "   down 返回 于 $(date +%s.%3N) 口态=$(vppctl "show interface $TUN" 2>/dev/null | awk -v n="$TUN" '$1==n{print $3}')"
sleep "$HOLD"
echo "   up   发出 于 $(date +%s.%3N)"
vppctl "set interface state $TUN up"
echo "   up   返回 于 $(date +%s.%3N) 口态=$(vppctl "show interface $TUN" 2>/dev/null | awk -v n="$TUN" '$1==n{print $3}')"
INJ
} | tee -a "$LOG"

wait "$SAMPLER" "$RIBSAMPER" "$PROBER" || true

{
  python3 - "$SAMPLE" "$PROBE" "$RIBCSV" "$TAG" "$WORK" <<'PY'
import sys
sample, probe, ribcsv, tag, work = sys.argv[1:6]
ts, st, ours, tun = [], [], [], []
for ln in open(sample):
    p = ln.split()
    if p[0] == "S":
        ts.append(float(p[1])); st.append(p[2]); ours.append(int(p[3])); tun.append(p[4])
gaps = sorted(b - a for a, b in zip(ts, ts[1:]))
med = gaps[len(gaps) // 2] if gaps else float("nan")
base = max(set(ours[:20]), key=ours[:20].count) if ts else 0
print(f"   紧采样 {len(ts)} 发，间隔中位 {med*1000:.0f}ms（=分辨率）；起手 mpls via 条数={base}")
print(f"   本机口全程：{'全是 up' if set(tun) == {'up'} else sorted(set(tun))}（必须一直 up，否则这一臂测的是 VPP 的接口级反应，不是 B3）")


def first(pred, lo=0.0):
    for i, t in enumerate(ts):
        if t >= lo and pred(i):
            return t
    return None


def win(name, a, b, hard=None):
    if a is None or b is None:
        print(f"   {name}: 读不出来{'（=从未发生）' if hard else ''}")
        return None
    d = (b - a) * 1000
    print(f"   {name}: {d:.0f}ms（{a:.3f} → {b:.3f}）")
    return d


# 前 10 发是静默基线：那儿出现非 Up 说明认错了会话或注入提前了，不作数
t_bfd_down = first(lambda i: i >= 10 and st[i] != "Up")
t_bfd_up = first(lambda i: st[i] == "Up", t_bfd_down or 0) if t_bfd_down else None
if t_bfd_down is None:
    print("   !! 整段没看到 BFD 状态翻转：注入没生效或被测会话不是这一条 —— 这一臂作废")
    sys.exit(0)
t_ours_gone = first(lambda i: ours[i] < base, t_bfd_down)
t_ours_back = first(lambda i: ours[i] >= base, t_bfd_up or 0) if t_bfd_up else None
win("BFD 判 down → 摘掉死腿（这一段归 B3）", t_bfd_down, t_ours_gone)
if t_ours_gone is None:
    dead = sum(1 for t, n in zip(ts, ours) if t_bfd_down <= t <= (t_bfd_up or ts[-1]) and n == base)
    print(f"   !! 故障全程 {dead * med:.0f}s 里那条死腿一直挂在 FIB 上（sr-fib 一次都没收敛）")
else:
    print(f"   故障窗口时长 {(t_bfd_up - t_bfd_down):.2f}s，其中死腿还在的采样时长 "
          f"{sum(1 for t, n in zip(ts, ours) if t_bfd_down <= t < t_ours_gone and n == base) * med:.2f}s")
win("BFD 回到 Up → 装回这条腿（这一段归 B3）", t_bfd_up, t_ours_back)
if t_ours_back is None:
    print("   !! 到采样结束这条腿仍未装回")
win("端到端（BFD 判 down → 又回到 Up）", t_bfd_down, t_bfd_up)

rt, rin, rok = [], [], []
for ln in open(ribcsv):
    p = ln.split()
    if p[0] == "R":
        rt.append(float(p[1])); rin.append(p[2]); rok.append(p[3])
trans = [(rt[i], f"rib={rin[i - 1]}/{rok[i - 1]}→{rin[i]}/{rok[i]}")
         for i in range(1, len(rt)) if (rin[i], rok[i]) != (rin[i - 1], rok[i - 1])]
print(f"   rib 采样 {len(rt)} 发（0.5s 一发）；翻转 {len(trans)} 次" + ("：成因分不清，摘路不能记到 B3 头上" if trans else "（无 ⇒ 条数变少只能是 BFD 摘路，不是对端撤路由）"))
for t, d in trans:
    print(f"     {t:.3f} {d}")

rows = []
for ln in open(probe):
    p = ln.split()
    if len(p) == 2 and p[1] in ("ok", "FAIL"):
        rows.append((float(p[0]), p[1] == "ok"))
bad = [t for t, ok in rows if not ok]
print(f"   客户探测 {len(rows)} 发，FAIL {len(bad)} 发 = {100 * len(bad) / max(1, len(rows)):.1f}%")
runs, cur = [], []
for t in bad:
    if cur and t - cur[-1] <= 2.5:
        cur.append(t)
    else:
        if cur:
            runs.append(cur)
        cur = [t]
if cur:
    runs.append(cur)
for r in runs:
    print(f"     连续断流 {len(r)} 发  {r[-1] - r[0]:.2f}s")
with open(f"{work}/timeline-{tag}.txt", "w") as f:
    for i, t in enumerate(ts):
        f.write(f"{t:.3f} bfd={st[i]:<9} ours={ours[i]} tun={tun[i]}\n")
print(f"   逐发明细已落 {work}/timeline-{tag}.txt")
PY
  echo "   --- 这一枪期间 sr-fib 自己说了什么（fib.log 尾部 25 行）---"
  ssh -n -o BatchMode=yes "$BOX" "tail -25 /var/log/sr-box/fib.log" | sed 's/^/     /'
} | tee -a "$LOG"
