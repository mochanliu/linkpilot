#!/bin/bash
# B4 的「今天的人工把手」那一臂：只换 sr-frr，看本地前缀集是怎么回来的。
#
# 为什么要单列这一臂：修之前盒子上唯一的重建路径就是重启 sr-frr —— 它的 reporter
# 一起来就是 synced=false，第一轮 reconcile 必然发全量 PUT。也就是说 B4 的补丁
# 不是「让 sr-route 变聪明一点」，而是「少死一次 sr-frr」。这一臂量的是这个差别。
#
# 用法：bash b4-heal.sh <BOX> <观察秒数> [探测机] [源 loopback] [目的前缀]
set -eu
BOX=${1:?CPE1|CPE2|CPE3|CPE4}
SECS=${2:?观察秒数}
PB=${3:?cust12}
SRC=${4:?172.16.12.1}
DST=${5:?172.16.3.1}
WORK=${WORK:-/tmp/p276}
TAG=$BOX-heal
LOG=$WORK/b4-$TAG.txt
RAW=$WORK/probe-b4-$TAG.raw
mkdir -p "$WORK"

{
  echo "== B4 $TAG：$BOX 只换 sr-frr（sr-route 不动），观察 ${SECS}s  探测 $PB: -I $SRC -> $DST  $(date -u +%FT%TZ)"
  ssh -n -o BatchMode=yes "$BOX" "
    printf '   换之前 sr-route pid=%s 跑=%s 盘=%s local=%s\n' \"\$(pgrep -x sr-route|head -1)\" \
      \"\$(sha256sum /proc/\$(pgrep -x sr-route|head -1)/exe|cut -c1-16)\" \"\$(sha256sum /usr/local/bin/sr-route|cut -c1-16)\" \
      \"\$(curl -s --max-time 3 http://127.0.0.1:19001/status | tr -d ' \n' | python3 -c 'import sys,json;print(len(json.load(sys.stdin).get(\"local\") or []))')\"
    printf '   换之前 sr-frr   pid=%s 跑=%s 盘=%s\n' \"\$(pgrep -x sr-frr|head -1)\" \
      \"\$(sha256sum /proc/\$(pgrep -x sr-frr|head -1)/exe|cut -c1-16)\" \"\$(sha256sum /usr/local/bin/sr-frr|cut -c1-16)\"
  "
} | tee "$LOG"

ssh -n -o BatchMode=yes "$PB" "end=\$((SECONDS+$SECS+15)); while [ \$SECONDS -lt \$end ]; do printf \"%s \" \$(date +%s.%3N); timeout 1 ping -c1 -W1 -I $SRC $DST >/dev/null 2>&1 && echo ok || echo FAIL; done" > "$RAW" 2>/dev/null &
PROBER=$!
sleep 4

ssh -o BatchMode=yes "$BOX" bash -s -- "$SECS" <<'REMOTE' | tee -a "$LOG"
set -u
SECS=$1
old=$(pgrep -x sr-frr | head -1)
route=$(pgrep -x sr-route | head -1)
echo "   SIGTERM sr-frr pid=$old 于 $(date +%s.%3N)（sr-route pid=$route 不动）"
kill -TERM "$old"
T0=$(date +%s%3N)
systemctl start sr-box-watch.service
new=""
for i in $(seq 1 80); do
  new=$(pgrep -x sr-frr | head -1 || true)
  [ -n "$new" ] && [ "$new" != "$old" ] && break
  sleep 0.1
done
[ -n "$new" ] && [ "$new" != "$old" ] || { echo "   !! 8 秒内没起来新 sr-frr"; exit 1; }
echo "   新 sr-frr pid=$new 起来用时 $(( $(date +%s%3N) - T0 ))ms  跑=$(sha256sum /proc/$new/exe|cut -c1-16)"
prev=""
for i in $(seq 1 "$SECS"); do
  n=$(curl -s --max-time 2 http://127.0.0.1:19001/status 2>/dev/null | tr -d ' \n' |
      python3 -c "import sys,json;print(len(json.load(sys.stdin).get('local') or []))" 2>/dev/null || echo 取不到)
  [ "$n" = "$prev" ] && continue
  printf '   t+%ss sr-route.local=%s（sr-route pid=%s 全程未变）\n' "$i" "$n" "$route"
  prev=$n
done
echo "   新 sr-frr 的控制口能不能答 GET /local-prefixes："
curl -s --max-time 3 -w '\n   HTTP %{http_code}\n' --unix-socket /run/sr-frr/control.sock http://sr-frr/local-prefixes | sed 's/^/     /'
REMOTE

wait "$PROBER" || true
python3 - "$TAG" "$WORK" <<'PY' | tee -a "$LOG"
import sys
tag, work = sys.argv[1], sys.argv[2]
rows = []
for ln in open(f"{work}/probe-b4-{tag}.raw"):
    p = ln.split()
    if len(p) == 2 and p[1] in ("ok", "FAIL"):
        rows.append((float(p[0]), p[1] == "ok"))
ok = [t for t, g in rows if g]
bad = [t for t, g in rows if not g]
print(f"   客户探测共 {len(rows)} 发，FAIL {len(bad)} 发（这一臂前面那段是 A 臂留下的洞，不是这一枪的成本）")
if ok:
    print(f"   恢复绿的第一发在 {ok[-1]:.3f}（最后一发 ok）；本臂内 ok/FAIL 交替 {sum(1 for i in range(1,len(rows)) if rows[i][1]!=rows[i-1][1])} 次")
PY
