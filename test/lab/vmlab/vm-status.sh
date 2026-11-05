#!/bin/bash
# vm-status.sh - VM 研发台的只读快照（在 Mac 上跑，ssh 口径）。
#
# 与 vm-verify.sh 的分工：verify 判红绿并给退出码，status 只把数报出来。
# 两边用的是同一份台账（vm-ledger.py）、同一批取数口径、同一份证据行
# （vm-state.py），所以"体检说一致"和"快照里的数字"不会各说各话。
#
# 只读：不改任何配置、不重启任何进程、一条下发命令都没有（AGENTS.md §1）。
# 唯一的副作用是 ssh 与 curl 本身。
#
# 排障顺序（哪一层空了就断在哪一层）：
#   1 宿主 → 2 盒子/VPP → 3 隧道绑定与 BFD → 4 接管形状与内核
#   → 5 控制面（Mac）→ 6 每台 route/fib 与台账 → 7 VPP FIB
#   → 8 盒内 FRR → 9 客户机 → 10 计数器与摘路
#
# 用法：test/lab/vmlab/vm-status.sh [节点号...]   默认四台全打
# 台账缺失时本脚本自己调 vm-ledger.py 生成一份（只走控制台的 GET）。
# pipefail 是给 ssh_run 用的：里面的 `ssh … | tr -d '\r'` 要保证 ssh 的失败能传出来。
set -u -o pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
LEDGER=${VM_LEDGER:-/tmp/vm-verify-ledger.tsv}
WORKDIR=${VM_STATUS_WORK:-/tmp/vm-status-work}
mkdir -p "$WORKDIR"

VPN=0                    # 客户 VPN ID = VPP 表号
OVERLAY=250              # 隧道与 overlay 下一跳所在的表
LCP_IF=lcp-x0            # 客户口的内核 TAP
ROUTE_PORT=19001         # 盒子里 sr-route 的状态口（每台一个 loopback，端口不必错开）
FIB_PORT=19011           # 盒子里 sr-fib 的状态口
CTRL_ADDR=192.168.1.4    # 控制面在 Mac 上（S2）
CTRL_PORT=8444
CONSOLE_PORT=8088
RR_STATUS=( "" 19446 19447 )    # rr-a / rr-b 在 Mac 上的状态口（设备口是 18446/18447）
RR_TENANT=tenant-vm
FIB_LOG=/var/log/sr-box/fib.log
SSHO="-o BatchMode=yes -o ConnectTimeout=8 -o LogLevel=ERROR"

NODES=""
for a in "$@"; do
    case "$a" in
        [1-4]) NODES="$NODES $a" ;;
        *) echo "不认识参数：$a"; exit 2 ;;
    esac
done
[ -n "$NODES" ] || NODES="1 2 3 4"
NODES=$(echo "$NODES" | tr ' ' '\n' | awk 'NF' | sort -nu | tr '\n' ' ')

if [ ! -s "$LEDGER" ]; then
    echo "   台账不在（$LEDGER），从控制台现生成一份"
    "$HERE/vm-ledger.py" > "$LEDGER" || { echo "台账生成失败（控制台在跑吗？）"; exit 1; }
fi

# 空值一律打成"未上报"，不打 0、不留白：留白和"真的是 0"在终端里长得一样
# （AGENTS.md §2：控制台不造数据，脚本也一样）。
val() { local v; v=$(printf '%s' "$1" | tr -s ' ' ' ' | sed 's/^ *//;s/ *$//'); [ -n "$v" ] && printf '%s' "$v" || printf '未上报'; }

# -n 与 vm-verify.sh 同一条硬规则：循环体的 stdin 接的是台账/here-doc，ssh 不带 -n
# 会把剩下的行当自己的输入读走。
# known_hosts 那行 "Warning: Permanently added …" 不受 LogLevel 控制（实测 ERROR/quiet
# 都照打），它会混进 2>&1 把 awk 的字段错位（cust 的 lo 行首多出 '192.168.1.101'）；
# 这里按行滤掉它，但把 ssh 自己的退出码留着 return —— 报错仍然传得下去。
ssh_run() { local h=$1 out rc; shift
    out=$(ssh -n $SSHO "$h" "$*" 2>&1 | tr -d '\r'); rc=$?
    printf '%s\n' "$out" | grep -v '^Warning: Permanently added'
    return $rc; }
vppc()    { local h=$1; shift; ssh_run "$h" "vppctl $*"; }
# vtysh 走 ssh_run：外层引号给远端 shell 剥掉，远端看到的是 vtysh -c 'show …'。
vtysh()   { local h=$1; shift; ssh_run "$h" "vtysh $*"; }

