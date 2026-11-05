#!/bin/bash
# 缺陷⑦ (#268) 判据：VPP 重启后，sr-fib 自己重连 VPP 的 API socket 并按整份计划重装修表。
# 全程不碰 sr-fib 进程 —— 这是这条缺陷要证的唯一一件事。
#
# 为什么这个操作要在页面上点名：它 systemctl restart vpp，是必然中断客户流量的操作。
# 本脚本用客户侧 loopback 互 ping 把中断时长量出来（见 gate.sh 之后的 gap 分析）。
#
# 跑法：在 CPE 上以 root 后台执行（ssh 通道本身走 VPP，重启期间会断，所以必须脱离 ssh）：
#   setsid nohup bash /tmp/gate.sh > /tmp/gate268/run.out 2>&1 < /dev/null &
#
# GATE_PHASE=all（默认）跑满四个阶段：换件 → 经产品清单换进程 → 只重启 VPP。
# GATE_PHASE=vpp 跳过阶段 1/2，只做"VPP 重启，其它一律不碰" —— 这是产品现状的形状：
#   sr-fib 进程活着、内存里那张隧道表还在，此时判据B（装表自动回来）才第一次真正可判。
#   2026-11-06 加这一档，是为了把"隧道表完好时会不会自愈"和 G-2 分开量。
set -u

GATE_PHASE=${GATE_PHASE:-all}

BIN=/usr/local/bin/sr-fib
NEWBIN=/tmp/sr-fib.new
FIBLOG=/var/log/sr-box/fib.log
START=/usr/local/lib/sr-box/start.sh
OUT=/tmp/gate268
SAMP=$OUT/sampler.csv
# status_addr 只是 host:port，sr-fib 只在 /status 这个路径上答；
# 少了这一段就是 404 + 19 字节 "404 page not found"，json 解析失败一律打 0 ——
# 2026-11-06 之前两轮采样的 converged/隧道/跳过/routes 四列全是这把尺子的假 0。
STATUS=http://$(python3 -c 'import json;print(json.load(open("/etc/sdwan/fib.json"))["status_addr"])' 2>/dev/null)/status

