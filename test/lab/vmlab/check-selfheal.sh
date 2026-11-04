#!/bin/bash
# S3 的形状校验器：判据①②③ 都用这同一口径出"前后对比"，不靠退出码。
# 每行一个判据，PASS/FAIL 写死；任何一条 FAIL 就整体 FAIL（AGENTS.md §2：没有前后两组数字的"应该没问题"不算验证）。
# 用法：check-selfheal.sh <物理口> <该口 CIDR> <TAP 名> <对端 IP> [标签]
set -u
IF=${1:?物理口}
IPCIDR=${2:?该口 CIDR}
TAP=${3:?TAP 名}
PEER=${4:?对端 IP}
TAG=${5:-check}
IP=${IPCIDR%/*}
vc() { vppctl "$@" 2>&1 | tr -d '\r'; }
FAILS=0
ok()   { printf '  PASS %s\n' "$1"; }
bad()  { printf '  FAIL %s\n' "$1"; FAILS=$((FAILS+1)); }
say()  { printf '%s %s\n' "$(date +%s.%3N)" "$*"; }

KMAC=$(ip -o link show dev "$IF" 2>/dev/null | grep -o 'link/ether [0-9a-f:]*' | cut -d' ' -f2)
VMAC=$(vc show hardware-interfaces "host-$IF" 2>/dev/null | grep -i 'Ethernet address' | awk '{print $3}')
# VPP 侧的 tap 名是 tap4096 这种（`show lcp` 第 4 段），内核那边才叫 $TAP（第 5 段）
VTAP=$(vc show lcp 2>/dev/null | grep -m1 "host-$IF" | awk '{print $4}')
TMAX=$(vc show hardware-interfaces "$VTAP" 2>/dev/null | grep -i 'Ethernet address' | awk '{print $3}')
KMAX=$(ip -o link show dev "$TAP" 2>/dev/null | grep -o 'link/ether [0-9a-f:]*' | cut -d' ' -f2)

say "=== 形状校验 [$TAG] IF=$IF TAP=$TAP IP=$IPCIDR PEER=$PEER ==="
echo "C1 三处 MAC 一致（内核 netdev / VPP host-if / 内核侧 tap 设备）"
if [ -n "$KMAC" ] && [ "$KMAC" = "$VMAC" ] && [ "$KMAC" = "$KMAX" ]; then
    ok "三处全等 $KMAC（内核 $IF / VPP host-$IF / 内核 $TAP）；VPP 的 tap 对象 MAC=$TMAX（出生随机，S3-j 实测不参与 L3 判定）"
else
    bad "内核 $IF=[$KMAC] VPP host-$IF=[$VMAC] 内核 $TAP=[$KMAX]（VPP tap 对象=[$TMAX]）"
fi

echo "C2 地址归属：$IF 无全局地址、$TAP 有 $IPCIDR"
KA=$(ip -o -4 addr show dev "$IF" 2>/dev/null | awk '{printf "%s ",$4}')
TA=$(ip -o -4 addr show dev "$TAP" 2>/dev/null | awk '{printf "%s ",$4}')
[ -z "$KA" ] && ok "$IF 上没有全局地址（真机 iface0 同形状）" || bad "$IF 上还有地址 [$KA] ⇒ networkd 在抢"
echo "$TA" | grep -q "$IPCIDR" && ok "$TAP 上有 $IPCIDR" || bad "$TAP 上是 [$TA]，缺 $IPCIDR"
# 生成物（/run/systemd/network/10-netplan-<口>.network）是 networkd 真正读的那份，
# 只改磁盘 yaml + restart networkd 不会刷新它。这里只报数不判 FAIL：
# 收口它属于阶段 D（vm-bringup.sh 的 D0 是硬闸门），C 阶段跑校验时物理口的地址本来就还在。
GEN=$(grep -c '^Address=' "/run/systemd/network/10-netplan-$IF.network" 2>/dev/null || echo 0)
echo "       生成物 10-netplan-$IF.network 里 Address= $GEN 条（阶段 D 收口后应为 0）"

echo "C3 LCP 配对在位"
LCP=$(vc show lcp | grep -m1 "host-$IF" | tr -s ' ' | cut -d' ' -f3-5)
[ -n "$LCP" ] && ok "itf-pair: $LCP" || bad "show lcp 里没有 host-$IF"

echo "C4 VPP 收得进本机 IP（判据① 口径：dpo-receive + 计数在涨）"
fibc() { vc show ip fib | grep -A3 -E "^${IP}/32$" | grep -m1 -oE 'to:\[[0-9]+:[0-9]+\]'; }
cnt() { echo "$1" | sed -E 's/to:\[([0-9]+):([0-9]+)\]/\1 \2/'; }
F1=$(fibc); N1=$(vc show errors | grep -m1 'ip4-icmp-input.*echo replies sent' | awk '{print $1+0}')
ping -c 10 -i 0.2 -W1 -I "$TAP" "$PEER" >/dev/null 2>&1
F2=$(fibc); N2=$(vc show errors | grep -m1 'ip4-icmp-input.*echo replies sent' | awk '{print $1+0}')
if [ -n "$F1" ] && [ -n "$F2" ]; then
    set -- $(cnt "$F2"); x=$1; y=$2
    set -- $(cnt "$F1"); u=$1; v=$2
    if [ "$((x+y))" -gt "$((u+v))" ]; then ok "$F1 → $F2（VPP 对本机 IP 的收包计数在涨 ⇒ 包真是 VPP 收的）"
    else bad "$F1 → $F2（计数没动：本机流量没走 VPP）"; fi
    vc show ip fib | grep -A3 -E "^${IP}/32$" | grep -m1 'dpo-receive' | sed 's/^/       /'
else
    bad "fib 里找不到 $IP/32 的 to:[…]（本机 IP 没进 VPP 的 fib？）"
fi
echo "       本端发出的 icmp 应答计数 $N1 → $N2（只有对端 ping 本机时才涨，做旁证用）"

echo "C5 功能：ping -I $TAP $PEER"
P=$(ping -c 20 -i 0.2 -W1 -I "$TAP" "$PEER" 2>&1)
LOSS=$(echo "$P" | grep -oE '[0-9]+% packet loss')
RTT=$(echo "$P" | tail -1 | sed 's/.*= //')
NEI=$(ip neigh show dev "$TAP" | tr '\n' ';')
if [ "$LOSS" = "0% packet loss" ]; then ok "0% loss，rtt $RTT"; else bad "$LOSS（rtt $RTT）"; fi
echo "       neigh[$TAP]=[$NEI]"

echo "C6 没有残留的 L2 不一致"
MM=$(vc show errors | grep -m1 'l3 mac mismatch' | awk '{print $1+0}')
vc clear errors >/dev/null 2>&1
sleep 3
MM2=$(vc show errors | grep -m1 'l3 mac mismatch' | awk '{print $1+0}')
if [ -z "$MM2" ] || [ "${MM2:-0}" = "0" ]; then ok "清零后 3 秒空闲：mism 无新增（此前累计 ${MM:-0}）"
else bad "3 秒内新增 ${MM2} 条 l3 mac mismatch"; fi

echo "C7 自愈单元在位"
E1=$(systemctl is-enabled vpp.service 2>&1)
[ "$E1" = enabled ] && ok "vpp.service enabled" || bad "vpp.service = $E1"
# S4-E 起：接管和搬地址都收进一个常驻单元 vmlab-underlay.service（由 vpp.service 的 ExecStartPost
# 每次 VPP 起来后拉起）。旧的两个 vmlab-tap-<TAP>.service 已退役，接管也不许再写进 VPP 的
# unix{exec} —— 那样一次 restart 会稳定丢掉第一条接管的创建（docs §5.6(3) 实测）。
EU=$(systemctl is-enabled vmlab-underlay.service 2>&1)
[ "$EU" = enabled ] && ok "vmlab-underlay.service enabled" || bad "vmlab-underlay.service = $EU"
[ -x /root/vmlab/underlay.sh ] && ok "underlay.sh 在位可执行" || bad "underlay.sh 缺失或不可执行"
# 读「VPP 实际在跑的那份 startup」，不硬编码文件名：ExecStart 由 50-vmlab.conf 覆盖成
# -c /root/vmlab/startup-vmlab.conf，而盒子里还躺着 S3 时代的 startup-s3.conf 等死文件 ——
# 照死名字 grep，以后多一份遗留文件就多一处「读的不是它在跑的」。
VPPCONF=$(systemctl cat vpp.service 2>/dev/null | grep -oE '^ExecStart=.* -c [^ ]+' | tail -1 | awk '{print $NF}')
if [ -z "$VPPCONF" ] || [ ! -f "$VPPCONF" ]; then
    bad "取不到 VPP 实际用的 startup 配置（ExecStart 里 -c =[$VPPCONF]）"
elif grep -qE '^[[:space:]]*exec ' "$VPPCONF"; then
    bad "$VPPCONF 里还有 exec 行：接管不该写在 VPP 启动配置里"
else
    ok "$VPPCONF 里没有 exec 行"
fi
if systemctl cat vpp.service 2>/dev/null | grep -q 'ExecStartPost=+systemctl --no-block restart vmlab-underlay.service'; then
    ok "vpp.service 的 ExecStartPost 会触发接管"
else
    bad "vpp.service 缺 ExecStartPost：VPP 重启后没人补接管"
fi
FRAG=$(systemctl show -p FragmentPath --value vpp.service 2>/dev/null)
PID=$(pidof vpp || echo 无)
echo "       vpp pid=$PID  常驻单元文件=$FRAG"
if [ "$FAILS" = 0 ]; then V="全部 PASS"; else V="$FAILS 条 FAIL"; fi
say "=== 结论：$V ==="
[ "$FAILS" = 0 ]
