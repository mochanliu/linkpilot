#!/bin/bash
# shot.sh —— exp-286 单枪编排器（在 Mac 上跑）
#
# 用法:
#   shot.sh <TAG> <MODE> <OBS_BOX> <PFX> <INJECT_BOX> <INJECT_IF> <CUST> <SRC> <DST> <HOLD>
#
#   MODE=local  注入口就在被测盒上（链①：本机口 down，VPP 原生回落）
#   MODE=peer   注入口在对端盒上（链②：只有对端死，被测盒本机口全程 up）
#
# 干的事：先起两个采样器（被测盒 + 注入盒，各用自己的钟）+ 客户侧 20 包/秒的流，
# 3 秒后注入一次 down，保持 HOLD 秒，再 up，然后把原始数据拉回 $WORK。
# 不改配置、不重启进程、不碰 lab 以外的任何东西；down/up 各一次，单发不循环。
set -u

TAG=${1:?tag}; MODE=${2:?mode}; OBS=${3:?obs}; PFX=${4:?pfx}
INJ=${5:?inj}; IF=${6:?if}; CUST=${7:?cust}; SRC=${8:?src}; DST=${9:?dst}; HOLD=${10:?hold}
PEERS=${11:-10.250.0.0}

HERE=$(cd "$(dirname "$0")" && pwd)
WORK=${WORK:-/Users/louis/srvmlab/out-286}
PROBE=/tmp/p286-probe.sh
DUR=$((HOLD + 15))
mkdir -p "$WORK"

say(){ printf '%s\n' "$*"; }

# ---- 0. 推采样器（串行，落地对哈希，AGENTS.md：并发 scp 会静默截断）----
for box in $(echo "$OBS $INJ" | tr ' ' '\n' | sort -u); do
    scp -q "$HERE/probe.sh" "$box:$PROBE" || { say "scp 到 $box 失败"; exit 3; }
    scp -q "$HERE/probe-status.sh" "$box:/tmp/p286-status.sh" || { say "scp status 到 $box 失败"; exit 3; }
    a=$(shasum -a 256 "$HERE/probe.sh" | awk '{print $1}')
    b=$(ssh "$box" "sha256sum $PROBE" 2>/dev/null | awk '{print $1}')
    [ "$a" = "$b" ] || { say "哈希不一致 $box"; exit 3; }
    ssh "$box" "chmod +x $PROBE /tmp/p286-status.sh" 2>/dev/null
done

say "== $TAG  ($MODE)  被测=$OBS 前缀=$PFX 注入=$INJ:$IF  客户=$CUST($SRC->$DST) 保持=${HOLD}s"

# ---- 1. 起手形状（这一枪之前的现场）----
say "-- 起手 FIB --"
ssh "$OBS" "timeout 10 vppctl \"show ip fib $PFX table 0\" 2>/dev/null | tr -d '\r' | grep -E 'path-list|path:\[|buckets:|mpls via'" 2>/dev/null

# ---- 2. 起采样器与客户流 ----
ssh "$OBS" "$PROBE $PFX $PEERS $IF $DUR /tmp/p286-$TAG-obs.csv" >/dev/null 2>&1 &
P1=$!
ssh "$OBS" "/tmp/p286-status.sh $PFX $DUR /tmp/p286-$TAG-status.csv" >/dev/null 2>&1 &
P4=$!
if [ "$INJ" != "$OBS" ]; then
    ssh "$INJ" "$PROBE $PFX $PEERS $IF $DUR /tmp/p286-$TAG-inj.csv" >/dev/null 2>&1 &
    P2=$!
else
    P2=""
fi
N=$(( (HOLD + 10) * 20 ))
ssh "$CUST" "ping -I $SRC -c $N -i 0.05 -W 0.2 -D $DST > /tmp/p286-$TAG-ping.txt 2>&1" >/dev/null 2>&1 &
P3=$!

sleep 3

# ---- 3. 注入（盒内自己的时钟）----
t0=$(ssh "$INJ" "date +%s%3N" 2>/dev/null)
ssh "$INJ" "timeout 10 vppctl \"set interface state $IF down\"" >/dev/null 2>&1
t0b=$(ssh "$INJ" "date +%s%3N" 2>/dev/null)
say "-- 注入 down: $IF  t0=$t0 (命令返回 $t0b)"

sleep "$HOLD"

t1=$(ssh "$INJ" "date +%s%3N" 2>/dev/null)
ssh "$INJ" "timeout 10 vppctl \"set interface state $IF up\"" >/dev/null 2>&1
t1b=$(ssh "$INJ" "date +%s%3N" 2>/dev/null)
say "-- 恢复 up  : $IF  t1=$t1 (命令返回 $t1b)  保持 $(( t1b - t0 ))ms"

printf '%s %s %s %s\n' "$t0" "$t0b" "$t1" "$t1b" > "$WORK/$TAG-inject.txt"
ssh "$INJ" "printf '%s %s %s %s\n' $t0 $t0b $t1 $t1b > /tmp/p286-$TAG-inject.txt" 2>/dev/null

# ---- 4. 收工 ----
wait $P3 2>/dev/null
wait $P1 2>/dev/null
wait $P4 2>/dev/null
[ -n "$P2" ] && wait $P2 2>/dev/null

ssh "$OBS" "cat /tmp/p286-$TAG-obs.csv" 2>/dev/null > "$WORK/$TAG-obs-$OBS.csv"
ssh "$OBS" "cat /tmp/p286-$TAG-status.csv" 2>/dev/null > "$WORK/$TAG-status-$OBS.csv"
ssh "$OBS" "tail -60 /var/log/sr-box/fib.log" 2>/dev/null > "$WORK/$TAG-obs-$OBS-fiblog.txt"
if [ "$INJ" != "$OBS" ]; then
    ssh "$INJ" "cat /tmp/p286-$TAG-inj.csv" 2>/dev/null > "$WORK/$TAG-inj-$INJ.csv"
    ssh "$INJ" "tail -60 /var/log/sr-box/fib.log" 2>/dev/null > "$WORK/$TAG-inj-$INJ-fiblog.txt"
fi
ssh "$CUST" "cat /tmp/p286-$TAG-ping.txt" 2>/dev/null > "$WORK/$TAG-ping.txt"

say "-- 原始数据 --"
ls -l "$WORK/$TAG"* | awk '{print "   ", $9, $5"B"}'