# 装表读数（vppctl 输出带 CRLF，模式一律不锚行尾）：
#   r0  = 主表里 sr-fib 装的 overlay 路由（颜色 0 落在主表）
#   t250= overlay 表 250 里按隧道表装的 /32
# 默认是 CPE4 现场（客户前缀 172.16.3.1 / 172.16.12.1，三条隧道）；换盒子跑用 R0PAT 覆盖。
R0PAT=${R0PAT:-'^172\.16\.(3|12)\.1/32'}
T250PAT=${T250PAT:-'via 10\.250\.0\.[0-9]+ wg'}
R0_WANT=${R0_WANT:-2}
T250_WANT=${T250_WANT:-3}
r0() { vppctl "show ip fib" 2>/dev/null | grep -cE "$R0PAT"; }
t250() { vppctl "show ip fib table 250" 2>/dev/null | grep -cE "$T250PAT"; }
bp() { awk '/broken pipe/{n++} END{print n+0}' "$FIBLOG" 2>/dev/null; }
# sr-fib 的判定藏在 last 里，键名是 "converged":true（冒号后没有空格），
# 而且顶层根本没有 converged 这个键，所以只能解析 JSON，不能 grep。
# 2026-11-06 又发现第二轮的四列假 0 来自另一个更低的错误：STATUS 少了 /status 路径。
#   converged  = last.converged（本轮整份计划收敛成功没有）
#   tunnels    = 顶层 tunnels 的条数（=0 就是缺口 G-2：进程重启后内存里那张表空了）
#   warn       = last.warnings 的条数（>0 就是这一轮有路径被跳过，原因写在日志里）
#   routes     = last.routes_set（装进 VPP 的客户前缀条数）
status_get() {  # $1 = 要读的口径
  curl -s --max-time 2 "$STATUS" 2>/dev/null | python3 -c '
import json,sys
try:
    d = json.load(sys.stdin)
except Exception:
    print(-1); sys.exit()   # 取不到/解析不了要打 -1，不能打 0：0 是"隧道表空"的有效读数
k = sys.argv[1]
la = d.get("last") or {}
if k == "tunnels":
    print(len(d.get("tunnels") or []))
elif k == "warn":
    print(len(la.get("warnings") or []))
elif k == "routes":
    print(la.get("routes_set") or 0)
else:
    v = la.get(k)
    print(1 if v else 0)' "$1" 2>/dev/null || echo -1; }
converged() { status_get converged; }
tunnels() { status_get tunnels; }
warn() { status_get warn; }
routes() { status_get routes; }

say() { echo "[$(date '+%F %T')] $*"; }

# ---- 采样器：一秒一行，覆盖整个实验窗口 ----
(
  printf 'ts,vpp_pid,fib_pid,r0,t250,broken_pipe,converged,tunnels,warn,routes\n' > "$SAMP"
  for _ in $(seq 1 "${SAMP_N:-460}"); do
    printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
      "$(date '+%T')" "$(pidof vpp | awk '{print $1}')" "$(pidof sr-fib | awk '{print $1}')" \
      "$(r0)" "$(t250)" "$(bp)" "$(converged)" "$(tunnels)" "$(warn)" "$(routes)" >> "$SAMP"
    sleep 1
  done
) &
SAMPLER=$!

say "== 阶段 0：改前的现场 =="
FIB_PID0=$(pidof sr-fib | awk '{print $1}')
say "sr-fib  sha256=$(sha256sum "$BIN" | cut -c1-64) pid=$(pidof sr-fib) vpp_pid=$(pidof vpp)"
say "装表 r0=$(r0) t250=$(t250) broken_pipe=$(bp) converged=$(converged) 隧道=$(tunnels) 跳过=$(warn) routes=$(routes)"

if [ "$GATE_PHASE" != vpp ]; then
say "== 阶段 1：原子换二进制（不动进程）=="
if [ ! -f "$NEWBIN" ]; then say "FAIL: 没有 $NEWBIN"; exit 1; fi
say "新件 sha256=$(sha256sum "$NEWBIN" | cut -c1-64)"
cmp -s "$NEWBIN" "$BIN" && say "盒子上已经是这一版（换件这步没有可观察变化）"
# 覆盖正在执行的 ELF 会 ETXTBSY，所以同盘起个临时名再 rename —— 原子，且运行中的进程照旧抱着老 inode。
cp "$NEWBIN" "$BIN.gate-tmp"
chmod 0755 "$BIN.gate-tmp"
mv -f "$BIN.gate-tmp" "$BIN" && say "已原子换到 $BIN"
say "运行中的进程 inode 不变：pid=$(pidof sr-fib) exe=$(readlink /proc/$(pidof sr-fib)/exe) ino=$(stat -c %i /proc/$(pidof sr-fib)/exe)"

say "== 阶段 2：经产品路径把 sr-fib 换到新二进制（只动 sr-fib，VPP 不动）=="
# 幂等清单的规矩是"在跑的绝不重启"，所以这里显式停掉 fib 这一步再走一遍 up。
pkill -x sr-fib
for _ in $(seq 1 10); do pgrep -x sr-fib >/dev/null || break; sleep 0.3; done
say "sr-fib 已退出，改由 $START up 带回来"
"$START" up > "$OUT/start-up-1.log" 2>&1
say "up 退出码=$? 现 pid=$(pidof sr-fib)"
sleep 15
say "阶段 2 后：r0=$(r0) t250=$(t250) broken_pipe=$(bp) converged=$(converged) 隧道=$(tunnels) 跳过=$(warn) routes=$(routes)"
say "   （隧道=$(tunnels)：磁盘 fib.json 里 tunnels 是空的，进程一重启内存里那张表就没了 —— 这是缺口 G-2，不是重连缺陷）"
else
say "== 阶段 1/2 跳过（GATE_PHASE=vpp）：不换件、不动 sr-fib 进程 =="
say "   这一档的形状是产品现状：进程没重启过、内存里隧道表=$(tunnels) 条完好。"
fi

say "== 阶段 3：注入缺陷⑦的场景 —— 只重启 VPP，sr-fib / agent 一律不碰 =="
BP_BEFORE=$(bp)
# 重连行按"整份日志里这条出现了几次"来判，不按 tail 里有没有来判：
# 盒子上此前已经跑过一轮，旧日志里就躺着这条行，tail -40 会一眼假阳性。
RC_BEFORE=$(grep -c "已连上 VPP api.sock" "$FIBLOG" 2>/dev/null)
RC_BEFORE=${RC_BEFORE:-0}
RECONNECT=""
say "重启前：vpp_pid=$(pidof vpp) fib_pid=$(pidof sr-fib) r0=$(r0) t250=$(t250) broken_pipe=$BP_BEFORE 重连行累计=$RC_BEFORE"
date '+%s %N' > "$OUT/clock-box.txt"   # 与 gap 分析对时的锚点（盒子侧 epoch）
systemctl restart vpp
say "systemctl restart vpp 已发出，等待自恢复（不干预）"

# 两半分开判，别把 G-2 记到重连缺陷头上：
#   判据A（缺陷⑦ 本体，这次修的）：sr-fib 进程没被重启、broken pipe 不再逐秒增长、日志里出现重连行。
#   判据B（装表齐）：客户前缀 r0 回到 $R0_WANT。隧道表空（阶段 2 换过进程）时注定回不来 = G-2；
#     隧道表完好（GATE_PHASE=vpp，产品现状）时应当几秒内自己回来 —— 这才是"机制支持动态装表"的正面判据。
HEAILED=""
for i in $(seq 1 120); do
    sleep 1
    n_r0=$(r0); n_t=$(t250); n_fib=$(pidof sr-fib | awk '{print $1}'); n_bp=$(bp)
    n_rc=$(grep -c "已连上 VPP api.sock" "$FIBLOG" 2>/dev/null); n_rc=${n_rc:-0}
    if [ -z "$RECONNECT" ] && [ "$n_rc" -gt "$RC_BEFORE" ] \
       && [ "$n_bp" -le $((BP_BEFORE + 5)) ]; then
        RECONNECT=$i
        say "第 ${i}s：判据A 通过 —— 新增重连行 $RC_BEFORE→$n_rc，broken pipe 已止住（+$((n_bp-BP_BEFORE))），fib_pid=$n_fib"
    fi
    if [ "$n_r0" = "$R0_WANT" ] && [ "$n_t" = "$T250_WANT" ]; then
        say "第 ${i}s：判据B 通过 —— 装表恢复 r0=$n_r0 t250=$n_t fib_pid=$n_fib broken_pipe=$n_bp"
        HEALED=$i
        break
    fi
    if [ $((i % 10)) -eq 0 ]; then
        say "第 ${i}s：r0=$n_r0 t250=$n_t fib_pid=$n_fib broken_pipe=$n_bp(+$((n_bp-BP_BEFORE))) 隧道=$(tunnels) 跳过=$(warn)"
    fi
done

say "== 阶段 4：结论 =="
say "判据A（VPP 重连，#268 本次修复）：重连用时 = ${RECONNECT:-未观察到} 秒；重连后 fib_pid=$(pidof sr-fib)"
say "判据B（装表齐；本档形状 GATE_PHASE=$GATE_PHASE，隧道表=$(tunnels) 条）：恢复用时 = ${HEALED:-未恢复} 秒"
say "判据C（这条路径里没有重启 sr-fib）：阶段 0 pid=$FIB_PID0 → 现在 $(pidof sr-fib | awk '{print $1}')（相同=没靠重启兜底）"
say "装表 r0=$(r0) t250=$(t250) converged=$(converged) 隧道=$(tunnels) 跳过=$(warn) routes=$(routes)"
say "broken_pipe：改前 $BP_BEFORE → 现在 $(bp)（增量 $(($(bp)-BP_BEFORE))）"
say "sr-fib 日志尾部："
tail -25 "$FIBLOG" | sed 's/^/    /'
say "清单在跑的形状："
"$START" check 2>&1 | sed 's/^/    /'

kill "$SAMPLER" 2>/dev/null
say "DONE"
