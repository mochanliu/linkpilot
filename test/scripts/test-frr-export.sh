#!/bin/bash
# test-frr-export.sh - sr-frr 把控制面 RIB 导出成 FRR 非主表路由（任务 #52）
#
# 这条脚本用一个**一次性的 FRR 10.7.1 容器**验证 ZAPI 接缝，四个问题各对应
# 一组断言：
#
#   (A) 前缀进了 FRR 的旁路表（table 1000/1001）和**内核同名表**，而主表
#       （FRR RIB 与内核 main）保持干净 —— 后者是这套方案存在的理由：
#       linux-cp 只把内核表 N 同步成 VPP table N，N > 255 就不会和 sr-fib 的
#       VRF 表（1..255）撞号，也不会污染 VPP table 0。
#   (B) 客户端实例用 `redistribute table-direct N` 真的能把它们拿进 BGP
#       （下一跳 0.0.0.0、origin ?），即"给客户路由看的事实"确实成立。
#   (C) 没有可解析下一跳的前缀不导出：sr-fib 不会把它装进 FIB，导出去只会
#       把流量吸到这台 CPE 上再黑洞掉。
#   (D) 生命周期的三种结束方式都收得干净：正常 SIGTERM 逐条撤回并删状态
#       文件；SIGKILL 留下的残留由下次启动按状态文件对账撤掉；zebra 自己
#       重启时客户端能发现断链、重连并重发整份计划。
#
# 为什么不在共享 lab 的 srwg-* 里做：这些容器跑的是我们自己的 VPP 数据面，
# FRR 只是客户侧的路由进程，两者不该互相污染。这里只用 srwg-node1 的 Go
# 工具链构建 sr-frr（主机没有可用的 Go 1.22）。
#
# 前置：quay.io/frrouting/frr:10.7.1 已在本地；srwg-node1 在跑（借它构建）。
#
# 用法:
#   test/scripts/test-frr-export.sh
set -u

C=srwg-frr-e2e
BUILD=${BUILD:-srwg-node1}
IMAGE=${IMAGE:-quay.io/frrouting/frr:10.7.1}
TMPBIN=/tmp/sr-frr-e2e-bin

PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); echo "  [PASS] $*"; }
bad() { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
die() { echo "FATAL: $*" >&2; exit 2; }

cleanup() {
    docker rm -f "$C" >/dev/null 2>&1
    rm -f "$TMPBIN"
    docker exec "$BUILD" rm -f /tmp/sr-frr >/dev/null 2>&1
}
trap cleanup EXIT

docker image inspect "$IMAGE" >/dev/null 2>&1 \
    || die "缺少镜像 $IMAGE：先 docker pull $IMAGE"
docker inspect "$BUILD" >/dev/null 2>&1 \
    || die "$BUILD 没在跑：本脚本借它的 Go 工具链构建 sr-frr"

BGP_AS=${BGP_AS:-65000}
PFX1=${PFX1:-10.20.0.0/16}
PFX2=${PFX2:-10.21.0.0/24}
PFX_VRF2=${PFX_VRF2:-172.16.0.0/12}
PFX_NOPATH=${PFX_NOPATH:-10.99.0.0/16}
PFX_NEW=${PFX_NEW:-10.30.0.0/24}

vty()   { docker exec "$C" vtysh -c "$1"; }
sh_c()  { docker exec "$C" sh -c "$1"; }
rib_n() { vty "show ip route table $1" | grep -c '^T>\*'; }
rib_is()   { [ "$(rib_n "$1")" -eq "$2" ]; }
bgp_n() { vty "show bgp ipv4 unicast" | grep -c '\*>'; }
bgp_is()   { [ "$(bgp_n)" -eq "$1" ]; }
has_state() { sh_c 'test -s /tmp/sr-frr/state.json'; }
no_state()  { sh_c 'test ! -e /tmp/sr-frr/state.json'; }

# wait_for <秒> <命令...>：命令返回 0 即成功。
wait_for() {
    local deadline=$((SECONDS + $1)); shift
    while [ $SECONDS -lt $deadline ]; do
        "$@" && return 0
        sleep 0.5
    done
    return 1
}

write_status() {
    # $1 = 文件内容
    sh_c "mkdir -p /tmp/www && cat >/tmp/www/status <<'EOF'
$1
EOF"
}

echo "== 构建 sr-frr（$BUILD）"
docker exec "$BUILD" sh -c 'cd /workspace/controller && CGO_ENABLED=0 go build -o /tmp/sr-frr ./cmd/sr-frr' \
    || die "构建失败"
docker cp "$BUILD:/tmp/sr-frr" "$TMPBIN" >/dev/null || die "取二进制失败"

echo "== 启动一次性 FRR 容器"
docker rm -f "$C" >/dev/null 2>&1
# zebra 需要 CAP_SYS_ADMIN（privs_init），只给 NET_ADMIN 会直接起不来。
docker run -d --name "$C" \
    --cap-add NET_ADMIN --cap-add NET_RAW --cap-add SYS_ADMIN \
    "$IMAGE" >/dev/null || die "启动 $C 失败"
docker cp "$TMPBIN" "$C:/usr/local/bin/sr-frr" >/dev/null || die "拷二进制失败"

# zebra/staticd 总是启动；bgpd 要在 daemons 里打开。配置必须在 restart 之前
# 写好，docker-start 只在启动时通过 vtysh -b 应用一次。
sh_c 'cat >/etc/frr/frr.conf <<EOF
frr defaults traditional
hostname frr-e2e
log stdout
service integrated-vtysh-config

router bgp '"$BGP_AS"'
 bgp router-id 10.0.0.1
 no bgp ebgp-requires-policy
 address-family ipv4 unicast
  redistribute table-direct 1000
  redistribute table-direct 1001
 exit-address-family
EOF
touch /etc/frr/vtysh.conf
chown frr:frr /etc/frr/frr.conf /etc/frr/vtysh.conf
sed -i "s/^bgpd=no/bgpd=yes/" /etc/frr/daemons' || die "写 FRR 配置失败"
docker restart "$C" >/dev/null || die "重启 $C 失败"
wait_for 20 vty "show version" >/dev/null || die "FRR 没起来（zebra/vtysh 不可用）"
wait_for 20 sh_c 'vtysh -c "show running-config" | grep -q "redistribute table-direct 1000"' \
    || die "bgpd 没读到 redistribute table-direct 配置"

write_status '{"rib":{"1":[{"vrf_id":1,"prefix":"'"$PFX1"'","next_hops":["192.168.99.2"]},{"vrf_id":1,"prefix":"'"$PFX2"'","next_hops":["192.168.99.2","192.168.99.3"]},{"vrf_id":1,"prefix":"'"$PFX_NOPATH"'","next_hops":[]}],"2":[{"vrf_id":2,"prefix":"'"$PFX_VRF2"'","next_hops":["192.168.99.2"]}]}}'
sh_c 'cat >/tmp/sr-frr.json <<EOF
{"status_url":"http://127.0.0.1:9080/status","zserv_socket":"/run/frr/zserv.api","state_file":"/tmp/sr-frr/state.json","poll_sec":1,"distance":1,"metric":20,"vrfs":[{"vrf_id":1,"table_id":1000},{"vrf_id":2,"table_id":1001}]}
EOF'
docker exec -d "$C" python3 -m http.server 9080 --bind 127.0.0.1 --directory /tmp/www
wait_for 10 sh_c 'wget -qO- http://127.0.0.1:9080/status >/dev/null' >/dev/null \
    || die "状态服务器没起来"

echo
echo "== (A) -once：写进 FRR 旁路表 + 内核同名表，主表保持干净"
sh_c '/usr/local/bin/sr-frr -config /tmp/sr-frr.json -once' || die "sr-frr -once 失败"
wait_for 10 rib_is 1000 2 || true
wait_for 10 sh_c "ip route show table 1000 | grep -q 'blackhole $PFX1'" || true
wait_for 10 bgp_is 3 || true

[ "$(rib_n 1000)" -eq 2 ] && ok "table 1000 有 2 条 TABLE 路由" || bad "table 1000 条目数 $(rib_n 1000)"
[ "$(rib_n 1001)" -eq 1 ] && ok "table 1001 有 1 条 TABLE 路由" || bad "table 1001 条目数 $(rib_n 1001)"
vty "show ip route table 1000" | grep -q "$PFX1" && ok "$PFX1 在 table 1000" || bad "$PFX1 不在 table 1000"
vty "show ip route table 1000" | grep -q "$PFX_NOPATH" \
    && bad "$PFX_NOPATH（没有下一跳）不应被导出" || ok "没有下一跳的前缀未导出"
sh_c "ip route show table 1000" | grep -q "blackhole $PFX1" \
    && ok "内核 table 1000 是 blackhole $PFX1" || bad "内核 table 1000 没有 blackhole 路由"
sh_c "ip route show table 1001" | grep -q "blackhole $PFX_VRF2" \
    && ok "内核 table 1001 是 blackhole $PFX_VRF2" || bad "内核 table 1001 没有 blackhole 路由"
if vty "show ip route" | grep -qE "$PFX1|$PFX2|$PFX_VRF2"; then
    bad "FRR 主 RIB 被污染"
else
    ok "FRR 主 RIB 干净"
fi
if sh_c "ip route show table main" | grep -qE "$PFX1|$PFX2|$PFX_VRF2"; then
    bad "内核 main 表被污染"
else
    ok "内核 main 表干净"
fi

echo
echo "== (B) 客户实例 redistribute table-direct"
n=$(bgp_n)
[ "$n" -eq 3 ] && ok "BGP 里出现 3 条（redistribute table-direct 生效）" || bad "BGP 条目数 $n"
vty "show bgp ipv4 unicast" | grep -q "$PFX_VRF2" && ok "$PFX_VRF2 经 BGP 可见" || bad "$PFX_VRF2 不在 BGP 里"

echo
echo "== (C) 状态文件与 -reset"
has_state && ok "状态文件已落盘" || bad "状态文件不存在"
n=$(sh_c "grep -c '\"prefix\"' /tmp/sr-frr/state.json")
[ "$n" -eq 3 ] && ok "状态文件里有 3 条" || bad "状态文件条目数 $n"
sh_c '/usr/local/bin/sr-frr -config /tmp/sr-frr.json -reset' || die "sr-frr -reset 失败"
[ "$(rib_n 1000)" -eq 0 ] && [ "$(rib_n 1001)" -eq 0 ] \
    && ok "-reset 后两张表都空了" || bad "-reset 后仍有路由（1000=$(rib_n 1000) 1001=$(rib_n 1001)）"
no_state && ok "-reset 删掉了状态文件" || bad "状态文件还在"

echo
echo "== (D) 生命周期：SIGTERM 干净退出 / SIGKILL 后按状态文件对账"
sh_c 'rm -f /tmp/sr-frr/state.json; /usr/local/bin/sr-frr -config /tmp/sr-frr.json > /tmp/sr-frr.log 2>&1 & echo $! > /tmp/sr-frr.pid'
wait_for 20 rib_is 1000 2 || true
[ "$(rib_n 1000)" -eq 2 ] && ok "常驻模式收敛到 2 条" || bad "常驻模式没有收敛（$(rib_n 1000)）"
sh_c 'kill -TERM $(cat /tmp/sr-frr.pid)'
wait_for 10 no_state || true
[ "$(rib_n 1000)" -eq 0 ] && [ "$(rib_n 1001)" -eq 0 ] \
    && ok "SIGTERM 后路由撤销" || bad "SIGTERM 后仍有路由"
no_state && ok "SIGTERM 后状态文件删除" || bad "SIGTERM 后状态文件还在"

sh_c '/usr/local/bin/sr-frr -config /tmp/sr-frr.json > /tmp/sr-frr.log 2>&1 & echo $! > /tmp/sr-frr.pid'
wait_for 20 rib_is 1000 2 || true
sh_c 'kill -9 $(cat /tmp/sr-frr.pid)'
sleep 1
[ "$(rib_n 1000)" -eq 2 ] && ok "SIGKILL 后路由留在 FRR 里（zebra 不替客户端收尾）" || bad "SIGKILL 后路由不见了"
has_state && ok "SIGKILL 后状态文件还在" || bad "SIGKILL 后状态文件不见了"

write_status '{"rib":{"1":[{"vrf_id":1,"prefix":"'"$PFX_NEW"'","next_hops":["192.168.99.2"]}]}}'
sh_c '/usr/local/bin/sr-frr -config /tmp/sr-frr.json -once' || die "对账 -once 失败"
wait_for 10 rib_is 1000 1 || true
[ "$(rib_n 1000)" -eq 1 ] && vty "show ip route table 1000" | grep -q "$PFX_NEW" \
    && ok "重启对账：旧前缀撤掉、只剩新计划" || bad "重启对账后 table 1000 不对"
[ "$(rib_n 1001)" -eq 0 ] && ok "重启对账：vrf2 的旧前缀也撤掉了" || bad "vrf2 旧前缀残留"
n=$(sh_c "grep -c '\"prefix\"' /tmp/sr-frr/state.json")
[ "$n" -eq 1 ] && ok "状态文件更新为新计划（1 条）" || bad "状态文件条目数 $n"

echo
echo "== (E) zebra 重启：客户端重连并重发整份计划"
write_status '{"rib":{"1":[{"vrf_id":1,"prefix":"'"$PFX1"'","next_hops":["192.168.99.2"]},{"vrf_id":1,"prefix":"'"$PFX2"'","next_hops":["192.168.99.2"]}],"2":[{"vrf_id":2,"prefix":"'"$PFX_VRF2"'","next_hops":["192.168.99.2"]}]}}'
sh_c '/usr/local/bin/sr-frr -config /tmp/sr-frr.json > /tmp/sr-frr.log 2>&1 & echo $! > /tmp/sr-frr.pid'
wait_for 20 rib_is 1000 2 || true
[ "$(rib_n 1000)" -eq 2 ] && ok "重连前已收敛" || bad "重连前没有收敛（$(rib_n 1000)）"
old_pid=$(sh_c 'pidof zebra')
[ -n "$old_pid" ] && ok "杀掉 zebra（pid $old_pid）" || bad "找不到 zebra 进程"
sh_c "kill -9 $old_pid"
wait_for 10 sh_c '! pidof zebra >/dev/null' || true
# 不依赖 watchfrr 拉起：实测它会被残留的 zebra.vty 套接字骗成"存活"，
# 这次跑了 30s 也没重启；偶尔又会自己起来。清掉残留手动起，时序才确定。
if sh_c 'pidof zebra >/dev/null'; then
    ok "watchfrr 已重新拉起 zebra"
else
    sh_c 'rm -f /run/frr/zebra.pid /run/frr/zebra.vty'
    sh_c '/usr/lib/frr/zebra -d -F traditional -A 127.0.0.1 -s 90000000 >/dev/null 2>&1' \
        || bad "手动重启 zebra 失败"
fi
wait_for 10 sh_c 'pidof zebra >/dev/null' || true
new_pid=$(sh_c 'pidof zebra')
[ -n "$new_pid" ] && [ "$new_pid" != "$old_pid" ] \
    && ok "zebra 已重启（$old_pid → $new_pid）" || bad "zebra 没换成新进程（$old_pid → ${new_pid:-无}）"
wait_for 30 sh_c 'grep -q "重连" /tmp/sr-frr.log' || true
sh_c 'grep -q "重连" /tmp/sr-frr.log' && ok "发现 zebra 断链并重连" || bad "没有观察到重连"
wait_for 30 rib_is 1000 2 || true
[ "$(rib_n 1000)" -eq 2 ] && ok "重连后 table 1000 恢复 2 条" || bad "重连后没恢复（$(rib_n 1000)）"
n=$(sh_c "grep -c '收敛 routes+3' /tmp/sr-frr.log")
[ "$n" -ge 2 ] && ok "重连后重发了整份计划（收敛 $n 次）" || bad "重连后没有重发（收敛 $n 次）"
sh_c 'kill -TERM $(cat /tmp/sr-frr.pid)'
wait_for 10 no_state || true
[ "$(rib_n 1000)" -eq 0 ] && ok "收尾：路由已撤" || bad "收尾：路由残留"

echo
echo "  PASS=$PASS  FAIL=$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
