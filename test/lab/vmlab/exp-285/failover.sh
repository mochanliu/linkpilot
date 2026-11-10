#!/bin/bash
# 主/备切换实测：BFD 判 down 之后，「次优顶上」这一段是谁做的、用了多久。
#
# 为什么要单独开这一枪（2026-11-10）：exp-b3 那一枪的前缀有两条**同权**腿（两条隧道都是颜色 0，
# VPP 里 pref 都是 10 ⇒ ECMP，buckets:2），它量到的是「死腿摘掉、活腿继续承载」，
# 里面没有「次优」这个东西 —— 摘掉一条 ECMP 腿不叫主备切换。本盒这一枪把形状换成真主/备：
#   主 = 颜色 0（pref=10，经 CPE1），备 = 颜色 2（pref=12，经 CPE2），逃生开关开 ⇒ 两条都进主表。
# 起手时 VPP 的转发链 buckets:1，只有 pref=10 那条在承载，pref=12 挂着不承载。
#
# 这一枪要分辨的是两条完全不同的因果链，而它们长得一模一样：
#   ①VPP 原生的 preference 回落 —— 主路的**邻接**死了（口 down / 下一跳解析没了），
#     VPP 自己就把 uPRF 挪到次优那条，控制面一个字都不用说。
#   ②可达性输入 —— 只有 BFD 判 down，本机口还是 up、邻接还是 resolved。VPP 的 BFD 会话
#     不写回接口状态（产品里没有任何地方在 BFD down 时下发 sw_interface_set_flags：
#     vppapi.SetInterfaceUp 唯一的调用点是 internal/agent/dataplane.go:814，且恒传 up=true），
#     所以这条链里「次优顶上」只能由 sr-fib 重算 + 整条 ip_route_add_del 覆盖来做。
# 两个臂各测一条：MODE=peer 是②的形状（在对端 down 隧道口 ⇒ 本盒口全程 up），
# MODE=local 是①的形状（在本盒 down 主路口 ⇒ 不等 sr-fib）。
#
# 用法：bash failover.sh [BOX] [PFX] [PRIM_NH] [BAK_NH] [PB] [SRC] [PEERBOX] [DOWN秒] [MODE]
#   默认  CPE3 172.16.12.1/32 10.250.0.0 10.250.0.6 cust3 172.16.3.1 CPE1 20 peer
#   PRIM_NH/BAK_NH = 主/备两条腿在本盒 sr-fib 隧道表里的下一跳（也就是对端那条隧道自己的 overlay /32）。
#   注入点：MODE=peer 在 PEERBOX 上 down「L3 地址 == PRIM_NH/32」的那个 wg 口（自己查，不手写）；
#           MODE=local 在本盒上 down「隧道表里 next_hop == PRIM_NH」的那个 wg 口。
#   时间轴：静默 12s → down → DOWN 秒 → up → 采满 52s。
# **会动状态**：对注入的那个口各一次 down / 一次 up（单发，不循环）。
set -eu
BOX=${1:-CPE3}
PFX=${2:-172.16.12.1/32}
PRIM=${3:-10.250.0.0}
BAK=${4:-10.250.0.6}
PB=${5:-cust3}
SRC=${6:-172.16.3.1}
PEERBOX=${7:-CPE1}
HOLD=${8:-20}
MODE=${9:-peer}
DST=${PFX%/*}
WORK=${WORK:-/tmp/p285}
TABLE=${TABLE:-0}          # 主表；读别的表用 TABLE=2000 覆盖
# local 臂的口态就是注入本身，每发都读；peer 臂它整枪不变，5 发一次换分辨率。
if [ "$MODE" = local ]; then IFEVERY=1; else IFEVERY=5; fi
mkdir -p "$WORK"
TAG=$BOX-$MODE
LOG=$WORK/failover-$TAG.txt
SAMPLE=$WORK/sample-$TAG.csv
PROBE=$WORK/probe-$TAG.raw

# 注入没收口的兜底：脚本若在 down 与 up 之间死掉（set -e、Ctrl-C、ssh 掉线），
# 对端那个口会一直 down —— 现场留下一个没人承认的故障。EXIT 时补一次 up（口本来就 up 时 VPP 无变化）。
INJMARK=$WORK/inj-$TAG
cleanup() {
  if [ -f "$INJMARK" ] && [ ! -f "$INJMARK.done" ]; then
    echo "   !! 注入没收口（脚本中途退出）：在 $INJBOX 上补一次 $INJTUN up" >&2
    ssh -o BatchMode=yes "$INJBOX" "vppctl \"set interface state $INJTUN up\"" >/dev/null 2>&1 || true
  fi
  rm -f "$INJMARK"
}
trap cleanup EXIT

# 本盒两条腿的口名：只认 sr-fib 活表（写错就白测一臂）。
tunof() {
  ssh -n -o BatchMode=yes "$BOX" "curl -s --max-time 3 http://127.0.0.1:19011/status" 2>/dev/null |
    tr -d ' \n' | python3 -c '
import json,sys
want=sys.argv[1]
try:
    d=json.load(sys.stdin)
except Exception:
    raise SystemExit
for t in d.get("tunnels", []):
    if t.get("next_hop") == want:
        print(t["interface"]); break
' "$1" 2>/dev/null || true
}
PRIMTUN=$(tunof "$PRIM")
BAKTUN=$(tunof "$BAK")
[ -n "$PRIMTUN" ] && [ -n "$BAKTUN" ] || { echo "!! 在 $BOX 的 sr-fib 活表里查不到主/备口名（主=$PRIM 备=$BAK）—— 查 sr-fib 隧道表是否被 agent 派生过（#288 起由 sr-agent 维护，手工脚本已退役）" >&2; exit 1; }

# 注入点：peer 模式的口在 PEERBOX 上，判据是「该口 L3 == PRIM/32」（对端那条隧道的本端 overlay 就是 PRIM）。
if [ "$MODE" = "peer" ]; then
  INJBOX=$PEERBOX
  INJTUN=$(ssh -n -o BatchMode=yes "$PEERBOX" 'vppctl "show interface addr"' 2>/dev/null |
    awk -v want="$PRIM/" '(/^wg[0-9]+ /){n=$1} $0 ~ "L3 "want {print n}' | head -1 || true)
  [ -n "$INJTUN" ] || { echo "!! 在 $PEERBOX 上没找到 L3==$PRIM/32 的 wg 口 —— 注入点认不出来，这一臂作废" >&2; exit 1; }
else
  INJBOX=$BOX
  INJTUN=$PRIMTUN
fi

{
  echo "== 主/备切换: 被测盒=$BOX 前缀=$PFX 主=$PRIM($PRIMTUN) 备=$BAK($BAKTUN) 注入=$INJBOX:$INJTUN 模式=$MODE 探测=$PB:$SRC→$DST  $(date -u +%FT%TZ)"
  echo "   逃生开关=$(ssh -n -o BatchMode=yes "$BOX" "curl -s --max-time 3 http://127.0.0.1:19011/status" | tr -d ' \n' | python3 -c 'import json,sys;print(json.load(sys.stdin).get("escape_all_colors"))' 2>/dev/null)"
  # heredoc 不能配 ssh -n：stdin 被换成 /dev/null，整段会一声不响输出 0 行（exp-b3 踩过）。
  ssh -o BatchMode=yes "$BOX" bash -s -- "$PFX" "$PRIM" "$BAK" "$PRIMTUN" "$BAKTUN" "$TABLE" <<'PRE'
set -u
PFX=$1; PRIM=$2; BAK=$3; PT=$4; BT=$5; TABLE=$6
PID=$(pgrep -x sr-fib | head -1 || true)
printf '   被测盒 sr-fib pid=%s 跑=%s 盘=%s\n' "$PID" \
  "$(sha256sum /proc/$PID/exe 2>/dev/null | cut -c1-16)" "$(sha256sum /usr/local/bin/sr-fib | cut -c1-16)"
echo "   起手 表 $TABLE $PFX 的路径与承载："
vppctl "show ip fib $PFX table $TABLE" 2>/dev/null | awk '
  /path:\[/ && /pref=/{printf "     %s  oper=%s\n", $0, ($0 ~ /resolved/ ? "resolved" : "?")}' | sed 's/  */ /g'
vppctl "show ip fib $PFX table $TABLE" 2>/dev/null | awk '
  /forwarding:/{f=1} f && /mpls via/{print "     承载: " $0}' | sed 's/  */ /g'
printf '   本机两口态：主 %s=%s 备 %s=%s（peer 模式必须整枪 up，否则测的是接口级反应不是可达性输入）\n' \
  "$PT" "$(vppctl "show interface $PT" 2>/dev/null | awk -v n="$PT" '$1==n{print $3}')" \
  "$BT" "$(vppctl "show interface $BT" 2>/dev/null | awk -v n="$BT" '$1==n{print $3}')"
PRE
} | tee "$LOG"

