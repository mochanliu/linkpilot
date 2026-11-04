#!/bin/bash
# Mac 侧驱动：把装包脚本 + 产物 + 每台的参数推上去，然后在盒子里跑 vm-bringup.sh。
# 用法：bringup-mac.sh <CPE1|CPE2|CPE3|CPE4> [phase]
# 参数（口名/TAP/MAC/地址）全部手抄，控制器不猜：MAC 从 VBoxManage showvminfo 的烧录值取，
# takeover-if.sh 会拿它和 netdev 现值比对，不等就停手 —— 从活口读会把上一次失败接管留下的脏值焊死。
set -u
B=${1:?CPE1|CPE2|CPE3|CPE4}
PHASE=${2:-all}
REPO=/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan
SRC=/Users/louis/srvmlab/artifacts
VL=$REPO/test/lab/vmlab

SR_SHA=$(cd "$SRC/plugins" && shasum -a 256 sr_wg_plugin.so | awk '{print $1}')
CR_SHA=$(cd "$SRC/plugins" && shasum -a 256 crypto_openssl_plugin.so | awk '{print $1}')

case "$B" in
    CPE1) CIDR_WAN=192.168.1.101/24; CIDR_CUST=172.31.12.1/24
          MAC_WAN=08:00:27:c3:58:58; MAC_CUST=08:00:27:9f:f9:1f
          PEER_CUST=172.31.12.13;;
    CPE2) CIDR_WAN=192.168.1.102/24; CIDR_CUST=172.31.12.2/24
          MAC_WAN=08:00:27:50:6c:f5; MAC_CUST=08:00:27:4c:ed:b7
          PEER_CUST=172.31.12.13;;
    CPE3) CIDR_WAN=192.168.1.103/24; CIDR_CUST=172.31.3.1/24
          MAC_WAN=08:00:27:a1:84:93; MAC_CUST=08:00:27:c7:47:b8
          PEER_CUST=172.31.3.2;;
    CPE4) CIDR_WAN=192.168.1.104/24; CIDR_CUST=172.31.4.1/24
          MAC_WAN=08:00:27:9c:b6:b3; MAC_CUST=08:00:27:65:45:1d
          PEER_CUST=172.31.4.2;;
    *) echo "不认识这台：$B"; exit 1;;
esac
GW=192.168.1.1
# VBox 网卡的出生 MTU：2026-11-05 在接管前量的四台 × 两个口全是 1500（CPE2/3/4 的
# enp0s8/enp0s9 + CPE1 enp0s8）。接管之后内核侧会被 VPP 反写成 1544，所以这个数只能
# 在这里写死传进去，不能让盒子里的脚本去读活口。
MTU_WAN=1500
MTU_CUST=1500
# 控制面在 Mac 上（S2）：8444 是设备侧 TLS 入口，控制台 8088 只绑 127.0.0.1，盒子够不着也不需要。
CTRL_ADDR=192.168.1.4
CTRL_PORT=8444
# 反射器也在 Mac 上（18446/18447）。route.json 的 tls_server_name 用它：RR 证书的 SAN 就是这个 IP。
RR_ADDR=192.168.1.4
STATE=$HOME/srvmlab/state
CA=$HOME/srvmlab/ctrl/data/ca.pem

cat > /tmp/box.conf <<EOF
# 由 bringup-mac.sh 生成（$B，$(date +%Y-%m-%dT%H:%M:%S)）
HOST_LABEL=$B
IF_WAN=enp0s9
IF_CUST=enp0s8
TAP_WAN=vpp0
TAP_CUST=lcp-x0
CIDR_WAN=$CIDR_WAN
CIDR_CUST=$CIDR_CUST
MAC_WAN=$MAC_WAN
MAC_CUST=$MAC_CUST
MTU_WAN=$MTU_WAN
MTU_CUST=$MTU_CUST
GW=$GW
PEER_WAN=$GW
PEER_CUST=$PEER_CUST
CTRL_ADDR=$CTRL_ADDR
CTRL_PORT=$CTRL_PORT
RR_ADDR=$RR_ADDR
BOX_SR_SHA=$SR_SHA
BOX_CRYPTO_SHA=$CR_SHA
EOF

