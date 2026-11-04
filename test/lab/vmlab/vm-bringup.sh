#!/bin/bash
# VM lab 一台 CPE 盒子的装包 + 落产物 + 管理通道形状 + 入网 + 盒内管道 bootstrap。在盒子里以 root 跑，参数从 /root/vmlab/box.conf 读。
# 阶段可单跑（--phase A|B|C|D|E|F|G|all），顺序是有讲究的：
#   A 装包  B 落产物  C 管理通道形状（此时物理口地址还在，SSH 不会丢）  D 才把 netplan 里的地址注释掉
#   E 入网（身份 + 心跳 + 接口清单）  F 盒内三份管道配置 + FRR 空壳  G 装产品那份 sr-box 开机清单。
# D 放最后：C 一跑，underlay.sh 就把地址搬到 TAP 上了；先注释 netplan 再装 VPP，
# 中途 VPP 起不来就成了没有地址、只能从虚拟机控制台进去的盒子。
# E/F 只做 loop-bringup.sh 的 P-5/P-6 那两段（产品没有对应下发路径的本地管道字段 + FRR 起进程）。
# 隧道、每隧道端口、overlay /32、VRF/RT、导通组、RR 绑定、路由泄露、客户口地址、OSPF/BGP、VRRP、MTU
# 全部退回控制台 —— 这一枪里一条都不配，配了就是绕控制台。
# F 里那次 restart frr 是点名的中断：只在 running-config 还是空壳时允许（脚本里有闸门），
# 客户配置上线之后要改 daemons 只能走 frr-reload.py，本阶段不许重跑。
set -u
CONF=${BOX_CONF:-/root/vmlab/box.conf}
[ -r "$CONF" ] || { echo "FAIL 缺 $CONF（由 Mac 侧 bringup-mac.sh 生成）"; exit 1; }
. "$CONF"

PHASE=all
while [ $# -gt 0 ]; do
    case "$1" in
        --phase) PHASE=$2; shift 2;;
        *) echo "用法：vm-bringup.sh [--phase all|A|B|C|D|E|F|G]"; exit 1;;
    esac
done

VPP_VER=${BOX_VPP_VER:-26.06-release}
FRR_VER=${BOX_FRR_VER:-10.7.1-0~ubuntu24.04.1}
# agent 的身份目录：入网写 device key / 证书 / state.json 的地方，也是 F 阶段那份 route.json
# 里 identity_dir 指的地方。两者必须同一个目录 —— sr-route 从这里读证书，指错就报「没有证书」。
IDENT=${IDENT:-/etc/sdwan/identity}
WANT_SR_SHA=${BOX_SR_SHA:?box.conf 里要有 sr_wg_plugin.so 的 sha256}
WANT_CRYPTO_SHA=${BOX_CRYPTO_SHA:?box.conf 里要有 crypto_openssl_plugin.so 的 sha256}
PLUGDIR=/usr/lib/x86_64-linux-gnu/vpp_plugins
KEYDIR=/etc/apt/keyrings

say() { echo "[$(date +%H:%M:%S)] $*"; }
disk() { df -h / | tail -1 | awk '{print "已用 "$3"/"$2" 剩 "$4}'; }
fail() { echo "FAIL $*"; exit 1; }
# 身份目录里那份 state.json 是 sr-agent 写的（E 阶段的产物），F 阶段拿它拼管道配置。
node_of()   { python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("node_id",""))'    "$IDENT/state.json" 2>/dev/null; }
tenant_of() { python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("tenant_id",""))'  "$IDENT/state.json" 2>/dev/null; }

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

# 第 6 列是真实 MTU（VBox 网卡的出生值，接管前在盒子里量到的）。af-packet 口在 VPP 里
# 出生记 9000，控制器拿上报值算隧道 MTU ⇒ 9000 会配出 8868 的隧道口，而 agent 按真实链路
# 算预算会拒装。所以接管时必须把 VPP 那份压回真实值，且只能显式传（见 takeover-if.sh 的 fix_mtu）。
printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$IF_CUST" "$TAP_CUST" "$MAC_CUST" "$CIDR_CUST" "-" "${MTU_CUST:?box.conf 里要有 MTU_CUST}" > /root/vmlab/underlay.list
printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$IF_WAN" "$TAP_WAN" "$MAC_WAN" "$CIDR_WAN" "$GW" "${MTU_WAN:?box.conf 里要有 MTU_WAN}" >> /root/vmlab/underlay.list

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

