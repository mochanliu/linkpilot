#!/bin/bash
# Mac 侧驱动：给三台客户路由器装 FRR。
#
# 为什么需要这一份：cust12/cust3/cust4 各只有一块网卡，挂在 VirtualBox 的 internal
# network（lan12/lan3/lan4）上 —— 那块网络除了本端站点 LAN 不接任何东西，所以这三台
# 从出生起就没有 apt 源可访问（实测 `ping 192.168.1.4` 报 Network is unreachable）。
# 办法是**临时**插一块 NAT 网卡（槽 3）装完包再拔掉：lab 定案是客户路由器单网卡
# （docs/vmlab-rebuild-plan.md §3），临时出网不属于最终形状，所以这个脚本的最后一步
# 必须把槽 3 恢复成 none，并把 netplan 那份 DHCP 文件删掉。
#
# VirtualBox 7.2 不能给"当前是 disabled 的槽"热改 attachment（实测报
# "The NIC 3 is currently disabled and thus its attachment type can't be changed"），
# 所以插/拔都要停机改持久配置 ⇒ 每台两次开关机，时长在脚本里量出来并打印。
#
# 用法：cust-nat-install.sh <cust12|cust3|cust4> [--keep-nat]
#   --keep-nat：装完不拔掉那块 NAT 网卡（只在排查时用，交出来的 lab 不许带它）。
set -u
VM=${1:?cust12|cust3|cust4}
KEEP=${2:-}
VB=${VBOXMANAGE:-/Applications/VirtualBox.app/Contents/MacOS/VBoxManage}
REPO=/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan
SSH="ssh -o BatchMode=yes -o ConnectTimeout=6"
say() { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "FAIL $*"; exit 1; }
# 墙钟：每一步都记秒，交出去的时候要写"这台 VM 从关机到能 ssh 是多少秒"。
# macOS 的 date 没有 %N（`date +%s.%N` 出的是字面 "…​.N"，python 一转就 ValueError，
# cust12 第一次跑的所有时长因此丢了），所以这里用 python 取浮点秒。
now() { python3 -c 'import time;print("%.3f"%time.time())'; }
dur() { python3 -c 'import sys;print("%.2fs"%(float(sys.argv[2])-float(sys.argv[1])))' "$1" "$(now)"; }

# `--machinereadable` 里的字面值是 **poweroff**（2026-11-05 实测），和人读的
# `State: powered off` 不是一回事 —— 拿后者去比，ACPI 3 秒就关完了还要白等 60 秒，
# 然后走进硬停分支、被"虚拟机没在跑"顶回来（cust12 第一次跑就是这么挂的）。
state() { "$VB" showvminfo "$VM" --machinereadable 2>/dev/null | awk -F= '/^VMState=/{gsub(/"/,"",$2);print $2}'; }

poweroff_wait() {
    local t0=$(now) s
    say "  关机：$VM（acpipowerbutton）"
    "$VB" controlvm "$VM" acpipowerbutton > /dev/null 2>&1 || fail "$VM acpipowerbutton 失败"
    for i in $(seq 1 60); do
        s=$(state); [ "$s" = "poweroff" ] && { say "  已关机（$(dur "$t0")）"; return 0; }
        sleep 1
    done
    s=$(state)
    if [ "$s" != "poweroff" ]; then
        say "  ACPI 没在 60 秒内关掉它（现状态 $s），改用 poweroff 硬停"
        "$VB" controlvm "$VM" poweroff > /dev/null 2>&1 || fail "$VM poweroff 失败"
        for i in $(seq 1 20); do
            s=$(state); [ "$s" = "poweroff" ] && { say "  已关机（$(dur "$t0")）"; return 0; }
            sleep 1
        done
    fi
    fail "$VM 关不掉（状态 $(state)）"
}

start_wait() {
    local t0=$(now)
    "$VB" startvm "$VM" --type headless > /dev/null || fail "$VM 起不来"
    for i in $(seq 1 90); do
        $SSH "$VM" true > /dev/null 2>&1 && { say "  能 ssh 了（$(dur "$t0")）"; return 0; }
        sleep 2
    done
    fail "$VM 起来了但 180 秒内 ssh 不回来"
}

