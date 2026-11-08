#!/bin/bash
# #276 判据（切片 C）：**一条远端前缀在对端消失 / 回来时，本盒 VPP 主表跟着消失 / 回来慢多少，
# 以及这一前一后两段账分别记在谁头上。**
#
# 从 sr-route 知道这条前缀，到客户流量真的走我们那条隧道路径，中间是三段，不是一段：
#
#   1 in  -> 本机 sr-route 的 Loc-RIB 里有这条前缀（/status 的 rib 段或 pending 段任一）
#   2 ok  -> 它进了「可装视图」：/status 的 rib 段带 next_hops，即 sr-route 认为下一跳落得了地
#            （resolvable 吃活动隧道，见 #275）
#   3 ours-> VPP 里这条前缀下面真的有 `mpls via` 路径（sr-fib 写进去的）
#
# 切片 B 只负责 2→3：改前这段被 sr-fib 的轮询周期封顶（lab 盘上 poll_sec=1 ⇒ 平均 ~500ms、
# 最坏 ~1s，#262 量的"BFD 150ms 判 down、sr-fib 等到下一轮才摘路"就是它）；换成 SSE /watch
# 之后理论上只剩 sr-route 的 10ms 合并窗口 + 一次下发。
# 1→2 不归切片 B —— 那是可达性输入（#262/#275 的地盘）。第一版量具只有一列 rib=grep "prefix"，
# 把 1 和 2 读成同一个信号（/status 的 pending 段每条也带 "prefix"），于是 2→3 没改善的账被记到了
# 1→2 头上，装路边算出 873ms/887ms 这种"改了等于没改"的数。所以这里分成三列。
#
# **输出信号为什么是 ours 而不是"前缀行在不在"**（2026-11-08 实测踩的坑）：这盒开了 linux-cp
# （`lcp-sync on`），内核主表的路由会被镜像进 VPP 主表，署名 lcp-rt / lcp-rt-dynamic（pref=20，
# 出口 host-enp0s8/enp0s9），而 sr-frr 正向注入的 K 路由（172.16.3.1 dev lo proto boot）就在同一张
# 内核主表里 ⇒ 我们撤掉自己那条 MPLS 路径之后 `show ip fib` 那一行**还在**，只是换了署名。
# 拿"行在不在"当输出会读到一个从来没消失的信号。line 只用来数"第二个写手"（line=Y 且 ours=0）。
#
# 注入用 PRODUCER 的 `systemctl restart frr`：zebra 重启 ⇒ 该盒 sr-frr 的本地前缀集清空
# （#274 起的定案：重连即重置，不留乐观窗口），这条前缀全网消失，refill 之后再回来。
# 这是**一次性 dev 台里的被测故障**，不是配置生效路径（同 exp-274）。
# 客户可见后果：该前缀在这段窗口里不可达，脚本一并量出来。
#
# 采样全在 CONSUMER 盒内跑（sample-leg.sh），所以这几段数只有本盒一个时钟，不受"盒子之间没对时"
# 影响。探测机那条是另一台机器的时间轴，只用于形状对照。收敛日志（/var/log/sr-box/fib.log）只有
# 秒级时间戳，所以它只当旁证：这一轮 sr-fib 做了几轮收敛、是不是走 watch 事件起来的，看它；时长不用它算。
#
# 用法：TAG=before|after WORK=/tmp/p276 DUR=90 bash leg-window.sh \
#          [CONSUMER=CPE1] [PRODUCER=CPE3] [PFX=172.16.3.1/32] [PROBER=cust12] [SRC=172.16.12.1]
set -u
CONSUMER=${1:-CPE1}
PRODUCER=${2:-CPE3}
PFX=${3:-172.16.3.1/32}
PROBER=${4:-cust12}
SRC=${5:-172.16.12.1}
DST=${PFX%/32}
WORK=${WORK:-/tmp/p276}
TAG=${TAG:-run}
DUR=${DUR:-90}
HERE=$(cd "$(dirname "$0")" && pwd)
SAMPLER=${SAMPLER:-$HERE/sample-leg.sh}
mkdir -p "$WORK"
RAW=$WORK/leg-$CONSUMER-$TAG.txt
PROBE=$WORK/probe-$PROBER-$TAG.txt
FIBLOG=$WORK/fiblog-$CONSUMER-$TAG.txt
LOG=$WORK/window-$CONSUMER-$TAG.txt