# 客户侧探测：46 秒，速率由链路自己决定（通时 ~30-45 发/秒，断时 1 发/秒）。
ssh -n -o BatchMode=yes "$PB" \
  'end=$((SECONDS+46)); while [ $SECONDS -lt $end ]; do printf "%s " $EPOCHREALTIME; timeout 1 ping -c1 -W1 -I '"$SRC"' '"$DST"' >/dev/null 2>&1 && echo ok || echo FAIL; done' \
  >"$PROBE" 2>/dev/null &
PROBER=$!

# 紧采样器（被测盒）：一发四个 fork（bfd 会话 / 表 TABLE 的 FIB 行 / 两口态），分辨率由 fork 数决定。
ssh -o BatchMode=yes "$BOX" bash -s -- "$PFX" "$PRIM" "$BAK" "$PRIMTUN" "$BAKTUN" "$TABLE" "$IFEVERY" <<'REMOTE' >"$SAMPLE" &
set -u
PFX=$1; PRIM=$2; BAK=$3; PT=$4; BT=$5; TABLE=$6; IFEVERY=$7
end=$(( ${EPOCHREALTIME%.*} + 52 ))
state() {   # 从 show bfd sessions 里挑出 peer==$2 那条会话的 Session state
  local peer=$1 out=NA seen=0 ln
  while IFS= read -r ln; do
    case $ln in
      *"$peer"*) seen=1 ;;
      *"IPv4 address"*) [ $seen = 0 ] && continue ;;
    esac
    if [ $seen = 1 ] && [ "${ln#*Session state}" != "$ln" ]; then
      set -- $ln; out=$3; seen=0
    fi
  done <<<"$2"
  printf '%s' "$out"
}
n=0; cached="NA/NA"
while :; do
  b=$(vppctl show bfd sessions 2>/dev/null) || b=""
  ps=$(state "$PRIM" "$b"); bs=$(state "$BAK" "$b")
  f=$(vppctl "show ip fib $PFX table $TABLE" 2>/dev/null) || f=""
  # 口态是控制变量：peer 臂它整枪不变，5 发读一次就够（少一个 fork，分辨率 ~70ms）；
  # local 臂它就是注入本身，必须每发都读，否则缓存会把 down 的时刻滞后几百 ms。
  n=$((n + 1))
  if [ $((n % IFEVERY)) -eq 1 ] || [ "$IFEVERY" = 1 ]; then
    i=$(vppctl show interface 2>/dev/null) || i=""
    pt=NA; bt=NA
    while IFS= read -r ln; do
      set -- $ln
      [ "$1" = "$PT" ] && pt=$3
      [ "$1" = "$BT" ] && bt=$3
    done <<<"$i"
    cached="$pt/$bt"
  else
    pt=${cached%/*}; bt=${cached#*/}
  fi
  # 这一行必须存在，否则整发采样读的是别人的路由：show ip fib 是 LPM 查询，
  # 精确行被删掉时它回落打印覆盖它的那条（实测回落到 0.0.0.0/0 的管理网默认路由，
  # 里面 pref=20 via 192.168.1.1 + 一条 drop 路径）。第一版没判前缀行，把默认路由的
  # 两条路径当成我们的行读了（2026-11-09 peer 臂第二枪就是这么污染的）。
  # 判据 = 输出第二行「<前缀> fib:<表> index:…」的第一列必须逐字等于 $PFX。
  rowpfx=$(printf '%s' "$f" | awk '$1 ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\/[0-9]+$/ && $2 ~ /^fib:/ {print $1; exit}')
  if [ "$rowpfx" != "$PFX" ]; then
    # 行不在 = sr-fib 把这条前缀的**所有**腿都判成不可达，整行删掉了（不是重写成一条）。
    # 这本身是要量的事件，给它显式记号，绝不拿回落路由的路径集冒充。
    fwd=ABSENT; allp=ABSENT; meta="?/?"
  else
    # 承载集 = forwarding: 之后出现的 mpls via 下一跳。
    fwd=$(printf '%s' "$f" | awk '/forwarding:/{n=1} n && /mpls via/{printf "%s,", $4}')
    # 行内路径 = pref=下一跳:oper标志。**必须带 oper 标志与 path-list 的 len**：
    # 口 down 时 VPP 会把那条路径标成非 resolved 并把它踢出 uPRF，但路径仍留在 list 里；
    # 只有 sr-fib 重写整行才会换一个新的 path-list（len 变小）。只看「下一跳还在不在」
    # 会把这两种成因读成同一件事（2026-11-09 第一版就这么误读过）。
    allp=$(printf '%s' "$f" | awk '
      /path:\[/ && /pref=/{
        match($0,/pref=[0-9]+/); p=substr($0,RSTART+5,RLENGTH-5)
        op=($0 ~ /oper-flags:resolved/) ? "R" : "U"
        nhv="?"
        if (getline nl) {split(nl,a," "); if (a[1] ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/) nhv=a[1]}
        printf "%s=%s:%s,", p, nhv, op
      }')
    # len 取**第一个** uPRF-list（= 我们这条行的 path-list）。回落输出里有两个 path-list，
    # 最后一个是 drop 那条 len:0，取最后一个会把「行不在」读成「行变短到 0」。
    meta=$(printf '%s' "$f" | awk '
      /uPRF-list:/ && l=="" {match($0,/len:[0-9]+/); l=substr($0,RSTART+4,RLENGTH-4)}
      /buckets:/ && k=="" {match($0,/buckets:[0-9]+/); k=substr($0,RSTART+8,RLENGTH-8)}
      END{printf "%s/%s", (l==""?"?":l), (k==""?"?":k)}')
  fi
  # 空字段会打散 python 那边的定长切分，一律给占位符。
  printf 'S %s %s %s %s %s %s %s %s\n' "$EPOCHREALTIME" "${ps:-NA}" "${bs:-NA}" "${fwd:-NONE}" "${allp:-NONE}" "${meta:-?/?}" "${pt:-NA}" "${bt:-NA}"
  [ ${EPOCHREALTIME%.*} -ge $end ] && break
  sleep 0.02
done
REMOTE
SAMPLER=$!

# 注入：单发 down → HOLD 秒 → 单发 up
sleep 12
rm -f "$INJMARK.done"; : > "$INJMARK"
UPMARK=$(grep -c 'up   返回' "$LOG" 2>/dev/null || true)
{
  echo "   --- 注入（$INJBOX 的钟；$([ "$INJBOX" = "$BOX" ] && printf '与被测盒同钟，可当锚' || printf '不与被测盒对齐，锚点只能取本盒自己采到的翻转')）---"
  ssh -o BatchMode=yes "$INJBOX" bash -s -- "$INJTUN" "$HOLD" "$INJBOX" <<'INJ'
set -u
TUN=$1; HOLD=$2; BX=$3
# 兜底也装在注入端自己身上：这条 ssh 掉线或这里出错时，口照样回到 up（本地 trap 是第二层）。
trap 'vppctl "set interface state $TUN up" >/dev/null 2>&1 || true' EXIT
echo "   down 发出 于 $BX 钟 $(date +%s.%3N) 口态=$(vppctl "show interface $TUN" 2>/dev/null | awk -v n="$TUN" '$1==n{print $3}')"
vppctl "set interface state $TUN down"
echo "   down 返回 于 $(date +%s.%3N) 口态=$(vppctl "show interface $TUN" 2>/dev/null | awk -v n="$TUN" '$1==n{print $3}')"
sleep "$HOLD"
echo "   up   发出 于 $(date +%s.%3N)"
vppctl "set interface state $TUN up"
echo "   up   返回 于 $(date +%s.%3N) 口态=$(vppctl "show interface $TUN" 2>/dev/null | awk -v n="$TUN" '$1==n{print $3}')"
INJ
} | tee -a "$LOG"
# 只有这一枪真的把口 up 回来了才算收口（按本枪新增的那一行判，日志是追加的）。
if [ "$(grep -c 'up   返回' "$LOG" 2>/dev/null || true)" -gt "${UPMARK:-0}" ]; then
  touch "$INJMARK.done"
fi

wait "$SAMPLER" "$PROBER" || true

# local 臂的注入就在被测盒上，「down 返回」那一刻与采样器同钟，可以当锚；
# peer 臂绝不能这么干（盒钟互差几小时，跨机对齐会把几十 ms 量成几百 ms）。
DOWN_TS=""
if [ "$MODE" = local ]; then
  DOWN_TS=$(grep 'down 返回 于' "$LOG" | tail -1 | awk '{print $4}')
fi

{
  python3 - "$SAMPLE" "$PROBE" "$PRIM" "$BAK" "$WORK" "$TAG" "$DOWN_TS" "$MODE" <<'PY'
import sys
sample, probe, prim, bak, work, tag = sys.argv[1:7]
inj = float(sys.argv[7]) if len(sys.argv) > 7 and sys.argv[7] else None
MODE_IS_LOCAL = "1" if len(sys.argv) > 8 and sys.argv[8] == "local" else "0"
ts, ps, bs, fwd, allp, plen, bk, pt, bt = [], [], [], [], [], [], [], [], []
absent = []
for ln in open(sample):
    p = ln.rstrip("\n").split(" ", 7)
    if p[0] != "S" or len(p) < 8:
        continue
    ts.append(float(p[1])); ps.append(p[2]); bs.append(p[3])
    if p[4] == "ABSENT":
        # 精确行被整条删掉（sr-fib 判这条前缀的所有腿都不可达）。采样器已挡住 LPM 回落，
        # 这里不能把它当成「承载为空 + 行内为空」参与归因，只能单独当一个事件记。
        fwd.append([]); allp.append([]); plen.append(-2); bk.append(-2)
        absent.append(float(p[1]))
    else:
        fwd.append([] if p[4] == "NONE" else [x for x in p[4].split(",") if x])
        allp.append([] if p[5] == "NONE" else [x for x in p[5].split(",") if x])
        m = p[6].split("/")
        plen.append(int(m[0]) if m[0].isdigit() else -1)
        bk.append(int(m[1]) if len(m) > 1 and m[1].isdigit() else -1)
    pt.append(p[7].split()[0] if p[7] else "NA")
    bt.append(p[7].split()[1] if len(p[7].split()) > 1 else "NA")
if not ts:
    print("   !! 采样器没出货 —— 这一臂作废"); sys.exit(0)


def nh(tok):
    """记号是 `pref=下一跳:oper`（R=resolved / U=非 resolved；下一跳读不出来时是 ?）。"""
    return tok.split("=", 1)[1].split(":", 1)[0] if "=" in tok else tok


def oper(tok):
    return tok.rsplit(":", 1)[1] if ":" in tok else "?"


head = [x for x in plen[:20] if x >= 0]
headb = [x for x in bk[:20] if x >= 0]
if not head:
    print("   !! 起手 20 发里精确行一次都不在 —— 这条前缀根本没装进表，查 sr-fib 隧道表（#288 起由 sr-agent 派生）与 overlay 发布"); sys.exit(0)
base_plen = max(set(head), key=head.count)
print(f"   起手 path-list len={base_plen}（主+备都在行里）；buckets={max(set(headb), key=headb.count) if headb else '?'}"
      f"（只有最优那条在承载）")
if base_plen < 2:
    print(f"   !! 起手行里只有 {base_plen} 条路径，没有「次优」可顶 —— 这一臂作废。"
          f"多半是逃生开关没开（非颜色 0 的腿根本不进主表）或备路隧道没发布。"); sys.exit(0)
if absent:
    print(f"   !! 精确行整条消失 {len(absent)} 发（{absent[0]:.3f} → {absent[-1]:.3f}，"
          f"{absent[-1]-absent[0]:.2f}s）：sr-fib 判这条前缀的**所有**腿都不可达，把行删了，"
          f"客户流量落到覆盖它的默认路由。这不是主备切换，是这一枪的形状被破坏了。")

gaps = sorted(b - a for a, b in zip(ts, ts[1:]))
med = gaps[len(gaps) // 2]
print(f"   紧采样 {len(ts)} 发，间隔中位 {med*1000:.0f}ms（=分辨率）")
print(f"   主路口全程：{'全是 up' if set(pt) <= {'up'} else sorted(set(pt))}；备路口：{'全是 up' if set(bt) <= {'up'} else sorted(set(bt))}")

def first(pred, lo=0.0):
    for i, t in enumerate(ts):
        if t >= lo and pred(i):
            return t
    return None

def win(name, a, b):
    if a is None or b is None:
        print(f"   {name}: 读不出来{'（=从未发生）' if a is not None else ''}"); return
    d = 1000*(b-a)
    if d < med*1000:
        # 两个事件落在同一发采样里：只能说「比分辨率快」，不能写成 0ms（那不是测出来的 0）。
        print(f"   {name}: ≤{med*1000:.0f}ms（同一发采样内完成；{med*1000:.0f}ms = 本枪分辨率，{a:.3f}）")
    else:
        print(f"   {name}: {d:.0f}ms（{a:.3f} → {b:.3f}）")

t_down = first(lambda i: i >= 10 and ps[i] != "Up")
if t_down is None:
    print("   !! 整段没看到主路 BFD 翻转：注入没生效或认错了会话 —— 这一臂作废"); sys.exit(0)
t_up = first(lambda i: ps[i] == "Up", t_down)
# 形状闸门：peer 臂只该死主路那一条，而且**在要它顶上的那一刻备路必须是活的**。
# 这台 VM 的备路 BFD 本来就在每隔几分钟自己抖一下（宿主停摆，#261/#262），
# 抖在窗口外不影响归因，只报数；抖在窗口里 = 没有备路可切，这一枪作废。
# 精确行整条消失同理：那是 sr-fib 把所有腿都判不可达（实测 2026-11-09 10:15:03
# 三条会话同时 down），量到的是整盒停摆，不是主备切换。
lo0 = inj if inj is not None else t_down
t_bak_on0 = first(lambda i: bak in fwd[i], lo0)
sw_end = t_bak_on0 if t_bak_on0 is not None else (t_up if t_up is not None else ts[-1])
bad_bak = [t for t, s in zip(ts, bs) if s != "Up" and lo0 - 0.5 <= t <= sw_end + 0.5]
bad_row = [t for t in absent if lo0 - 0.5 <= t <= (t_up or ts[-1]) + 0.5]
flaps = [t for t, s in zip(ts, bs) if s != "Up"]
if MODE_IS_LOCAL == "0" and (bad_bak or bad_row):
    print(f"   !! 这一臂作废：切换窗口 [{lo0:.3f}, {sw_end:.3f}] 里备路 BFD 非 Up {len(bad_bak)} 发、"
          f"精确行消失 {len(bad_row)} 发。peer 臂只允许主路那一条死；备路也死就没有「次优顶上」可量。")
    if bad_bak:
        print(f"      备路窗口内首次非 Up：{bad_bak[0]:.3f}（主路判 down {t_down:.3f}）")
    sys.exit(0)
if flaps:
    print(f"   注：备路 BFD 全程自己抖了 {len(flaps)} 发（宿主停摆噪声，#261/#262），"
          f"都不在切换窗口里，不影响本枪归因。")
# 三个读者，各自能证明的事不一样，必须分开读：
#   承载 fwd  = 客户流量真正走哪条 —— VPP 挪 uPRF 会改它，sr-fib 重写整行也会改它
#   plen      = path-list 里有几条路径 —— **只有 sr-fib 重写整行会把它变小**；
#               VPP 把邻接判死时路径仍留在 list 里，只是 oper 变成非 resolved
#   oper      = 那条路径在 VPP 眼里解析没解析（R/U）
# 锚点：local 臂注入就在本盒，用「口 down 返回」那一刻（同盒同钟，可以对齐）；
#       peer 臂注入在对端，盒钟互差几小时，只能用本盒自己采到的 BFD 翻转当锚。
lo = inj if inj is not None else t_down
anchor = "本盒口 down（注入返回）" if inj is not None else "主路 BFD 判 down"
t_bak_on = first(lambda i: bak in fwd[i], lo)
t_gone = first(lambda i: plen[i] >= 0 and prim not in fwd[i], lo)
t_unres = first(lambda i: any(nh(x) == prim and oper(x) == "U" for x in allp[i]), lo)
t_shrink = first(lambda i: 0 <= plen[i] < base_plen, lo)
t_native = first(lambda i: bak in fwd[i] and plen[i] >= base_plen, lo)
win(f"{anchor} → 主路退出承载链", lo, t_gone)
win(f"{anchor} → 备路进承载链（=「次优顶上」客户可见的那一下）", lo, t_bak_on)
win(f"{anchor} → VPP 把主路标成非 resolved", lo, t_unres)
win(f"{anchor} → path-list 变短（=sr-fib 重写了整行）", lo, t_shrink)
win(f"{anchor} → 承载已挪到备路、但行里还是两条（=纯 VPP uPRF 回落的那一段）", lo, t_native)
if inj is not None:
    win("主路 BFD 判 down → 备路进承载链（BFD 锚，与上面那把对照）", t_down, t_bak_on)
    win("本盒口 down → 主路 BFD 判 down（=BFD 检测时间，不归任何收敛逻辑）", inj, t_down)
back = first(lambda i: prim in fwd[i], t_up) if t_up else None
rowback = first(lambda i: plen[i] >= base_plen, t_up) if t_up else None
win("主路 BFD 回到 Up → 主路重回承载链（回落）", t_up, back)
win("主路 BFD 回到 Up → path-list 回到起手长度（主路重新装回行里）", t_up, rowback)

if t_bak_on is None:
    print("   !! 备路从未进承载链：这一枪没有主备切换，客户流量随主路一起掉。")
elif t_native is not None and (t_shrink is None or t_native < t_shrink):
    gap = "" if t_shrink is None else f"，{1000*(t_shrink-t_native):.0f}ms 后 sr-fib 才把行收拾成一条"
    print(f"   判读：**VPP 原生 preference 回落**先打的第一枪 —— 承载挪到备路时 path-list 还是 "
          f"{base_plen} 条（主路那条只是被踢出 uPRF）{gap}。这一段不需要控制面说任何话。")
elif t_shrink is not None and t_native is None:
    print("   判读：**sr-fib 重写整行**触发的切换 —— 从头到尾没见过「承载已挪、行还是两条」那一发，"
          "path-list 变短与承载挪动同时（或更早）发生。这是可达性输入那条链，不是 VPP 的邻接级反应。")
else:
    print("   判读：两个信号落在同一发采样里，本枪分辨率分不出先后 —— 只能说切换发生了，"
          "归因要看 sr-fib 日志里有没有「该路径跳过」。")
print(f"   故障窗口时长 {(t_up - t_down):.2f}s；其中 path-list 仍是起手长度的采样时长 "
      f"{sum(1 for t, l in zip(ts, plen) if t_down <= t < (t_up or ts[-1]) and l >= base_plen) * med:.2f}s")
marks = {}
for t, nm in ((inj, "口down(注入)"), (t_down, "主bfd判down"), (t_gone, "主退出承载"),
              (t_bak_on, "备进承载"), (t_unres, "主路非resolved"), (t_shrink, "行变短"),
              (t_native, "承载挪/行未变"), (t_up, "主bfd判up"), (back, "主回承载"),
              (rowback, "行装回主路")):
    if t is not None:
        k = min(range(len(ts)), key=lambda j: abs(ts[j] - t))
        marks.setdefault(k, []).append(nm)
for k in sorted(marks):
    print(f"     {ts[k]:.3f} [{'+'.join(marks[k]):<30}] 主bfd={ps[k]} 备bfd={bs[k]} "
          f"承载={fwd[k]} len={plen[k]} buckets={bk[k]} 行内={allp[k]} 口={pt[k]}/{bt[k]}")

rows = []
for ln in open(probe):
    p = ln.split()
    if len(p) == 2 and p[1] in ("ok", "FAIL"):
        rows.append((float(p[0]), p[1] == "ok"))
bad = [t for t, ok in rows if not ok]
print(f"   客户探测 {len(rows)} 发，FAIL {len(bad)} 发 = {100*len(bad)/max(1,len(rows)):.1f}%")
runs, cur = [], []
for t in bad:
    if cur and t - cur[-1] <= 2.5: cur.append(t)
    else:
        if cur: runs.append(cur)
        cur = [t]
if cur: runs.append(cur)
for r in runs:
    print(f"     连续断流 {len(r)} 发 {r[-1]-r[0]:.2f}s")
with open(f"{work}/timeline-{tag}.txt", "w") as f:
    for i, t in enumerate(ts):
        row = "精确行不在（sr-fib 整条删了，流量落默认路由）" if plen[i] == -2 else \
              f"承载={'|'.join(fwd[i]):<22} len={plen[i]} buckets={bk[i]} 行内={allp[i]}"
        f.write(f"{t:.3f} 主bfd={ps[i]:<9} 备bfd={bs[i]:<9} {row} 口={pt[i]}/{bt[i]}\n")
print(f"   逐发明细 → {work}/timeline-{tag}.txt")
PY
  echo "   --- 这一枪期间 sr-fib 自己说了什么（fib.log 尾部 20 行）---"
  ssh -n -o BatchMode=yes "$BOX" "tail -20 /var/log/sr-box/fib.log" | sed 's/^/     /'
} | tee -a "$LOG"
