#!/bin/bash
# run-of.sh -- does `output-feature` keep the management path alive through NAT?
#
# Lesson from 2026-11-12: a multi-command ssh chain dies on SIGHUP the moment
# the link drops, so every enable sequence must run detached ON the box.
# nat-of-test.sh does that: enable(plugin + interfaces WITH output-feature +
# pool) -> dumps -> hold 90s -> disable itself.
#
# This driver: arms a delayed customer ping on cust4 FIRST (its ssh also rides
# CPE4), launches the detached of-test, then polls ssh availability from the
# Mac every 5s for 100s and prints the timeline.
#
# Verdict:
#   ssh comes back/persists during the hold  -> output-feature fixes mgmt
#   ssh dead the whole hold                  -> output-feature does not help;
#                                               mgmt needs static mappings or
#                                               a separate (non-iface) pool
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

# wait for underlay convergence after whatever reboot we are in
for i in $(seq 1 30); do
    pairs=$(run "$BOX" "vppctl show lcp 2>/dev/null | grep -c '^itf-pair'" 2>/dev/null || echo 0)
    [ "$pairs" -ge 2 ] && break
    sleep 3
done
note "underlay pairs=$pairs"

note "== 武装 cust4 延迟 ping（20s 后开打，8 包） =="
run "$CUST" "rm -f /root/nat-of-ping.txt; nohup bash -c 'sleep 20; ping -c8 -W2 $GW > /root/nat-of-ping.txt 2>&1' >/dev/null 2>&1 & echo armed"

note "== 部署 + 拉起 detached of-test =="
scp -q $SSHO "$HERE/nat-of-test.sh" "$BOX:/tmp/nat-of-test.sh"
run "$BOX" "install -m 0755 /tmp/nat-of-test.sh /root/vmlab/nat-of-test.sh && rm -f /root/vmlab/nat-bare-keep && systemd-run --unit nat-of-run-\$\$ /root/vmlab/nat-of-test.sh" || note "拉起失败"

note "== ssh 存活时间线（每 5s 一拍，共 100s） =="
: > "$RAW/ssh-timeline-$STAMP.txt"
for i in $(seq 1 20); do
    t=$((i * 5))
    if ssh -n $SSHO -o ConnectTimeout=4 "$BOX" true >/dev/null 2>&1; then
        echo "t=+$t  ALIVE" | tee -a "$RAW/ssh-timeline-$STAMP.txt"
    else
        echo "t=+$t  dead" | tee -a "$RAW/ssh-timeline-$STAMP.txt"
    fi
    sleep 5
done

note "== of-test 收尾后取结果 =="
sleep 15
{
    echo "== cust4 ping (armed, fired during hold) =="
    run "$CUST" "cat /root/nat-of-ping.txt 2>/dev/null || echo 'no log'"
    echo
    echo "== box of-test log =="
    run "$BOX" "cat /root/vmlab/nat-of-test.log 2>/dev/null || echo 'no log'"
} | tee "$RAW/of-test-$STAMP.txt"
