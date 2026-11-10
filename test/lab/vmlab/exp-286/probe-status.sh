#!/bin/bash
# probe-status.sh —— 在被测盒本地跑：采样 sr-route 的 /status
#
# 用法: probe-status.sh <PFX> <DUR_SEC> <OUT>
# 输出: ts_ms|in_rib|in_pending:reason|rib_routes|local_n|dropped_no_path
#
# 只回答一个问题：这条前缀此刻在「可装」那一列（rib）还是在「被闸住」那一列（pending）。
# 两列的分野见 internal/route/client.go 的 AllRIB()/Pending()。
set -u
PFX=${1:?pfx}; DUR=${2:?dur}; OUT=${3:?out}
end=$(( $(date +%s%3N) + DUR * 1000 ))
: > "$OUT"
while :; do
    now=$(date +%s%3N)
    [ "$now" -ge "$end" ] && break
    curl -s --max-time 3 http://127.0.0.1:19001/status 2>/dev/null |
        python3 -c '
import json,sys
pf=sys.argv[1]
try: d=json.load(sys.stdin)
except Exception: print("ERR|ERR|ERR|ERR|ERR"); raise SystemExit
inrib=0; pend="-"
for rs in (d.get("rib") or {}).values():
    for r in rs:
        if r.get("prefix")==pf: inrib=1
for p in (d.get("pending") or []):
    if p.get("prefix")==pf: pend=p.get("reason","?")
loc=len(d.get("local") or [])
print("%d|%s|%s|%s|%s"%(inrib,pend,d.get("rib_routes",-1),loc,d.get("dropped_no_path",-1)))
' "$PFX" >> "$OUT.tmp"
    tail -1 "$OUT.tmp" | sed "s/^/$now|/" >> "$OUT"
    sleep 0.4
done
rm -f "$OUT.tmp"
