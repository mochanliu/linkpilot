#!/bin/bash
# run-datapath.sh -- the decisive data-plane test through NAT, ssh-safe.
#
# The management path dies ~1-2s after NAT is enabled (measured), so the
# customer traffic has to be armed BEFORE and read AFTER:
#
#   t=0    Mac: baseline ping from cust4 (NAT off) + arm delayed pinger on cust4
#   t=3    Mac: launch nat-bare-probe.sh detached on CPE4 (enables NAT at ~t=3)
#   t=5    cust4: delayed ping fires (8 pings) -> through NAT
#   t=50   box probe: captures sessions/errors/counters, disables NAT
#   t=75   Mac: reads cust4's log + the box probe log
#
# Verdict:
#   sessions>0 + cust4 replies   -> data plane WORKS; the wedge is only
#                                   inbound-to-box (pool == mgmt address), i.e.
#                                   a config-shape consequence, not a crash
#   sessions==0 + no replies     -> in2out is dead too: real data-plane defect
set -u -o pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
RAW=$HERE/raw
mkdir -p "$RAW"
STAMP=$(date +%Y%m%d-%H%M%S)
SSHO="-o BatchMode=yes -o ConnectTimeout=8 -o LogLevel=ERROR"
BOX=CPE4
CUST=cust4
GW=192.168.1.1

run() { local h=$1 out rc; shift
    out=$(ssh -n $SSHO "$h" "$*" 2>&1 | tr -d '\r'); rc=$?
    printf '%s\n' "$out" | grep -v '^Warning: Permanently added'
    return $rc; }
vppc() { run "$BOX" "vppctl $*"; }
note() { printf '.... %s\n' "$*"; }

note "== 1) 基线：cust4 -> ${GW}（NAT 关，预期不通：网关没有 172.31.4.0/24 回程） =="
run "$CUST" "ping -c3 -W2 $GW 2>&1 | tail -2"

note "== 2) 在 cust4 上武装延迟 ping（12s 后开打，8 个包） =="
run "$CUST" "rm -f /root/nat-cust4-ping.txt; nohup bash -c 'sleep 12; ping -c8 -W2 ${GW} > /root/nat-cust4-ping.txt 2>&1' >/dev/null 2>&1 & echo armed"

note "== 3) 部署并拉起盒上探针（自己会关 NAT） =="
scp -q $SSHO "$HERE/nat-bare-probe.sh" "$BOX:/tmp/nat-bare-probe.sh"
run "$BOX" "install -m 0755 /tmp/nat-bare-probe.sh /root/vmlab/nat-bare-probe.sh && rm -f /root/vmlab/nat-bare-keep && systemd-run --unit nat-dp-probe-\$\$ /root/vmlab/nat-bare-probe.sh" || note "拉起失败"

note "== 4) 等客户流量窗口 + 探针收尾（75s，期间 ssh 会断，属预期） =="
sleep 75

note "== 5) 取结果 =="
{
    echo "== cust4 side =="
    run "$CUST" "cat /root/nat-cust4-ping.txt 2>/dev/null || echo 'no cust4 log'"
    echo
    echo "== box probe log =="
    run "$BOX" "cat /root/vmlab/nat-bare-probe.log 2>/dev/null || echo 'no box log'"
} | tee "$RAW/datapath-$STAMP.txt"

echo
echo "== 判定 =="
sess=$(vppc "show nat44 ei sessions" 2>/dev/null | grep -c 'i2o\|o2i' || true)
cust=$(run "$CUST" "grep -c 'ttl=' /root/nat-cust4-ping.txt 2>/dev/null || true")
echo "sessions_after_nat_off=$sess cust4_replies=$cust"
echo "（注意：探针结尾已关 NAT，sessions 计数看日志里的快照，不要看这一行）"
grep -E 'i2o|o2i|NAT44 sessions|packets transmitted|ttl=' "$RAW/datapath-$STAMP.txt" | head -20
