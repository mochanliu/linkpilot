#!/bin/bash
# run-plainpool.sh -- driver for Shape E (plain outside + dedicated pool).
#
# Arms a delayed customer ping on cust4, arms the on-box watchdog, launches the
# detached test script, polls ssh from the Mac every 5s for 100s, then collects
# the on-box log. Everything on-box is detached: a multi-command ssh chain dies
# on SIGHUP the moment the link drops (measured 2026-11-12).
#
# Verdict:
#   ssh ALIVE for the whole hold
#   + cust4 8/8 with sessions > 0
#   + no "out of ports"
#   -> Shape E is the product shape; NAT needs no output-interface at all.
set -u -o pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
RAW=$HERE/raw
mkdir -p "$RAW"
STAMP=$(date +%Y%m%d-%H%M%S)
SSHO="-o BatchMode=yes -o ConnectTimeout=6 -o LogLevel=ERROR"
BOX=CPE4
CUST=cust4
GW=192.168.1.1

run() { local h=$1 out rc; shift
    out=$(ssh -n $SSHO "$h" "$*" 2>&1 | tr -d '\r'); rc=$?
    printf '%s\n' "$out" | grep -v '^Warning: Permanently added'
    return $rc; }
note() { printf '.... %s\n' "$*"; }

for i in $(seq 1 30); do
    pairs=$(run "$BOX" "vppctl show lcp 2>/dev/null | grep -c '^itf-pair'" 2>/dev/null || echo 0)
    [ "$pairs" -ge 2 ] && break
    sleep 3
done
note "underlay pairs=$pairs"

note "== 武装 cust4 延迟 ping（20s 后开打，8 包） =="
run "$CUST" "rm -f /root/nat-plainpool-ping.txt; nohup bash -c 'sleep 20; ping -c8 -W2 $GW > /root/nat-plainpool-ping.txt 2>&1' >/dev/null 2>&1 & echo armed"

note "== 部署脚本 + 武装看门狗（240s 兜底）+ 拉起 detached 测试 =="
scp -q $SSHO "$HERE/nat-plainpool-test.sh" "$BOX:/tmp/nat-plainpool-test.sh"
run "$BOX" "install -m 0755 /tmp/nat-plainpool-test.sh /root/vmlab/nat-plainpool-test.sh && rm -f /root/vmlab/nat-bare-keep && systemd-run --unit nat-pp-wd-\$\$ /root/vmlab/nat-bare-watchdog.sh"
sleep 2
run "$BOX" "systemd-run --unit nat-pp-run-\$\$ /root/vmlab/nat-plainpool-test.sh" || note "拉起失败"

note "== ssh 存活时间线（每 5s 一拍，共 100s） =="
: > "$RAW/ssh-timeline-plainpool-$STAMP.txt"
for i in $(seq 1 20); do
    t=$((i * 5))
    if ssh -n $SSHO -o ConnectTimeout=4 "$BOX" true >/dev/null 2>&1; then
        echo "t=+$t  ALIVE" | tee -a "$RAW/ssh-timeline-plainpool-$STAMP.txt"
    else
        echo "t=+$t  dead" | tee -a "$RAW/ssh-timeline-plainpool-$STAMP.txt"
    fi
    sleep 5
done

note "== 取结果（等测试脚本自己收尾） =="
sleep 20
{
    echo "== cust4 ping (armed, fired during hold) =="
    run "$CUST" "cat /root/nat-plainpool-ping.txt 2>/dev/null || echo 'no log'"
    echo
    echo "== box plainpool log =="
    run "$BOX" "cat /root/vmlab/nat-plainpool-test.log 2>/dev/null || echo 'no log'"
} | tee "$RAW/plainpool-test-$STAMP.txt"
