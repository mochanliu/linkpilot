#!/bin/bash
# test-multivrf.sh - L-6 多 VRF 判据：VPN 0 / VPN 100 / VPN 101 同时泄露
#
# 回答的是"多 VPN 到底能不能同时工作"，只验控制面与内核注入这一层：
#
#   (A) sr-frr 按 (VPN, 来源) 订阅：vpn0 订 BGP，vpn100/101 订 connected；
#       每台盒子上报 6 条本地前缀（3 个 VPN × 2 条）。
#   (B) sr-route 的 RIB 按目标 VPN 分桶：0/100/101 各 4 条（另两站的
#       LAN + 共用前缀），RT 是 65000:0 / 65000:100 / 65000:101。
#   (C) sr-frr 把远端前缀注入**对应 VRF 的内核表**：table 100/101 里
#       4 条 K 路由，出口是 vpn100/vpn101；本站 connected 保持 C 不动。
#   (D) 盒内 FRR：Vrf 路由表里能看到 K，`redistribute kernel` 让 BGP vrf
#       实例里有 4 条 origin ?（这就是 SDWAN→CPE 的出口）。
#   (E) 同一个前缀（10.200.<站点>.0/24）在 100 和 101 里同时存在、出口不同，
#       两个 VPN 不串；主表 254 里一条 10.100/10.101/10.200 都没有。
#   (F) touch 窗口：sr-frr 用 `/config` + `refresh_vrfs` 原地重刷（协议集合
#       变化时的真实路径），逐条测 del→add 间隔，BGP 版本号不许动。
#   (G) 全程 sr-frr / sr-route / FRR 守护进程 pid 不变；只有 sr-fib 会重启
#       ——它**没有热更入口**（docs/overlay-console-plan.md:125-127，D3 已拍
#       要补 `PUT`，还没做）。这是本脚本唯一的中断点，也必须点名。
#
# 前置：lab 已起好并跑到 verify.sh 全绿（基线 = 只有 VPN 0）。
# 收尾：默认把 lab 恢复成基线（sr-frr/sr-route PUT 回 [0]、FRR 热删、内核
# VRF 设备删掉、fib.json 回 [0] 并重启一次 sr-fib）；`--keep` 留着现场。
# 结束后可以直接再跑 test/lab/verify.sh。
#
# 用法:
#   test/lab/test-multivrf.sh [--keep]
set -u

