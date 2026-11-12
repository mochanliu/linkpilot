#!/bin/bash
# VM lab 的"接管 + 搬地址 + 隧道层要的 VPP 全局"清单执行器（真机 vpp-tap-config 的同形状，一台跑一次、可反复跑）。
# 清单 /root/vmlab/underlay.list 每行一条，制表符分隔：物理口 <TAB> TAP <TAB> netdev真实MAC <TAB> CIDR <TAB> 网关(可空) <TAB> 真实MTU
# 任一口没达成就整体非 0 退出，并如实报是哪一步 —— 不重启 VPP、不清空已有配置。
set -u
LIST=${LIST:-/root/vmlab/underlay.list}
HERE=$(dirname "$0")
DLIMIT=${DLIMIT:-20}
DSTEP=${DSTEP:-0.5}

# af-packet 口在 VPP 里出生记 9000（内核 netdev 真实是 1500）。控制器 tunnelMTU() 按设备
# 上报的那格算隧道 MTU ⇒ 9000 会算出 8868，agent 又按真实链路预算硬拒装不下；所以这里把
# 清单第 6 列（VBox 网卡的出生 MTU，接管前量的，不能从活口读 —— VPP 会把 netdev 反写成 1544）
# 显式压回 VPP。
# 位置是硬要求：必须在搬地址之后、而且失败只置 rc 不 continue。管理地址是唯一一条能登录进去的
# 路 —— 这一步以前写在 takeover-if.sh 里，那里非 0 退出会让本脚本 `continue`，fresh 接管时
# 地址就再也落不到 TAP 上（那次 CPE2 失联的归因细节见 takeover-if.sh 头部注释）。
sync_mtu() {   # <物理口> <期望 MTU>
    local ifn=$1 want=$2 got
    vppctl set interface mtu packet "$want" "host-$ifn" >/dev/null 2>&1
    got=$(vppctl show interface "host-$ifn" 2>&1 | tr -d '\r' \
          | awk -v n="host-$ifn" '$1 == n {split($4, m, "/"); print m[1]}')
    if [ "$got" = "$want" ]; then
        echo "OK  MTU host-$ifn=$got（内核 $ifn=$(cat "/sys/class/net/$ifn/mtu" 2>/dev/null)，VPP 反写过所以不是 $want）"
        return 0
    fi
    echo "FAIL MTU host-$ifn=[$got] ≠ 清单期望 [$want]；地址已经搬好，但这条口上的隧道会按错 MTU 建"
    return 1
}
[ -f "$LIST" ] || { echo "FAIL 缺清单 $LIST"; exit 1; }
rc=0

# MPLS 的默认表（表号 0）在 VPP 里**不会自己存在**，必须显式建一次：
# src/vnet/mpls/interface.c 的 mpls_sw_interface_enable_disable() 头一步就是
# fib_table_find(FIB_PROTOCOL_MPLS, MPLS_FIB_DEFAULT_TABLE_ID)，找不到直接回
# VNET_API_ERROR_NO_SUCH_FIB（vnet/error.h 里 -3 = "No such FIB / VRF"）并且什么都不动；
# 上游给 `mpls table` 这条命令的说明写得很白：All Tables must be explicitly added before
# that can be used, **Including the default table**.
# 而 sr-agent 的 finishOverlay 对每条隧道无条件开 MPLS ⇒ 没这张表，隧道一次都装不上。
# 2026-11-04 CPE1 实测：agent.log 连报 3 次 `sw_interface_set_mpls_enable 失败，VPP retval=-3`，
# 第 4 次才收敛 —— 而那一次之所以能成，是我手工敲了 `mpls table add 0`
# （现在 `show mpls fib` 里那格 locks:[interface:2, CLI:1] 就是那次留下的，CLI 建的锁叫 CLI:1、
# API 建的叫 API:1，所以能分清是谁建的）。
# 容器台一直靠 lab 脚本手工补这一步（test/scripts/setup-node.sh:422、test/lab/loop-bringup.sh:286
# 的 P-3），产品侧没有：vppapi 没注册 mpls_table_add_del，sr-box 清单也不建表 —— 这条缺口单独报，
# 不在 lab 脚本里顺手改产品。
# 位置是硬要求：放在接管循环**之前**、而且跟着这条单元每次 VPP 起来都跑一遍（表随 VPP 进程消失）。
# sr-box 清单只 After=vpp.service、和本脚本一样是被 vpp.service 的 ExecStartPost 拉起来的，
# 谁先谁后没有保证；早一拍建表，隧道第一次 apply 就能装上，晚一拍只能靠 agent 的退避重试。
mpls_default_table() {
    local i ok=0
    for i in $(seq 1 40); do
        if vppctl show version >/dev/null 2>&1; then ok=1; break; fi
        sleep 0.5
    done
    if [ "$ok" != 1 ]; then
        echo "FAIL vppctl 20 秒不回话，MPLS 默认表没建（隧道的 MPLS 开不了）"
        return 1
    fi
    vppctl mpls table add 0 >/dev/null 2>&1      # 静默且幂等：已在也回 0
    if vppctl show mpls fib 2>&1 | tr -d '\r' | grep -q '^MPLS-VRF:0,'; then
        echo "OK  MPLS 默认表 0 在（隧道口开 MPLS 的前提）"
        return 0
    fi
    echo "FAIL MPLS 默认表 0 没建出来：隧道口开不了 MPLS，agent 会报 sw_interface_set_mpls_enable retval=-3"
    return 1
}
mpls_default_table || rc=1

