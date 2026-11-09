#!/bin/bash
# B4 双臂判据：只换 sr-route 一枪（sr-frr 全程不动），看本地前缀集能不能自己回来。
#
# 为什么 sr-frr 不能动：B4 的空洞正是「sr-frr 以为它已经说过了」。换 sr-frr 会让它的
# reporter 复位、重发全量，那条路会把缺陷和修法同时治好，量不出归因。
#
# 形状只有一个（AGENTS.md §1.1）：SIGTERM 老进程 → 产品自己的开机清单把它补回来。
# 不 kill -9、不 systemctl restart 业务单元。
#
# 用法：bash b4-arm.sh <BOX> <秒数> [探测机] [源 loopback] [目的前缀]
#   例（A 臂，老产物）：bash b4-arm.sh CPE3 150 cust12 172.16.12.1 172.16.3.1
#   例（B 臂，新产物）：同上，装盘换新 sr-route 之后再跑一次
set -eu
BOX=${1:?CPE1|CPE2|CPE3|CPE4}
SECS=${2:?观察秒数}
PB=${3:?cust12}
SRC=${4:?172.16.12.1}
DST=${5:?172.16.3.1}
WORK=${WORK:-/tmp/p276}
TAG=$BOX-arm${6:-A}
LOG=$WORK/b4-$TAG.txt
RAW=$WORK/probe-b4-$TAG.raw
mkdir -p "$WORK"

{
  echo "== B4 $TAG：$BOX 只换 sr-route，观察 ${SECS}s  探测 $PB: -I $SRC -> $DST  $(date -u +%FT%TZ)"
  ssh -n -o BatchMode=yes "$BOX" '
    printf "   sr-route pid=%s 跑=%s 盘=%s\n" "$(pgrep -x sr-route|head -1)" \
      "$(sha256sum /proc/$(pgrep -x sr-route|head -1)/exe|cut -c1-16)" "$(sha256sum /usr/local/bin/sr-route|cut -c1-16)"
    printf "   sr-frr   pid=%s 跑=%s 盘=%s\n" "$(pgrep -x sr-frr|head -1)" \
      "$(sha256sum /proc/$(pgrep -x sr-frr|head -1)/exe|cut -c1-16)" "$(sha256sum /usr/local/bin/sr-frr|cut -c1-16)"
    curl -s --max-time 3 --unix-socket /run/sr-frr/control.sock http://sr-frr/status |
      python3 -c "import sys,json;d=json.load(sys.stdin);print(\"   sr-frr reported=%s zebra_connected=%s\"%(d.get(\"reported\"),d.get(\"zebra_connected\")))"
    curl -s --max-time 3 http://127.0.0.1:19001/status | tr -d " \n" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(\"   sr-route local=%s rib_routes=%s\" % (len(d.get(\"local\") or []), d.get(\"rib_routes\")))
for x in (d.get(\"local\") or []): print(\"     \", x)
"'
} | tee "$LOG"

# 探测：1 秒一发，覆盖整段观察窗（换进程 + RR 撤回 + 空洞）。
ssh -n -o BatchMode=yes "$PB" "end=\$((SECONDS+$SECS+15)); while [ \$SECONDS -lt \$end ]; do printf \"%s \" \$(date +%s.%3N); timeout 1 ping -c1 -W1 -I $SRC $DST >/dev/null 2>&1 && echo ok || echo FAIL; done" > "$RAW" 2>/dev/null &
PROBER=$!
sleep 4

ssh -o BatchMode=yes "$BOX" bash -s -- "$SECS" <<'REMOTE' | tee -a "$LOG"
set -u
SECS=$1
old=$(pgrep -x sr-route | head -1)
echo "   SIGTERM sr-route pid=$old 于 $(date +%s.%3N)（sr-frr pid=$(pgrep -x sr-frr|head -1) 不动）"
kill -TERM "$old"
T0=$(date +%s%3N)
systemctl start sr-box-watch.service
new=""
for i in $(seq 1 80); do
  new=$(pgrep -x sr-route | head -1 || true)
  [ -n "$new" ] && [ "$new" != "$old" ] && break
  sleep 0.1
done
[ -n "$new" ] && [ "$new" != "$old" ] || { echo "   !! 8 秒内没起来新进程"; exit 1; }
echo "   新 pid=$new 起来用时 $(( $(date +%s%3N) - T0 ))ms  跑=$(sha256sum /proc/$new/exe|cut -c1-16)"
frrpid=$(pgrep -x sr-frr | head -1)
prev=""
for i in $(seq 1 "$SECS"); do
  n=$(curl -s --max-time 2 http://127.0.0.1:19001/status 2>/dev/null | tr -d ' \n' |
      python3 -c "import sys,json;print(len(json.load(sys.stdin).get('local') or []))" 2>/dev/null || echo 取不到)
  rep=$(curl -s --max-time 2 --unix-socket /run/sr-frr/control.sock http://sr-frr/status 2>/dev/null |
      tr -d ' \n' | python3 -c "import sys,json;print(json.load(sys.stdin).get('reported'))" 2>/dev/null || echo 取不到)
  [ "$n" = "$prev" ] && continue
  printf '   t+%ss local=%s sr-frr.reported=%s sr-frr.pid=%s\n' "$i" "$n" "$rep" "$frrpid"
  prev=$n
done
printf '   %ss 结束时 local=%s（sr-frr reported=%s，pid=%s 全程未变）\n' "$SECS" "${prev:-?}" \
  "$(curl -s --max-time 2 --unix-socket /run/sr-frr/control.sock http://sr-frr/status | tr -d ' \n' | python3 -c "import sys,json;print(json.load(sys.stdin).get('reported'))" 2>/dev/null || echo 取不到)" \
  "$frrpid"
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
bad = [t for t, ok in rows if not ok]
print(f"   客户探测共 {len(rows)} 发，FAIL {len(bad)} 发")
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
    print(f"     连续断流 {len(r)} 发  {r[0]:.3f} → {r[-1]:.3f}  {r[-1]-r[0]:.2f}s")
if runs:
    print(f"   最长断流 {max(r[-1]-r[0] for r in runs):.2f}s（这一枪客户看见的时长；观察窗截断的不算闭合）")
else:
    print("   无断流")
PY
