#!/bin/bash
# #276 换件第三步，也是唯一会动运行事实的一步：把 install276.sh 已经装上盘的那一份抬起来。
#
# 形状只有一个：SIGTERM 老进程 → 让产品自己的开机清单（sr-box-watch.service →
# start.sh up）把它补回来 → 新进程从盘上那份起。
# 不 kill -9、不 systemctl restart 任何业务单元、不手改 FRR/VPP/内核路由
# —— 这三条都是冷启动形状，AGENTS.md §1.1 禁止。
#
# 全程有客户侧秒级探测跟着，跑完直接给「这一枪客户看见几秒」：§1.4 要求必然中断
# 的操作点名并给实测时长，这个脚本就是那条要求的量具。
#
# 用法：bash swap276.sh <BOX> <name> [探测机] [源 loopback] [目的]
#   name = sr-fib | sr-route | sr-frr | sr-agent
#   探测默认 cust12 -I 172.16.12.1 → 172.16.4.1（走被换那条腿的方向）
# 环境：MON_PFX=被盯的远端前缀（默认 172.16.12.1/32，只在换 sr-fib 时核对）
#       WORK=scratch 目录（默认 /tmp/p276）
set -eu
BOX=${1:?CPE1|CPE2|CPE3|CPE4}
NAME=${2:?sr-route|sr-fib|sr-frr|sr-agent}
PB=${3:-cust12}
SRC=${4:-172.16.12.1}
DST=${5:-172.16.4.1}
# 盯的前缀默认取探测目的：被换的盒上，远端前缀才走 mpls via，本盒自己的 local 前缀
# 是 sr-frr 注入的 K 路由（dev lo proto boot），mpls via 天然 0 —— 别把那个 0 当成故障。
MON_PFX=${MON_PFX:-$DST/32}
WORK=${WORK:-/tmp/p276}
TAG=$BOX-$NAME
LOG=$WORK/swap-$TAG.txt
RAW=$WORK/probe-swap-$TAG.raw
mkdir -p "$WORK"

{
echo "== swap $NAME on $BOX $(date -u +%FT%TZ)  探测 $PB: -I $SRC -> $DST"
ssh -o BatchMode=yes "$BOX" bash -s -- "$NAME" "$MON_PFX" <<'REMOTE'
set -u
NAME=$1; MON=$2
PID=$(pgrep -x "$NAME" | head -1 || true)
[ -n "$PID" ] || { echo "   $NAME 没在跑，swap 无从谈起（先查开机清单）" >&2; exit 1; }
printf '   换之前 pid=%s 跑=%s 盘=%s\n' "$PID" \
  "$(sha256sum /proc/$PID/exe 2>/dev/null | cut -c1-16)" \
  "$(sha256sum /usr/local/bin/$NAME | cut -c1-16)"
if [ "$(sha256sum /proc/$PID/exe 2>/dev/null | cut -c1-16)" = "$(sha256sum /usr/local/bin/$NAME | cut -c1-16)" ]; then
  echo "   !! 跑 == 盘：这一枪不会改变任何运行事实。要么先跑 install276.sh，要么本次只是「重启自检」"
fi
REMOTE
} | tee "$LOG"

# 探测：1 秒一发共 75 发。链路通时约 32 发/秒；只有真断流才塌成 1 发/秒的 FAIL，
# 所以后面「连续 FAIL 一片」判的是硬断流，不是采样噪声（§9 测量陷阱）。
ssh -n -o BatchMode=yes "$PB" "end=\$((SECONDS+75)); while [ \$SECONDS -lt \$end ]; do printf \"%s \" \$(date +%s.%3N); timeout 1 ping -c1 -W1 -I $SRC $DST >/dev/null 2>&1 && echo ok || echo FAIL; done" > "$RAW" 2>/dev/null &
PROBER=$!
sleep 4