# ---------------------------------------------------------------- E 入网（身份 + 心跳 + 接口清单）
if [ "$PHASE" = all ] || [ "$PHASE" = E ]; then
CTRL=${CTRL_ADDR:?box.conf 里要有 CTRL_ADDR}:${CTRL_PORT:?box.conf 里要有 CTRL_PORT}
[ -s /root/vmlab/ca.pem ] || fail "缺 /root/vmlab/ca.pem：Mac 侧没把控制器 CA 推上来（bringup-mac.sh）"
mkdir -p "$IDENT" && chmod 700 "$IDENT"
# 0600：这是这台盒子用来担保控制器的唯一一份根证书，进不了别人的写权限。
install -m 0600 /root/vmlab/ca.pem "$IDENT/pinned_ca.pem"

# 起 agent：setsid + 重定向，让它脱离这次 ssh 会话活下来（容器版用 docker exec -d，这里是同一形状）。
# 入网码走 --claim 的位置参数，不写进 box.conf、不进任何日志文件；它会出现在 agent 自己的 argv 里
# （loop-bringup 同一口径），所以这台盒子上任何打印 argv 的调试都不许做。
start_agent() {
    local claim=${1:-}
    local argv=(/usr/local/bin/sr-agent
        --ctrl "$CTRL"
        --ca "$IDENT/pinned_ca.pem"
        --id "$IDENT" --serial "$HOST_LABEL"
        --preauth "$HOST_LABEL"
        --status 127.0.0.1:8089
        --vpp-api /run/vpp/api.sock)
    [ -n "$claim" ] && argv+=(--claim "$(cat "$claim")")
    setsid nohup "${argv[@]}" >>/var/log/sr-agent.log 2>&1 </dev/null &
}

before=$(node_of)
say "E0 入网前：node_id=[${before:-无}] sr-agent=[$(pgrep -x sr-agent | tr '\n' ' ')]"
if [ -n "$before" ]; then
    # 已有身份：这一枪只保证 agent 在跑（比如刚冷开、 systemd 单元还没装起来的场合），不再带码入网。
    if pgrep -x sr-agent >/dev/null; then
        say "E1 已有身份 node_id=$before，agent 在跑 —— 不动它"
    else
        say "E1 已有身份 node_id=$before，agent 没在跑 —— 不带码重新起"
        start_agent
    fi
else
    [ -s /root/vmlab/claim.txt ] || fail "缺 /root/vmlab/claim.txt（Mac 侧先跑 vm-console.py code，再由 bringup-mac.sh 推上来）"
    start_agent /root/vmlab/claim.txt
    # 判据不是 sr-agent 的退出码（它就不该退出），是「身份文件里出 node_id」——有界等待，等到为止。
    i=0
    while [ $i -lt 60 ]; do
        before=$(node_of)
        [ -n "$before" ] && break
        pgrep -x sr-agent >/dev/null || { tail -20 /var/log/sr-agent.log | sed 's/^/       /'; fail "sr-agent 已经退出且没有 node_id（日志在上面）"; }
        i=$((i + 1)); sleep 0.5
    done
    [ -n "$before" ] || { tail -20 /var/log/sr-agent.log | sed 's/^/       /'; fail "30 秒内 $IDENT/state.json 里没有 node_id"; }
    # 码是一次性名额（这张 4 次），用完就从盒子上删掉；Mac 侧那份仍留在 ~/srvmlab/state/claim.txt。
    rm -f /root/vmlab/claim.txt
    say "E1 入网拿到 node_id=$before（$i 拍 × 0.5 秒），盒子侧入网码已删"
fi

# 心跳：控制器侧「看到这台」的本地旁证是 agent 状态口的 heartbeats 计数（AGENTS.md §2 的前后对比）。
i=0; hb=""
while [ $i -lt 30 ]; do
    hb=$(curl -s --max-time 2 http://127.0.0.1:8089/status |
         python3 -c 'import json,sys;print(json.load(sys.stdin).get("heartbeats",""))' 2>/dev/null)
    case "$hb" in ''|*[!0-9]*) ;; *) [ "$hb" -ge 1 ] && break;; esac
    pgrep -x sr-agent >/dev/null || { tail -20 /var/log/sr-agent.log | sed 's/^/       /'; fail "等心跳时 sr-agent 退出了"; }
    i=$((i + 1)); sleep 1
done
case "$hb" in ''|*[!0-9]*) tail -20 /var/log/sr-agent.log | sed 's/^/       /'; fail "30 秒内 127.0.0.1:8089 没有 heartbeats≥1（控制器 $CTRL 连不上？）";; esac
say "E2 心跳 $hb 次（$i 秒），node_id=$(node_of) tenant=$(tenant_of) 序列号=$HOST_LABEL"
say "E3 身份目录：$(ls -1 "$IDENT" | tr '\n' ' ')"
fi

