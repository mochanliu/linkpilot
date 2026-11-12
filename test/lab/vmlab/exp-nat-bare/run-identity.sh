#!/bin/bash
# run-identity.sh -- driver for Shape G (plain outside + pool + identity mapping).
#
# Pass criteria (all four, measured on the box and from the Mac):
#   1. ssh to the box's WAN address stays alive for the whole hold
#   2. cust4 -> gateway 8/8 (customer traffic through the pool)
#   3. box -> gateway 3/3 (box-originated traffic, WAN source)
#   4. no new "out of ports" and no new "no translation" beyond a handful
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

note "== 部署新 probe + 测试脚本，武装看门狗 =="
scp -q $SSHO /tmp/nat-probe-linux "$BOX:/tmp/nat-probe-linux"
scp -q $SSHO "$HERE/nat-identity-test.sh" "$BOX:/tmp/nat-identity-test.sh"
run "$BOX" "install -m 0755 /tmp/nat-probe-linux /root/vmlab/nat-probe && install -m 0755 /tmp/nat-identity-test.sh /root/vmlab/nat-identity-test.sh && rm -f /root/vmlab/nat-bare-keep && systemd-run --unit nat-id-wd-\$\$ /root/vmlab/nat-bare-watchdog.sh" >/dev/null
sleep 1

note "== 武装 cust4 延迟 ping（20s 后开打，8 包） =="
run "$CUST" "rm -f /root/nat-identity-ping.txt; nohup bash -c 'sleep 20; ping -c8 -W2 $GW > /root/nat-identity-ping.txt 2>&1' >/dev/null 2>&1 & echo armed"

note "== 拉起 detached 测试 =="
run "$BOX" "systemd-run --unit nat-id-run-\$\$ /root/vmlab/nat-identity-test.sh" | grep -o 'Running as unit.*' || note "拉起失败"

note "== ssh 存活时间线（每 2s 一拍，共 100s） =="
: > "$RAW/ssh-timeline-identity-$STAMP.txt"
for i in $(seq 1 50); do
    ts=$(date +%H:%M:%S)
    if ssh -n $SSHO -o ConnectTimeout=3 "$BOX" true >/dev/null 2>&1; then
        echo "$ts ALIVE" | tee -a "$RAW/ssh-timeline-identity-$STAMP.txt"
    else
        echo "$ts dead" | tee -a "$RAW/ssh-timeline-identity-$STAMP.txt"
    fi
    sleep 2
done

note "== 取结果 =="
sleep 15
{
    echo "== cust4 ping =="
    run "$CUST" "cat /root/nat-identity-ping.txt 2>/dev/null || echo 'no log'"
    echo
    echo "== box log =="
    run "$BOX" "cat /root/vmlab/nat-identity-test.log 2>/dev/null || echo 'no log'"
} | tee "$RAW/identity-test-$STAMP.txt"
