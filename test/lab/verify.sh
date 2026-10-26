#!/bin/bash
# verify.sh - 对 bringup.sh 搭好的三站点 lab 做端到端体检（宿主机执行）
#
# 拓扑要点：每个站点是一台"盒子"= lab-cpeN（VPP 数据面 + 盒内 FRR +
# sr-route/sr-fib/sr-frr），盒子外接一台客户路由器 lab-custN，再往外是
# lab-termN。盒子里的分工是：FRR 只算路由（BGP 跑在 linux-cp 送出的内核 TAP
# lcp-x0 上），VPP 转发**全部**数据（WAN 口与客户口都在 VPP 里，盒内核不转发）。
# 所以报文路径是：
#   终端 → 客户路由器 → VPP 客户口 → （远端：SR/WG + MPLS 标签 1000）→ 对端
#   VPP 客户口 → 对端客户路由器 → 对端终端
#
# 再单独验证三条联动路径：
#   正向：sr-route RIB → sr-frr → FRR table 1250 → BGP → 客户路由器
#   反向：客户 BGP（tag 1001）→ 盒内 FRR → sr-frr → sr-route local
#   数据面接缝：FRR 主表 → zebra 装内核 → linux-nl → VPP table 0（本地前缀
#   交给客户路由器）；远端前缀由 sr-fib 直接装 VPP 并压标签
#
# 只读：不改任何配置，不重启任何进程。失败项逐条打印，最后给总数。
#
# 用法: test/lab/verify.sh [--quiet]
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
VPN=0            # 客户 VPN ID，直接当 VPP 表号（0 = global）
OVERLAY=250      # 隧道与 overlay 下一跳所在的表，不是 VPN
LABEL=$((1000 + VPN))
TUNNEL_MTU=1360
FRR_TABLE=1250
BGP_TAG=1001
ROUTE_STATUS=( "" 19001 19002 19003 )
WAN_IP=( "" 10.88.1.11 10.88.1.12 10.88.1.13 )
LCP_IF=lcp-x0
XCONN_IP=( "" 10.79.1.2 10.79.2.2 10.79.3.2 )
CUST_X_IP=( "" 10.79.1.3 10.79.2.3 10.79.3.3 )
LAN_PFX=( "" 10.68.1.0/24 10.68.2.0/24 10.68.3.0/24 )
TERM_IP=( "" 10.68.1.10 10.68.2.10 10.68.3.10 )
LOOP_IP=( "" 172.16.1.1 172.16.2.1 172.16.3.1 )
BOX_AS=( "" 65101 65102 65103 )
CUST_AS=( "" 65001 65002 65003 )
RR_STATUS=18447
CTRL_ADDR=10.99.0.200
CTRL_PORT=8444

# 每个 CPE 的 WG 接口：站点 -> "接口:端口:overlay"（顺序无关）
CPE_WG=(
  ""
  "wg0:51820:192.168.99.1 wg1:51840:192.168.100.1"
  "wg0:51820:192.168.99.2 wg1:51860:192.168.101.1"
  "wg0:51840:192.168.100.2 wg1:51860:192.168.101.2"
)
CPE_PEERS=( "" 2 2 2 )
CPE_PATHS=( "" "1,2" "3,4" "5,6" )

QUIET=0
[ "${1:-}" = "--quiet" ] && QUIET=1

PASS=0 FAIL=0

