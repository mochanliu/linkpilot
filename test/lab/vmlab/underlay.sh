#!/bin/bash
# VM lab 的"接管 + 搬地址"清单执行器（真机 vpp-tap-config 的同形状，一台跑一次、可反复跑）。
# 清单 /root/vmlab/underlay.list 每行一条，制表符分隔：物理口 <TAB> TAP <TAB> netdev真实MAC <TAB> CIDR <TAB> 网关(可空)
# 任一口没达成就整体非 0 退出，并如实报是哪一步 —— 不重启 VPP、不清空已有配置。
set -u
LIST=${LIST:-/root/vmlab/underlay.list}
HERE=$(dirname "$0")
DLIMIT=${DLIMIT:-20}
DSTEP=${DSTEP:-0.5}
[ -f "$LIST" ] || { echo "FAIL 缺清单 $LIST"; exit 1; }
rc=0

while IFS=$'\t' read -r IF TAP MAC CIDR GW; do
    [ -n "${IF:-}" ] || continue
    case "$IF" in \#*) continue;; esac

    if ! "$HERE/takeover-if.sh" "$IF" "$TAP" "$MAC"; then
        rc=1; continue
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
    [ -n "${GW:-}" ] && ip route replace default via "$GW" dev "$TAP" metric 20

    got=$(ip -o -4 addr show dev "$TAP" 2>/dev/null | grep -o "$CIDR" | head -1)
    left=$(ip -o -4 addr show dev "$IF" 2>/dev/null | grep -c 'scope global')
    if [ "$got" != "$CIDR" ]; then
        echo "FAIL $TAP 上没有 $CIDR"; rc=1
    elif [ "$left" != "0" ]; then
        # networkd 手里那份内存配置会在线路事件上把地址补回物理口 ⇒ 双应答。磁盘 yaml 必须注释掉再
        # systemctl restart systemd-networkd，只改文件不算数（docs §5.5、§5.6(2)）
        echo "FAIL $IF 上还剩 $left 个全局地址：netplan 没注释掉或 networkd 没重启，会出现重复应答"; rc=1
    else
        echo "OK  $IF → $TAP $CIDR${GW:+ 默认路由 via $GW metric 20}"
    fi
done < "$LIST"

vppctl show lcp 2>/dev/null | tr -d '\r' | grep '^itf-pair'
exit $rc
