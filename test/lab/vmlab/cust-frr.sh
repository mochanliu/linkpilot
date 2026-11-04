#!/bin/bash
# VM lab 的客户路由器（cust12/cust3/cust4）：只装 FRR + 打开四个 daemon，不配邻居。
# 版本必须和 CPE 盒内那份一致（同一个 apt 源、同一个 FRR_VER），否则两边的
# `show running-config` 形状不同，frr-reload.py 的差量就不是同一口径。
# 邻居/前缀是第二阶段（cust-config.sh）的事：lab 值手工配、控制器不猜客户网络。
set -u
FRR_VER=${CUST_FRR_VER:-10.7.1-0~ubuntu24.04.1}
KEYDIR=/etc/apt/keyrings
say()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "FAIL $*"; exit 1; }
disk() { df -h / | tail -1 | awk '{print "已用 "$3"/"$2" 剩 "$4}'; }

say "0 装前磁盘：$(disk)"
rm -rf /var/lib/apt/lists/*; apt-get clean
mkdir -p "$KEYDIR"
# 显式 if，不写成 `[ -f 钥匙 ] || curl … && { … } || fail`：bash 会把那串解析成
# `((A||B)&&C)||D`，钥匙本来就在位时 C 照样跑，撞上一次多余的 dearmor（同一条 bug 在
# vm-bringup.sh 的 vpp 钥匙块上把 CPE4 卡过 10 分钟）。
if [ ! -f "$KEYDIR/frr.gpg" ]; then
    curl -fsSL https://deb.frrouting.org/frr/keys.asc -o /tmp/frr.asc || fail "拿不到 frr 的 GPG 钥匙"
    gpg --dearmor < /tmp/frr.asc > "$KEYDIR/frr.gpg" || fail "frr 钥匙转二进制失败"
    rm -f /tmp/frr.asc
fi
echo "deb [signed-by=$KEYDIR/frr.gpg] https://deb.frrouting.org/frr noble frr-stable" > /etc/apt/sources.list.d/frr.list
apt-get update > /tmp/apt-update.log 2>&1 || { tail -20 /tmp/apt-update.log; fail "apt-get update 失败"; }
got=$(apt-cache policy frr 2>/dev/null | grep -c "$FRR_VER")
if [ "$got" = 0 ]; then
    fail "apt 源里没有 frr $FRR_VER（候选 $(apt-cache policy frr 2>/dev/null | awk '/Candidate:/{print $2}')，发行版自带那个版本不对）"
fi
apt-get install -y --no-install-recommends \
    "frr=$FRR_VER" "frr-pythontools=$FRR_VER" \
    > /tmp/apt-frr.log 2>&1 || { tail -25 /tmp/apt-frr.log; fail "装 frr $FRR_VER 失败"; }
rm -rf /var/lib/apt/lists/*; apt-get clean
say "1 装完：$(dpkg -l | awk '/^ii  frr/{printf "%s=%s ", $2, $3}')；磁盘 $(disk)"

# 这台是客户路由器，不是盒子：没有 VPP、没有 linux-cp，路由全部走内核 FIB。
sed -i 's/^zebra=no/zebra=yes/; s/^bgpd=no/bgpd=yes/; s/^ospfd=no/ospfd=yes/; s/^staticd=no/staticd=yes/' /etc/frr/daemons
HOST_LABEL=${HOST_LABEL:-$(hostname)}
printf 'frr defaults traditional\nhostname %s\nlog file /var/log/frr/frr.log\nservice integrated-vtysh-config\n!\n' \
    "$HOST_LABEL" > /etc/frr/frr.conf
touch /etc/frr/vtysh.conf
chown -R frr:frr /etc/frr
# 点名中断：下面这一次 restart frr 会让 zebra/bgpd/ospfd/staticd 全部换 pid。
# 此刻这台上面还没有任何邻居与前缀（刚装完的空壳），所以客户可见影响 = 0；
# 邻居建立之后要改 daemons 只能走 frr-reload.py，本阶段不许重跑这一枪。
systemctl restart frr || { journalctl -u frr -n 20 --no-pager | tail -20; fail "systemctl restart frr 失败"; }
systemctl enable frr > /dev/null 2>&1 || fail "systemctl enable frr 失败"
i=0
until vtysh -c 'show running-config' > /dev/null 2>&1; do
    i=$((i + 1)); [ "$i" -gt 30 ] && fail "vtysh 15 秒不回话（journalctl -u frr）"; sleep 0.5
done
say "2 frr 起来了：zebra=$(pidof zebra) bgpd=$(pidof bgpd) ospfd=$(pidof ospfd) staticd=$(pidof staticd)"
say "OK 只装完、没配邻居（等第二阶段 cust-config.sh）"
