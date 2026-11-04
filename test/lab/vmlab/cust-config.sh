#!/bin/bash
# VM lab 客户路由器（cust12/cust3/cust4）的第一份 FRR 配置：只播自己的客户前缀。
#
# 逐行抄的参照：容器环路台 test/lab/loop-bringup.sh:300-347（两份 heredoc），
# 数值来自那张表：loop 172.16.12.1 / 172.16.3.1 / 172.16.4.1（loop-bringup.sh:93-95）、
# 盒子 AS 65103/65104 与客户 AS 65003/65004（:97-98，与 vm-console.py 的 BOX 表同源）。
#
# 相对参照刻意去掉的三样（每一样都是你拍的定案，不是我加的）：
#   1. `ip route 0.0.0.0/0 …`：站点 12 有 OSPF、站点 3/4 有 eBGP，能到哪儿是学来的，
#      不在客户路由器上手写任何静态路由。
#   2. `network ${lan}` / 播 LAN 前缀：不建客户终端 ⇒ 路由器后面没有 LAN 子网，
#      客户 IP 一律用 loopback 播。
#   3. VRRP 虚地址：这轮不发 VRRP 配置，所以这里不出现任何指向 VIP 的东西。
#
# 生效方式：frr-reload.py 差量热更（AGENTS.md §1.1 配置生效路径里不许重启 daemon）。
# 此刻这台上面还没有邻居、没有宣告前缀，所以这一枪的客户可见影响 = 0；
# 邻居 Established 之后再改这份文件，只能走同一条 --reload 路径。
#
# 用法：在 cust VM 上跑 `bash /root/cust-config.sh <cust12|cust3|cust4>`
set -u
ROLE=${1:?cust12|cust3|cust4}
RELOAD=${FRR_RELOAD:-/usr/lib/frr/frr-reload.py}
say()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "FAIL $*"; exit 1; }
vty()  { vtysh -c "$1" 2>&1 | tr -d '\r'; }

# X = 这台面向站点 LAN 的地址（也是 router-id），LOOP = 播出去的客户 IP，
# BOX/BOX_NAME = 对端盒子的客户口地址（照 loop-bringup.sh 里 box= 那一项）。
case "$ROLE" in
    # 站点 12：两台盒子（CPE1/CPE2）+ cust12 跑 OSPF area 0 —— 环路的来源，照抄参照。
    # OSPF 不认对端地址，所以这里没有邻居列表：邻接由 network 语句自己碰出来。
    cust12) PROTO=ospf; X=172.31.12.13; LOOP=172.16.12.1 ;;
    # 站点 3/4：各自和一台盒子跑 eBGP，只播 loopback。
    cust3)  PROTO=bgp; X=172.31.3.2;  LOOP=172.16.3.1; MY_AS=65003; BOX=172.31.3.1; BOX_AS=65103; BOX_NAME=CPE3 ;;
    cust4)  PROTO=bgp; X=172.31.4.2;  LOOP=172.16.4.1; MY_AS=65004; BOX=172.31.4.1; BOX_AS=65104; BOX_NAME=CPE4 ;;
    *) fail "未知角色 $ROLE" ;;
esac

# frr-reload.py --test 的差量按产品同一口径读：只看 "Lines To Add/Delete" 两节里的行
# （controller/internal/apply/apply.go:911 frrDelta）。它不打印 +/- 前缀，
# 拿 grep '^[+-]' 去数是恒等于 0 的假判据（cust12 第一枪就是这么蒙过去的）。
delta() {
    python3 - "$1" <<'PY'
import sys
in_delta, lines = False, []
for raw in open(sys.argv[1], errors="replace"):
    t = raw.strip()
    if t in ("Lines To Delete", "Lines To Add"):
        in_delta = True
    elif not in_delta or t == "" or t.startswith("==="):
        pass
    else:
        lines.append(t)
print(len(lines))
for x in lines[:8]:
    print("      " + x, file=sys.stderr)
PY
}