ssh -o BatchMode=yes "$BOX" bash -s -- "$NAME" "$MON_PFX" <<'REMOTE' | tee -a "$LOG"
set -u
NAME=$1; MON=$2
old=$(pgrep -x "$NAME" | head -1)
echo "   SIGTERM pid=$old 于 $(date +%s.%3N)"
kill -TERM "$old"
T0=$(date +%s%3N)
systemctl start sr-box-watch.service
new=""
for i in $(seq 1 80); do
  new=$(pgrep -x "$NAME" | head -1 || true)
  [ -n "$new" ] && [ "$new" != "$old" ] && break
  sleep 0.1
done
if [ -z "$new" ] || [ "$new" = "$old" ]; then
  echo "   !! 8 秒内没起来新进程 —— 看 journalctl -u sr-box-watch.service 与 /var/log/sr-box/start.log"
  exit 1
fi
echo "   新 pid=$new 起来用时 $(( $(date +%s%3N) - T0 ))ms（含开机清单里它前面那几步）"
printf '   跑=%s 盘=%s %s\n' \
  "$(sha256sum /proc/$new/exe 2>/dev/null | cut -c1-16)" \
  "$(sha256sum /usr/local/bin/$NAME | cut -c1-16)" \
  "$( [ "$(sha256sum /proc/$new/exe 2>/dev/null | cut -c1-16)" = "$(sha256sum /usr/local/bin/$NAME | cut -c1-16)" ] && echo '（run==盘，符合预期）' )"
case "$NAME" in
  sr-fib)
    N=$(vppctl "show ip fib $MON" 2>/dev/null | grep -c 'mpls via')
    ISLOCAL=$(curl -s --max-time 3 http://127.0.0.1:19001/status | tr -d ' \n' | python3 -c "import sys,json;d=json.load(sys.stdin);print(any(x.get('prefix')=='$MON' for x in d.get('local',[])) if isinstance(d.get('local'),list) else '')" 2>/dev/null || echo 取不到)
    echo "   $MON 的 mpls via 条数=$N（本盒 local=$ISLOCAL；local 的那条天然 0，不是故障）"
    ;;
  sr-route)
    # B4：本地前缀集是 sr-frr 变更驱动上报的，sr-route 起来后没人重发 ⇒ 集会空着。
    n=$(curl -s --max-time 3 http://127.0.0.1:19001/status | tr -d ' \n' | python3 -c "import sys,json;print(len(json.load(sys.stdin).get('local',[])))" 2>/dev/null || echo 取不到)
    echo "   /status 的 local 条数=$n"
    [ "$n" = "0" ] && echo "   !! B4：换了 sr-route，本地前缀集空了且不会自愈（sr-frr 只在变更时上报）。修法（2026-11-08 实测可用，在 Mac 上仓库根跑）：bash test/lab/vmlab/exp-273/window-swap.sh $BOX cust12 172.16.12.1 172.16.4.1 —— 顺带再换 sr-frr 一枪，让它重连后重报全量"
    ;;
esac
REMOTE

wait "$PROBER"
{
python3 - "$TAG" "$WORK" <<'PY'
import sys
tag, work = sys.argv[1], sys.argv[2]
rows = []
for ln in open(f"{work}/probe-swap-{tag}.raw"):
    p = ln.split()
    if len(p) == 2 and p[1] in ("ok", "FAIL"):
        rows.append((float(p[0]), p[1] == "ok"))
bad = [t for t, ok in rows if not ok]
print(f"   客户探测共 {len(rows)} 发，FAIL {len(bad)} 发")
if bad:
    runs, cur = [], []
    for t in bad:
        if cur and t - cur[-1] <= 2.5:
            cur.append(t)
        else:
            if cur: runs.append(cur)
            cur = [t]
    if cur: runs.append(cur)
    print("   连续断流段：")
    for r in runs:
        print(f"     {len(r)} 发  {r[0]:.3f} → {r[-1]:.3f}  {r[-1]-r[0]:.2f}s")
    print(f"   最长断流 {max(r[-1]-r[0] for r in runs):.2f}s（这就是这一枪的客户可见时长）")
PY
} | tee -a "$LOG"