# ---------------------------------------------------------------- F 盒内管道 bootstrap（P-5 + P-6）
if [ "$PHASE" = all ] || [ "$PHASE" = F ]; then
NODE=$(node_of); TENANT=$(tenant_of)
[ -n "$NODE" ] && [ -n "$TENANT" ] || fail "先跑 --phase E：身份目录里没有 node_id/tenant_id"

# 三份本地管道配置：我是谁 / 状态口 / socket 路径。**rrs、tunnels、vrfs 一律留空是故意的**
# —— 这三个键归控制台的第 4 段，脚本里配上去就等于绕过控制台（loop-bringup 的 P-5 同口径）。
mkdir -p /etc/sdwan /var/lib/sdwan /var/lib/sr-frr /var/log/frr
cat > /etc/sdwan/route.json <<EOF
{
  "tenant": "${TENANT}",
  "device_id": ${NODE},
  "identity_dir": "${IDENT}",
  "serial": "${HOST_LABEL}",
  "hold_sec": 12,
  "tls_server_name": "${RR_ADDR:?box.conf 里要有 RR_ADDR}",
  "rrs": [],
  "tunnels": [],
  "vrfs": [],
  "status_addr": "127.0.0.1:19001"
}
EOF
cat > /etc/sdwan/fib.json <<EOF
{
  "status_url": "http://127.0.0.1:19001/status",
  "status_addr": "127.0.0.1:19011",
  "vpp_socket": "/run/vpp/api.sock",
  "poll_sec": 1,
  "vrfs": [0],
  "tunnels": []
}
EOF
cat > /etc/sr-frr.json <<EOF
{
  "status_url": "http://127.0.0.1:19001/status",
  "zserv_socket": "/run/frr/zserv.api",
  "state_file": "/var/lib/sr-frr/state.json",
  "poll_sec": 1,
  "export_vrfs": [],
  "import_rules": []
}
EOF
say "F1 管道三份就位：node=$NODE tenant=$TENANT serial=$HOST_LABEL（rrs/tunnels/vrfs 留空，等控制台）"

# P-6：FRR 起进程、配置是空壳。真配置由控制台的第 1 段经 frr-reload.py 差量下发。
# 这道闸门必须有：FRR 一旦带上客户配置（客户口地址 + router 段），重启就是断客户流量的操作，
# AGENTS.md §1 第 1 条禁止；那时候要改 daemons 只能走 frr-reload，不许再跑这一枪。
RUNNING=$(vtysh -c 'show running-config' 2>/dev/null | grep -cE '^(interface|router|ip route|ipv6 route) ')
[ "${RUNNING:-x}" = 0 ] || fail "盒内 FRR 已经有 $RUNNING 行配置（不是空壳）—— 这一步只许在控制台发布之前跑一次"
sed -i 's/^zebra=no/zebra=yes/; s/^bgpd=no/bgpd=yes/; s/^ospfd=no/ospfd=yes/; s/^staticd=no/staticd=yes/' /etc/frr/daemons
printf 'frr defaults traditional\nhostname %s\nlog file /var/log/frr/frr.log\nservice integrated-vtysh-config\n!\n' "$HOST_LABEL" > /etc/frr/frr.conf
touch /etc/frr/vtysh.conf
chown -R frr:frr /etc/frr
# 点名中断：下面这一次 restart frr 会让 zebra/bgpd/ospfd/staticd 全部换 pid。
# 此刻盒子里既没有客户配置（上面那道闸门刚证明 running-config 是空壳）也没有隧道，
# 客户可见影响 = 0；真配置上线之后这条路径归 frr-reload.py，本阶段不再重跑。
before_pids=$(pgrep -x zebra; pgrep -x bgpd; pgrep -x ospfd; pgrep -x staticd)
systemctl restart frr || { journalctl -u frr -n 20 --no-pager | tail -20; fail "systemctl restart frr 失败"; }
i=0
while [ $i -lt 40 ]; do
    vtysh -c 'show version' >/dev/null 2>&1 && break
    i=$((i + 1)); sleep 0.5
