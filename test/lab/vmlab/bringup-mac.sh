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
GW=$GW
PEER_WAN=$GW
PEER_CUST=$PEER_CUST
BOX_SR_SHA=$SR_SHA
BOX_CRYPTO_SHA=$CR_SHA
EOF

ssh -o BatchMode=yes "$B" 'mkdir -p /root/vmlab /root/vmlab/artifacts/plugins /root/vmlab/artifacts/linux' || exit 1
scp -o BatchMode=yes -q /tmp/box.conf "$B":/root/vmlab/
scp -o BatchMode=yes -q "$VL"/{vm-bringup.sh,takeover-if.sh,underlay.sh,check-selfheal.sh,vmlab-underlay.service,52-vmlab-underlay.conf} "$B":/root/vmlab/

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
ssh -o BatchMode=yes "$B" "bash /root/vmlab/vm-bringup.sh --phase $PHASE"
