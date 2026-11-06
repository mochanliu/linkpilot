#!/bin/bash
# 换件这一枪的客户可见时长：客户 VM 用自己的 loopback 秒级探测一条**当前通**的方向，
# 中途 SIGTERM <BOX> 的 sr-frr，由产品自己的开机清单（sr-box-watch.service）把它补回来。
# 全程不碰 VPP / FRR / sr-agent / sr-route / sr-fib。
# 用法：window-swap.sh <被换件的盒> [探测机 cust12|cust3|cust4] [源 loopback] [目的]
BOX=${1:?CPE1|CPE2|CPE3|CPE4}
PB=${2:-cust12}
SRC=${3:-172.16.12.1}
DST=${4:-172.16.4.1}
WORK=${WORK:-/tmp/p273}
TAG=$BOX-${PB}-${DST}
LOG=$WORK/window-$TAG.txt
RAW=$WORK/probe-$TAG.raw
mkdir -p $WORK
echo "== $TAG $(date -u +%FT%TZ) 探测 $PB: -I $SRC -> $DST" | tee $LOG
ssh -n -o BatchMode=yes $BOX "sha256sum /usr/local/bin/sr-frr | cut -c1-16; pgrep sr-frr" | tee -a $LOG

# 探测：1 秒一发，共 75 发。链路通时约 32 发/秒，只有一条通着的路由挂了才降到 1 发/秒
# —— 所以"连续 FAIL 一大片"就是硬断流，不是采样噪声。
ssh -n -o BatchMode=yes $PB "end=\$((SECONDS+75)); while [ \$SECONDS -lt \$end ]; do printf \"%s \" \$(date +%s.%3N); timeout 1 ping -c1 -W1 -I $SRC $DST >/dev/null 2>&1 && echo ok || echo FAIL; done" > $RAW 2>/dev/null &
PPID_=$!
sleep 6
echo "-- 第 6 秒：SIGTERM sr-frr（pid $(ssh -n -o BatchMode=yes $BOX pgrep sr-frr)）" | tee -a $LOG
ssh -n -o BatchMode=yes $BOX 'kill -TERM $(pgrep sr-frr); sleep 2; systemctl start sr-box-watch.service; date -u +%FT%TZ; echo -n "新 pid: "; pgrep sr-frr' | tee -a $LOG
wait $PPID_
python3 - "$TAG" "$WORK" <<'PY' >> $LOG
import sys
tag,work=sys.argv[1],sys.argv[2]
rows=[]
for ln in open(f"{work}/probe-{tag}.raw"):
    p=ln.split()
    if len(p)==2: rows.append((float(p[0]), p[1].lower()))
fails=[t for t,s in rows if s=="fail"]
print(f"总探测 {len(rows)} 发，FAIL {len(fails)} 发")
if fails:
    runs=[]; st=pre=fails[0]
    for t in fails[1:]:
        if t-pre>2.0: runs.append((st,pre)); st=t
        pre=t
    runs.append((st,pre))
    print("断流段（起止秒、长度）：")
    for a,b in runs: print(f"  {a:.3f} → {b:.3f}  {(b-a):.1f}s")
    print(f"最长断流 {max(b-a for a,b in runs):.1f}s")
PY
echo "-- 换件后：" | tee -a $LOG
ssh -n -o BatchMode=yes $BOX "sha256sum /usr/local/bin/sr-frr | cut -c1-16; pgrep sr-frr; ps -o etimes= -p \$(pgrep sr-frr) | tr -d ' '" | tee -a $LOG
cat $LOG
