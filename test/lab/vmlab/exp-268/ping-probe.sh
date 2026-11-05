#!/bin/bash
# 客户视角的连续单发 ping：每发一条记「时间戳 + 通/不通 + rtt」。
# 断的时候这一条要等 -W 1 秒超时才返回，所以"连续多少条 LOSS"就近似"黑洞了多少秒"。
#
# 用法：ping-probe.sh <源 loopback> <目的> <总秒数> <输出文件>
set -u
SRC=$1; DST=$2; SECS=$3; OUT=$4
END=$(( $(date +%s) + SECS ))
: > "$OUT"
while [ "$(date +%s)" -lt "$END" ]; do
    t=$(date +%s.%N)
    line=$(ping -c1 -W1 -I "$SRC" "$DST" 2>/dev/null | grep 'time=' | head -1)
    if [ -n "$line" ]; then
        echo "$t OK ${line##*time=}" >> "$OUT"
    else
        echo "$t LOSS" >> "$OUT"
    fi
done
echo "PROBE_DONE $(date '+%F %T')" >> "$OUT"