echo "== #276 这条腿 $TAG $(date -u +%FT%TZ)" | tee "$LOG"
echo "   消费者=$CONSUMER 前缀=$PFX 注入=$PRODUCER restart frr 探测=$PROBER($SRC→$DST)" | tee -a "$LOG"
ssh -n -o BatchMode=yes "$CONSUMER" \
  "for x in sr-route sr-fib; do printf '   %-8s run=%s disk=%s\n' \$x \"\$(sha256sum /proc/\$(pgrep -x \$x|head -1)/exe|cut -c1-16)\" \"\$(sha256sum /usr/local/bin/\$x|cut -c1-16)\"; done;
   echo \"   fib.json poll_sec=\$(python3 -c 'import json;print(json.load(open(\"/etc/sdwan/fib.json\")).get(\"poll_sec\"))')\"" | tee -a "$LOG"
ssh -n -o BatchMode=yes "$CONSUMER" 'curl -s --max-time 3 http://127.0.0.1:19011/status' |
  python3 -c 'import json,sys; d=json.load(sys.stdin); print("   起手 sr-fib:", d.get("last"))' 2>/dev/null | tee -a "$LOG"
FIBLOGSTART=$(ssh -n -o BatchMode=yes "$CONSUMER" 'wc -l < /var/log/sr-box/fib.log' 2>/dev/null)
FIBLOGSTART=${FIBLOGSTART:-1}
echo "   fib.log 起算行=$FIBLOGSTART" | tee -a "$LOG"

scp -q -o BatchMode=yes "$SAMPLER" "$CONSUMER:/tmp/sample-leg.sh"

# 采样 + 探测同时起，注入落在中间：三段都要在窗口里。
ssh -n -o BatchMode=yes "$CONSUMER" "bash /tmp/sample-leg.sh $DUR $PFX" > "$RAW" 2>/dev/null &
S=$!
ssh -n -o BatchMode=yes "$PROBER" "end=\$((SECONDS+$DUR)); while [ \$SECONDS -lt \$end ]; do printf '%s ' \$(date +%s.%3N); timeout 1 ping -c1 -W1 -I $SRC $DST >/dev/null 2>&1 && echo ok || echo FAIL; done" > "$PROBE" 2>/dev/null &
P=$!

sleep 8
T0=$(ssh -n -o BatchMode=yes "$PRODUCER" 'date +%s.%3N')
echo "-- 第 8 秒注入 restart frr（$PRODUCER 自己的墙钟 $T0）" | tee -a "$LOG"
ssh -n -o BatchMode=yes "$PRODUCER" 'systemctl restart frr' &
wait $S $P
echo "   采样 $(wc -l < "$RAW") 发 / 探测 $(wc -l < "$PROBE") 发" | tee -a "$LOG"

python3 - "$RAW" "$PROBE" "$CONSUMER" "$PFX" <<'PY' | tee -a "$LOG"
import collections
import sys

raw, probe, box, pfx = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]

# (时刻, in, ok, 原因, ours, line)；解析不了的样本整条丢掉，不猜。
rows = []
for ln in open(raw):
    p = ln.split()
    if len(p) < 6:
        continue
    d = {kv.split("=", 1)[0]: kv.split("=", 1)[1] for kv in p[1:]}
    ours = int(d["ours"]) if d.get("ours", "").isdigit() else None
    tri = lambda k: {"Y": True, "N": False}.get(d.get(k))
    if tri("in") is None or tri("ok") is None or ours is None or tri("line") is None:
        continue
    rows.append((float(p[0]), tri("in"), tri("ok"), d.get("rsn", "na"), ours, tri("line")))
if len(rows) < 20:
    sys.exit("可用样本只有 %d 发，太少，这一轮不算数" % len(rows))