report() {
    echo "  running-config 行数=$(vty 'show running-config' | grep -c .)"
    echo "  路由条目数=$(vty 'show ip route' | grep -cE '^[A-Z]')"
    echo "  zebra=$(pgrep -x zebra || echo 无) ospfd=$(pgrep -x ospfd || echo 无) bgpd=$(pgrep -x bgpd || echo 无)"
    vty 'show ip ospf neighbor' | grep -E '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' | sed 's/^/  ospf邻居: /'
    vty 'show bgp summary' 2>/dev/null | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' | sed 's/^/  bgp邻居: /'
}

echo "== 改前基线 [$ROLE] =="
report

{
    printf 'frr defaults traditional\nhostname %s\nlog file /var/log/frr/frr.log\nservice integrated-vtysh-config\n!\n' "$ROLE"
    printf 'interface lo\n ip address %s/32\n!\n' "$LOOP"
    if [ "$PROTO" = ospf ]; then
        printf 'router ospf\n ospf router-id %s\n log-adjacency-changes\n network %s/32 area 0\n network %s/32 area 0\n!\n' \
            "$X" "$X" "$LOOP"
    else
        printf 'router bgp %s\n bgp router-id %s\n no bgp ebgp-requires-policy\n' "$MY_AS" "$X"
        printf ' neighbor %s remote-as %s\n neighbor %s description %s\n' "$BOX" "$BOX_AS" "$BOX" "$BOX_NAME"
        printf ' address-family ipv4 unicast\n  network %s/32\n exit-address-family\n!\n' "$LOOP"
    fi
} > /etc/frr/frr.conf || fail "写 /etc/frr/frr.conf 失败"
chown frr:frr /etc/frr/frr.conf; chmod 640 /etc/frr/frr.conf
echo "== 要下发的配置 =="
sed 's/^/  /' /etc/frr/frr.conf

[ -x "$RELOAD" ] || fail "找不到 $RELOAD（frr-pythontools 没装上？）"
python3 "$RELOAD" --test /etc/frr/frr.conf > /tmp/reload-test-before.log 2>&1
BEFORE=$(delta /tmp/reload-test-before.log)
[ "$BEFORE" != 0 ] || fail "改前差量就是 0：要么这份配置早就生效过，要么 delta() 又没读到节 ⇒ 先别往下走"
say "差量预览：$BEFORE 行还没生效（明细见下）"
python3 "$RELOAD" --reload /etc/frr/frr.conf --stdout --confdir /etc/frr > /tmp/reload.log 2>&1 ||
    { tail -30 /tmp/reload.log; fail "frr-reload.py --reload 失败（按 §1.5 不 fallback 到 restart）"; }

# 复检：以"前后状态对比"判定，不看退出码。
python3 "$RELOAD" --test /etc/frr/frr.conf > /tmp/reload-test-after.log 2>&1
LEFT=$(delta /tmp/reload-test-after.log)
[ "$LEFT" = 0 ] || { tail -20 /tmp/reload-test-after.log | sed 's/^/  /'; fail "热更后仍剩 $LEFT 条差异"; }
say "OK frr-reload.py --test 差量 $BEFORE 行 → 0 行"
[ "$PROTO" = ospf ] && vty 'show running-config' | grep -A4 'router ospf' | sed 's/^/  /'
[ "$PROTO" = bgp ]  && vty 'show running-config' | grep -A6 'router bgp'  | sed 's/^/  /'
LP=$(vty 'show ip route' | grep -F "$LOOP")
[ -n "$LP" ] && echo "  本端前缀: $LP" || fail "loopback $LOOP 没进本机路由表（zebra 没把它装上）"
echo "== 改后 [$ROLE] =="
report
say "说明：邻居此刻 Idle/Active 属正常 —— 盒子那侧的 OSPF/BGP 由控制台基础配置下发，S6 发布后才 Established"