ok()  { PASS=$((PASS + 1)); printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad() { FAIL=$((FAIL + 1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
sect(){ echo; echo "== $*"; }
note(){ [ "$QUIET" = "1" ] || printf '  .... %s\n' "$*"; }

vppc(){ local c=$1; shift; docker exec "$c" vppctl "$@" 2>&1 | tr -d '\r'; }
vtysh(){ local c=$1; shift; docker exec "$c" vtysh "$@" 2>&1 | tr -d '\r'; }
peer_up() {   # <cpe> <want>
    local n
    n=$(vppc "$1" show wireguard peer | grep -c 'flags: 2')
    [ "$n" -ge "$2" ]
}
vpp_if_for_ip() {   # <cpe> <ip> -> VPP 里持有该地址的接口名
    vppc "$1" show interface addr |
        awk -v want="$2" '
            /:[[:space:]]*$/ { n=$1; next }
            index($0, "L3 " want "/") { print n; exit }'
}
kern_ipfwd() {   # <cpe> -> Ip: Forwarding（MIB: 1=forwarding, 2=notForwarding）
    docker exec "$1" awk '/^Ip:/{n++; if(n==2){print $2; exit}}' /proc/net/snmp 2>/dev/null || echo NA
}

# 反复执行直到成功（收敛类断言用）
retry() {   # <秒> <命令...>
    local limit=$1 i=0; shift
    while [ "$i" -lt "$((limit * 2))" ]; do
        "$@" >/dev/null 2>&1 && return 0
        sleep 0.5; i=$((i + 1))
    done
    "$@" >/dev/null 2>&1
}

# 在容器里用 python3 断言 /status（Go 的 JSON 是紧凑格式，用 JSON 解析而不是 grep）
# want_rib 格式："prefix:nh,prefix:nh"；want_local 是逗号分隔的 prefix。
status_check() {   # <cpe> <port> <want_rib> <want_local>
    local c=$1 port=$2 want=$3 wantl=$4
    docker exec "$c" python3 - "$port" "$VPN" "$want" "$wantl" <<'PY' >/dev/null 2>&1
import json, sys, urllib.request
port, vrf, want, wantl = sys.argv[1:5]
d = json.load(urllib.request.urlopen("http://127.0.0.1:%s/status" % port, timeout=3))
if d.get("active_rr") != 0:
    raise SystemExit("active_rr=%r" % d.get("active_rr"))
got = sorted("%s:%s" % (r["prefix"], ",".join(sorted(r["next_hops"])))
             for r in d.get("rib", {}).get(vrf, []))
if got != sorted(x for x in want.split(",") if x):
    raise SystemExit("rib=%r want=%r" % (got, want))
gotl = sorted(x["prefix"] for x in (d.get("local") or []))
if gotl != sorted(x for x in wantl.split(",") if x):
    raise SystemExit("local=%r want=%r" % (gotl, wantl))
PY
}

# 站点 N 上"另一个站点 M"的期望 RIB 行：<prefix>:<对端 overlay>
peer_overlay() {   # <a> <b>  -> b 在 a 侧的 overlay
    case "$1-$2" in
      1-2) echo 192.168.99.2 ;;   2-1) echo 192.168.99.1 ;;
      1-3) echo 192.168.100.2 ;;  3-1) echo 192.168.100.1 ;;
      2-3) echo 192.168.101.2 ;;  3-2) echo 192.168.101.1 ;;
    esac
}

ping4() { docker exec "$1" ping -c 3 -W 2 -i 0.3 "$2" >/dev/null 2>&1; }
# vppctl ping 默认查 table 0，overlay 路由在 table 250，不发 `source wgN` 会直接
# 报 "no egress interface" 并以 "0 sent, 0 received, 0% packet loss" 收尾——只 grep
# 丢包率会把没发出去的包当成通。必须同时校验发包数。
overlay_ping() {   # <容器> <目标 overlay> <出接口>
    vppc "$1" ping "$2" source "$3" repeat 3 interval 0.2 | grep -Eq '(^|[^0-9])3 sent, 3 received'
}

echo "===== 1. 容器与 VPP ====="
for c in lab-cpe1 lab-cpe2 lab-cpe3 lab-cust1 lab-cust2 lab-cust3 \
         lab-transit lab-rr lab-ctrl lab-term1 lab-term2 lab-term3; do
    docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null | grep -q true && ok "$c 运行中" || bad "$c 没运行"
