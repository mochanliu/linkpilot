#!/bin/bash
# VM lab 一台 CPE 盒子的装包 + 落产物 + 管理通道形状。在盒子里以 root 跑，参数从 /root/vmlab/box.conf 读。
# 阶段可单跑（--phase A|B|C|D|all），顺序是有讲究的：
#   A 装包  B 落产物  C 管理通道形状（此时物理口地址还在，SSH 不会丢）  D 才把 netplan 里的地址注释掉。
# D 放最后：C 一跑，underlay.sh 就把地址搬到 TAP 上了；先注释 netplan 再装 VPP，
# 中途 VPP 起不来就成了没有地址、只能从虚拟机控制台进去的盒子。
set -u
CONF=${BOX_CONF:-/root/vmlab/box.conf}
[ -r "$CONF" ] || { echo "FAIL 缺 $CONF（由 Mac 侧 bringup-mac.sh 生成）"; exit 1; }
. "$CONF"

PHASE=all
while [ $# -gt 0 ]; do
    case "$1" in
        --phase) PHASE=$2; shift 2;;
        *) echo "用法：vm-bringup.sh [--phase all|A|B|C|D]"; exit 1;;
    esac
done

VPP_VER=${BOX_VPP_VER:-26.06-release}
FRR_VER=${BOX_FRR_VER:-10.7.1-0~ubuntu24.04.1}
WANT_SR_SHA=${BOX_SR_SHA:?box.conf 里要有 sr_wg_plugin.so 的 sha256}
WANT_CRYPTO_SHA=${BOX_CRYPTO_SHA:?box.conf 里要有 crypto_openssl_plugin.so 的 sha256}
PLUGDIR=/usr/lib/x86_64-linux-gnu/vpp_plugins
KEYDIR=/etc/apt/keyrings

say() { echo "[$(date +%H:%M:%S)] $*"; }
disk() { df -h / | tail -1 | awk '{print "已用 "$3"/"$2" 剩 "$4}'; }
fail() { echo "FAIL $*"; exit 1; }