[ -x "$VB" ] || fail "找不到 VBoxManage：$VB"
# 入口允许两种状态：在跑（正常），或停在关机态（上一轮跑挂在中途，cust12 就是这样）。
# 关机就用和后面一样的姿势拉起来，保证重跑不靠手工动作。
if [ "$(state)" = "poweroff" ]; then
    say "  $VM 现在是关机态，先拉起来"
    start_wait
fi
$SSH "$VM" true > /dev/null 2>&1 || fail "$VM 现在 ssh 不回来，先别动它"

# ---- 1 停机插 NAT 网卡
poweroff_wait
"$VB" modifyvm "$VM" --nic3 nat --nictype3 virtio > /dev/null 2>&1 || fail "$VM modifyvm --nic3 nat 失败"
say "  槽 3 已改成 NAT（持久配置，稍后一起清掉）"
start_wait

# ---- 2 认出那块新网卡，只给它一份 DHCP 的 netplan
NEW=$($SSH "$VM" "ip -o -br link | awk '\$1!~\"^(lo|enp0s8)\$\" && \$1 ~ /^e/ {print \$1}' | head -1")
[ -n "$NEW" ] || { $SSH "$VM" "ip -o -br link"; fail "$VM 上找不到那块新网卡（上面是它现在的所有链路）"; }
NORMAC=$($SSH "$VM" "ip -o link show dev $NEW | grep -o 'link/ether [0-9a-f:]*' | awk '{print \$2}'")
say "  新网卡 = $NEW（MAC $NORMAC），给它 dhcp4"
$SSH "$VM" "cat > /etc/netplan/70-lab-nat.yaml <<EOF
network:
  version: 2
  ethernets:
    $NEW:
      match:
        macaddress: \"$NORMAC\"
      dhcp4: true
      dhcp6: false
EOF
chmod 600 /etc/netplan/70-lab-nat.yaml; netplan apply" || fail "$VM netplan 落盘失败"
for i in $(seq 1 30); do
    $SSH "$VM" "ip -4 -o addr show dev $NEW scope global" 2>/dev/null | grep -q inet && break
    sleep 1
done
$SSH "$VM" "ip -4 -o addr show dev $NEW scope global; ip route show default; grep -r nameserver /run/systemd/resolve/resolv.conf 2>/dev/null | head -1" \
    || fail "$VM 在新网卡上拿不到地址/默认路由"
# apt 只走这块网卡：把源先更新一遍，装了包再一起清理。
$SSH "$VM" "getent hosts deb.frrouting.org || echo 'WARN DNS 没解析出 deb.frrouting.org'"

# ---- 3 推装包脚本并跑（在 VM 上跑，不在这台 shell 的前台跑长命令）
scp -o BatchMode=yes -q "$REPO"/test/lab/vmlab/cust-frr.sh "$VM":/root/ || fail "推 cust-frr.sh 失败"
t0=$(now)
$SSH "$VM" "HOST_LABEL=$VM bash /root/cust-frr.sh" || fail "$VM 装 FRR 失败（上面是最后 25 行 apt 日志）"
say "  装完（$(dur "$t0")）"

# ---- 4 拔掉临时网卡，回到"单网卡"的定案形状
if [ "$KEEP" = --keep-nat ]; then
    say "  --keep-nat：留着那块 NAT 网卡（这不是交出去的形状）"
    exit 0
fi
poweroff_wait
"$VB" modifyvm "$VM" --nic3 none > /dev/null 2>&1 || fail "$VM 恢复 --nic3 none 失败"
say "  槽 3 已恢复 none"
start_wait
$SSH "$VM" "rm -f /etc/netplan/70-lab-nat.yaml && netplan apply" || fail "$VM 删掉临时 netplan 失败"
say "  临时 netplan 已删"

# ---- 5 收尾复核：形状必须回到"只有一块 enp0s8 + 它的静态地址 + 没有默认路由"
$SSH "$VM" "ip -o -br link | grep -v '^lo'; echo '--- addr ---'; ip -o -4 addr show scope global | awk '{print \$2,\$4}'; echo '--- default ---'; ip route show default | wc -l; echo '--- frr ---'; dpkg -l | awk '/^ii  frr/{printf \"%s=%s\n\",\$2,\$3}'; pgrep -x zebra >/dev/null && echo 'zebra 在跑' || echo 'FAIL zebra 没跑'" \
    || fail "$VM 收尾复核跑不动"
say "OK $VM 装完 FRR 并回到单网卡形状"