lfld()  { awk -F'\t' -v t="$1" -v n="$2" -v i="$3" '$1 == t && $2 == n {print $i; exit}' "$LEDGER"; }
lrows() { awk -F'\t' -v t="$1" -v n="$2" '$1 == t && $2 == n' "$LEDGER"; }
box_loop()  { lfld box "$1" 4; }
box_cust()  { lfld box "$1" 5; }
box_peer()  { lfld box "$1" 6; }
box_proto() { lfld box "$1" 8; }
fib_block() { awk -v p="$1" '$0 == p { f = 1; next } f && /^[^[:space:]]/ { f = 0 } f' "$2"; }

hr() { echo "------------------------------------------------------------"; }

echo "VM 研发台快照 $(date '+%F %T %z') · 台账 $LEDGER（$(grep -c . "$LEDGER") 行）· 节点 $NODES"

hr
echo "宿主（Mac）"
# VBoxManage 每行是 `\"CPE1\" {uuid}`；awk -F'\"' 放进 \"$( )\" 里会被外层引号吃掉，
# 换 cut（2026-11-05 实测：awk 那版报 syntax error，整份 VM 清单漏进正文）。
vms=$(VBoxManage list runningvms 2>/dev/null | cut -d'"' -f2 | paste -sd' ' -)
echo "  $(uptime | sed 's/.*up /up /' )"
echo "  CPU 核数 $(sysctl -n hw.ncpu)。缺陷⑥（#262）的触发条件是宿主超订：BFD 50ms×3"
echo "  ⇒ 一次调度饥饿就被判非 Up，sr-fib 单样本摘路 ⇒ 客户侧 ≥1s 黑洞。"
echo "  在跑的虚拟机 $(val "$vms")"
echo "  本机 Docker 容器数：$(docker ps -q 2>/dev/null | wc -l | tr -d ' ')（colima 里跑着的容器台这里看不全）"

for n in $NODES; do
    h=CPE$n
    hr
    echo "盒子 $h（node $n，WAN $(val "$(lfld node "$n" 4)")）"
    ver=$(vppc "$h" show version)
    [ -n "$ver" ] || { echo "  $h: VPP 不可用（ssh 通吗 / vpp.service 在跑吗）"; continue; }
    echo "  VPP  $(val "$ver")"
    echo "  运行 $(val "$(ssh_run "$h" "uptime -p" | sed 's/^up //')")；盒内负载 $(val "$(ssh_run "$h" "cut -d' ' -f1-3 /proc/loadavg")")"
    # sr-box.service 与 sr-box-watch.service 都是 oneshot：跑完就 inactive(dead) 是设计，
    # 不是没起来。真正该盯的是 vpp=active、timer 在等下一次、看门狗最近一次退出码 0
    # （2026-11-05 实测：WatchdogSec 每分钟重跑 start.sh，日志里"已在跑（pid …）"）。
    echo "  常驻单元 vpp=$(val "$(ssh_run "$h" "systemctl is-active vpp")")，开机清单 sr-box=$(val "$(ssh_run "$h" "systemctl is-active sr-box")")（oneshot，dead 为正常），看门狗 timer=$(val "$(ssh_run "$h" "systemctl is-active sr-box-watch.timer")")"
    echo "  看门狗最近一次 退出码=$(val "$(ssh_run "$h" "systemctl show sr-box-watch.service -p ExecMainStatus" | cut -d= -f2)") @$(val "$(ssh_run "$h" "systemctl show sr-box-watch.service -p ExecMainExitTimestamp" | cut -d' ' -f2,3)")（盒子 UTC）；缺陷①（#257）留下的尾巴：unit stopped 后 daemon 仍在 cgroup 里"
    echo "  盒内进程 $(val "$(ssh_run "$h" "for d in sr-agent sr-route sr-fib sr-frr; do printf '%s=%s ' \$d \"\$(pidof \$d | awk '{print \$1}')\"; done")")"
    echo "  sr-wg 监听端口："
    vppc "$h" show sr-wg ports | sed 's/^/    /'
done