# ---------------------------------------------------------------- A 装包
if [ "$PHASE" = all ] || [ "$PHASE" = A ]; then
say "A0 装前磁盘：$(disk)"
rm -rf /var/lib/apt/lists/*; apt-get clean

mkdir -p "$KEYDIR"
# 钥匙的 URL 是 /gpgkey：/gpg 现在 404（2026-11-04 实测），照发行版文档抄会卡在这一步。
# 用显式 if，不要写成 `[ -f 钥匙 ] || curl … && { … } || fail`：那种串在 bash 里是
# `((A || B) && C) || D`，A 成立时照样往下跑 C —— CPE4 的钥匙本来就在，却被判成"拿不到钥匙"（2026-11-04 踩过）。
if [ ! -f "$KEYDIR/fdio_release-archive-keyring.gpg" ]; then
    curl -fsSL https://packagecloud.io/fdio/release/gpgkey -o /tmp/fdio.asc || fail "拿不到 fdio/release 的 GPG 钥匙"
    [ -s /tmp/fdio.asc ] || fail "fdio 的 GPG 钥匙是空的"
    gpg --dearmor < /tmp/fdio.asc > "$KEYDIR/fdio_release-archive-keyring.gpg" || fail "fdio 钥匙转二进制失败"
    rm -f /tmp/fdio.asc
fi
cat > /etc/apt/sources.list.d/fdio_release.list <<EOF
deb [signed-by=$KEYDIR/fdio_release-archive-keyring.gpg] https://packagecloud.io/fdio/release/ubuntu/ noble main
EOF

if [ ! -f "$KEYDIR/frr.gpg" ]; then
    curl -fsSL https://deb.frrouting.org/frr/keys.asc -o /tmp/frr.asc || fail "拿不到 frr 的 GPG 钥匙"
    gpg --dearmor < /tmp/frr.asc > "$KEYDIR/frr.gpg" || fail "frr 钥匙转二进制失败"
    rm -f /tmp/frr.asc
fi
echo "deb [signed-by=$KEYDIR/frr.gpg] https://deb.frrouting.org/frr noble frr-stable" > /etc/apt/sources.list.d/frr.list

apt-get update > /tmp/apt-update.log 2>&1 || { tail -20 /tmp/apt-update.log; fail "apt-get update 失败"; }
got=$(apt-cache policy vpp 2>/dev/null | grep -c "$VPP_VER")
[ "$got" != 0 ] || fail "apt 源里没有 vpp $VPP_VER（该源默认候选是 26.10，不钉就飘）"
got=$(apt-cache policy frr 2>/dev/null | grep -c "$FRR_VER")
[ "$got" != 0 ] || fail "apt 源里没有 frr $FRR_VER"
# 大页必须赶在装 VPP 之前关掉。CPE1 实测（2026-11-04）：先装后关会卡死——
# vpp 包的 postinst 把 vpp.service 拉起来，发行版 80-vpp.conf 这时已经抢下 818 页(1636MB)，
# 1967MB 的盒子只剩 6MB available，apt 的 Post-Invoke 钩子 apt-check 直接僵住（1 分 44 秒没进展），
# 装包这条命令根本不返回。顺序改成「写清单 → 立刻释放已有页 → 再装」就不给它抢内存的机会。
cat > /etc/sysctl.d/zz-vm-lab-vpp.conf <<'EOF'
# vm lab：2GB 的 VM 起不动大页，VPP 靠主堆跑（见 startup 里 memory 段）。
# 这个文件名比发行版的 80-vpp.conf(要 1024 页) 靠后，开机时 systemd-sysctl 以这份为准。
vm.nr_hugepages=0
EOF
systemctl stop vpp > /dev/null 2>&1
sysctl -w vm.nr_hugepages=0 > /dev/null 2>&1

apt-get install -y --no-install-recommends \
    "vpp=$VPP_VER" "libvppinfra=$VPP_VER" "vpp-plugin-core=$VPP_VER" \
    > /tmp/apt-vpp.log 2>&1 || { tail -25 /tmp/apt-vpp.log; fail "装 vpp 三件失败"; }
apt-get install -y --no-install-recommends \
    "frr=$FRR_VER" "frr-pythontools=$FRR_VER" \
    > /tmp/apt-frr.log 2>&1 || { tail -25 /tmp/apt-frr.log; fail "装 frr $FRR_VER 失败"; }

sysctl --system > /dev/null 2>&1

rm -rf /var/lib/apt/lists/*; apt-get clean
say "A1 装完：$(dpkg -l | awk '/^ii  (vpp|libvppinfra|vpp-plugin-core|frr) /{printf "%s=%s ", $2, $3}')"
say "A2 大页：$(sysctl -n vm.nr_hugepages)（应为 0）"
say "A3 装后磁盘：$(disk)"
fi

# ---------------------------------------------------------------- B 落产物
if [ "$PHASE" = all ] || [ "$PHASE" = B ]; then
[ -d /root/vmlab/artifacts/plugins ] || fail "缺 /root/vmlab/artifacts/plugins（Mac 侧没 scp 上来）"
for f in sr_wg_plugin.so crypto_openssl_plugin.so; do
    want=$WANT_SR_SHA; [ "$f" = crypto_openssl_plugin.so ] && want=$WANT_CRYPTO_SHA
    have=$(sha256sum "/root/vmlab/artifacts/plugins/$f" | awk '{print $1}')
    [ "$have" = "$want" ] || fail "$f sha256=$have 期望=$want（ABI 不匹配会把 VPP 直接带崩）"
    install -m 0644 "/root/vmlab/artifacts/plugins/$f" "$PLUGDIR/$f"
done
install -m 0755 /root/vmlab/artifacts/linux/sr-agent /root/vmlab/artifacts/linux/sr-apply \
    /root/vmlab/artifacts/linux/sr-fib /root/vmlab/artifacts/linux/sr-frr \
    /root/vmlab/artifacts/linux/sr-route /root/vmlab/artifacts/linux/sr-stunprobe /usr/local/bin/
say "B1 插件：$(sha256sum "$PLUGDIR"/sr_wg_plugin.so "$PLUGDIR"/crypto_openssl_plugin.so | awk '{printf "%s ", substr($1,1,12)}')"
say "B2 Go：$(ls /usr/local/bin | tr '\n' ' ')"
fi

# ---------------------------------------------------------------- C 管理通道形状
if [ "$PHASE" = all ] || [ "$PHASE" = C ]; then
cat > /root/vmlab/startup-vmlab.conf <<EOF
# 由 vm-bringup.sh 生成（$HOST_LABEL）。
# unix{} 里**没有 exec**：接管放在 vpp.service 起来之后的 vmlab-underlay.service 里做。
# 写在 exec 里时一次 systemctl restart vpp 会稳定丢掉一条接管的创建（docs/vmlab-rebuild-plan.md §5.6(3)）。
unix {
  nodaemon
  cli-listen /run/vpp/cli.sock
  gid vpp
  log /var/log/vpp/vpp.log
}
memory { main-heap-size 256M }
buffers { buffers-per-numa 8192
  default data-size 2048 }
plugins {
  plugin linux_cp_plugin.so { enable }
  plugin linux_nl_plugin.so { enable }
}
EOF

mkdir -p /etc/systemd/system/vpp.service.d /var/log/vpp
cat > /etc/systemd/system/vpp.service.d/50-vmlab.conf <<EOF
# 覆盖发行版 ExecStart：跑 vm lab 的 startup（关大页/收主堆，见该文件注释）。
[Service]
ExecStart=
ExecStart=/usr/bin/vpp -c /root/vmlab/startup-vmlab.conf
EOF
install -m 0644 /root/vmlab/52-vmlab-underlay.conf /etc/systemd/system/vpp.service.d/
install -m 0644 /root/vmlab/vmlab-underlay.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable vpp.service vmlab-underlay.service > /dev/null 2>&1

printf '%s\t%s\t%s\t%s\t%s\n' "$IF_CUST" "$TAP_CUST" "$MAC_CUST" "$CIDR_CUST" "" > /root/vmlab/underlay.list
printf '%s\t%s\t%s\t%s\t%s\n' "$IF_WAN" "$TAP_WAN" "$MAC_WAN" "$CIDR_WAN" "$GW" >> /root/vmlab/underlay.list

chmod +x /root/vmlab/takeover-if.sh /root/vmlab/underlay.sh /root/vmlab/check-selfheal.sh
systemctl restart vpp.service
# pidof 有值不等于 CLI 口能连（CPE1 实测：restart 返回后 pid 先到、cli.sock 后监听，
# 中间那一拍 vppctl 报 "connect: Connection refused"）。所以两步都用有界等待，等到通为止。
i=0
while [ $i -lt 40 ]; do
    pid=$(pidof vpp); okcli=$(vppctl show version >/dev/null 2>&1; echo $?)
    [ -n "$pid" ] && [ "$okcli" = 0 ] && break
    i=$((i + 1)); sleep 0.25
done
[ -n "$pid" ] || { journalctl -u vpp -n 20 --no-pager | tail -20; fail "VPP 没起来（10 秒内 pidof 为空），后面不用跑了"; }
[ "$okcli" = 0 ] || { journalctl -u vpp -n 20 --no-pager | tail -20; fail "pid=$pid 在跑但 10 秒内 vppctl 连不上 CLI 口"; }
# VPP 认不出 lcp 命令 = linux_cp 插件没加载 = 跑的还是发行版 startup.conf，ExecStart 的 drop-in 没生效。
if ! vppctl show lcp > /dev/null 2>&1; then
    vppctl show lcp 2>&1 | sed 's/^/       /'
    ps -o args= -p "$pid" | sed 's/^/       /'
    fail "pid=$pid 的 VPP 不认 lcp 命令：ExecStart 的 drop-in 没生效（看 systemctl cat vpp）"
fi
say "C1 vpp pid=$pid linux_cp 已加载"
# vpp.service 的 ExecStartPost 会把 vmlab-underlay 拉起来；等它把两条 itf-pair 建出来，
# 等不到才自己补一次 restart（两个实例同时在跑会互相抢 create，所以不无脑补）
i=0
while [ $i -lt 30 ]; do
    n=$(vppctl show lcp 2>/dev/null | tr -d '\r' | grep -c '^itf-pair')
    [ "$n" = 2 ] && break
    i=$((i + 1)); sleep 0.25
done
if [ "$n" != 2 ]; then
    say "C1b 等 $(awk -v a=$i -v b=0.25 'BEGIN{printf "%.2f", a*b}') 秒只有 $n/2 条 itf-pair，手工补一次 restart vmlab-underlay"
    systemctl restart vmlab-underlay.service
    sleep 2
fi
sleep 1
say "C2 underlay: $(systemctl is-active vmlab-underlay.service)  形状：$(vppctl show lcp 2>/dev/null | tr -d '\r' | grep -c '^itf-pair') 条 itf-pair"
# 闸门不在这里跑：CPE3 实测（2026-11-04 14:49）此刻 networkd 还握着磁盘 yaml 里的地址，
# underlay 刚 flush 完它就补回来 ⇒ enp0s9 报「物理口还有地址 + l3 mac mismatch」两条 FAIL。
# 地址归属这条判据只有在阶段 D 收口之后才是可判的，所以挪到 D 的末尾。
# ip -br addr 的列是「接口 状态 地址…」，地址在第 3 列（写 $4 会打印成空的，别被吓到）
say "C3 地址：$(ip -br -4 addr show dev "$TAP_CUST" | awk '{print $3}') 在 $TAP_CUST、$(ip -br -4 addr show dev "$TAP_WAN" | awk '{print $3}') 在 $TAP_WAN"
fi

# ---------------------------------------------------------------- D netplan 收口
if [ "$PHASE" = all ] || [ "$PHASE" = D ]; then
# 被 VPP 接管的口一个全局地址都不许留：networkd 手里那份配置会在接管产生的链路事件上
# 把同一个 IP 补回物理口 ⇒ 双应答（S4-B 实测 1065 条 DUP）。
#
# 两步都不能省（CPE1 实测 2026-11-04）：
#   1) netplan generate —— 只改磁盘 yaml、只 restart networkd 是没用的。networkd 读的是
#      /run/systemd/network/10-netplan-<口>.network 这份「生成物」，restart 不会重新生成它，
#      于是旧文件里的 Address=192.168.1.101/24 被原封不动再配一遍（当时的 FAIL 就是这个）。
#   2) systemctl restart systemd-networkd —— 生成物换了不等于生效，得让 networkd 重读一遍
#      才会把已经配上的地址摘掉。
# 这里不用 netplan apply：它会连带把地址重新推一遍，推的时机落在接管之后就是双应答的来源。
cat > /etc/netplan/60-lab.yaml <<EOF
# 由 vm-bringup.sh 生成（$HOST_LABEL）：两个口都归 VPP，这里不留地址和路由。
# 地址由 vmlab-underlay.service 放到 TAP（$TAP_CUST / $TAP_WAN）上。
network:
  version: 2
  ethernets:
    $IF_CUST:
      optional: true
    $IF_WAN:
      nameservers:
        addresses: [114.114.114.114]
      optional: true
EOF
netplan generate || fail "netplan generate 失败（yaml 没写成/权限），生成物还是旧的"
gen=$(grep -c '^Address=' "/run/systemd/network/10-netplan-$IF_WAN.network" "/run/systemd/network/10-netplan-$IF_CUST.network" 2>/dev/null | awk -F: '{s+=$2} END{print s+0}')
say "D0 生成物里的 Address= 行数：$gen（应为 0）"
[ "$gen" = 0 ] || fail "生成物里还有 $gen 条 Address=：$IF_WAN/$IF_CUST 的磁盘 yaml 没写干净"
systemctl restart systemd-networkd
# 再看 20 秒：networkd 补地址不是立刻发生的，接管产生的链路事件会把它再触发一次，
# 所以「此刻是 0」不算过，要稳。
p=0; c=0
for w in 3 5 8 4; do
    sleep $w
    p=$(ip -o -4 addr show dev "$IF_WAN" 2>/dev/null | grep -c 'scope global')
    c=$(ip -o -4 addr show dev "$IF_CUST" 2>/dev/null | grep -c 'scope global')
    [ "$p" = 0 ] && [ "$c" = 0 ] || break
done
[ "$p" = 0 ] && [ "$c" = 0 ] || fail "物理口还留着全局地址（WAN=$p CUST=$c）：networkd 在抢"
say "D1 物理口全局地址：WAN=$p CUST=$c（观察 20 秒后仍为 0）"
say "D2 功能：$(ping -c2 -W1 -I "$TAP_WAN" "$GW" 2>&1 | tail -1)"
say "D3 功能：$(ping -c2 -W1 -I "$TAP_CUST" "$PEER_CUST" 2>&1 | tail -1)"
# 双口闸门：地址归属收口之后才是可判的（见 C2 末尾那条注释）
for spec in "$IF_CUST|$CIDR_CUST|$TAP_CUST|$PEER_CUST" "$IF_WAN|$CIDR_WAN|$TAP_WAN|$PEER_WAN"; do
    IFS='|' read -r i1 i2 i3 i4 <<< "$spec"
    if /root/vmlab/check-selfheal.sh "$i1" "$i2" "$i3" "$i4" after-bringup > "/tmp/gate-$i1.log" 2>&1; then
        say "D4 $i1 闸门 PASS=$(grep -c PASS "/tmp/gate-$i1.log") FAIL=0"
    else
        say "D4 $i1 闸门 FAIL："; grep -E 'FAIL' "/tmp/gate-$i1.log"; fail "$i1 的形状没过闸门"
    fi
done
fi

say "完成（phase=$PHASE）磁盘 $(disk)"
