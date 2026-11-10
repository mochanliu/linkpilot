#!/usr/bin/env bash
# boot-check.sh —— 「VM 真开机后是不是最优状态」的判据脚本（#270 的收口证据）。
#
# 为什么要真开机：前面所有臂（exp-270 的 A/B/C/F/G/F2/K/L/M/N）都是在跑着的盒子里
# 用 taskset 改运行时掩码量出来的。运行时钉到 cpu0 和开机就落在 cpu0 不是同一件事 ——
# 前者是我手动写的，后者才是 owner 要的"开机就是最优"。所以这一支只做一件事：
# 真的把 VM 关掉再打开，然后按同一口径把开机自己形成的状态量一遍。
#
# 脚本不改任何东西（除了它自己触发的这次开关机）。每一项判据都把实际值打出来，
# 不靠"应该生效了"。
#
# 用法：  BOX=CPE4 ./boot-check.sh            # 关机→开机→读回
#         BOX=CPE4 SKIP_POWER=1 ./boot-check.sh   # 只读回当前状态（不关不开）
set -u

BOX=${BOX:?要指定 BOX=CPE4}
IP=${IP:-}            # 管理 IP，缺省按盒子尾号推（CPE4 → 192.168.1.104）
BOOT_WAIT=${BOOT_WAIT:-180}
SKIP_POWER=${SKIP_POWER:-}

[ -n "$IP" ] || IP="192.168.1.10${BOX##*[^0-9]}"

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); printf '  PASS %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$1"; }
say()  { printf '%s\n' "$*"; }
b()    { ssh -o ConnectTimeout=6 "$BOX" "$@" 2>/dev/null; }

# ------------------------------------------------------------------ 0 关/开
# 关机走 ACPI 按钮（guest 自己 systemctl default，~3s，见 §VirtualBox 宿主事实）：
# 不用 controlvm poweroff（那是拔电，会跳过 umount/journal 收口，测出来的是"半开机"）。
# VBoxManage 7.2 没有 controlpoweroff 这个子命令；poweroff 是 controlvm 的参数。
# machinereadable 里关机后的状态字面是 "poweroff"（不是 "powered off"）。
if [ -z "$SKIP_POWER" ]; then
    say "关掉 $BOX（ACPI 按钮）"
    VBoxManage controlvm "$BOX" acpipowerbutton >/dev/null 2>&1
    s=running
    for _ in $(seq 1 60); do
        s=$(VBoxManage showvminfo "$BOX" --machinereadable 2>/dev/null | awk -F'=' '/^VMState=/{gsub("\"","",$2); print $2}')
        [ "$s" != "running" ] && break
        sleep 1
    done
    say "  关机后状态：$s（等 ${_}s）"
    [ "$s" = "poweroff" ] || say "  注意：状态不是 poweroff，下面的读回可能量的还是原来那份进程"
    sleep 2
    t0=$(date +%s)
    VBoxManage startvm "$BOX" --type headless >/dev/null 2>&1
    say "开机，等待 ssh 通（上限 ${BOOT_WAIT}s）"
    until ssh -o ConnectTimeout=3 "$BOX" true 2>/dev/null; do
        [ $(( $(date +%s) - t0 )) -gt "$BOOT_WAIT" ] && { say "  超时：$BOX 起不来"; exit 1; }
        sleep 2
    done
    say "  ssh 通：$(( $(date +%s) - t0 ))s（从下电到 guest 可登录）"
fi

# ------------------------------------------------------------------ 1 VPP 落点
say "1) VPP 主线程落点（#270 的正题）"
conf=$(b "grep -E '^[[:space:]]*cpu[[:space:]]*{' /root/vmlab/startup-vmlab.conf")
[ -n "$conf" ] && ok "启动配置里有 cpu 段：$(echo "$conf" | head -1)" || bad "启动配置里没有 cpu 段（开机就会碰运气）"
read -r pid core mask <<EOF2
$(b "p=\$(pgrep -x vpp_main); c=\$(vppctl show threads | awk '\$2==\"vpp_main\"{print \$7}' | head -1); m=\$(grep Cpus_allowed_list /proc/\$p/status | cut -d: -f2 | tr -d '[:space:]'); echo \$p \$c \$m")
EOF2
[ "${core:-x}" = "0" ] && ok "show threads 自报 Core=0（pid $pid）" || bad "show threads 自报 Core=${core:-空}，期望 0"
[ "${mask:-x}" = "0" ] && ok "Cpus_allowed_list=0" || bad "Cpus_allowed_list=${mask:-空}，期望 0"
aff=$(b "systemctl show vpp -p CPUAffinity --value")
[ -z "$aff" ] && ok "vpp.service 没有 CPUAffinity（钉核不是靠 systemd 覆盖的）" || bad "vpp.service 带 CPUAffinity=$aff（说明还在靠 systemd 兜）"
ts=$(b "ls /etc/systemd/system/vpp.service.d/ | tr '\n' ' '")
say "  drop-in：$ts"
say "  ExecStart：$(b "systemctl show vpp -p ExecStart --value" | head -c 160)"