done
vtysh -c 'show version' >/dev/null 2>&1 || fail "vtysh 20 秒内没响应"
vtysh -c 'show running-config' | grep -q "^hostname $HOST_LABEL" || fail "空壳配置没应用（running-config 里没有 hostname $HOST_LABEL）"
after_pids=$(pgrep -x zebra; pgrep -x bgpd; pgrep -x ospfd; pgrep -x staticd)
say "F2 FRR 空壳已起：pid 前[$(echo $before_pids | tr '\n' ' ')] 后[$(echo $after_pids | tr '\n' ' ')]，$(vtysh -c 'show running-config' | grep -cE '^(interface|router|ip route|ipv6 route) ') 条业务配置行（应为 0）"
[ -S /run/frr/zserv.api ] && say "F3 zebra 的 ZAPI socket 在位：/run/frr/zserv.api" || fail "/run/frr/zserv.api 不在（sr-frr 起不来）"
systemctl enable frr > /dev/null 2>&1 && say "F4 frr.service enabled（冷开重放靠它）" || fail "frr.service enable 失败"
fi

# ---------------------------------------------------------------- G sr-box 开机清单（用产品那一份，不在 lab 里另写一套）
if [ "$PHASE" = all ] || [ "$PHASE" = G ]; then
SB=/usr/local/lib/sr-box
[ -s /root/vmlab/sr-box/start.sh ] || fail "Mac 侧没把 controller/deploy/sr-box/ 推上来（bringup-mac.sh 的推送清单）"
[ -n "$(node_of)" ] || fail "先跑 --phase E：身份目录里没有 node_id"
[ -s /etc/sdwan/route.json ] || fail "先跑 --phase F：/etc/sdwan/route.json 不在"

install -d -m 0755 "$SB"
install -m 0755 /root/vmlab/sr-box/start.sh "$SB/start.sh"
install -m 0644 /root/vmlab/sr-box/sr-box.service /root/vmlab/sr-box/sr-box-watch.service \
                /root/vmlab/sr-box/sr-box-watch.timer /etc/systemd/system/
mkdir -p /etc/sr-box
# 只写这台盒子与真盒子不一样的那几项：控制面在 Mac（box.conf 的 CTRL_ADDR:CTRL_PORT）、
# store 根在 /var/lib/sdwan/store（与 loop 台同口径）、VPP 由 vpp.service 管所以清单只等不启。
# 入网码和 pinned CA 都不进这里 —— 码是一次性的，CA 已经在身份目录里了。
cat > /etc/sr-box/sr-box.conf <<EOF
SR_BOX_AGENT_CTRL=${CTRL_ADDR:?}:${CTRL_PORT:?}
SR_BOX_AGENT_APPLY=1
SR_BOX_AGENT_TUNNELS=1
SR_BOX_APPLY_ROOT=/var/lib/sdwan/store
SR_BOX_VPP_START=no
EOF
say "G1 清单装好：$SB/start.sh + 3 份薄壳单元 + /etc/sr-box/sr-box.conf（隧道/配置从此走控制台）"

# 换 agent 的参数必须由人停一次：清单的规矩是"已在跑的进程绝不重启"（start.sh 头部第 2 条），
# 它见到 sr-agent 在跑就直接报 running 过去，不会把 E 阶段那次不带 -apply/-tunnels 的替换掉。
# 点名中断：这一次停 + 起实测约 2 秒，客户可见影响 0 —— 此刻隧道 0 条、配置 0 版（还没发布过），
# 数据面没有任何一个包经过 sr-agent；断的只是控制器这一侧的心跳一拍。
pkill -x sr-agent 2>/dev/null
i=0
while pgrep -x sr-agent >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -gt 20 ] && fail "sr-agent 停了 10 秒还在（不做强杀第二遍，先手工查）"
    sleep 0.5
done
say "G2 E 阶段那次不带 -apply/-tunnels 的 agent 已停（$i 拍 × 0.5 秒）"

# 第一遍 up：agent 会以 -apply -tunnels 起来；route/fib/bridge 现在起不来是**预期**的，
# 因为 /etc/sdwan/route.json 里的 rrs 还是空的（那份要等控制台发布、agent 落第 4 段）。
# 所以上面那行 g_rc 允许非 0，非 0 的那几步由 sr-box-watch.timer 每 60 秒补齐。
say "G3 第一遍 up（此刻 rrs 未落盘，route/fib/bridge 报 SKIP/FAIL 是预期的）"
"$SB/start.sh" up
say "G3 up 跑完（清单自己的退出码见上面每一步的结论行）"
systemctl daemon-reload
systemctl enable sr-box.service sr-box-watch.timer >/dev/null 2>&1 || fail "sr-box 单元 enable 失败"
systemctl start sr-box-watch.timer || fail "sr-box-watch.timer 起不来"
say "G4 常驻：sr-box.service enabled（开机跑一遍）+ sr-box-watch.timer 在跑（每 60 秒补哪步缺哪步）"
say "G5 现状 check：$("$SB/start.sh" check | tr '\n' '|' | sed 's/|$//')"
fi

say "完成（phase=$PHASE）磁盘 $(disk)"
