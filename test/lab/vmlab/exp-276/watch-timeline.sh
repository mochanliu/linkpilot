#!/bin/bash
# #276 切片 C 的第二条判据：**装路边那 201~755ms 到底花在谁身上。**
#
# 已知（2026-11-08 四枪，sample-tight.sh，分辨率 43~61ms）：
#   摘路边 可装→路径下架 <= 40~45ms（改前 <=793ms）—— 事件推这一段赢了。
#   装路边 in→ok **一次都没有分开过**：可达性门不背这个账。
#   装路边 ok→ours = 201 / 272 / 294 / 755ms：波动大，且改前(轮询)的 873ms 落在同一个带里
#     ⇒ 这不是轮询周期的账。要么在 sr-route 发事件，要么在 sr-fib 写 VPP。
#
# 分开它们的唯一办法是直接看事件什么时候离开 sr-route。/watch 只有一个订阅位（实测 409），
# 所以这一轮要把 sr-fib 请出去、由我占用这个位掐表。
#
# 必然中断（AGENTS.md §1.4，点名）：本脚本运行期间（约 DUR+10s，默认 ~55s）CPE1 上 **sr-fib 不在跑**，
# 后果是这段时间的路由变化不落数据面 —— 已经装好的路径不动，所以已有客户流量不掉
# （同口径实测：kill sr-fib + 走开机清单起来 = 0/1561 丢包）。
# 脚本在同一次运行里做收尾：走产品自己的路径（sr-box-watch.service）把 sr-fib 拉回来，
# 并核对 pid 变了、盘上哈希==跑的哈希、VPP 里这条前缀的 mpls via 路径数恢复。
#
# 用法：DUR=45 bash watch-timeline.sh [CONSUMER=CPE1] [PRODUCER=CPE3] [PFX=172.16.3.1/32]
set -u
CONSUMER=${1:-CPE1}
PRODUCER=${2:-CPE3}
PFX=${3:-172.16.3.1/32}
DST=${PFX%/32}
DUR=${DUR:-45}
WORK=${WORK:-/tmp/p276}
TAG=${TAG:-watch}
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$WORK"
EV=$WORK/ev-$CONSUMER-$TAG.txt
RAW=$WORK/leg-$CONSUMER-$TAG.txt
LOG=$WORK/timeline-$CONSUMER-$TAG.txt

say() { printf '%s\n' "$*" | tee -a "$LOG"; }
say "== #276 事件时间轴 $TAG $(date -u +%FT%TZ)  消费者=$CONSUMER 前缀=$PFX 注入=$PRODUCER"

say "-- 0) 起手现场"
ssh -n "$CONSUMER" 'printf "   sr-fib pid=%s 跑=%s 盘=%s\n" "$(pgrep -x sr-fib|tr "\n" ,)" \
    "$(sha256sum /proc/$(pgrep -x sr-fib|head -1)/exe|cut -c1-16)" "$(sha256sum /usr/local/bin/sr-fib|cut -c1-16)"
   echo "   timer=$(systemctl is-active sr-box-watch.timer) service=$(systemctl is-active sr-box-watch.service)"
   echo -n "   VPP 里这条前缀的 mpls via 路径数="; vppctl "show ip fib '"$PFX"'" | grep -c "mpls via"' | tee -a "$LOG"

say "-- 1) 请 sr-fib 让位（停 timer 防它被清单抢先占位，再 SIGTERM）"
ssh -n "$CONSUMER" 'systemctl stop sr-box-watch.timer
   p=$(pgrep -x sr-fib); [ -n "$p" ] && kill -TERM $p
   for i in $(seq 1 30); do pgrep -x sr-fib >/dev/null || break; sleep 0.1; done
   echo "   现在 sr-fib pid=$(pgrep -x sr-fib|tr "\n" ,) （空=已让位） timer=$(systemctl is-active sr-box-watch.timer)"' | tee -a "$LOG"

scp -q "$HERE/stamp-watch.sh" "$HERE/sample-tight.sh" "$CONSUMER:/tmp/" 2>/dev/null
ssh -n "$CONSUMER" 'bash /tmp/stamp-watch.sh' > "$EV" 2>/dev/null &
E=$!
ssh -n "$CONSUMER" "bash /tmp/sample-tight.sh $((DUR+20)) $PFX" > "$RAW" 2>/dev/null &
S=$!
sleep 4
say "   起手帧：$(head -1 "$EV" | cut -c1-90)"
T0=$(ssh -n "$PRODUCER" 'date +%s.%3N')
say "-- 2) 注入 restart frr（$PRODUCER 墙钟 $T0；两盒没对时，只用事件流自己的钟）"
ssh -n "$PRODUCER" 'systemctl restart frr' >/dev/null 2>&1