hr
echo "隧道绑定与 BFD"
for n in $NODES; do
    h=CPE$n
    echo "  $h（台账 $(val "$(lrows tun "$n" | wc -l | tr -d ' ')") 条隧道端点）："
    # 整份打出来：wg 口名只有盒子知道（按创建顺序自动命名），path_id/src_port/usable
    # 是绑定键，"N configured, M usable" 里 M<N 就说明有隧道没起来。
    vppc "$h" show sr-wg tunnels | sed 's/^/    /'
    # CLI 是 show wireguard peer（单数）。写成 peers 得到空输出，于是打出
    # "0 established / 0 total" —— 看着像事实的假数（2026-11-05 第一版栽在这里）。
    wg=$(vppc "$h" show wireguard peer)
    echo "    WG peers $(val "$(echo "$wg" | grep -c '^\[[0-9]*\] endpoint:')") 个，flags: 2（握手完成）$(val "$(echo "$wg" | grep -c 'flags: 2')") 个"
    echo "$wg" | grep -E '^\[[0-9]+\] endpoint:' |
        sed -E 's/^\[([0-9]+)\] endpoint:\[([^]]+)\] ([^ ]+).*flags: *([0-9]+)./      [\1] \2 -> \3 flags=\4/'
    b=$(vppc "$h" show bfd sessions)
    # 会话总数直接读 VPP 给的尾行 "Number of configured BFD sessions: N"；
    # vm-verify.sh 用 grep -c 'Hop Type'（每条会话印一次，实测两者相等），这里是同一个数
    # 的另一种取法，不改变口径。Detection Time 那行只有一个值 ⇒ 取 $NF（$3 是 "(usec)"）。
    echo "    BFD $(val "$(echo "$b" | sed -n 's/^Number of configured BFD sessions: //p')") 条多跳会话；本机+对端都 Up $(val "$(echo "$b" | grep -cE 'Session state +Up +Up')") 条；检测时间 $(val "$(echo "$b" | awk '/Detection Time/ {print $NF; exit}')")us（= 50ms×3，缺陷⑥ #262 的判定窗口）"
    echo "$b" | awk '/IPv4 address/{i=$1; l=$4; r=$5} /Session state/{printf "      [%s] %s<->%s 本机 %s 对端 %s\n", i, l, r, $3, $4}'
done

hr
echo "接管形状与内核（VPP 独占转发，FRR 跑在 $LCP_IF 上，盒内核不转发）"
for n in $NODES; do
    h=CPE$n
    cust=$(box_cust "$n")
    echo "  $h:"
    # show lcp 的头几行是全局开关，配对行以 itf-pair: 开头；旧版 grep 'lcp itf'
    # 里根本没有这个字串，于是恒报"0 组"（2026-11-05 实测）。
    lcp=$(vppc "$h" show lcp)
    echo "    linux-cp 配对 $(val "$(echo "$lcp" | grep -c 'itf-pair:')") 组；lcp-sync=$(val "$(echo "$lcp" | awk '$2=="lcp-sync"{print $3}')") auto-subint=$(val "$(echo "$lcp" | awk '$2=="lcp-auto-subint"{print $3}')")"
    echo "$lcp" | grep 'itf-pair:' | sed 's/^/      /'
    # ip -o link 的字段是 `2: enp0s8: <FLAGS> mtu 1544 qdisc …` ⇒ mtu 在 $5，
    # 旧版取 $6 打出的是 "qdisc"。但 1544 不是出生值：`set interface mtu packet 1500`
    # 会把 netdev 反写成 1544（underlay.sh:13、#254 实测），所以那格只作旁证；
    # 判定看 VPP 那格 MTU(L3/IP4/IP6/MPLS)，期望值取 underlay.list 第 6 列。
    link=$(ssh_run "$h" "ip -o link show enp0s8")
    vppmtu=$(vppc "$h" show interface host-enp0s8 | awk '$1=="host-enp0s8"{print $4}')
    want=$(ssh_run "$h" "awk -F'\t' -v i=enp0s8 '\$1==i{print \$6}' /root/vmlab/underlay.list" | tr -d '[:space:]')
    echo "    客户口 $(val "$(echo "$link" | grep -o '<[^>]*>' | head -1)")：VPP host-enp0s8 MTU(L3/IP4/IP6/MPLS) $(val "$vppmtu")，期望（underlay.list 第 6 列）$(val "$want")；内核 netdev $(val "$(echo "$link" | awk '{print $5}')")（这格是 VPP 接管时反写的，不参与判定）"
    echo "    $LCP_IF：$(val "$(ssh_run "$h" "ip -o -4 addr show dev $LCP_IF" | awk '{print $4}')")；arp_ignore=$(val "$(ssh_run "$h" "cat /proc/sys/net/ipv4/conf/$LCP_IF/arp_ignore")")（=1 才不让内核替 VPP 的地址抢答 ARP）"
    krt=$(ssh_run "$h" "ip -4 route")
    echo "    ip_forward=$(val "$(ssh_run "$h" "cat /proc/sys/net/ipv4/ip_forward")")；内核主表 172.16 前缀 $(val "$(echo "$krt" | grep -c '^172\.16\.')") 条，其中 sr-frr 注入的 lo 出口 $(val "$(echo "$krt" | grep -c '^172\.16\..*dev lo')") 条、客户协议学来的 $(val "$(echo "$krt" | grep '^172\.16\.' | grep -vc 'dev lo')") 条"
    echo "$krt" | grep '^172\.16\.' | sed 's/^/      /'
    echo "    内核默认路由：$(val "$(ssh_run "$h" "ip route show default" | head -1)")（VM 台与容器台的已知差别：管理通道 vpp0 留了一条给 ssh/心跳）"
