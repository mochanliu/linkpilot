#!/bin/bash
# #284 换件前后的现场快照：四台的 sr-route / sr-fib 状态 + 三件产物的 run/盘哈希。
# 只读，不动 lab 现状（AGENTS.md §10）。用法：bash snapshot284.sh <标签>
set -u
TAG=${1:-snap}
OUT=/tmp/p276/snap284-$TAG.txt
mkdir -p /tmp/p276
{
echo "== snapshot $TAG  $(date -u +%FT%TZ)"
for b in CPE1 CPE2 CPE3 CPE4; do
  echo "-- $b"
  ssh -n -o BatchMode=yes "$b" 'for n in sr-route sr-frr sr-fib; do
      p=$(pgrep -x $n | head -1)
      r=$(sha256sum /proc/$p/exe 2>/dev/null | cut -c1-8)
      d=$(sha256sum /usr/local/bin/$n 2>/dev/null | cut -c1-8)
      echo "   $n pid=${p:-没跑} run=${r:-?} disk=${d:-?} $([ "$r" = "$d" ] && echo run==盘 || echo !!跑≠盘)"
    done
    curl -s --max-time 2 http://127.0.0.1:19001/status | tr -d " \n" | python3 -c "
import sys,json
d=json.load(sys.stdin)
rib={k:len(v) for k,v in d[\"rib\"].items()}
print(\"   route local=%d rib=%s rib_routes=%s dropped_own_route=%s pending=%s sess=%s\" % (
  len(d[\"local\"]), rib, d[\"rib_routes\"], d[\"dropped_own_route\"], d[\"pending_total\"],
  [(s[\"config_id\"],s[\"connected\"],s[\"synced\"],s[\"routes\"]) for s in d[\"sessions\"]]))"
    curl -s --max-time 2 http://127.0.0.1:19011/status | tr -d " \n" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(\"   fib tunnels=%d vrfs=%d escape_all_colors=%s last=%s\" % (
  len(d[\"tunnels\"]), len(d[\"vrfs\"]), d[\"escape_all_colors\"], json.dumps(d[\"last\"])[:90]))"' 2>/dev/null
  ssh -n -o BatchMode=yes "$b" 'vppctl "show ip fib table 0" 2>/dev/null | grep -c "mpls via"' 2>/dev/null |
    sed 's/^/   VPP table0 mpls via 行数=/'
done
echo "== 说明：客户侧零丢包判据请用 exp-273/six.sh，本脚本只抓状态与哈希"
} | tee "$OUT"