ssh -o BatchMode=yes "$B" 'mkdir -p /root/vmlab /root/vmlab/artifacts/plugins /root/vmlab/artifacts/linux /root/vmlab/sr-box' || exit 1
scp -o BatchMode=yes -q /tmp/box.conf "$B":/root/vmlab/
scp -o BatchMode=yes -q "$VL"/{vm-bringup.sh,takeover-if.sh,underlay.sh,check-selfheal.sh,vmlab-underlay.service,52-vmlab-underlay.conf} "$B":/root/vmlab/
# G 阶段要装的是产品那份开机清单（不是 lab 自己写的第六套启动逻辑）：真盒子上 sr-box.service
# 是薄壳、ExecStart 就是 start.sh up，VM 台装的是同一份文件与同一份单元。
scp -o BatchMode=yes -q "$REPO"/controller/deploy/sr-box/{start.sh,sr-box.service,sr-box-watch.service,sr-box-watch.timer} "$B":/root/vmlab/sr-box/

# 产物按哈希比对，只推盒子上缺的或对不上的那份。
# 2026-11-04 实测：管理口被 VPP 接管后 Mac→盒子的批量吞吐 <25KB/s，45MB 要跑 40 分钟
# （接管前走内核路径 45MB 用 3.16 秒）。无条件重推会让 phase C 之后的每一枪都卡在传输上，
# 而 ssh/心跳这类小包不受影响 —— 所以这里省的是批量，不是连接。
HAVE=$(ssh -o BatchMode=yes "$B" 'cd /root/vmlab/artifacts 2>/dev/null && sha256sum plugins/* linux/* 2>/dev/null')
PL=(); LN=()
for f in "$SRC"/plugins/*.so "$SRC"/linux/*; do
    rel=${f#"$SRC"/}
    want=$(shasum -a 256 "$f" | awk '{print $1}')
    got=$(printf '%s\n' "$HAVE" | awk -v k="$rel" '$2 == k {print $1}')
    [ "$got" = "$want" ] && continue
    case "$rel" in plugins/*) PL+=("$f");; *) LN+=("$f");; esac
done
[ ${#PL[@]} -gt 0 ] && scp -o BatchMode=yes -q "${PL[@]}" "$B":/root/vmlab/artifacts/plugins/
[ ${#LN[@]} -gt 0 ] && scp -o BatchMode=yes -q "${LN[@]}" "$B":/root/vmlab/artifacts/linux/
echo "== $B 参数已推上去（产物新推 $((${#PL[@]} + ${#LN[@]})) 份），开始 phase=$PHASE =="

# 阶段 E 的两份前置：控制器 CA（公的，推谁都行）与入网码（秘密，只推还没身份的盒子）。
# 码不进 box.conf、不进命令行（那会留在 Mac 的 ps 输出里），走一个 0600 的文件；
# 盒子那侧 vm-bringup.sh 用完就删（sr-agent 的 argv 里会有它，和 loop-bringup 同一口径）。
if [ "$PHASE" = all ] || [ "$PHASE" = E ]; then
    [ -s "$CA" ] || { echo "FAIL 缺控制器 CA：$CA"; exit 1; }
    scp -o BatchMode=yes -q "$CA" "$B":/root/vmlab/ca.pem || exit 1
    if ssh -o BatchMode=yes "$B" 'test -s /etc/sdwan/identity/state.json'; then
        echo "== $B 已有身份，不推入网码"
    else
        [ -s "$STATE/claim.txt" ] || { echo "FAIL 缺 $STATE/claim.txt（先跑 vm-console.py code）"; exit 1; }
        scp -o BatchMode=yes -q "$STATE/claim.txt" "$B":/root/vmlab/claim.txt || exit 1
        ssh -o BatchMode=yes "$B" 'chmod 600 /root/vmlab/claim.txt' || exit 1
    fi
fi

ssh -o BatchMode=yes "$B" "bash /root/vmlab/vm-bringup.sh --phase $PHASE"
