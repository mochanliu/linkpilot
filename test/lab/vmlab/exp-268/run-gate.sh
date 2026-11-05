#!/bin/bash
# 缺陷⑦ (#268) 判据的编排：铺件 → 起客户侧连续探针 → 在盒子里后台跑 gate.sh → 收数。
# gate.sh 必须脱离 ssh 跑：CPE 的 ssh 通道本身走 VPP，restart vpp 期间这条通道会断。
set -eu

# 用法（在 Mac 上跑）：D 是本实验的工作目录，BIN 是待验证的 linux/sr-fib
#   S=$(pwd) D=/tmp/gate-268 BIN=/path/to/linux/sr-fib bash run-gate.sh
S=${S:-$(cd "$(dirname "$0")" && pwd)}
D=${D:?工作目录（raw 数据落在这里，不进仓库）}
# BIN 只有 GATE_PHASE=all（要换件）才需要；vpp 档不换件，别为它白等一趟 8.3MB 的慢链路。
BIN=${BIN:-}
PROBE_SECS=${PROBE_SECS:-400}
BOX=${BOX:-CPE4}

echo "== 1. 铺脚本（换件档才铺新二进制）=="
scp -q "$S/gate.sh" "$BOX:/tmp/gate.sh"
scp -q "$S/ping-probe.sh" "$BOX:/tmp/ping-probe.sh"
if [ -n "$BIN" ]; then
    scp -q "$BIN" "$BOX:/tmp/sr-fib.new"
    ssh "$BOX" 'chmod 755 /tmp/gate.sh /tmp/ping-probe.sh; mkdir -p /tmp/gate268; sha256sum /tmp/sr-fib.new'
else
    ssh "$BOX" 'chmod 755 /tmp/gate.sh /tmp/ping-probe.sh; mkdir -p /tmp/gate268'
fi

echo "== 2. 先停 watch 定时器，免得缺陷⑧ 在同一秒把 sr-fib 起两份、把读数搅浑 =="
ssh "$BOX" 'systemctl stop sr-box-watch.timer; systemctl is-active sr-box-watch.timer || true; pgrep -cx sr-fib'

echo "== 3. 起客户侧连续探针（cust4↔cust3，loopback 互为源）=="
# 先把各家的钟钉到同一条轴上：客户 VM 的 epoch 和盒子差好几个小时，
# 不对齐就没法回答"中断落在哪一阶段"（2026-11-05 那一轮就是事后才发现这一点）。
mkdir -p "$D/raw"
"$S/clock-anchor.sh" "$D/raw/clock.txt" "$BOX" cust4 cust3
scp -q "$S/ping-probe.sh" cust4:/tmp/ping-probe.sh
scp -q "$S/ping-probe.sh" cust3:/tmp/ping-probe.sh
ssh cust4 "rm -f /tmp/probe.out; setsid nohup bash /tmp/ping-probe.sh 172.16.4.1 172.16.3.1 $PROBE_SECS /tmp/probe.out >/dev/null 2>&1 < /dev/null & echo cust4 探针已起"
ssh cust3 "rm -f /tmp/probe.out; setsid nohup bash /tmp/ping-probe.sh 172.16.3.1 172.16.4.1 $PROBE_SECS /tmp/probe.out >/dev/null 2>&1 < /dev/null & echo cust3 探针已起"
sleep 8
echo "   探针起势（各 3 行）："
ssh cust4 'tail -3 /tmp/probe.out' 2>&1 | sed 's/^/   cust4 /'
ssh cust3 'tail -3 /tmp/probe.out' 2>&1 | sed 's/^/   cust3 /'

echo "== 4. 放盒子上的判据脚本脱离 ssh 跑起来 =="
ssh "$BOX" "cd /tmp && GATE_PHASE=${GATE_PHASE:-all} SAMP_N=${SAMP_N:-460} setsid nohup bash /tmp/gate.sh > /tmp/gate268/run.out 2>&1 < /dev/null & echo 已启动 pid=\$!"
echo "   等待实验走完（探针 ${PROBE_SECS}s）…"

# 5. 收数：探针跑完 + 判据脚本自己的相位表落盘，再回 Mac 一份，中间留 40s 余量。
sleep $((PROBE_SECS + 40))
mkdir -p "$D/raw"
scp -q "$BOX:/tmp/gate268/run.out" "$D/raw/run.out" 2>/dev/null
scp -q "$BOX:/tmp/gate268/sampler.csv" "$D/raw/sampler.csv" 2>/dev/null
scp -q "$BOX:/tmp/gate268/clock-box.txt" "$D/raw/clock-box.txt" 2>/dev/null
scp -q cust4:/tmp/probe.out "$D/raw/probe-cust4.out" 2>/dev/null
scp -q cust3:/tmp/probe.out "$D/raw/probe-cust3.out" 2>/dev/null
echo "== 5. 判据读数（盒子侧 run.out 的结论三段）=="
grep -E '判据|阶段 4|DONE' "$D/raw/run.out" | tail -12
echo "   中断窗口（时间轴已按 clock.txt 对齐）："
python3 "$S/gap-report.py" --anchor "$D/raw/clock.txt" --events "$D/raw/run.out" \
    --box-anchor "$BOX" \
    "$D/raw/probe-cust4.out=cust4=cust4→cust3" \
    "$D/raw/probe-cust3.out=cust3=cust3→cust4"

echo "== 6. 把盒子的 watch 定时器恢复成产品形状（跑之前是为了读数干净才停的）=="
ssh "$BOX" 'systemctl start sr-box-watch.timer; systemctl is-active sr-box-watch.timer' 2>/dev/null