wait $S
kill $E 2>/dev/null; wait $E 2>/dev/null
say "   帧 $(grep -c '^' "$EV") 行 / 采样 $(wc -l < "$RAW") 发"

say "-- 3) 恢复 sr-fib（产品路径：跑一遍开机清单）"
ssh -n "$CONSUMER" 'systemctl start sr-box-watch.service
   for i in $(seq 1 60); do p=$(pgrep -x sr-fib); [ -n "$p" ] && break; sleep 0.1; done
   systemctl start sr-box-watch.timer
   printf "   sr-fib pid=%s 跑=%s 盘=%s timer=%s\n" "$(pgrep -x sr-fib|tr "\n" ,)" \
     "$(sha256sum /proc/$(pgrep -x sr-fib|head -1)/exe|cut -c1-16)" "$(sha256sum /usr/local/bin/sr-fib|cut -c1-16)" "$(systemctl is-active sr-box-watch.timer)"
   echo -n "   恢复后 VPP 里 mpls via 路径数="; vppctl "show ip fib '"$PFX"'" | grep -c "mpls via"
   curl -s --max-time 3 http://127.0.0.1:19011/status | head -c 300; echo' | tee -a "$LOG"

python3 - "$EV" "$RAW" "$PFX" <<'PY' | tee -a "$LOG"
import json
import sys

evf, rawf, pfx = sys.argv[1], sys.argv[2], sys.argv[3]
frames = []
for ln in open(evf):
    p = ln.split(None, 1)
    if len(p) < 2 or not p[1].startswith("data:"):
        continue
    t = float(p[0])
    try:
        b = json.loads(p[1][5:].strip())
    except Exception:
        frames.append((t, "坏帧", None, None))
        continue
    hit = [e for e in b.get("events", []) if e.get("prefix") == pfx]
    kind = "INIT" if b.get("init") else "批次"
    ops = {e.get("op") for e in hit}
    frames.append((t, kind, len(b.get("events", [])), "+".join(sorted(ops)) or "-"))

print("   /watch 帧（只列与本条前缀有关的和 INIT）：")
for t, kind, n, op in frames[:1] + [f for f in frames[1:] if f[3] != "-"]:
    print("     %.3f  %s  本批 %s 条  该前缀 op=%s" % (t, kind, n, op))
quiet = sum(1 for f in frames if f[3] == "-")
print("   与该前缀无关的批次 %d 个；data 帧共 %d 个" % (quiet, len(frames)))

rows = []
for ln in open(rawf):
    p = ln.split()
    if len(p) < 6:
        continue
    d = {kv.split("=", 1)[0]: kv.split("=", 1)[1] for kv in p[1:]}
    if d.get("ok") not in ("Y", "N"):
        continue
    rows.append((float(p[0]), d["ok"] == "Y"))
if len(rows) < 20:
    sys.exit("   采样太少，不算数")
gaps = sorted(rows[i + 1][0] - rows[i][0] for i in range(len(rows) - 1))
res = gaps[len(gaps) // 2]
print("   采样中位间隔（分辨率）= %.0fms，共 %d 发" % (res * 1000, len(rows)))

edges = []
prev = None
for r in rows:
    if prev and r[1] != prev[1]:
        edges.append((prev[0], r[0], r[1]))
    prev = r
down = next((e for e in edges if not e[2]), None)
up = next((e for e in edges if e[2]), None)
if not up:
    sys.exit("   这一轮 ok 没回来，无法给装路边的账")

# INIT 那批每条前缀都是 upsert，不能当"装路边的事件"——只在非 INIT 帧里找。
ev_up = next((f for f in frames if f[1] == "批次" and "upsert" in (f[3] or "")), None)
ev_dn = next((f for f in frames if f[1] == "批次" and "withdraw" in (f[3] or "")), None)
print("   ── 对齐（全是 %s 自己的钟）" % "本机")
if down and ev_dn:
    print("   摘：ok 消失 落在 %.3f~%.3f / withdraw 帧到达 %.3f ⇒ 事件发出 <= %.0fms（分辨率 %.0fms）"
          % (down[0], down[1], ev_dn[0], (ev_dn[0] - down[0]) * 1000, res * 1000))
print("   装：ok 回来 落在 %.3f~%.3f" % (up[0], up[1]))
if ev_up:
    print("       upsert 帧到达 %.3f ⇒ 相对「最后一次看见 ok=N」<= %.0fms；相对「第一次看见 ok=Y」%+.0fms"
          % (ev_up[0], (ev_up[0] - up[0]) * 1000, (ev_up[0] - up[1]) * 1000))
else:
    print("       这一轮没看到本条前缀的 upsert 帧 ⇒ sr-route 没发事件，是它的问题")
PY