gaps = sorted(rows[i + 1][0] - rows[i][0] for i in range(len(rows) - 1))
res = gaps[len(gaps) // 2]
span = rows[-1][0] - rows[0][0]
print("   采样中位间隔（=本量具的分辨率）= %.0fms，共 %d 发 / %.1fs" % (res * 1000, len(rows), span))


def flips(key):
    """每条边 = (翻转前最后一个已知时刻, 第一次看出翻转的时刻, 翻转后的值)；None 的样本跳过。"""
    out, prev = [], None
    for r in rows:
        v = key(r)
        if v is None:
            continue
        if prev is not None and v != prev[1]:
            out.append((prev[0], r[0], v))
        prev = (r[0], v)
    return out


def state_rows(pred):
    """同一发里同时成立的两列 = 不靠边界推算的**确认窗口**（分辨率只会让它偏小，不会偏大）。"""
    runs, cur = [], None
    for r in rows:
        if pred(r):
            cur = [r[0], r[0]] if cur is None else [cur[0], r[0]]
        elif cur:
            runs.append(tuple(cur))
            cur = None
    if cur:
        runs.append(tuple(cur))
    return runs


def fmt(runs, res):
    if not runs:
        return "一次都没有（<= 1 个采样周期，只能说到了分辨率以下）"
    longest = max((b - a for a, b in runs)) + res
    return "共 %d 段，最长连续 %.0fms" % (len(runs), longest * 1000)


ins, oks, ours = (flips(lambda r: r[1]), flips(lambda r: r[2]), flips(lambda r: r[4] > 0))
lines = flips(lambda r: r[5])
print("   翻转事件（时刻全是 %s 自己的墙钟，最后是翻转后的值）：" % box)
for name, seq in (("in  ", ins), ("ok  ", oks), ("ours", ours), ("line", lines)):
    for t0, t1, to in seq:
        print("     %s -> %s  落在 %.3f ~ %.3f" % (name, to, t0, t1))

in_d, in_u = (next((e for e in ins if not e[2]), None), next((e for e in ins if e[2]), None))
ok_d, ok_u = (next((e for e in oks if not e[2]), None), next((e for e in oks if e[2]), None))
ours_d, ours_u = (next((e for e in ours if not e[2]), None), next((e for e in ours if e[2]), None))
if not in_d or not ours_d:
    sys.exit("   这一轮没抓到前缀消失（注入没生效或窗口太短），不算数")

# 摘路边：保守上界 = 「最后一次看见它还在」到「第一次看出路径没了」，两个观测点之间是真值，
# 所以这个数是上界；小于分辨率时只能说"到分辨率以下"，不能说成 0。
print("   ── 消失方向")
print("   切片 B 这段：可装(ok)还在 -> 我们的路径下架 <= %.0fms（分辨率 %.0fms）"
      % ((ours_d[1] - ok_d[0]) * 1000, res * 1000))
print("   总账：知道(in)还在 -> 我们的路径下架 <= %.0fms" % ((ours_d[1] - in_d[0]) * 1000))
# 装路边：按段给确认窗口，不给推算的界。
print("   ── 回来方向（每段是同一发采样里两列同时成立，确认值、不是推算）")
gate = state_rows(lambda r: r[1] and not r[2])
leg = state_rows(lambda r: r[2] and r[4] == 0)
why = collections.Counter(r[3] for r in rows if r[1] and not r[2])
print("   1→2 知道但不可装（sr-route 的可达性门，不归切片 B）：%s" % fmt(gate, res))
if why:
    print("      理由分布：%s" % ", ".join("%s×%d" % (k, v) for k, v in why.most_common()))
print("   2→3 可装但未生效（**切片 B 的账**）：%s" % fmt(leg, res))
if ours_u:
    print("   整条前缀不可用 %.2fs（含上游 refill 与 1→2 那段，不全是这条腿的账）"
          % (ours_u[1] - ours_d[0]))
else:
    print("   这一轮没抓到 ours 回来（refill 超出窗口），只报消失方向")

ghost = sum(1 for r in rows if r[5] and r[4] == 0)
print("   第二个写手：line=有 且 ours=0 共 %d 发 / %d（>0 说明这条前缀被内核镜像接管过，见头部说明；不影响本判据）"
      % (ghost, len(rows)))
print("   收口：末样本 in=%s ok=%s ours=%d line=%s —— in/ok/ours 必须都是'有'，否则这一轮是以黑洞结束的"
      % ("有" if rows[-1][1] else "无", "有" if rows[-1][2] else "无", rows[-1][4],
         "有" if rows[-1][5] else "无"))

ok = sum(1 for ln in open(probe) if len(ln.split()) == 2 and ln.split()[1].lower() == "ok")
allp = sum(1 for ln in open(probe) if len(ln.split()) == 2)
print("   探测机 ok %d / %d 发（另一台机器的时间轴，只用于形状对照）" % (ok, allp))
PY

ssh -n -o BatchMode=yes "$CONSUMER" \
  "tail -n +$FIBLOGSTART /var/log/sr-box/fib.log" > "$FIBLOG" 2>/dev/null
echo "-- 这一轮 sr-fib 的账（$FIBLOGSTART 行之后的日志，秒级时间戳，只当旁证）：" | tee -a "$LOG"
grep -cE "收敛" "$FIBLOG" | sed 's/^/   收敛轮次=/' | tee -a "$LOG"
grep -E "watch|警告|错误|error" "$FIBLOG" | tail -12 | sed 's/^/   /' | tee -a "$LOG"
tail -6 "$FIBLOG" | sed 's/^/   /' | tee -a "$LOG"

echo "== 原始读数：$RAW / $PROBE / $FIBLOG（不进仓库）"