done
for c in lab-cpe1 lab-cpe2 lab-cpe3 lab-transit; do
    vppc "$c" show version | grep -q "vpp v" && ok "$c: VPP 在运行" || bad "$c: VPP 不可用"
    vppc "$c" show plugins | grep -q sr_wg_plugin.so && ok "$c: sr_wg 插件已加载" || bad "$c: sr_wg 插件缺失"
done
# 这两个插件是"FRR 算路由、VPP 转发"的接缝；没加载时 show lcp 直接不存在。
for c in lab-cpe1 lab-cpe2 lab-cpe3; do
    for p in linux_cp_plugin.so linux_nl_plugin.so; do
        vppc "$c" show plugins | grep -q "$p" && ok "$c: $p 已加载" || bad "$c: $p 未加载"
    done
done

sect "2. 盒子接缝（linux-cp：FRR 跑在内核 TAP，VPP 独占转发）"
for n in 1 2 3; do
    c=lab-cpe$n
    addr=$(vppc "$c" show interface addr)
    wan=$(vpp_if_for_ip "$c" "${WAN_IP[$n]}")
    xc=$(vpp_if_for_ip "$c" "${XCONN_IP[$n]}")
    if [ -z "$wan" ]; then
        bad "$c: VPP 里没有 WAN 地址 ${WAN_IP[$n]}"
    else
        ok "$c: VPP $wan 持有 WAN ${WAN_IP[$n]}"
        echo "$addr" | grep "L3 ${WAN_IP[$n]}/" | grep -q "ip4 table-id" \
            && bad "$c: WAN ${WAN_IP[$n]} 不在 table 0" \
            || ok "$c: WAN ${WAN_IP[$n]} 在 table 0"
    fi
    if [ -z "$xc" ]; then
        bad "$c: VPP 里没有客户口地址 ${XCONN_IP[$n]}"
    else
        ok "$c: VPP $xc 持有客户口 ${XCONN_IP[$n]}"
        echo "$addr" | grep "L3 ${XCONN_IP[$n]}/" | grep -q "ip4 table-id" \
            && bad "$c: 客户口 ${XCONN_IP[$n]} 不在 table 0" \
            || ok "$c: 客户口 ${XCONN_IP[$n]} 在 table 0"
    fi
    # 地址只在 VPP + linux-cp 的 TAP 上：内核原始网卡不能再有它（否则 weak
    # host 会抢先答 ARP）。
    docker exec "$c" ip -o -4 addr show | grep -q "inet ${WAN_IP[$n]}/" \
        && bad "$c: 内核还持有 WAN ${WAN_IP[$n]}（地址没搬进 VPP）" \
        || ok "$c: 内核不持有 WAN ${WAN_IP[$n]}"
    kxif=$(docker exec "$c" ip -o -4 addr show |
        awk -v w="${XCONN_IP[$n]}" '{split($4,a,"/"); if (a[1]==w) {print $2; exit}}')
    [ "$kxif" = "$LCP_IF" ] \
        && ok "$c: 客户口地址只在内核 $LCP_IF（不在原始网卡）" \
        || bad "$c: 客户口地址在内核上属于 '${kxif:-无}'，期望 $LCP_IF"
    # linux-cp 配对 + lcp-sync：FRR 的 BGP 会话从 TAP 起。
    vppc "$c" show lcp | grep -q 'lcp lcp-sync on' \
        && ok "$c: lcp-sync 已打开" || bad "$c: lcp-sync 没打开"
    vppc "$c" show lcp | grep -q " $xc .* $LCP_IF " \
        && ok "$c: linux-cp 已配对 $xc ↔ $LCP_IF" \
        || bad "$c: linux-cp 没有配对 $xc ↔ $LCP_IF"
    # 盒内核不转发任何包：全局与每个接口都必须关，也没有默认路由。
    [ "$(docker exec "$c" cat /proc/sys/net/ipv4/ip_forward)" = "0" ] \
        && ok "$c: 全局 ip_forward=0" || bad "$c: 全局 ip_forward 不是 0"
    fw=$(docker exec "$c" sh -c 'for i in /sys/class/net/*; do
            [ -d "$i" ] || continue; i=$(basename "$i"); [ "$i" = lo ] && continue
            cat "/proc/sys/net/ipv4/conf/$i/forwarding"; done' | tr -d '\r' | sort -u)
    [ "$fw" = "0" ] && ok "$c: 所有非 lo 接口 forwarding=0" \
        || bad "$c: 仍有接口 forwarding≠0（$fw）"
    docker exec "$c" ip route show default | grep -q . \
        && bad "$c: 内核还有默认路由（内核不该做路由器）" \
        || ok "$c: 内核没有默认路由"
    docker exec "$c" ip -o link show | grep -Eq '^[0-9]+: vti[01][@:]' \
        && bad "$c: 还存在 vti0/vti1（旧架构残留）" \
        || ok "$c: 没有 vti0/vti1"
    # 原始网卡 arp_ignore=1：地址在 VPP，内核不该替它用错 MAC 回答 ARP。
    [ -n "$xc" ] || continue
    kif=${xc#host-}
    [ "$(docker exec "$c" cat "/proc/sys/net/ipv4/conf/$kif/arp_ignore" 2>/dev/null)" = "1" ] \
        && ok "$c: $kif 上 arp_ignore=1（不会抢答 VPP 的地址）" \
        || bad "$c: $kif 上 arp_ignore 不是 1"
done

sect "3. WG 隧道（端口 / 路径 / 绑定 / peer）"
for n in 1 2 3; do
    c=lab-cpe$n
    ports=$(vppc "$c" show sr-wg ports | awk '/^vpn/{i=index($0,": "); s=substr($0,i+2); gsub(/ /,"",s); print s}')
    for item in ${CPE_WG[$n]}; do
        wgif=${item%%:*}; rest=${item#*:}; port=${rest%%:*}; ov=${rest#*:}
        vppc "$c" show interface "$wgif" | awk -v n="$wgif" '$1==n{f=1} END{exit !f}' \
            && ok "$c: $wgif 存在" || bad "$c: $wgif 不存在"
        vppc "$c" show interface addr | grep -q "L3 $ov/32" \
            && ok "$c: $wgif 持有 overlay $ov/32" || bad "$c: $wgif 没有 $ov/32"
        vppc "$c" show interface addr | grep -A1 "^$wgif (" |
            grep -q "ip4 table-id $OVERLAY " \
            && ok "$c: $wgif 在 overlay 表 $OVERLAY" \
            || bad "$c: $wgif 不在 overlay 表 $OVERLAY（show interface addr）"
        case ",$ports," in *",$port,"*) ok "$c: vpn 端口集合含 $port" ;;
            *) bad "$c: vpn 端口集合 '$ports' 不含 $port" ;; esac
        mtu=$(vppc "$c" show interface "$wgif" | awk -v n="$wgif" '$1==n{print $4; exit}')
        [ "${mtu%%/*}" = "$TUNNEL_MTU" ] && ok "$c: $wgif MTU=${mtu%%/*}" \
            || bad "$c: $wgif MTU=$mtu（期望 $TUNNEL_MTU）"
    done
    for p in $(echo "${CPE_PATHS[$n]}" | tr ',' ' '); do
        vppc "$c" show sr-wg paths | grep -q "path_id=$p " \
            && ok "$c: SR 路径 $p 在表里" || bad "$c: SR 路径 $p 不在表里"
    done
    tun=$(vppc "$c" show sr-wg tunnels)
    enc_ifs=$(echo "$tun" | sed -n 's/.*encap enabled on \([0-9]*\) interface.*/\1/p')
    [ "${enc_ifs:-0}" -ge 1 ] && ok "$c: encap 已挂在 $enc_ifs 个 underlay 接口上" \
        || bad "$c: encap 没挂到 underlay（SR 路径不会生效）"
    usable=$(echo "$tun" | sed -n 's/.*binding(s) configured, \([0-9]*\) usable.*/\1/p')
    [ "${usable:-0}" -ge 2 ] && ok "$c: 隧道绑定可用 $usable 条" \
        || bad "$c: 隧道绑定可用 ${usable:-0} 条（期望 2）"
    retry 30 peer_up "$c" "${CPE_PEERS[$n]}" \
        && ok "$c: WG peer 已建立（>=${CPE_PEERS[$n]}）" \
        || bad "$c: WG peer 未全部 established（$(vppc "$c" show wireguard peer | grep -c 'flags: 2')/${CPE_PEERS[$n]}）"
done
vppc lab-transit show sr-wg ports | grep -q "已启用" && ok "lab-transit: transit 已启用" || bad "lab-transit: transit 未启用"
for p in 51821 51841 51861; do
    vppc lab-transit show sr-wg ports | grep -Eq "(^|[^0-9])$p([^0-9]|$)" \
        && ok "lab-transit: transit 端口 $p 已登记" || bad "lab-transit: 缺 transit 端口 $p"
done

sect "4. overlay 可达性（VPP 自己发起的 ping，走 SR 两跳）"
# 每条隧道在两端的接口名不一定相同（cpe3 的 wg0 指向 cpe1），按实际绑定写。
for pair in "1 2 192.168.99.2 wg0" "2 1 192.168.99.1 wg0" \
            "1 3 192.168.100.2 wg1" "3 1 192.168.100.1 wg0" \
            "2 3 192.168.101.2 wg1" "3 2 192.168.101.1 wg1"; do
    set -- $pair
    retry 20 overlay_ping "lab-cpe$1" "$3" "$4" \
        && ok "lab-cpe$1 -> $3（cpe$2，source $4）overlay 通" \
        || bad "lab-cpe$1 -> $3（cpe$2，source $4）overlay 不通"
done

sect "5. 控制面（sr-ctrl / sr-rr / sr-route）"
docker exec lab-ctrl test -f /var/lib/sr-ctrl/ca.pem && ok "sr-ctrl CA 已生成" || bad "sr-ctrl 没有 CA"
for n in 1 2 3; do
    for f in device_ed25519.key node_cert.pem ca_cert.pem state.json; do
        docker exec "lab-cpe$n" test -s "/etc/sdwan/identity/$f" \
            && ok "lab-cpe$n: identity/$f 已落盘" || bad "lab-cpe$n: identity/$f 缺失"
    done
done
srs=$(docker exec lab-rr curl -sf "http://127.0.0.1:$RR_STATUS/status" 2>/dev/null)
echo "$srs" | grep -q '"tenant": *"tenant-a"' && ok "sr-rr 服务 tenant-a" || bad "sr-rr 状态异常"
n=$(echo "$srs" | grep -o '"device_id": *[0-9]*' | wc -l | tr -d ' ')
[ "${n:-0}" -ge 3 ] && ok "sr-rr 有 3 个设备会话" || bad "sr-rr 只有 ${n:-0} 个设备会话"
for n in 1 2 3; do
    c=lab-cpe$n
    want=""
    for m in 1 2 3; do
        [ "$m" = "$n" ] && continue
        ov=$(peer_overlay "$n" "$m")
        # 远端站点自己的 LAN + 客户路由器宣告的 loopback 都该被反射过来
        want+="${LAN_PFX[$m]}:$ov,${LOOP_IP[$m]}/32:$ov,"
    done
    wantl="${LAN_PFX[$n]},${LOOP_IP[$n]}/32"
    retry 30 status_check "$c" "${ROUTE_STATUS[$n]}" "${want%,}" "$wantl" \
        && ok "$c: RIB 有另两站的 LAN+loopback，local 含本站 LAN+反向导入的 loopback" \
        || bad "$c: sr-route 状态不符合预期（docker exec $c curl -s http://127.0.0.1:${ROUTE_STATUS[$n]}/status）"
done

sect "6. VPP FIB（FRR 算路由、VPP 转发：远端压 $LABEL，本地交给客户路由器）"
for n in 1 2 3; do
    c=lab-cpe$n
    xc=$(vpp_if_for_ip "$c" "${XCONN_IP[$n]}")
    fib=$(vppc "$c" show ip fib table "$VPN")
    for m in 1 2 3; do
        [ "$m" = "$n" ] && continue
        for pfx in "${LAN_PFX[$m]}" "${LOOP_IP[$m]}/32"; do
            echo "$fib" | grep -q "^$pfx" \
                && ok "$c: table $VPN 有远端 $pfx（sr-fib 直接装 VPP）" \
                || bad "$c: table $VPN 缺 $pfx"
            echo "$fib" | grep -A8 "^$pfx" |
                grep -qE "mpls-label\[@[0-9]+\]:\[$LABEL:" \
                && ok "$c: $pfx 压标签 $LABEL" || bad "$c: $pfx 没压标签 $LABEL"
        done
    done
    if [ -z "$xc" ]; then
        bad "$c: 找不到客户口的 VPP 接口（本地前缀无从核对）"
    else
        for pfx in "${LAN_PFX[$n]}" "${LOOP_IP[$n]}/32"; do
            one=$(vppc "$c" show ip fib table "$VPN" "$pfx")
            echo "$one" | grep -q "via ${CUST_X_IP[$n]} $xc" \
                && ok "$c: 本地 $pfx 在 VPP 里经 $xc 交给客户路由器 ${CUST_X_IP[$n]}" \
                || bad "$c: 本地 $pfx 没有在 VPP 里指向 ${CUST_X_IP[$n]} $xc"
            echo "$one" | grep -q "mpls-label" \
                && bad "$c: 本地 $pfx 被压了标签（本地前缀不该走标签）" \
                || ok "$c: 本地 $pfx 不打标签"
            echo "$one" | grep -q "dpo-drop" \
                && bad "$c: 本地 $pfx 是 dpo-drop" || ok "$c: 本地 $pfx 不是 dpo-drop"
            # FRR 装进内核主表的原样记录：下一跳是客户路由器，出口是 TAP。
            docker exec "$c" ip route show "$pfx" |
                grep -q "via ${CUST_X_IP[$n]} dev $LCP_IF" \
                && ok "$c: 内核主表 $pfx via ${CUST_X_IP[$n]} dev $LCP_IF（linux-nl 的输入）" \
                || bad "$c: 内核主表没有 $pfx via ${CUST_X_IP[$n]} dev $LCP_IF"
        done
    fi
    # overlay 表只放隧道与 /32 下一跳：客户前缀一律不许混进来。
    ovf=$(vppc "$c" show ip fib table "$OVERLAY")
    for m in 1 2 3; do
        [ "$m" = "$n" ] && continue
        echo "$ovf" | grep -q "^$(peer_overlay "$n" "$m")/32" \
            && ok "$c: overlay 表有对端 $(peer_overlay "$n" "$m")/32" \
            || bad "$c: overlay 表缺对端 $(peer_overlay "$n" "$m")/32"
    done
    echo "$ovf" | grep -qE "^(10\.68\.|172\.16\.)" \
        && bad "$c: overlay 表里混进了客户前缀" \
        || ok "$c: overlay 表只有隧道/overlay，没有客户前缀"
    # table 1250 是 FRR 专用停车场的镜像：只用于再分发，转发必须全是 drop。
    t1250=$(vppc "$c" show ip fib table "$FRR_TABLE")
    echo "$t1250" | grep -q "fib_index" \
        && ok "$c: VPP table $FRR_TABLE 存在（linux-nl 镜像了 FRR 停车场）" \
        || bad "$c: VPP table $FRR_TABLE 不存在"
    echo "$t1250" | grep -q "mpls-label" \
        && bad "$c: table $FRR_TABLE 里有压标签路由（不该参与转发）" \
        || ok "$c: table $FRR_TABLE 没有压标签路由"
    for m in 1 2 3; do
        [ "$m" = "$n" ] && continue
        echo "$t1250" | grep -A4 "^${LAN_PFX[$m]}" | grep -q "dpo-drop" \
            && ok "$c: table $FRR_TABLE 的远端 ${LAN_PFX[$m]} 是 dpo-drop（不转发）" \
            || bad "$c: table $FRR_TABLE 的远端 ${LAN_PFX[$m]} 不是 dpo-drop"
    done
done

sect "7. 正向导入：sr-frr → FRR table $FRR_TABLE → BGP → 客户路由器"
for n in 1 2 3; do
    c=lab-cpe$n
    vtysh "$c" -c 'show running-config' | grep -q "redistribute table-direct $FRR_TABLE" \
        && ok "$c: 已配置 redistribute table-direct $FRR_TABLE" || bad "$c: 缺 redistribute table-direct $FRR_TABLE"
    retry 20 bash -c "docker exec $c vtysh -c 'show bgp summary' | grep -q '^${CUST_X_IP[$n]} '" \
        && ok "$c: BGP 与客户路由器 ${CUST_X_IP[$n]}（AS ${CUST_AS[$n]}）建立" \
        || bad "$c: BGP 与 ${CUST_X_IP[$n]} 未建立"
    # 会话的源地址必须是客户口（TAP 上那份），证明 BGP 跑在 linux-cp 的 TAP 上。
    vtysh "$c" -c "show bgp neighbors ${CUST_X_IP[$n]}" |
        grep -q "Local host: ${XCONN_IP[$n]}," \
        && ok "$c: BGP 本地源地址是 ${XCONN_IP[$n]}（走 $LCP_IF，不经过内核转发）" \
        || bad "$c: BGP 本地源地址不是 ${XCONN_IP[$n]}"
    t1250=$(vtysh "$c" -c "show ip route table $FRR_TABLE")
    for m in 1 2 3; do
        [ "$m" = "$n" ] && continue
        for pfx in "${LAN_PFX[$m]}" "${LOOP_IP[$m]}/32"; do
            echo "$t1250" | grep -q "$pfx" \
                && ok "$c: table $FRR_TABLE 有远端 $pfx" || bad "$c: table $FRR_TABLE 缺 $pfx"
            retry 20 bash -c "docker exec $c vtysh -c 'show bgp ipv4 unicast' | grep -q '$pfx'" \
                && ok "$c: 本盒 BGP 里有 $pfx（box FRR 自身可见）" \
                || bad "$c: 本盒 BGP 不可见 $pfx"
        done
    done
done
for n in 1 2 3; do
    c=lab-cust$n
    retry 20 bash -c "docker exec $c vtysh -c 'show bgp summary' | grep -q '^${XCONN_IP[$n]} '" \
        && ok "$c: 与盒内 FRR ${XCONN_IP[$n]}（AS ${BOX_AS[$n]}）建立" \
        || bad "$c: 与 ${XCONN_IP[$n]} 未建立"
    bgp=$(vtysh "$c" -c 'show bgp ipv4 unicast')
    for m in 1 2 3; do
        [ "$m" = "$n" ] && continue
        for pfx in "${LAN_PFX[$m]}" "${LOOP_IP[$m]}/32"; do
            echo "$bgp" | grep -q "$pfx" \
                && ok "$c: 客户 BGP 学到远端 $pfx（正向导入生效）" \
                || bad "$c: 客户 BGP 没有远端 $pfx"
        done
    done
    vtysh "$c" -c 'show ip route 0.0.0.0/0' | grep -q "${XCONN_IP[$n]}" \
        && ok "$c: 默认路由指向盒子客户口 ${XCONN_IP[$n]}（VPP）" \
        || bad "$c: 默认路由没指向 ${XCONN_IP[$n]}"
done

sect "8. 反向导入：客户 BGP（tag $BGP_TAG）→ 盒内 FRR → sr-frr → sr-route local"
for n in 1 2 3; do
    c=lab-cpe$n
    r=$(vtysh "$c" -c "show ip route ${LOOP_IP[$n]}/32")
    echo "$r" | grep -q "tag $BGP_TAG" \
        && ok "$c: 客户 loopback 在 zebra 里带 tag $BGP_TAG（反向上报的门槛）" \
        || bad "$c: 客户 loopback 不带 tag $BGP_TAG（sr-frr 会丢弃它）"
    echo "$r" | grep -q "${CUST_X_IP[$n]}" \
        && ok "$c: 客户 loopback 的下一跳是客户路由器 ${CUST_X_IP[$n]}" \
        || bad "$c: 客户 loopback 的下一跳不是 ${CUST_X_IP[$n]}"
    # 我们经 table 1250 导出的远端前缀回到 BGP 时 tag 是 0，不能被反向再吃回来：
    # 它们只能停在 table 1250，主表里必须不存在。
    for m in 1 2 3; do
        [ "$m" = "$n" ] && continue
        vtysh "$c" -c "show ip route ${LAN_PFX[$m]}" | grep -q "Network not in table" \
            && ok "$c: 远端 ${LAN_PFX[$m]} 只停在 table $FRR_TABLE，没回灌主表（无环路）" \
            || bad "$c: 远端 ${LAN_PFX[$m]} 出现在主表（可能被反向再导入）"
    done
done

sect "9. 终端到终端 / 终端到远端客户 loopback（客户视角）"
# 采样内核转发计数：数据流量全程只该走 VPP，内核一个包都不许转。
FWD0=( "" "" "" "" )
for n in 1 2 3; do FWD0[$n]=$(kern_ipfwd "lab-cpe$n"); done
for a in 1 2 3; do
    for b in 1 2 3; do
        [ "$a" = "$b" ] && continue
        retry 20 ping4 "lab-term$a" "${TERM_IP[$b]}" \
            && ok "lab-term$a -> lab-term$b（${TERM_IP[$b]}）通" \
            || bad "lab-term$a -> lab-term$b（${TERM_IP[$b]}）不通"
        retry 20 ping4 "lab-term$a" "${LOOP_IP[$b]}" \
            && ok "lab-term$a -> cust$b loopback ${LOOP_IP[$b]} 通（反向导入落到了数据面）" \
            || bad "lab-term$a -> ${LOOP_IP[$b]} 不通"
    done
done
for n in 1 2 3; do
    c=lab-cpe$n
    fwd=$(kern_ipfwd "$c")
    [ "$fwd" = "${FWD0[$n]}" ] \
        && ok "$c: 跑完数据流量内核 Ip:Forwarding 没变（$fwd），证明内核没转发" \
        || bad "$c: 内核 Ip:Forwarding 从 ${FWD0[$n]} 变成 $fwd（内核参与了转发）"
    [ "$fwd" = "2" ] \
        && ok "$c: Ip:Forwarding=2（MIB notForwarding）" \
        || bad "$c: Ip:Forwarding=$fwd（期望 2 = notForwarding）"
done

sect "10. 数据面计数器"
for n in 1 2 3; do
    c=lab-cpe$n
    enc=$(vppc "$c" show sr-wg stats | awk '/Encap:/{e=1} e&&/ok:/{print $2; exit}')
    dec=$(vppc "$c" show sr-wg stats | awk '/Decap:/{d=1} d&&/ok:/{print $2; exit}')
    [ "${enc:-0}" -gt 0 ] && ok "$c: Encap ok=$enc" || bad "$c: Encap ok=${enc:-0}（没有报文被封装）"
    [ "${dec:-0}" -gt 0 ] && ok "$c: Decap ok=$dec" || bad "$c: Decap ok=${dec:-0}（没有报文被解封装）"
done
tr=$(vppc lab-transit show sr-wg stats | awk '/Transit:/{t=1} t&&/ok:/{print $2; exit}')
[ "${tr:-0}" -gt 0 ] && ok "lab-transit: Transit ok=$tr" || bad "lab-transit: Transit ok=${tr:-0}"

echo
echo "================================"
printf '总计: %d 通过, %d 失败\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
