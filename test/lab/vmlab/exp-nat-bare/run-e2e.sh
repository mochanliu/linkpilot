#!/bin/bash
# run-e2e.sh -- 产品路径的验收：sr-apply 自己按 bundle 里的 NAT 行把形状 G 装上。
#
# 前面的形状实验都是手工 vppctl + nat-probe 做的；这一步要证明的是**产品代码**
# （internal/apply/nat.go）能独立做对同一件事，而且删掉 NAT 行能干净退场。
#
# 判据（和形状实验同一套）：
#   1. ssh 全程存活（身份映射先于 outside 特性）
#   2. cust4 -> 网关 8/8，会话表里有 172.31.4.x
#   3. 池地址出现在 vpp0 与外口地址表上
#   4. v2（删掉 NAT 行）之后：NAT 特性/池地址/地址全没，盒子仍然可管理
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

run() { local h=$1 out rc; shift
    out=$(ssh -n $SSHO "$h" "$*" 2>&1 | tr -d '\r'); rc=$?
    printf '%s\n' "$out" | grep -v '^Warning: Permanently added'
    return $rc; }
note() { printf '.... %s\n' "$*"; }

note "== 预检：干净状态 =="
run "$BOX" "vppctl nat44 ei plugin disable >/dev/null 2>&1; vppctl set interface ip address del host-enp0s9 192.168.1.200/24 >/dev/null 2>&1; vppctl show nat44 ei interfaces | head -2; ip -o -4 addr show dev vpp0 | tr -s ' ' | cut -d' ' -f4"

note "== 部署 sr-apply + 两个 bundle，备份 svc.json =="
run "$BOX" "mkdir -p $APPDIR" >/dev/null
scp -q $SSHO /Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/.build-nat/sr-apply-linux "$BOX:$APPDIR/sr-apply.new"
scp -q $SSHO "$HERE/e2e/nat-e2e-v1.json" "$HERE/e2e/nat-e2e-v2.json" "$BOX:$APPDIR/"
run "$BOX" "install -m 0755 $APPDIR/sr-apply.new $APPDIR/sr-apply && [ -f /etc/sdwan/svc.json ] && cp -p /etc/sdwan/svc.json $APPDIR/svc.json.bak || true; rm -rf $APPDIR/store; mkdir -p $APPDIR/store; echo ok"

COMMON="--root $APPDIR/store --vpp-api /run/vpp/api.sock --frr-conf $APPDIR/frr.conf --frr-reload true --route-conf $APPDIR/route.json --srfrr-conf $APPDIR/srfrr.json --no-route-push --no-srfrr-push"

note "== 武装 cust4 延迟 ping（20s 后开打） =="
run "$CUST" "rm -f /root/nat-e2e-ping.txt; nohup bash -c 'sleep 20; ping -c8 -W2 $GW > /root/nat-e2e-ping.txt 2>&1' >/dev/null 2>&1 & echo armed"

note "== 第 1 步：apply v1（带 NAT 行）—— detached =="
run "$BOX" "rm -f $APPDIR/v1.out"   # 旧输出不能再被当成这一轮的结果
run "$BOX" "systemd-run --collect --unit nat-e2e-v1-\$\$ -p StandardOutput=file:$APPDIR/v1.out -p StandardError=file:$APPDIR/v1.out $APPDIR/sr-apply apply --bundle $APPDIR/nat-e2e-v1.json $COMMON" | grep -o 'Running as unit.*' || note "拉起失败"

note "== ssh 存活时间线（每 2s 一拍，共 80s） =="
: > "$RAW/ssh-timeline-e2e-$STAMP.txt"
for i in $(seq 1 40); do
    ts=$(date +%H:%M:%S)
    if ssh -n $SSHO -o ConnectTimeout=3 "$BOX" true >/dev/null 2>&1; then
        echo "$ts ALIVE" | tee -a "$RAW/ssh-timeline-e2e-$STAMP.txt"
    else
        echo "$ts dead" | tee -a "$RAW/ssh-timeline-e2e-$STAMP.txt"
    fi
    sleep 2
done

note "== v1 落地状态 =="
{
    echo "== sr-apply v1 输出 =="
    run "$BOX" "cat $APPDIR/v1.out 2>/dev/null || echo 'no output'"
    echo
    echo "== cust4 ping =="
    run "$CUST" "cat /root/nat-e2e-ping.txt 2>/dev/null || echo 'no log'"
    echo
    echo "== NAT 状态 =="
    run "$BOX" "vppctl show nat44 ei interfaces; vppctl show nat44 ei addresses; vppctl show nat44 ei static mappings; vppctl show nat44 ei sessions | head -8; ip -o -4 addr show dev vpp0 | tr -s ' ' | cut -d' ' -f4"
} | tee "$RAW/e2e-v1-$STAMP.txt"

note "== 第 2 步：apply v2（删掉 NAT 行）—— 退场路径 =="
run "$BOX" "$APPDIR/sr-apply apply --bundle $APPDIR/nat-e2e-v2.json $COMMON; echo apply-rc=\$?"
{
    echo "== sr-apply v2 输出（上） =="
    echo "== 退场后状态 =="
    run "$BOX" "vppctl show nat44 ei interfaces; vppctl show nat44 ei addresses; ip -o -4 addr show dev vpp0 | tr -s ' ' | cut -d' ' -f4; echo ---; systemctl is-active vpp vmlab-underlay"
} | tee "$RAW/e2e-v2-$STAMP.txt"

note "== 收尾：还原 svc.json =="
run "$BOX" "[ -f $APPDIR/svc.json.bak ] && cp -p $APPDIR/svc.json.bak /etc/sdwan/svc.json && echo restored || echo 'no backup'"