export DOCKER_HOST=${DOCKER_HOST:-unix:///Users/louis/.colima/default/docker.sock}

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

PASS=0
FAIL=0
ok()  { PASS=$((PASS + 1)); printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad() { FAIL=$((FAIL + 1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
sect(){ echo; echo "== $*"; }

dx()  { local c=$1; shift; docker exec "$c" "$@"; }
dsh() { local c=$1; shift; docker exec "$c" sh -c "$*"; }
vppc(){ local c=$1; shift; dx "$c" vppctl "$@" 2>&1 | tr -d '\r'; }
retry() {   # <秒> <命令...>
    local limit=$1 i=0; shift
    while [ "$i" -lt "$((limit * 2))" ]; do
        "$@" >/dev/null 2>&1 && return 0
        sleep 0.5; i=$((i + 1))
    done
    "$@" >/dev/null 2>&1
}

# 站点参数（与 bringup.sh 一致）
LAN_PFX=( "" "10.68.1.0/24" "10.68.2.0/24" "10.68.3.0/24" )
AS=( "" 65101 65102 65103 )
ROUTE_PORT=( "" 19001 19002 19003 )

srfrr_body() {   # <mode: multi|base>
    if [ "$1" = "multi" ]; then
        echo '{"export_vrfs":[0,100,101],"import_rules":[{"vpn":0,"sources":["bgp"],"prefixes":[{"prefix":"0.0.0.0/0","le":32}]},{"vpn":100,"sources":["connected"],"prefixes":[{"prefix":"0.0.0.0/0","le":32}]},{"vpn":101,"sources":["connected"],"prefixes":[{"prefix":"0.0.0.0/0","le":32}]}]}'
    else
        echo '{"export_vrfs":[0],"import_rules":[{"vpn":0,"sources":["bgp"],"prefixes":[{"prefix":"0.0.0.0/0","le":32}]}]}'
    fi
}

srfrr_put() {   # <cpe> <body>
    dx "$1" curl -s -o /tmp/srfrr-put.out -w '%{http_code}' \
        -X PUT --unix-socket /run/sr-frr/control.sock \
        -H 'Content-Type: application/json' -d "$2" http://localhost/config
}

srfrr_status() { dx "$1" curl -s --unix-socket /run/sr-frr/control.sock http://localhost/status; }

route_put() {   # <n> <mode>
    local n=$1 body
    if [ "$2" = "multi" ]; then
        body='{"vrfs":[{"id":0,"export_rts":["65000:0"],"import_rts":["65000:0"]},{"id":100,"export_rts":["65000:100"],"import_rts":["65000:100"]},{"id":101,"export_rts":["65000:101"],"import_rts":["65000:101"]}]}'
    else
        body='{"vrfs":[{"id":0,"export_rts":["65000:0"],"import_rts":["65000:0"]}]}'
    fi
    dx "lab-cpe$n" curl -s -o /dev/null -w '%{http_code}' -X PUT \
        -H 'Content-Type: application/json' -d "$body" \
        "http://127.0.0.1:${ROUTE_PORT[$n]}/vrfs"
}

rib_keys() {   # <n> -> 排序后的 RIB 里出现过的 VPN 号，空格分隔
    dx "lab-cpe$n" curl -s "http://127.0.0.1:${ROUTE_PORT[$n]}/status" |
        python3 -c 'import json,sys; d=json.load(sys.stdin); print(" ".join(sorted(d["rib"])))'
}

rib_size() {   # <n> <vpn>
    dx "lab-cpe$n" curl -s "http://127.0.0.1:${ROUTE_PORT[$n]}/status" |
        python3 -c "import json,sys; d=json.load(sys.stdin); print(len(d['rib'].get('$2',[])))"
}

frr_add() {   # <n>
    local c=lab-cpe$n as=${AS[$n]}
    dx "$c" sed -i '/^! BEGIN test-multivrf$/,/^! END test-multivrf$/d' /etc/frr/frr.conf
    docker exec -i "$c" sh -c 'cat >> /etc/frr/frr.conf' <<EOF
! BEGIN test-multivrf
ip prefix-list SDWAN2CPE-vpn100 seq 5 permit 0.0.0.0/0 le 32
ip prefix-list SDWAN2CPE-vpn101 seq 5 permit 0.0.0.0/0 le 32
route-map SDWAN2CPE-vpn100 permit 10
 match ip address prefix-list SDWAN2CPE-vpn100
exit
route-map SDWAN2CPE-vpn101 permit 10
 match ip address prefix-list SDWAN2CPE-vpn101
exit
vrf vpn100
exit-vrf
vrf vpn101
exit-vrf
router bgp $as vrf vpn100
 !
 address-family ipv4 unicast
  redistribute kernel route-map SDWAN2CPE-vpn100
 exit-address-family
exit
router bgp $as vrf vpn101
 !
 address-family ipv4 unicast
  redistribute kernel route-map SDWAN2CPE-vpn101
 exit-address-family
exit
! END test-multivrf
EOF
    dx "$c" python3 /usr/lib/frr/frr-reload.py --reload /etc/frr/frr.conf >/dev/null 2>&1
    dx "$c" sh -c 'python3 /usr/lib/frr/frr-reload.py --test /etc/frr/frr.conf >/dev/null 2>&1'
}

frr_del() {   # <n>
    local c=lab-cpe$n
    dx "$c" sed -i '/^! BEGIN test-multivrf$/,/^! END test-multivrf$/d' /etc/frr/frr.conf
    dx "$c" python3 /usr/lib/frr/frr-reload.py --reload /etc/frr/frr.conf >/dev/null 2>&1
    dx "$c" sh -c 'python3 /usr/lib/frr/frr-reload.py --test /etc/frr/frr.conf >/dev/null 2>&1'
}

fib_set() {   # <n> <vrfs-json>
    # 必须 -i：python3 - 从 stdin 读脚本，docker exec 不接 stdin 时会读到 EOF
    # 然后安静地什么都不做（fib.json 不变，sr-fib 起来还是旧 VRF 集合）。
    docker exec -i "lab-cpe$1" python3 - "$2" <<'PY'
import json, sys
p = "/etc/sdwan/fib.json"
d = json.load(open(p))
d["vrfs"] = json.loads(sys.argv[1])
open(p, "w").write(json.dumps(d, indent=2) + "\n")
PY
}

srfib_restart() {   # <n>
    local c=lab-cpe$1
    dx "$c" sh -c 'kill -TERM $(pgrep -x sr-fib) 2>/dev/null
        for i in $(seq 1 25); do pgrep -x sr-fib >/dev/null || break; sleep 0.2; done'
    docker exec -d "$c" sh -c 'exec /usr/local/bin/sr-fib -config /etc/sdwan/fib.json >> /var/log/sr-fib.log 2>&1'
    retry 20 bash -c "docker exec $c sh -c 'tail -n 3 /var/log/sr-fib.log | grep -q \"$2 个 VRF\"'"
}

pids_of() {   # <cpe> -> "zebra=.. bgpd=.. staticd=.. sr-frr=.. sr-route=.."
    dx "$1" sh -c 'for p in zebra bgpd staticd sr-frr sr-route; do
        printf "%s=%s " "$p" "$(pgrep -x $p || echo -)"
    done'
}

# ==================================================================
setup() {
    local n m body pids
    sect "0. 前置检查（基线 = 只有 VPN 0）"
    for n in 1 2 3; do
        [ "$(rib_keys "$n")" = "0" ] && ok "lab-cpe$n: 基线 RIB 只有 VPN 0" \
            || bad "lab-cpe$n: 基线 RIB 不是只有 VPN 0（$(rib_keys "$n")），先跑 verify.sh/恢复基线"
    done

    sect "1. 内核 VRF 设备 + dummy（table 100/101；共用 10.200.<站点>.0/24 做隔离对照）"
    for n in 1 2 3; do
        dsh "lab-cpe$n" "
            ip link add vpn100 type vrf table 100 2>/dev/null || true
            ip link add vpn101 type vrf table 101 2>/dev/null || true
            ip link set vpn100 up; ip link set vpn101 up
            ip link add dum100 type dummy 2>/dev/null || true
            ip link add dum101 type dummy 2>/dev/null || true
            ip link set dum100 master vpn100; ip link set dum101 master vpn101
            ip link set dum100 up; ip link set dum101 up
            ip addr flush dev dum100; ip addr flush dev dum101
            ip addr add 10.100.$n.1/24 dev dum100
            ip addr add 10.200.$n.1/24 dev dum100
            ip addr add 10.101.$n.1/24 dev dum101
            ip addr add 10.200.$n.1/24 dev dum101"
        dsh "lab-cpe$n" 'ip -d link show vpn100 | grep -q "vrf table 100"' \
            && ok "lab-cpe$n: vpn100 是 VRF table 100" || bad "lab-cpe$n: 建不出 vpn100（内核没 VRF？）"
        dsh "lab-cpe$n" "ip route show table 100 | grep -q '^10.100.$n.0/24 dev dum100'" \
            && ok "lab-cpe$n: table 100 有本站 connected" || bad "lab-cpe$n: table 100 没本站 connected"
    done

    sect "2. sr-frr 按 (VPN, 来源) 订阅（热改，不重启）"
    for n in 1 2 3; do
        body=$(srfrr_body multi)
        [ "$(srfrr_put "lab-cpe$n" "$body")" = "200" ] \
            && ok "lab-cpe$n: /config 接受 3 个 VPN" || bad "lab-cpe$n: /config 返回非 200"
    done
    retry 10 bash -c "for c in lab-cpe1 lab-cpe2 lab-cpe3; do
        h=\$(docker exec \$c curl -s --unix-socket /run/sr-frr/control.sock http://localhost/status |
            python3 -c 'import json,sys; d=json.load(sys.stdin); print(d[\"reported\"], d[\"config_hash\"])')
        set -- \$h; [ \"\$1\" = 6 ] || exit 1
    done"
    for n in 1 2 3; do
        body=$(srfrr_status "lab-cpe$n")
        echo "$body" | python3 -c 'import json,sys
d=json.load(sys.stdin)
assert d["reported"]==6, d["reported"]
# sr-route 还没 PUT /vrfs（下一节），RIB 只有 vpn0 的 4 条远端，所以
# 注入只能是 4；12 要等分桶之后。此时 last_error 会是 /local-prefixes 的
# 400（VPN 100/101 未配置），这是打包顺序反过来时的预期，不在这里判。
assert d["injected"]==4, d["injected"]
assert d["dropped_loop"]==0 and d["dropped_unknown_vrf"]==0, d
assert not d["skipped"], d["skipped"]
assert len(d["subscriptions"])==3, d["subscriptions"]' \
            && ok "lab-cpe$n: 上报 6 / 注入 4（vpn0 现有）/ 订阅 3 组 / dropped_loop=0" \
            || bad "lab-cpe$n: sr-frr 状态不符（$body）"
    done

    sect "3. sr-route PUT /vrfs（VPN 0/100/101 + 各自 RT）"
    for n in 1 2 3; do
        [ "$(route_put "$n" multi)" = "200" ] \
            && ok "lab-cpe$n: PUT /vrfs 200" || bad "lab-cpe$n: PUT /vrfs 非 200"
    done
    retry 20 bash -c '[ "$(docker exec lab-cpe1 curl -s http://127.0.0.1:19001/status |
        python3 -c "import json,sys; d=json.load(sys.stdin); print(len(d[\"rib\"]))")" = 3 ]'
    for n in 1 2 3; do
        [ "$(rib_keys "$n")" = "0 100 101" ] \
            && ok "lab-cpe$n: RIB 分桶 0/100/101" || bad "lab-cpe$n: RIB 分桶是 $(rib_keys "$n")"
    done
    retry 10 bash -c 'for n in 1 2 3; do
        h=$(docker exec lab-cpe$n curl -s --unix-socket /run/sr-frr/control.sock http://localhost/status |
            python3 -c "import json,sys; d=json.load(sys.stdin); print(d[\"injected\"], 1 if d[\"last_error\"] else 0)")
        set -- $h; [ "$1" = 12 ] && [ "$2" = 0 ] || exit 1
    done'
    for n in 1 2 3; do
        srfrr_status "lab-cpe$n" | python3 -c 'import json,sys
d=json.load(sys.stdin)
assert d["injected"]==12, d["injected"]
assert d["last_error"]=="", d["last_error"]' \
            && ok "lab-cpe$n: 分桶后注入 12、last_error 清空" \
            || bad "lab-cpe$n: 分桶后 sr-frr 没收敛（$(srfrr_status "lab-cpe$n")）"
    done

    sect "4. FRR 加 vrf 实例（frr-reload 热改，守护进程不重启）"
    for n in 1 2 3; do
        pids=$(pids_of "lab-cpe$n")
        frr_add "$n" && ok "lab-cpe$n: frr-reload 后 --test 0 差异" \
            || bad "lab-cpe$n: frr-reload 后仍有差异"
        [ "$pids" = "$(pids_of "lab-cpe$n")" ] \
            && ok "lab-cpe$n: FRR/sr-frr/sr-route pid 没变" \
            || bad "lab-cpe$n: 有进程被重启（$pids -> $(pids_of "lab-cpe$n")）"
    done

    sect "5. sr-fib 装 VPN 100/101（唯一中断点：它没有热更入口）"
    for n in 1 2 3; do
        fib_set "$n" '[0,100,101]'
    done
    # 量这次重启的客户可见窗口：term1 -> term2（这条路径在改动前后都该通）。
    docker exec -d lab-term1 sh -c 'exec ping -i 0.01 -c 800 10.68.2.10 > /tmp/ping-srfib-restart.log 2>&1'
    sleep 1
    for n in 1 2 3; do
        srfib_restart "$n" 3 && ok "lab-cpe$n: sr-fib 已按 3 个 VRF 收敛" \
            || bad "lab-cpe$n: sr-fib 没收敛到 3 个 VRF（tail /var/log/sr-fib.log）"
    done
    sleep 8
    # 800 个包 × 10ms ≈ 8.8s；要等 ping 自己收尾，否则读到的是进度行。
    for i in $(seq 1 40); do
        docker exec lab-term1 pgrep -x ping >/dev/null 2>&1 || break
        sleep 0.5
    done
    # 最后一行是 rtt，丢包率在倒数第二行；整篇只出现一次。
    loss=$(dx lab-term1 sh -c 'cat /tmp/ping-srfib-restart.log' | grep -o '[0-9]*% packet loss' | head -1)
    echo "  .... sr-fib 重启窗口 lab-term1 -> term2 丢包: ${loss:-测量失败}"
    retry 10 bash -c 'for n in 1 2 3; do
        docker exec lab-cpe$n ip route show table 100 | grep -q "10.100.0.0/24\|10.100.1.0/24\|10.100.2.0/24\|10.100.3.0/24" || exit 1
    done'
}

# ==================================================================
verify_scenario() {
    local n m
    sect "A. sr-route：RIB 分桶与 local 前缀"
    for n in 1 2 3; do
        echo "   .... lab-cpe$n RIB sizes: 0=$(rib_size "$n" 0) 100=$(rib_size "$n" 100) 101=$(rib_size "$n" 101)"
        [ "$(rib_size "$n" 0)" = 4 ] && [ "$(rib_size "$n" 100)" = 4 ] && [ "$(rib_size "$n" 101)" = 4 ] \
            && ok "lab-cpe$n: 三个 VPN 各 4 条远端" || bad "lab-cpe$n: RIB 条数不对"
        dx "lab-cpe$n" curl -s "http://127.0.0.1:${ROUTE_PORT[$n]}/status" | python3 -c 'import json,sys
d=json.load(sys.stdin)
assert len(d["local"])==6, d["local"]
assert d["pending"]==[], d["pending"]' \
            && ok "lab-cpe$n: local 6 条（3 VPN × 2）、pending 空" \
            || bad "lab-cpe$n: local/pending 不对"
    done

    sect "B. 内核表：远端进对应 VRF 表，本站 connected 不被改写"
    for n in 1 2 3; do
        t100=$(dsh "lab-cpe$n" 'ip route show table 100')
        t101=$(dsh "lab-cpe$n" 'ip route show table 101')
        for m in 1 2 3; do
            [ "$m" = "$n" ] && continue
            echo "$t100" | grep -q "^10.100.$m.0/24 dev vpn100 scope link" \
                && ok "lab-cpe$n: table 100 有远端 10.100.$m.0/24 dev vpn100" \
                || bad "lab-cpe$n: table 100 缺 10.100.$m.0/24"
            echo "$t101" | grep -q "^10.101.$m.0/24 dev vpn101 scope link" \
                && ok "lab-cpe$n: table 101 有远端 10.101.$m.0/24 dev vpn101" \
                || bad "lab-cpe$n: table 101 缺 10.101.$m.0/24"
            # 同一前缀在两个 VPN 里各有一份，出口必须不同（隔离的静态证据）
            echo "$t100" | grep -q "^10.200.$m.0/24 dev vpn100" \
                && echo "$t101" | grep -q "^10.200.$m.0/24 dev vpn101" \
                && ok "lab-cpe$n: 共用 10.200.$m.0/24 在两个 VPN 各一份、出口不同" \
                || bad "lab-cpe$n: 10.200.$m.0/24 的 VPN 隔离不成立"
        done
        echo "$t100" | grep -q "^10.100.$n.0/24 dev dum100 proto kernel" \
            && ok "lab-cpe$n: 本站 10.100.$n.0/24 仍是 connected（没被远端盖掉）" \
            || bad "lab-cpe$n: 本站 connected 丢了"
    done

    sect "C. 盒内 FRR：zebra 的 K、BGP vrf 的 redistribute、主表不动"
    for n in 1 2 3; do
        k=$(dx "lab-cpe$n" vtysh -c 'show ip route vrf vpn100' 2>/dev/null | grep -c '^K>\*')
        [ "$k" = 4 ] && ok "lab-cpe$n: zebra vrf vpn100 有 4 条 K 路由" \
            || bad "lab-cpe$n: zebra vrf vpn100 的 K 路由是 $k 条"
        dx "lab-cpe$n" vtysh -c 'show ip route vrf vpn100' 2>/dev/null |
            grep -qE 'K>\* 10\.100\.[0-9]+\.0/24.*vpn100' \
            && ok "lab-cpe$n: K 路由出口是 vpn100（不是 lcp TAP）" \
            || bad "lab-cpe$n: K 路由出口不对"
        b=$(dx "lab-cpe$n" vtysh -c 'show bgp vrf vpn100 ipv4 unicast' 2>/dev/null | grep -c '^ \*>')
        [ "$b" = 4 ] && ok "lab-cpe$n: BGP vrf vpn100 有 4 条（redistribute kernel 生效）" \
            || bad "lab-cpe$n: BGP vrf vpn100 是 $b 条"
        main=$(dsh "lab-cpe$n" 'ip route show table main' | grep -cE '^(10\.100\.|10\.101\.|10\.200\.)')
        [ "$main" = 0 ] && ok "lab-cpe$n: 主表 254 没有 VPN 100/101/200 的痕迹" \
            || bad "lab-cpe$n: 主表被 VPN 前缀污染（$main 条）"
    done

    sect "D. VPP：VRF 表有远端转发路径；K 路由没被 linux-nl 镜像成 dpo-drop"
    for n in 1 2 3; do
        for m in 1 2 3; do
            [ "$m" = "$n" ] && continue
            f=$(vppc "lab-cpe$n" show ip fib table 100)
            echo "$f" | grep -q "^10.100.$m.0/24" \
                && ok "lab-cpe$n: VPP table 100 有 10.100.$m.0/24" \
                || bad "lab-cpe$n: VPP table 100 缺 10.100.$m.0/24"
            echo "$f" | grep -A8 "^10.100.$m.0/24" | grep -q "dpo-drop" \
                && bad "lab-cpe$n: 10.100.$m.0/24 是 dpo-drop（K 覆盖了 sr-fib）" \
                || ok "lab-cpe$n: 10.100.$m.0/24 不是 dpo-drop"
        done
    done

    sect "E. touch 窗口：/config + refresh_vrfs 原地重刷，不重启进程"
    local pid0 ver0 n0 cfg0
    pid0=$(srfrr_status lab-cpe1 | python3 -c 'import json,sys; print(json.load(sys.stdin)["pid"])')
    ver0=$(dx lab-cpe1 vtysh -c 'show bgp vrf vpn100 ipv4 unicast' 2>/dev/null |
        sed -n 's/^BGP table version is \([0-9]*\).*/\1/p')
    cfg0=$(srfrr_status lab-cpe1 | python3 -c 'import json,sys; print(json.load(sys.stdin)["config_hash"])')
    dx lab-cpe1 sh -c 'rm -f /tmp/mv-touch.log'
    docker exec -d lab-cpe1 sh -c 'exec timeout 7 ip -ts monitor route > /tmp/mv-touch.log 2>&1'
    sleep 1.5
    body=$(srfrr_body multi)
    code=$(srfrr_put lab-cpe1 "${body%\}},\"refresh_vrfs\":[0,100,101]}")
    [ "$code" = "200" ] && ok "lab-cpe1: touch PUT 200" || bad "lab-cpe1: touch PUT 返回 $code"
    sleep 2.5
    sleep 4   # 等 timeout 7 退出
    # 注意：不能用 `python3 - <<PY` 读日志——heredoc 会占掉 python 的 stdin，
    # sys.stdin 读到的是 EOF（和 fib_set 的坑一样）。脚本先存进变量，再 -c。
    py=$(cat <<'PY'
import re, sys, datetime
# ip -ts monitor route 的真实格式（实测）：
#   [2026-11-02T03:51:26.743022] Deleted 10.100.2.0/24 dev vpn100 table 100 scope link
#   [2026-11-02T03:51:26.743231] 10.100.2.0/24 dev vpn100 table 100 scope link
# 主表(254)不打印 table 字段，非主表才有。
line = re.compile(
    r'^\[([^\]]+)\]\s+(Deleted\s+)?'
    r'(\d+\.\d+\.\d+\.\d+/\d+)\s+dev\s+(\S+)'
    r'(?:\s+table\s+(\d+))?')
ev = []
for ln in sys.stdin.read().splitlines():
    m = line.match(ln)
    if not m:
        continue
    ts, dele, pfx, dev, table = m.groups()
    ev.append((ts, 'del' if dele else 'add', pfx, dev, table or '254'))
def ts(s):
    return datetime.datetime.fromisoformat(s).timestamp()
last_del = {}
for t, kind, pfx, dev, table in ev:
    if table != '100' or dev != 'vpn100':
        continue
    if kind == 'del':
        last_del[pfx] = ts(t)
    elif pfx in last_del:
        print('%s %d' % (pfx, round((ts(t) - last_del.pop(pfx)) * 1e6)))
PY
)
    gaps=$(dx lab-cpe1 cat /tmp/mv-touch.log | python3 -c "$py")
    echo "  .... table 100 每条 del->add 间隔(us): $(echo "$gaps" | tr '\n' ' ')"
    cnt=$(echo "$gaps" | grep -c .)
    worst=$(echo "$gaps" | awk '{if ($2 > m) m = $2} END {print m+0}')
    [ "$cnt" = 4 ] && ok "lab-cpe1: 4 条路由都完成了一次 del->add" \
        || bad "lab-cpe1: 只测到 $cnt 条 del->add"
    [ "${worst:-999999}" -lt 10000 ] \
        && ok "lab-cpe1: 最坏单条窗口 ${worst}us < 10ms" \
        || bad "lab-cpe1: 单条窗口 ${worst}us 超过 10ms"
    pid1=$(srfrr_status lab-cpe1 | python3 -c 'import json,sys; print(json.load(sys.stdin)["pid"])')
    cfg1=$(srfrr_status lab-cpe1 | python3 -c 'import json,sys; print(json.load(sys.stdin)["config_hash"])')
    ver1=$(dx lab-cpe1 vtysh -c 'show bgp vrf vpn100 ipv4 unicast' 2>/dev/null |
        sed -n 's/^BGP table version is \([0-9]*\).*/\1/p')
    [ "$pid0" = "$pid1" ] && ok "lab-cpe1: sr-frr pid 没变（$pid0）" \
        || bad "lab-cpe1: sr-frr 被重启（$pid0 -> $pid1）"
    [ "$cfg0" = "$cfg1" ] && ok "lab-cpe1: 配置指纹没变（touch 不改配置）" \
        || bad "lab-cpe1: 配置指纹变了"
    [ "$ver0" = "$ver1" ] && ok "lab-cpe1: BGP vrf 表版本号没动（$ver0，客户看不到抖动）" \
        || bad "lab-cpe1: BGP vrf 版本号 $ver0 -> $ver1"
    dx lab-cpe1 sh -c 'tail -n 20 /var/log/sr-frr.log | grep -q "touch vpn100"' \
        && ok "lab-cpe1: sr-frr 日志有 touch vpn100" || bad "lab-cpe1: 日志没有 touch 记录"
}

# ==================================================================
restore_baseline() {
    local n
    sect "R1. sr-frr / sr-route / FRR / 内核 设备全部回基线（热改 + 删设备）"
    for n in 1 2 3; do
        srfrr_put "lab-cpe$n" "$(srfrr_body base)" >/dev/null
    done
    retry 10 bash -c 'for n in 1 2 3; do
        docker exec lab-cpe$n ip route show table 100 | grep -q "dev vpn100" && exit 1
    done'
    for n in 1 2 3; do
        dsh "lab-cpe$n" 'ip link del dum100 2>/dev/null; ip link del dum101 2>/dev/null
            ip link del vpn100 2>/dev/null; ip link del vpn101 2>/dev/null'
        dx "lab-cpe$n" vtysh -c 'configure terminal' -c 'no vrf vpn100' -c 'no vrf vpn101' >/dev/null 2>&1
        frr_del "$n" && ok "lab-cpe$n: FRR 回到基线（frr-reload 0 差异）" \
            || bad "lab-cpe$n: FRR 没回到基线"
    done
    for n in 1 2 3; do
        [ "$(route_put "$n" base)" = "200" ] || bad "lab-cpe$n: PUT /vrfs 回基线失败"
    done
    retry 20 bash -c 'for n in 1 2 3; do
        [ "$(docker exec lab-cpe$n curl -s http://127.0.0.1:1900$n/status |
            python3 -c "import json,sys; print(\" \".join(sorted(json.load(sys.stdin)[\"rib\"])))")" = 0 ] || exit 1
    done'
    for n in 1 2 3; do
        fib_set "$n" '[0]'
        srfib_restart "$n" 1 && ok "lab-cpe$n: sr-fib 回到 1 个 VRF" \
            || bad "lab-cpe$n: sr-fib 没回到 1 个 VRF"
    done
    for n in 1 2 3; do
        [ "$(rib_keys "$n")" = "0" ] && ok "lab-cpe$n: RIB 只剩 VPN 0" \
            || bad "lab-cpe$n: RIB 还有 $(rib_keys "$n")"
        dsh "lab-cpe$n" 'ip link show | grep -qE "vpn10[01]|dum10[01]"' \
            && bad "lab-cpe$n: 内核 VRF 设备没删干净" || ok "lab-cpe$n: 内核 VRF 设备已删"
    done
    # 回归（#195）：删 VRF 设备/接口会让 zebra 把自己的 K 路由视图整表清掉，
    # 内核里还在、客户侧从此学不到远端前缀。sr-frr 的链路事件监听必须在一个
    # 去抖窗口内重放，所以这里量的不是"日志里有没有 touch"，而是 zebra 实际
    # 还能看见 4 条 vpn0 的 K 路由（= redistribute kernel 还在宣告）。
    retry 10 bash -c 'for n in 1 2 3; do
        [ "$(docker exec lab-cpe$n vtysh -c "show ip route" | grep -cE "^K>\* .* directly connected, lo")" = 4 ] || exit 1
    done'
    for n in 1 2 3; do
        k=$(dx "lab-cpe$n" vtysh -c 'show ip route' 2>/dev/null |
            grep -cE '^K>\* .* directly connected, lo')
        kk=$(dsh "lab-cpe$n" 'ip route show table 254 | grep -c " dev lo"')
        [ "$k" = 4 ] && [ "$kk" = 4 ] \
            && ok "lab-cpe$n: 删 VRF 设备后 zebra 仍见 4 条 K、内核仍 4 条" \
            || bad "lab-cpe$n: 删 VRF 设备后 zebra K=$k 内核=$kk（导出静默失效）"
        dsh "lab-cpe$n" 'tail -n 40 /var/log/sr-frr.log | grep -q "链路事件" ' \
            && ok "lab-cpe$n: sr-frr 日志有链路事件重放记录" \
            || bad "lab-cpe$n: sr-frr 没有链路事件记录"
    done
    retry 20 bash -c 'for pair in "lab-term1 10.68.2.10" "lab-term2 10.68.1.10" "lab-term1 10.68.3.10"; do
        set -- $pair; docker exec $1 ping -c 2 -W 1 $2 >/dev/null 2>&1 || exit 1
    done'
    docker exec lab-term1 ping -c 2 -W 1 10.68.2.10 >/dev/null 2>&1 \
        && ok "基线数据面：term1 -> term2 通" || bad "基线数据面：term1 -> term2 不通"
}

# ==================================================================
BASELINE_PIDS=""
for n in 1 2 3; do
    BASELINE_PIDS="$BASELINE_PIDS lab-cpe$n[$(pids_of "lab-cpe$n")]"
done

setup
verify_scenario

sect "G. 进程不重启（sr-fib 除外，见文件头）"
NOW_PIDS=""
for n in 1 2 3; do
    NOW_PIDS="$NOW_PIDS lab-cpe$n[$(pids_of "lab-cpe$n")]"
done
[ "$BASELINE_PIDS" = "$NOW_PIDS" ] \
    && ok "sr-frr/sr-route/FRR 守护进程 pid 全程未变" \
    || bad "有守护进程被重启：$BASELINE_PIDS -> $NOW_PIDS"

if [ "$KEEP" = "1" ]; then
    echo
    echo "== --keep：多 VRF 场景保留，恢复请重跑本脚本（不带 --keep）"
else
    restore_baseline
    END_PIDS=""
    for n in 1 2 3; do
        END_PIDS="$END_PIDS lab-cpe$n[$(pids_of "lab-cpe$n")]"
    done
    [ "$BASELINE_PIDS" = "$END_PIDS" ] \
        && ok "恢复后 sr-frr/sr-route/FRR 守护进程 pid 仍与开始时一致" \
        || bad "恢复过程重启了守护进程：$BASELINE_PIDS -> $END_PIDS"
fi

echo
echo "================================"
echo "总计: $PASS 通过, $FAIL 失败"
exit $((FAIL > 0))