done

hr
echo "控制面（Mac 上：S2 起没有容器）"
echo "  控制台   http://127.0.0.1:$CONSOLE_PORT   $(curl -sf -o /dev/null "http://127.0.0.1:$CONSOLE_PORT/" && echo 在答 || echo 没答)"
echo "  设备入口 $CTRL_ADDR:$CTRL_PORT   $(nc -z -w 2 "$CTRL_ADDR" "$CTRL_PORT" 2>/dev/null && echo 在听 || echo 不通)"
for r in 1 2; do
    if curl -sf "http://127.0.0.1:${RR_STATUS[$r]}/status" > "$WORKDIR/rr-$r.json" 2>/dev/null; then
        echo "  rr$r（状态口 ${RR_STATUS[$r]}）：$(python3 "$HERE/vm-state.py" rr "$WORKDIR/rr-$r.json" "$RR_TENANT" "$(grep -c '^node' "$LEDGER")" 2>&1)"
    else
        echo "  rr$r（状态口 ${RR_STATUS[$r]}）：未上报（进程没在跑？）"
    fi
done

hr
echo "每台：控制台回执 + sr-route/sr-fib 与台账对账（证据行与 vm-verify.sh 同一份）"
for n in $NODES; do
    h=CPE$n
    echo "  $h: 控制台 status=$(val "$(lfld node "$n" 5)")，隧道下发 v$(val "$(lfld node "$n" 9)")/ok=$(val "$(lfld node "$n" 10)")，$(val "$(lfld node "$n" 11)")"
    if ssh_run "$h" "curl -sf http://127.0.0.1:$ROUTE_PORT/status" > "$WORKDIR/route-$n.json"; then
        echo "    route：$(python3 "$HERE/vm-state.py" route "$WORKDIR/route-$n.json" "$LEDGER" "$n" 2>&1)"
    else
        echo "    route：未上报（:$ROUTE_PORT 没答）"
    fi
    if ssh_run "$h" "curl -sf http://127.0.0.1:$FIB_PORT/status" > "$WORKDIR/fib-$n.json"; then
        echo "    fib  ：$(python3 "$HERE/vm-state.py" fib "$WORKDIR/fib-$n.json" "$LEDGER" "$n" "$VPN" 2>&1)"
    else
        echo "    fib  ：未上报（:$FIB_PORT 没答）"
    fi
done

