#!/bin/bash
# verify.sh - 对 bringup.sh 搭好的三站点 lab 做端到端体检（宿主机执行）
#
# 检查顺序就是一次报文的真实路径：
#   容器/VPP → WG 隧道（端口/路径/绑定/peer）→ 控制面 RIB → VPP FIB →
#   FRR 客户侧（table 1250 + BGP）→ 终端到终端。
#
# 只读：不改任何配置，不重启任何进程。失败项逐条打印，最后给总数。
#
# 用法: test/lab/verify.sh [--quiet]
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
VRF=250
LABEL=266
TUNNEL_MTU=1360
ROUTE_STATUS=( "" 19001 19002 19003 )
WAN_IP=( "" 10.88.1.11 10.88.1.12 10.88.1.13 )
INSIDE_IP=( "" 10.78.1.2 10.78.2.2 10.78.3.2 )
LAN_PFX=( "" 10.68.1.0/24 10.68.2.0/24 10.68.3.0/24 )
TERM_IP=( "" 10.68.1.10 10.68.2.10 10.68.3.10 )
FRR_IP=( "" 10.78.1.20 10.78.2.20 10.78.3.20 )
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
peer_up() {   # <cpe> <want>
    local n
    n=$(vppc "$1" show wireguard peer | grep -c 'flags: 2')
    [ "$n" -ge "$2" ]
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
status_check() {   # <cpe> <port> <expect: "prefix:nh,prefix:nh">
    local c=$1 port=$2 want=$3
    docker exec "$c" python3 - "$port" "$VRF" "$want" <<'PY' >/dev/null 2>&1
import json, sys, urllib.request
port, vrf, want = sys.argv[1], sys.argv[2], sys.argv[3]
d = json.load(urllib.request.urlopen("http://127.0.0.1:%s/status" % port, timeout=3))
if d.get("active_rr") != 0:
    raise SystemExit("active_rr=%r" % d.get("active_rr"))
got = sorted("%s:%s" % (r["prefix"], ",".join(sorted(r["next_hops"])))
             for r in d.get("rib", {}).get(vrf, []))
if got != sorted(x for x in want.split(",") if x):
    raise SystemExit("rib=%r want=%r" % (got, want))
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
overlay_ping() { vppc "$1" ping "$2" repeat 3 interval 0.2 | grep -q '0% packet loss'; }

echo "===== 1. 容器与 VPP ====="
for c in lab-cpe1 lab-cpe2 lab-cpe3 lab-transit lab-rr lab-ctrl lab-frr1 lab-frr2 lab-frr3 lab-term1 lab-term2 lab-term3; do
    docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null | grep -q true && ok "$c 运行中" || bad "$c 没运行"
done
for c in lab-cpe1 lab-cpe2 lab-cpe3 lab-transit; do
    vppc "$c" show version | grep -q "vpp v" && ok "$c: VPP 在运行" || bad "$c: VPP 不可用"
    vppc "$c" show plugins | grep -q sr_wg_plugin.so && ok "$c: sr_wg 插件已加载" || bad "$c: sr_wg 插件缺失"
done

echo
echo "===== 2. WG 隧道（端口 / 路径 / 绑定 / peer） ====="
for n in 1 2 3; do
    c=lab-cpe$n
    ports=$(vppc "$c" show sr-wg ports | awk '/^vpn/{i=index($0,": "); s=substr($0,i+2); gsub(/ /,"",s); print s}')
    for item in ${CPE_WG[$n]}; do
        wgif=${item%%:*}; rest=${item#*:}; port=${rest%%:*}; ov=${rest#*:}
        vppc "$c" show interface "$wgif" | awk -v n="$wgif" '$1==n{f=1} END{exit !f}' \
            && ok "$c: $wgif 存在" || bad "$c: $wgif 不存在"
        vppc "$c" show interface addr | grep -q "L3 $ov/32" \
            && ok "$c: $wgif 持有 overlay $ov/32" || bad "$c: $wgif 没有 $ov/32"
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

echo
echo "===== 3. overlay 可达性（VPP 自己发起的 ping，走 SR 两跳） ====="
for pair in "1 2 192.168.99.2" "2 1 192.168.99.1" \
            "1 3 192.168.100.2" "3 1 192.168.100.1" \
            "2 3 192.168.101.2" "3 2 192.168.101.1"; do
    set -- $pair
    retry 20 overlay_ping "lab-cpe$1" "$3" \
        && ok "lab-cpe$1 -> $3（cpe$2）overlay 通" \
        || bad "lab-cpe$1 -> $3（cpe$2）overlay 不通"
done

echo
echo "===== 4. 控制面（sr-ctrl / sr-rr / sr-route） ====="
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
        want+="${LAN_PFX[$m]}:$(peer_overlay "$n" "$m"),"
    done
    retry 30 status_check "$c" "${ROUTE_STATUS[$n]}" "${want%,}" \
        && ok "$c: RIB 收到另两个站点且 next_hop 正确" \
        || bad "$c: RIB 不符合预期（docker exec $c curl -s http://127.0.0.1:${ROUTE_STATUS[$n]}/status）"
done

echo
echo "===== 5. VPP FIB（table $VRF，VPN 标签 $LABEL） ====="
for n in 1 2 3; do
    c=lab-cpe$n
    for m in 1 2 3; do
        [ "$m" = "$n" ] && continue
        fib=$(vppc "$c" show ip fib table "$VRF")
        echo "$fib" | grep -q "^${LAN_PFX[$m]}" \
            && ok "$c: table $VRF 有远端 ${LAN_PFX[$m]}" || bad "$c: table $VRF 缺 ${LAN_PFX[$m]}"
        echo "$fib" | grep -A8 "^${LAN_PFX[$m]}" |
            grep -qE "mpls-label\[@[0-9]+\]:\[$LABEL:" \
            && ok "$c: ${LAN_PFX[$m]} 压标签 $LABEL" || bad "$c: ${LAN_PFX[$m]} 没压标签 $LABEL"
    done
    vppc "$c" show ip fib table "$VRF" | grep -q "^${LAN_PFX[$n]}" \
        && ok "$c: table $VRF 有本地 ${LAN_PFX[$n]}（via FRR）" \
        || bad "$c: table $VRF 缺本地 ${LAN_PFX[$n]}"
done

echo
echo "===== 6. FRR 客户侧（table 1250 + BGP + 默认路由） ====="
for n in 1 2 3; do
    c=lab-frr$n
    docker exec "$c" vtysh -c 'show running-config' | grep -q 'redistribute table-direct 1250' \
        && ok "$c: 已配置 redistribute table-direct 1250" || bad "$c: 缺 redistribute table-direct 1250"
    docker exec "$c" vtysh -c 'show ip route 0.0.0.0/0' | grep -q "${INSIDE_IP[$n]}" \
        && ok "$c: 默认路由指向 CPE ${INSIDE_IP[$n]}" || bad "$c: 默认路由没指向 ${INSIDE_IP[$n]}"
    for m in 1 2 3; do
        [ "$m" = "$n" ] && continue
        docker exec "$c" vtysh -c 'show ip route table 1250' | grep -q "${LAN_PFX[$m]}" \
            && ok "$c: table 1250 有 ${LAN_PFX[$m]}" || bad "$c: table 1250 缺 ${LAN_PFX[$m]}"
        docker exec "$c" vtysh -c 'show bgp ipv4 unicast' | grep -q "${LAN_PFX[$m]}" \
            && ok "$c: BGP 可见 ${LAN_PFX[$m]}" || bad "$c: BGP 不可见 ${LAN_PFX[$m]}"
    done
    docker exec "$c" vtysh -c 'show bgp summary' | grep -qi "No BGP neighbors" \
        && note "$c: 无 BGP 邻居（本 lab 只验证 redistribute 可见性）"
done

echo
echo "===== 7. 终端到终端（客户视角） ====="
for a in 1 2 3; do
    for b in 1 2 3; do
        [ "$a" = "$b" ] && continue
        retry 20 ping4 "lab-term$a" "${TERM_IP[$b]}" \
            && ok "lab-term$a -> lab-term$b（${TERM_IP[$b]}）通" \
            || bad "lab-term$a -> lab-term$b（${TERM_IP[$b]}）不通"
    done
done

echo
echo "===== 8. 数据面计数器 ====="
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
