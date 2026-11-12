#!/bin/bash
# run-e2e-boot.sh -- 重启后重放：VPP 起来是空的，boot 重放必须把 NAT 装回去，
# 而且装的过程中盒子不能失联（顺序仍然是 身份映射 → 池地址 → 特性）。
#
# 判据：VPP 重启 + boot 重放之后，ssh 全程存活、NAT 形状与 apply 时一致、
# cust4 仍然 8/8。
set -u -o pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
RAW=$HERE/raw
mkdir -p "$RAW"
STAMP=$(date +%Y%m%d-%H%M%S)
SSHO="-o BatchMode=yes -o ConnectTimeout=6 -o LogLevel=ERROR"
BOX=CPE4
CUST=cust4
GW=192.168.1.1
APPDIR=/root/vmlab/nat-e2e
BIN=/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/.build-nat/sr-apply-linux

run() { local h=$1 out rc; shift
    out=$(ssh -n $SSHO "$h" "$*" 2>&1 | tr -d '\r'); rc=$?
    printf '%s\n' "$out" | grep -v '^Warning: Permanently added'
    return $rc; }
note() { printf '.... %s\n' "$*"; }

note "== 换上新构建的 sr-apply =="
scp -q $SSHO "$BIN" "$BOX:$APPDIR/sr-apply.new"
run "$BOX" "install -m 0755 $APPDIR/sr-apply.new $APPDIR/sr-apply && echo installed"

note "== 重启 VPP（模拟断电/重启：NAT 状态全在内存里，会一起消失） =="
run "$BOX" "systemctl restart vpp && echo restarted"
for i in $(seq 1 30); do
    pairs=$(run "$BOX" "vppctl show lcp 2>/dev/null | grep -c '^itf-pair'" 2>/dev/null || echo 0)
    [ "$pairs" -ge 2 ] && break
    sleep 3
done
note "underlay pairs=$pairs"

note "== 武装 cust4 延迟 ping（15s 后开打） =="
run "$CUST" "rm -f /root/nat-boot-ping.txt; nohup bash -c 'sleep 15; ping -c8 -W2 $GW > /root/nat-boot-ping.txt 2>&1' >/dev/null 2>&1 & echo armed"

note "== boot 重放（detached，输出落盘） =="
run "$BOX" "rm -f $APPDIR/boot.out"
run "$BOX" "systemd-run --collect --unit nat-boot-\$\$ -p StandardOutput=file:$APPDIR/boot.out -p StandardError=file:$APPDIR/boot.out $APPDIR/sr-apply boot --root $APPDIR/store --vpp-api /run/vpp/api.sock --frr-conf $APPDIR/frr.conf --frr-reload true --route-conf $APPDIR/route.json --srfrr-conf $APPDIR/srfrr.json --no-route-push --no-srfrr-push" | grep -o 'Running as unit.*' || note "拉起失败"

note "== ssh 存活时间线（每 2s 一拍，共 60s） =="
: > "$RAW/ssh-timeline-boot-$STAMP.txt"
for i in $(seq 1 30); do
    ts=$(date +%H:%M:%S)
    if ssh -n $SSHO -o ConnectTimeout=3 "$BOX" true >/dev/null 2>&1; then
        echo "$ts ALIVE" | tee -a "$RAW/ssh-timeline-boot-$STAMP.txt"
    else
        echo "$ts dead" | tee -a "$RAW/ssh-timeline-boot-$STAMP.txt"
    fi
    sleep 2
done

{
    echo "== sr-apply boot 输出 =="
    run "$BOX" "cat $APPDIR/boot.out 2>/dev/null || echo 'no output'"
    echo
    echo "== cust4 ping =="
    run "$CUST" "cat /root/nat-boot-ping.txt 2>/dev/null || echo 'no log'"
    echo
    echo "== NAT 状态 =="
    run "$BOX" "vppctl show nat44 ei interfaces; vppctl show nat44 ei addresses | head -3; vppctl show nat44 ei static mappings; vppctl show nat44 ei sessions | head -6; ip -o -4 addr show dev vpp0 | tr -s ' ' | cut -d' ' -f4"
} | tee "$RAW/e2e-boot-$STAMP.txt"