hr
echo "VPP FIB：table $VPN（客户口 + 远端 loopback）· table $OVERLAY（隧道/overlay 下一跳）"
for n in $NODES; do
    h=CPE$n
    lb=$(box_loop "$n")
    dump=$(vppc "$h" show ip fib table "$VPN")
    [ -n "$dump" ] || { echo "  $h: FIB 取不到"; continue; }
    printf '%s\n' "$dump" > "$WORKDIR/fibtable-$h"
    # 表 dump 里前缀行是"光条"的（`172.16.3.1/32` 后面没东西），旧版正则多要求一个
    # 空格 ⇒ 恒等于 0 条（2026-11-05 实测）。单条查询才会带 ` fib:0 index:…`。
    echo "  $h: table $VPN 有 172.16 前缀 $(val "$(grep -cE '^172\.16\.[0-9.]+/[0-9]+$' "$WORKDIR/fibtable-$h")") 条（台账远端 $(val "$(lrows rib "$n" | wc -l | tr -d ' ')") 条 + 本地 1 条）"
    while IFS=$'\t' read -r _ _ pfx nhs; do
        [ -n "$pfx" ] || continue
        blk=$(fib_block "$pfx" "$WORKDIR/fibtable-$h")
        [ -n "$blk" ] || { echo "      $pfx：不在 table $VPN"; continue; }
        # 只取顶层 dpo 的 via 行。"stacked-on entry:" 后面那条是 wg 口自己的 underlay
        # 递归解析，印出来会被当成第二条路径；旧版 awk '{print $2}' 更是直接打出
        # "mpls ipv4" 这类 token 垃圾。
        paths=$(echo "$blk" | awk '/stacked-on/{s=1; next} / via /{if(!s) print; s=0}' |
            sed -E 's/.*\]: (mpls|ipv4) via ([0-9.]+) ([A-Za-z0-9_.-]+):.*/\1 \2@\3/' |
            awk '!seen[$0]++' | paste -sd' ' -)
        lbl=$(echo "$blk" | grep -oE 'mpls-label\[@[0-9]+\]:\[[0-9]+' | grep -oE '[0-9]+$' | paste -sd, -)
        bkt=$(echo "$blk" | grep -oE 'buckets:[0-9]+' | cut -d: -f2 | paste -sd, -)
        echo "      $pfx 台账下一跳[$nhs]：buckets=$(val "$bkt") 路径[$(val "$paths")]"
        echo "          标签=$(val "$lbl") $(echo "$blk" | grep -q dpo-drop && echo '· ** dpo-drop（此路不通）**' || echo '· 可转发')"
    done <<EOF
$(lrows rib "$n")
EOF
    one=$(vppc "$h" show ip fib table "$VPN" "$lb/32")
    lp=$(echo "$one" | awk '/stacked-on/{s=1; next} / via /{if(!s) print; s=0}' |
         sed -E 's/.*\]: (mpls|ipv4) via ([0-9.]+) ([A-Za-z0-9_.-]+):.*/\1 \2@\3/' |
         awk '!seen[$0]++' | paste -sd' ' -)
    # 单条前缀的 dump 会把同一条路径在 path-list 与 forwarding 两段各印一遍，
    # 不去重就显示成两条一样的路径（2026-11-05 第一版）。
    echo "      本地 $lb/32：路径[$(val "$lp")] 标签$(echo "$one" | grep -q mpls-label && echo '（被压了标签，不该）' || echo '不打（交回客户机）')"
    ov=$(vppc "$h" show ip fib table "$OVERLAY")
    echo "      overlay 表 $OVERLAY：10.250/32 $(val "$(echo "$ov" | grep -cE '^10\.250\.')") 条，混入的客户前缀 $(val "$(echo "$ov" | grep -cE '^(172\.16\.|172\.31\.)')") 条；table 1250 $(vppc "$h" show ip fib table 1250 | grep -q fib_index && echo '还在（不该存在）' || echo '不存在')"
done

hr
echo "盒内 FRR（$LCP_IF 上跑客户侧协议；正向 = 内核 K 路由 redistribute）"
for n in $NODES; do
    h=CPE$n
    lb=$(box_loop "$n"); peer=$(box_peer "$n"); proto=$(box_proto "$n")
    rt=$(vtysh "$h" -c "'show ip route'")
    echo "  $h:"
    # 同一条 172.16 前缀上会同时摆着 K（sr-frr 注入，出口 lo）和客户协议学回来的那条；
    # 谁被选中看行首的 >。这是「防环规则待拍」（#262 之外那条）的现场数字，只报不判。
    echo "    172.16 候选路由（K=注入 / O,B=客户协议回灌，> 为选中）："
    echo "$rt" | grep -E '^[KOB][^[:space:]]*[[:space:]]+172\.16\.' | sed 's/^/      /'
    if [ "$proto" = ospf ]; then
        # 邻居行字段：ID Pri State Up-Time Dead Address Interface ⇒ 状态是 $3。
        # 旧版取 $2 打出来的是优先级 1，看着像"从没到 Full"。
        vtysh "$h" -c "'show ip ospf neighbor'" |
            awk '/^[0-9]+\./ {printf "    ospf: %s %s（%s，up %s）\n", $1, $3, $7, $4}'
    else
        # 邻居表底下还有一行 "Total number of neighbors N"，旧版 h&&NF 把它当邻居印了。
        vtysh "$h" -c "'show bgp summary'" |
            awk '/^[0-9]+\./ {printf "    bgp: %s AS %s 收到 %s 条 / 播 %s 条，up %s\n", $1, $3, $10, $11, $9}'
    fi
    echo "    正向闸门：$(vtysh "$h" -c "'show running-config'" | grep -q "redistribute kernel route-map SDWAN2CPE-vpn$VPN" && echo 'redistribute kernel + route-map SDWAN2CPE-vpn0 在' || echo '** 缺 redistribute kernel：正向导出没接上 **')"
    echo "    本站播出去的 $lb/32：$(val "$(vtysh "$h" -c "'show ip route $lb/32'" | grep 'Known via' | head -1 | sed 's/^ *//')")"