# ------------------------------------------------------------------ 2 时延
say "2) 时延（同一口径三把尺子）"
# 取 avg 只认「min/avg」这一对小数，别的行一律当读不到。
# 上一版用 sed 's#.* = ##' + cut -d/ -f2，在 ping 全丢的那一晚把 "max" 当成了时延打出来
# （原始读回里那行「管理口 avg=maxms」就是它），数字坏了但判据恰好也判 FAIL —— 假对的读数比错更危险。
rtt=$(ping -c 30 -i 0.2 "$IP" 2>&1 | grep -oE '[0-9]+\.[0-9]+/[0-9]+\.[0-9]+/[0-9]+\.[0-9]+' | head -1 | cut -d/ -f2)
say "  主机→$IP 管理口 avg=${rtt:-无样本}ms（vCPU1 时代 5.3~7.3ms，钉 0 后 1.7~2.3ms）"
awk -v v="${rtt:-99}" 'BEGIN{exit !(v<4.0)}' && ok "管理口 ping <4ms" || bad "管理口 ping=${rtt:-无样本}ms，>=4ms 说明没落在好核上"
b "for i in \$(seq 1 12); do vppctl show sr-wg probe 2>/dev/null | grep -o 'rtt last=[0-9.]*' | head -1; sleep 1; done" \
    | awk -F= '{s+=$2;n++; if($2>m)m=$2} END{if(n)printf "  隧道内探针 avg=%.2fms max=%.2fms n=%d\n",s/n,m,n; else print "  隧道内探针：无样本"}'

# ------------------------------------------------------------------ 3 接管/编址
say "3) underlay 接管与编址（开机自愈）"
for i in host-enp0s8 host-enp0s9; do
    if b "vppctl show interface" | grep -q "$i"; then ok "$i 在 VPP 里"; else bad "$i 不在 VPP 里（接管单元没跑成）"; fi
done
b "vppctl show interface addr" | grep -E "host-enp0s(8|9)" | sed 's/^/    /'
mtus=$(b "vppctl show interface" | awk '/host-enp0s8|host-enp0s9/{printf "%s=%s ", $1, $4}')
say "  VPP 侧 MTU（L3/IP4/IP6/MPLS，接管时压回真实值）：$mtus"
k=$(b "ip -br addr show vpp0 | awk '{print \$3}'")
[ -n "$k" ] && ok "内核 vpp0 有地址：$k" || bad "内核 vpp0 没地址（管理通道没起来）"
b "ip route show default" | sed 's/^/    默认路由：/'

# ------------------------------------------------------------------ 4 守护进程与隧道
say "4) 守护进程与隧道（开机清单）"
for d in sr-agent sr-route sr-fib sr-frr; do
    p=$(b "pgrep -x $d | head -1")
    [ -n "$p" ] && ok "$d 在跑（pid $p）" || bad "$d 没在跑"
done
say "  watch 定时器：$(b "systemctl is-active sr-box-watch.timer")"
wg=$(b "vppctl show interface" | grep -cE '^wg[0-9]')
say "  盒里 wg 口：$wg 条"
[ "$wg" -gt 0 ] && ok "agent 重建了 wg 口（缺陷④ 的判据）" || bad "wg 口为 0：agent 没按盘上的配置重建隧道"
b "vppctl show bfd sessions" 2>/dev/null | awk '/Session state/{n++; if($3=="Up")u++} END{printf "    BFD 会话：%d 条，本端 Up %d 条\n", n+0, u+0}'

st=$(b "curl -s http://127.0.0.1:19011/status")
tun=$(echo "$st" | python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("tunnels") or []))' 2>/dev/null)
warn=$(echo "$st" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(len((d.get("last") or {}).get("warnings") or []))' 2>/dev/null)
orphan=$(echo "$st" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(len([w for w in ((d.get("last") or {}).get("warnings") or []) if "不在隧道表里" in w]))' 2>/dev/null)
disk=$(b "python3 -c 'import json;print(len(json.load(open(\"/etc/sdwan/fib.json\")).get(\"tunnels\") or []))'")
say "  sr-fib 隧道表=$tun 条、磁盘 fib.json=$disk 条、上次收敛 warnings=$warn 条（其中「不在隧道表里」$orphan 条）"
# 判据换过一版（#288）。旧判据只要求"表非空"，而那时候表是 lab 脚本 vm-fib-sync.sh 手工
# PUT 的 —— 于是「开机后产品里没人管这张表」这个缺口被脚本盖住，判据一直是绿的。
# 现在这张表由 sr-agent 每拍派生（不落盘），所以开机判据要同时看三条：
#   ① 非空；② 磁盘 fib.json 仍 0 条（非空只能是运行时推的，不是开机从盘上读的）；
#   ③ 没有「下一跳不在隧道表里」（表缺行的独有症状）。
if [ "${tun:-0}" -gt 0 ] && [ "${disk:-1}" = "0" ] && [ "${orphan:-1}" = "0" ]; then
    ok "sr-fib 隧道表 $tun 条，是 sr-agent 开机后派生的（磁盘 fib.json 仍 0 条、无缺行警告）"
elif [ "${disk:-0}" != "0" ]; then
    bad "sr-fib 隧道表 $tun 条，但磁盘 fib.json 也有 $disk 条 —— 分不清是开机读的还是 agent 推的（lab 又手工写盘了？）"
elif [ "${orphan:-0}" != "0" ]; then
    bad "sr-fib 隧道表缺行（$orphan 条「下一跳不在隧道表里」）—— 派生器没把表推全，overlay 装不出来"
else
    bad "sr-fib 隧道表=0（agent 派生器没推：看 /var/log/sr-box/agent.log 的「sr-fib 隧道表」行）"
fi

say ""
say "判据汇总：PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ] || say "（FAIL 非 0 不一定是 #270 的问题，逐条看上面的实际值）"