while IFS= read -r LINE; do
    case "$LINE" in ''|\#*) continue;; esac
    # 不能写成 `IFS=$'\t' read -r IF TAP MAC CIDR GW MTU`：tab 属于 IFS 空白字符，
    # read 会把连续的 tab 合成一个分隔符 ⇒ 网关留空时第 6 列的 MTU 会挪去填 GW
    # （2026-11-04 CPE1 实测：underlay 打印出 "默认路由 via 1500"，MTU 那条静默没做）。
    # 所以空网关写死成 "-"，并在这里校验列数 —— 手改成 5 列的旧清单一律报错，不再错位。
    n=$(printf '%s' "$LINE" | awk -F'\t' '{print NF}')
    if [ "$n" != 6 ]; then
        echo "FAIL 清单该行是 $n 列，要 6 列（物理口/TAP/MAC/CIDR/网关或-/MTU）：$LINE"
        rc=1; continue
    fi
    IF=$(printf '%s' "$LINE" | cut -f1)
    TAP=$(printf '%s' "$LINE" | cut -f2)
    MAC=$(printf '%s' "$LINE" | cut -f3)
    CIDR=$(printf '%s' "$LINE" | cut -f4)
    GW=$(printf '%s' "$LINE" | cut -f5)
    MTU=$(printf '%s' "$LINE" | cut -f6)
    [ "$GW" = "-" ] && GW=''
    case "$MTU" in ''|*[!0-9]*) echo "FAIL $IF 的 MTU=[$MTU] 不是数字"; rc=1; continue;; esac

    if ! "$HERE/takeover-if.sh" "$IF" "$TAP" "$MAC"; then
        rc=1; continue
    fi

    # #302（2026-11-08、2026-11-12 两次现场复发）：内核 TSO/GSO/GRO 开着时，af_packet
    # 数据面的大包流会确定性卡死（scp/任何 bulk 全挂）。ethtool 的开关不进磁盘配置，
    # 盒子重启必回 on —— 开机接管时在这里顺手幂等关掉（非致命，不置 rc）。
    # 产品侧同款在 apply.ensureHostIface（接管路径同一位置）；VPP 升级后 af_packet
    # 若自带 offload 支持，两处要一起重估（handover offload 条目）。
    if ethtool -K "$IF" tso off gso off gro off >/dev/null 2>&1; then
        echo "OK  $IF offload 已关（tso/gso/gro，#302）"
    else
        echo "WARN $IF offload 关不掉（ethtool 缺席或口名不符）—— 大包流可能卡死，手工补 ethtool -K $IF tso off gso off gro off"
    fi

    # 等 TAP 在内核里出现，再把地址从物理口搬过去（顺序反了就是往不存在的口上加地址）
    i=0
    until ip link show "$TAP" >/dev/null 2>&1; do
        i=$((i + 1))
        if [ "$i" -gt "$DLIMIT" ]; then
            echo "FAIL $TAP 在 VPP 里配好了，但内核侧等 $DLIMIT 拍（约 $(awk -v a="$DLIMIT" -v b="$DSTEP" 'BEGIN{printf "%.1f", a*b}') 秒）没出现"
            rc=1; i=-1; break
        fi
        sleep "$DSTEP"
    done
    [ "$i" -lt 0 ] && continue

    ip addr flush dev "$IF" 2>/dev/null
    ip link set "$TAP" up
    ip addr replace "$CIDR" dev "$TAP"
    [ -n "$GW" ] && ip route replace default via "$GW" dev "$TAP" metric 20

    got=$(ip -o -4 addr show dev "$TAP" 2>/dev/null | grep -o "$CIDR" | head -1)
    left=$(ip -o -4 addr show dev "$IF" 2>/dev/null | grep -c 'scope global')
    # 默认路由也要读回：上面那条 `ip route replace` 在 2026-11-04 拿 "via 1500" 报过错，
    # 而这一行照样打了 OK —— 退出码不算验证，读回才算（AGENTS.md §2）。
    if [ -n "$GW" ]; then
        rt=$(ip -o route show default 2>/dev/null | grep -m1 "via $GW dev $TAP ")
    else
        rt="(无网关)"
    fi
    if [ "$got" != "$CIDR" ]; then
        echo "FAIL $TAP 上没有 $CIDR"; rc=1
    elif [ "$left" != "0" ]; then
        # networkd 手里那份内存配置会在线路事件上把地址补回物理口 ⇒ 双应答。磁盘 yaml 必须注释掉再
        # systemctl restart systemd-networkd，只改文件不算数（docs §5.5、§5.6(2)）
        echo "FAIL $IF 上还剩 $left 个全局地址：netplan 没注释掉或 networkd 没重启，会出现重复应答"; rc=1
    elif [ -z "$rt" ]; then
        echo "FAIL 默认路由没落到 via $GW dev $TAP"; rc=1
    else
        echo "OK  $IF → $TAP $CIDR${GW:+  默认路由 $rt}"
    fi

    # 放在最后：前面无论走到哪条 FAIL，管理地址都已经尽力搬过了，MTU 这一步不可能再把盒子拖黑
    sync_mtu "$IF" "$MTU" || rc=1
done < "$LIST"

vppctl show lcp 2>/dev/null | tr -d '\r' | grep '^itf-pair'
exit $rc