done

hr
echo "客户机（loopback 播客户 IP；比容器台少一台独立终端）"
seen=""
while IFS=$'\t' read -r _ nid serial lb cust peer cssh proto; do
    [ -n "$cssh" ] || continue
    case " $seen " in *" $cssh "*) continue ;; esac
    seen="$seen $cssh"
    mark=$([ "$proto" = ospf ] && echo O || echo B)
    crt=$(vtysh "$cssh" -c "'show ip route'")
    echo "  $cssh（node $nid 侧，播 $lb，协议 $proto）："
    echo "    lo：$(val "$(ssh_run "$cssh" "ip -o -4 addr show dev lo" | awk '{printf "%s ", $4}')")"
    if [ "$proto" = ospf ]; then
        # $3 才是 state（Full/DRother…），$2 是优先级；旧版取错过。
        echo "    邻居：$(val "$(vtysh "$cssh" -c "'show ip ospf neighbor'" | awk '/^[0-9]+\./ {printf "%s(%s@%s) ", $1, $3, $7}')")"
    else
        echo "    邻居：$(val "$(vtysh "$cssh" -c "'show bgp summary'" | awk '/^[0-9]+\./ {printf "%s(收%s条,up %s) ", $1, $10, $9}')")"
    fi
    # 只数 172.16 的 /32，否则那条协议学到的默认路由会被算进来。
    echo "    从 $proto 学到的远端前缀 $(val "$(echo "$crt" | grep -cE "^$mark>?\*?[[:space:]]+172\.16\.")") 条："
    echo "$crt" | grep -E "^$mark>?\*?[[:space:]]+172\.16\." | sed 's/^/      /'
    drt=$(echo "$crt" | grep -E '^[OBSCLDK][^[:space:]]*[[:space:]]+0\.0\.0\.0/0' | head -1)
    # 没有默认路由不是缺失：OSPF 那侧只播 /32 明细，客户机互 ping 走明细路由。
    echo "    默认路由：$([ -n "$drt" ] && printf '%s' "$drt" || printf '无（本站靠上面的 /32 明细互通）')"
done < <(grep '^box' "$LEDGER")

hr
echo "sr-wg 计数器与摘路（fib.log 的摘路条数 = 缺陷⑥的可观测面）"
for n in $NODES; do
    h=CPE$n
    st=$(vppc "$h" show sr-wg stats)
    # 异常计数按"值非 0"报，不按"有没有 fail 这个字"报：标签行恒在，旧版
    # grep -ciE 'fail|error|drop' 数的是行数（永远 4 条上下），毫无信息量。
    an=$(echo "$st" | awk 'NF==2 && $1 ~ /:$/ && $2+0>0 {k=$1; sub(/:$/,"",k);
              if (k!="ok" && k!="bypass") printf "%s=%s ", k, $2}')
    echo "  $h: Encap ok=$(val "$(echo "$st" | awk '/Encap:/{e=1} e&&/ok:/{print $2; exit}')")，Decap ok=$(val "$(echo "$st" | awk '/Decap:/{d=1} d&&/ok:/{print $2; exit}')")，非零异常计数 $([ -n "$an" ] && printf '[%s]' "$an" || printf '全为 0（no_path/hmac_fail/drop_total 等）')"
    echo "      bypass=$(val "$(echo "$st" | awk '/Encap:/{e=1} e&&/bypass:/{print $2; exit}')")（不是异常：不走 sr-wg 的包从这条口子过）"
    echo "      fib.log $(val "$(ssh_run "$h" "wc -l < $FIB_LOG 2>/dev/null" | tr -d ' ')") 行 / 摘路 $(val "$(ssh_run "$h" "grep -c 'BFD 会话不是 Up' $FIB_LOG 2>/dev/null")") 条；最近一条 $(val "$(ssh_run "$h" "grep 'BFD 会话不是 Up' $FIB_LOG 2>/dev/null | tail -1 | cut -c1-19")")（盒子本地 UTC，不与 Mac 时钟对齐）"
done
hr
echo "判红绿请用：test/lab/vmlab/vm-verify.sh（同一份台账、同一批证据行、有退出码）"
