#!/bin/bash
# #274 判据：zebra 重启这一枪（= 控制面重连 + 客户会话重建）期间，本盒对外宣称的
# 客户侧前缀集（sr-route 的 local，由 sr-frr 每轮 PUT 上去）会不会短一段、短多久，
# 以及最后有没有自己回来。
#
# 预测与实测（2026-11-07，见同目录 README.md）：
#   预测  改前窗口维持旧集合、local 不掉；改后重连即重置、会短一段。
#   实测  两边都在 +2.4s 左右掉到 0 —— 改前那份窗口按 500ms 安静提前关上了，而当时 zebra
#         自己的 RIB 还没重新收录 sr-fib 那两条 K 路由，回放的答复就是 0 条。这一枪里
#         窗口一秒都没买到；空窗长度由 refill 决定（12~18s，归 #276）。
#
# 用法：reconnect-window.sh [BOX=CPE1] [PB=cust12] [SRC=172.16.12.1] [DST=172.16.4.1]
#   WORK=/tmp/p274 TAG=before DUR=70 bash reconnect-window.sh
# 故障注入是 systemctl restart frr：这是**一次性 dev 台**里的被测故障，不是配置生效路径。
set -u
BOX=${1:-CPE1}
PB=${2:-cust12}
SRC=${3:-172.16.12.1}
DST=${4:-172.16.4.1}
WORK=${WORK:-/tmp/p274}
TAG=${TAG:-run}
DUR=${DUR:-70}
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$WORK"
RAW=$WORK/samples-$BOX-$TAG.txt
PROBE=$WORK/probe-$BOX-$TAG.txt
LOG=$WORK/window-$BOX-$TAG.txt

echo "== $BOX $TAG $(date -u +%FT%TZ) 注入：$BOX 的 systemctl restart frr" | tee "$LOG"
ssh -n -o BatchMode=yes "$BOX" 'sha256sum /usr/local/bin/sr-frr | cut -c1-16; echo -n "sr-frr pid "; pgrep sr-frr; echo -n "zebra pid "; pgrep -x zebra' | tee -a "$LOG"
scp -q -o BatchMode=yes "$HERE/sample-local.sh" "$BOX:/tmp/sample-local.sh"

BASE=$(ssh -n -o BatchMode=yes "$BOX" 'curl -s http://127.0.0.1:19001/status' |
  python3 -c 'import json,sys;print(len(json.load(sys.stdin).get("local") or []))')
echo "基线 local 条数 = $BASE" | tee -a "$LOG"

ssh -n -o BatchMode=yes "$BOX" "bash /tmp/sample-local.sh $DUR" > "$RAW" 2>/dev/null &
SP=$!
ssh -n -o BatchMode=yes "$PB" "end=\$((SECONDS+$DUR)); while [ \$SECONDS -lt \$end ]; do printf '%s ' \$(date +%s.%3N); timeout 1 ping -c1 -W1 -I $SRC $DST >/dev/null 2>&1 && echo ok || echo FAIL; done" > "$PROBE" 2>/dev/null &
PP=$!

sleep 6
# 墙钟取盒子自己的（Mac 的 BSD date 没有 %N，采样时刻全是盒子时间）
T0=$(ssh -n -o BatchMode=yes "$BOX" 'date +%s.%3N')
echo "-- 第 6 秒注入 restart frr（盒子墙钟 $T0）" | tee -a "$LOG"
ssh -n -o BatchMode=yes "$BOX" 'systemctl restart frr; date +%s.%3N; echo -n "新 zebra pid "; pgrep -x zebra; echo -n "sr-frr pid(不该变) "; pgrep sr-frr' | tee -a "$LOG"
wait $SP; wait $PP

python3 - "$RAW" "$PROBE" "$BASE" "$T0" <<'PY' | tee -a "$LOG"
import re, sys
raw, probe, base, t0 = sys.argv[1], sys.argv[2], int(sys.argv[3]), float(sys.argv[4])
rows = []
for ln in open(raw):
    m = re.match(r'([\d.]+) local=(\S+) frr\((\S+) (\S+)\)', ln)
    if m:
        rows.append((float(m.group(1)), m.group(2), m.group(3), m.group(4)))
if not rows:
    sys.exit("采样为空")
lo = [r for r in rows if r[1] != str(base)]
print(f"采样 {len(rows)} 发（{(rows[-1][0]-rows[0][0]):.1f}s），基线 local={base}")
if lo:
    print(f"  偏离基线 {len(lo)} 发：{lo[0][0]-t0:+.2f}s → {lo[-1][0]-t0:+.2f}s（相对注入时刻）")
    print("  偏离取值：" + " ".join(sorted({r[1] for r in lo})))
    mn = min(int(r[1]) for r in lo if r[1].isdigit())
    print(f"  最低 local={mn}，偏离总时长 {(lo[-1][0]-lo[0][0]):.1f}s")
else:
    print("  全程没偏离基线（集合一次都没短）")
print(f"  收尾 local={rows[-1][1]} zebra_connected={rows[-1][3]}")
fails = []
for ln in open(probe):
    p = ln.split()
    if len(p) == 2:
        fails.append((float(p[0]), p[1].lower()))
bad = [t for t, s in fails if s == "fail"]
# 注意：探测机（PB）与盒子（BOX）的时钟不同步（实测差 ~7.7h），所以断流段只给
# **时长**和相对第一段起点的秒数，不和 local 采样对齐到同一个原点。
print(f"探测 {len(fails)} 发，FAIL {len(bad)} 发")
if bad:
    runs, st, pre = [], bad[0], bad[0]
    for t in bad[1:]:
        if t - pre > 2.0:
            runs.append((st, pre)); st = t
        pre = t
    runs.append((st, pre))
    for i, (a, b) in enumerate(runs):
        print(f"  断流段{i+1}：长度 {(b-a):.1f}s（第一发 FAIL 起 +{a-bad[0]:.1f}s）")
PY
echo "-- 收尾读数" | tee -a "$LOG"
ssh -n -o BatchMode=yes "$BOX" 'curl -s http://127.0.0.1:19001/status | python3 -c "import json,sys;print(\"  local =\",json.dumps(json.load(sys.stdin).get(\"local\")))"; curl -s --unix-socket /run/sr-frr/control.sock http://x/status | python3 -c "import json,sys;d=json.load(sys.stdin);print(\"  injected=%s reported=%s zebra=%s last_err=%r\"%(d.get(\"injected\"),d.get(\"reported\"),d.get(\"zebra_connected\"),d.get(\"last_error\")))"' | tee -a "$LOG"
echo "-- sr-frr 这一段日志（bridge.log）" | tee -a "$LOG"
ssh -n -o BatchMode=yes "$BOX" "tail -n 400 /var/log/sr-box/bridge.log | grep -E 'zebra|导入|对账|回放|上报|重连|注入' | tail -25" | tee -a "$LOG"
