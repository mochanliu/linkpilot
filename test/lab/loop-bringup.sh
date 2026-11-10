#!/bin/bash
# loop-bringup.sh - 环路 / 站点 HA 实验台的**平台启动段**（宿主机执行）
#
# 这套 lab 和 bringup.sh（3 站点、verify.sh 的口径）是两套并存的网：子网全部错开，
# 容器名 lp-*。环路测试期间只跑这一套，避免互相抢核、互相干扰结论。
#
# **这个脚本只做产品没有的那一段**，做完就停手，剩下全部从控制台操作：
#
#   做了的（平台启动段，产品里今天没有对应下发路径）：
#     P-1 把 WAN 与客户口交给 VPP（create host-interface v2）、从内核摘掉地址
#     P-2 lcp TAP 配对（linux-cp），让 FRR 有接口可配
#     P-3 VPP 全局：mpls table add 0 / ip table add 250 / local0 绑 250（多跳 BFD
#         查表要用）；**不配任何隧道，隧道由 agent 从控制台装**
#     P-4 盒内核：转发全关、无默认路由、arp_ignore=1（与 3 站点 lab 同一套实测口径）
#     P-5 每台盒子的 bootstrap：/etc/sdwan/{route.json,fib.json}、/etc/sr-frr.json
#         的**本地管道字段**（我是谁、状态口、socket 路径）。rrs/tunnels/vrfs 一律
#         留空，等控制台发布的第一份配置包合并进来。
#     P-6 FRR 起进程（daemons 打开 zebra/bgpd/ospfd/staticd，frr.conf 是空壳），
#         真正的配置由控制台的第 1 段经 frr-reload.py 差量下发。
#     P-7 客户路由器（lp-cust12/3/4）与终端：那是客户自己的网络，本来就不归控制器管。
#
#   没做的（故意留给控制台，脚本里有断言防止我手滑配上去）：
#     隧道、每隧道端口、overlay /32、VRF/RT、导通组、RR 库与绑定、路由泄露、
#     客户口地址、OSPF/BGP、VRRP、MTU 预算、密钥环。
#     唯一的例外是 sr-fib 的 fib.json（装哪张表 + 下一跳↔接口名 + 逃生开关）：
#     产品里没有它的段，只能在盒子本地 PUT /config，由 loop-fib-sync.sh 填，
#     报告里作为缺口 G-2 点名。
#     配置之外还有两处绕开控制台，报告 §2.5 逐条点名：P-7 那三台客户路由器的
#     frr.conf 是手工塞进容器的（它们本来不是我们的设备）；--stage stack 是**手工起
#     路由栈**，而且起之前必须先手工跑一次 sr-apply boot 重放，否则卡在首次发布死结
#     （缺口 G-5，见 stack 段那段注释）。
#
# 用法：
#   test/lab/loop-bringup.sh                  # = --stage all，做到 enroll 之前
#   test/lab/loop-bringup.sh --stage vpp      # 单阶段：containers|ctrl|vpp|platform|
#                                             #   enroll|base|agent|stack|all
#   test/lab/loop-bringup.sh --stage agent    # 控制台发布后再跑：给 agent 加 -apply/-tunnels
#   test/lab/loop-bringup.sh --stage stack    # 起 sr-route / sr-fib / sr-frr
#   test/lab/loop-bringup.sh --keep           # 不先 down（调试用）
#
# 入网码不在命令行里（会进 ps 输出与 shell 历史），从 run/loop/claim.txt 读，0600。
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
RUN="${HERE}/run/loop"
BIN="${RUN}/bin"
COMPOSE="${HERE}/loop-compose.yml"
PROJECT=sdwan-loop
IMAGE=srwg-lab:latest
CUST_IMAGE=quay.io/frrouting/frr:10.7.1
NET_PREFIX=sdwan-loop
# agent 的身份目录：入网写 device key / 证书 / state.json 的地方，也是 P-5 那份
# route.json 里 `identity_dir` 指的地方。两者必须是同一个目录 —— sr-route 从这里
# 读证书，指错了就报「目录里没有证书」，整条路由栈起不来。
IDENT=/etc/sdwan/identity

STAGE=all
KEEP=0
while [ $# -gt 0 ]; do
    case "$1" in
      --stage) STAGE=${2:-}; shift 2 ;;
      --keep)  KEEP=1; shift ;;
      -h|--help) awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 0 ;;
      *) echo "unknown option: $1" >&2; exit 2 ;;
    esac
done
case "$STAGE" in containers|ctrl|vpp|platform|enroll|base|agent|stack|rr|all) ;; *)
    echo "unknown stage: $STAGE" >&2; exit 2 ;;
esac

# ---------------- 地址表（环路口径） ----------------
WAN=( "" 10.88.2.11 10.88.2.12 10.88.2.13 10.88.2.14 )
MGMT=( "" 10.99.2.11 10.99.2.12 10.99.2.13 10.99.2.14 )
# 站点 12：一台客户路由器接两台盒子，共享一条 LAN
SITE_IP1=10.76.12.11      # lp-cpe1 客户口（进 VPP）
SITE_IP2=10.76.12.12      # lp-cpe2 客户口（进 VPP）
CUST12_X=10.76.12.13      # lp-cust12 面向站点 LAN
VIP=10.76.12.10           # VRRP 虚地址（由控制台的第 6 段下发，这里只用它写客户侧默认路由）
SITE_NET=10.76.12.0/24
# 每台盒子客户口上的地址（下标 = 盒子号）。站点 12 两台都在这条 LAN 上。
CUST_IP=( "" $SITE_IP1 $SITE_IP2 10.76.3.2 10.76.4.2 )
CTRL_ADDR=10.99.2.200
CTRL_PORT=8444
RR_ADDR=10.99.2.100
RR_PORT=18446
RR_STATUS=18447
LCP_IF=lcp-x0
OVERLAY=250
ROUTE_STATUS=( "" 19001 19002 19003 19004 )
FIB_STATUS=( "" 19011 19012 19013 19014 )
CUST12_LOOP=172.16.12.1
CUST3_LOOP=172.16.3.1
CUST4_LOOP=172.16.4.1
# eBGP 两端：盒子 AS 由控制台的第 1 段下发（loop-console.py 里同一张表），
# 客户路由器的 AS 属于客户自己的网络，写在 P-7 的客户侧配置里。
BOX_AS=( "" "" "" 65103 65104 )
PEER_AS=( "" "" "" 65003 65004 )

log()  { echo "== $*"; }
die()  { echo "FATAL: $*" >&2; exit 2; }
note() { echo "   $*"; }

exists() { docker ps -a --format '{{.Names}}' | grep -qx "$1"; }
up()     { docker ps --format '{{.Names}}' | grep -qx "$1"; }
dx()   { docker exec "$@"; }
vpp()  { local c=$1; shift; docker exec "$c" vppctl "$@" 2>&1 | tr -d '\r'; }
vtysh() { local c=$1; shift; docker exec "$c" vtysh "$@" 2>&1 | tr -d '\r'; }
box()  { echo "lp-cpe$1"; }

vpp_if_exists() { vpp "$1" show interface | awk -v n="$2" '$1==n {f=1} END{exit !f}'; }
vpp_addr_has()  { vpp "$1" show interface addr | grep -q "L3 $2/"; }
kernel_if_for_ip() { docker exec "$1" ip -o -4 addr show | awk -v w="$2" '{split($4,a,"/"); if (a[1]==w) {print $2; exit}}'; }
hint_if_for_ip()   { docker exec "$1" awk -v w="$2" '$1==w {print $2; exit}' /tmp/lab-ifstate 2>/dev/null; }
# 已经在 VPP 里（有地址或只是没地址都算）就用现成的名字
vpp_ifname_for_ip() {
    vpp "$1" show interface addr |
        awk -v want="$2" '/:[[:space:]]*$/ { n=$1; next } index($0, "L3 "want"/") { print n; exit }'
}
# VPP 里有这个 host- 接口但没有地址时（客户口把地址交给 FRR 之后）找名字
vpp_hostif_by_kernel() {
    vpp "$1" show interface | awk -v n="host-$2" '$1==n {print $1; exit}'
}
node_id_of() {   # 从盒子的 agent 身份目录读 node_id（state.json 是 agent 写的）
    dx "$(box "$1")" python3 -c '
import json,sys
print(json.load(open(sys.argv[1]))["node_id"])' "${IDENT}/state.json" 2>/dev/null
}
tenant_of() {    # 同一份文件里的 tenant_id
    dx "$(box "$1")" python3 -c '
import json,sys
print(json.load(open(sys.argv[1]))["tenant_id"])' "${IDENT}/state.json" 2>/dev/null
}

# 交给 VPP 但**不配地址**：地址属于 FRR（第 1 段渲染 interface <TAP> ip address，
# lcp-sync 再镜像回 VPP）。这是 P-1/P-2 与产品边界的接缝。
hand_if_to_vpp() {   # <cpe> <ip> <pfx> <with-addr 0|1>
    local c=$1 ip=$2 pfx=$3 withaddr=$4 ifn vif kmac mtu
    ifn=$(kernel_if_for_ip "$c" "$ip")
    if [ -z "$ifn" ]; then
        ifn=$(hint_if_for_ip "$c" "$ip")
        [ -n "$ifn" ] || die "$c: 内核上没有 ${ip}，也没有 /tmp/lab-ifstate 记录"
        dx "$c" ip addr add "$ip/$pfx" dev "$ifn" || die "$c: 恢复 $ip/$pfx 到 $ifn 失败"
    fi
    vif="host-$ifn"
    # V2 而不是默认 V3：VPP 的 V3 路径 tp_retire_blk_tov 写死 1ms，稀疏报文每接口
    # 每方向多等 0~1ms（2026-10-26 实测同一跳 RTT 0.75ms -> 0.27ms）。
    vpp_if_exists "$c" "$vif" ||
        dx "$c" vppctl create host-interface name "$ifn" v2 >/dev/null ||
        die "$c: create host-interface $ifn 失败"
    kmac=$(dx "$c" cat "/sys/class/net/$ifn/address")
    vpp "$c" set interface mac address "$vif" "$kmac" >/dev/null
    vpp "$c" set interface state "$vif" up >/dev/null
    mtu=$(dx "$c" cat "/sys/class/net/$ifn/mtu")
    vpp "$c" set interface mtu packet "$mtu" "$vif" >/dev/null
    if [ "$withaddr" = "1" ]; then
        vpp_addr_has "$c" "$ip" || vpp "$c" set interface ip address "$vif" "$ip/$pfx" >/dev/null
    fi
    dx "$c" ip addr del "$ip/$pfx" dev "$ifn" 2>/dev/null || true
    dx "$c" sh -c "grep -q '^$ip ' /tmp/lab-ifstate 2>/dev/null || echo '$ip $ifn $pfx' >> /tmp/lab-ifstate"
    echo "$vif"
}

lcp_pair() {   # <cpe> <vpp-if>
    local c=$1 vif=$2
    if ! vpp "$c" show lcp | grep -q " $vif "; then
        dx "$c" ip link del "$LCP_IF" 2>/dev/null || true
        vpp "$c" lcp create "$vif" host-if "$LCP_IF" >/dev/null || die "$c: lcp create $vif 失败"
    fi
    vpp "$c" lcp lcp-sync on >/dev/null
}

# 盒内核既不当路由器也不当主机（与 3 站点 lab 同一套实测结论）。
kernel_no_forwarding() {   # <cpe>
    local c=$1
    dx "$c" bash -lc '
        set -e
        sysctl -qw net.ipv4.ip_forward=0 net.ipv4.conf.all.forwarding=0 net.ipv4.conf.all.rp_filter=0
        for i in /sys/class/net/*; do
            [ -d "$i" ] || continue
            i=$(basename "$i"); [ "$i" = lo ] && continue
            sysctl -qw net.ipv4.conf."$i".forwarding=0 net.ipv4.conf."$i".rp_filter=0
            sysctl -qw net.ipv4.conf."$i".arp_ignore=1
        done
        # 交给 VPP 的口反过来要严格 rp_filter：AF_PACKET 只是旁路，内核照样收到同一
        # 帧，地址又在 lcp TAP 上，弱主机模型下内核会替这个口再回一次 ICMP。
        if [ -r /tmp/lab-ifstate ]; then
            while read -r _ ifn _; do
                [ -n "$ifn" ] && [ -d "/sys/class/net/$ifn" ] || continue
                sysctl -qw net.ipv4.conf."$ifn".rp_filter=1
            done < /tmp/lab-ifstate
        fi' || die "$c: 关内核转发失败"
    dx "$c" sh -c 'while ip route del default 2>/dev/null; do :; done' || true
}

stop_proc() {   # <container> <process-name>：SIGTERM 先走，等真退出
    dx "$1" bash -lc "pkill -x $2 2>/dev/null || true
        for i in \$(seq 1 40); do pgrep -x $2 >/dev/null 2>&1 || exit 0; sleep 0.25; done
        pkill -9 -x $2 2>/dev/null || true"
}

# ==================================================================
stage_containers() {
    log "containers: 重建 ${PROJECT}"
    docker image inspect "$IMAGE" >/dev/null 2>&1 ||
        die "缺少镜像 ${IMAGE}（cd test/lab && docker build -t $IMAGE .）"
    docker image inspect "$CUST_IMAGE" >/dev/null 2>&1 || die "缺少镜像 $CUST_IMAGE"
    mkdir -p "$RUN" "$BIN"
    [ "$KEEP" = "1" ] || docker compose -f "$COMPOSE" -p "$PROJECT" down -v --remove-orphans >/dev/null 2>&1 || true
    docker compose -f "$COMPOSE" -p "$PROJECT" up -d --no-build || die "docker compose up 失败"
    local c
    for c in lp-cpe1 lp-cpe2 lp-cpe3 lp-cpe4 lp-cust12 lp-cust3 lp-cust4 \
             lp-rr lp-ctrl lp-term12 lp-term3 lp-term4; do
        docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null | grep -q true || die "$c 没起来"
    done
    log "containers: 构建并分发二进制"
    dx lp-cpe1 bash -lc 'set -e; export PATH=/usr/local/go/bin:$PATH
        cd /workspace/controller && rm -rf /tmp/loop-bin && mkdir -p /tmp/loop-bin
        for b in sr-ctrl sr-rr sr-route sr-fib sr-frr sr-agent sr-apply; do
            CGO_ENABLED=0 go build -o /tmp/loop-bin/$b ./cmd/$b
        done' || die "构建失败（docker exec lp-cpe1 里手动跑一次看报错）"
    for c in lp-cpe1 lp-cpe2 lp-cpe3 lp-cpe4 lp-rr lp-ctrl; do
        dx lp-cpe1 tar -C /tmp/loop-bin -cf - . | docker exec -i "$c" tar -C /usr/local/bin -xf -
    done
    note "二进制：$(dx lp-ctrl ls /usr/local/bin | tr '\n' ' ')"
}

# ==================================================================
stage_ctrl() {
    log "ctrl: 起 sr-ctrl（设备面 ${CTRL_ADDR}:${CTRL_PORT}，控制台 127.0.0.1:8089）"
    dx lp-ctrl sh -c 'mkdir -p /var/lib/sr-ctrl
        [ -s /var/lib/sr-ctrl/console-token ] ||
          head -c 18 /dev/urandom | base64 | tr -d "=+/\n" > /var/lib/sr-ctrl/console-token'
    local token
    token=$(dx lp-ctrl cat /var/lib/sr-ctrl/console-token)
    printf '%s' "$token" > "$RUN/console-token"; chmod 600 "$RUN/console-token"
    stop_proc lp-ctrl sr-ctrl
    # 口令不进命令行历史以外的地方：token 只写 run/loop/console-token（git 忽略）。
    dx -d lp-ctrl sh -c "mkdir -p /var/log && /usr/local/bin/sr-ctrl \
        -data /var/lib/sr-ctrl -listen 0.0.0.0:${CTRL_PORT} \
        -tls-san ${CTRL_ADDR},10.88.2.200,lp-ctrl -console 0.0.0.0:8080 \
        -admin-token '$token' >> /var/log/sr-ctrl.log 2>&1"
    local i
    for i in $(seq 1 40); do dx lp-ctrl test -f /var/lib/sr-ctrl/ca.pem 2>/dev/null && break; sleep 0.5; done
    dx lp-ctrl test -f /var/lib/sr-ctrl/ca.pem || die "sr-ctrl 没起来（docker exec lp-ctrl tail -40 /var/log/sr-ctrl.log）"
    docker cp lp-ctrl:/var/lib/sr-ctrl/ca.pem "$RUN/ca.pem" >/dev/null
    curl -sfo /dev/null http://127.0.0.1:8089/ || die "控制台 8089 不通"
    note "控制器 CA：$RUN/ca.pem；控制台 http://127.0.0.1:8089/"
}

# ==================================================================
stage_vpp() {
    log "vpp: 起 4 个数据面实例"
    [ -f "$ROOT/vpp-plugin/build/lib/vpp_plugins/sr_wg_plugin.so" ] ||
        die "缺 sr_wg_plugin.so：先跑一次 test/scripts/test-e2e.sh"
    local c i
    for c in lp-cpe1 lp-cpe2 lp-cpe3 lp-cpe4; do
        dx "$c" bash -lc 'pkill -9 -x vpp_main; pkill -9 -f "script -qefc vpp"; sleep 1
            rm -f /run/vpp/*.sock; rm -f /tmp/lab-ifstate; ip link del '"$LCP_IF"' 2>/dev/null' >/dev/null 2>&1
        dx -d "$c" bash -lc "VPP_CONF=/workspace/test/docker/vpp-startup.conf \
            bash /workspace/test/docker/start-node.sh ${c}"
    done
    for c in lp-cpe1 lp-cpe2 lp-cpe3 lp-cpe4; do
        for i in $(seq 1 90); do dx "$c" vppctl show version >/dev/null 2>&1 && break; sleep 1; done
        dx "$c" vppctl show version >/dev/null 2>&1 || die "$c: VPP 没起来（tail /var/log/vpp/vpp.log）"
        for p in sr_wg_plugin.so linux_cp_plugin.so linux_nl_plugin.so; do
            dx "$c" vppctl show plugins | grep -q "$p" || die "$c: $p 没加载"
        done
        note "$c: $(dx "$c" vppctl show version | head -1)"
    done
}

# ==================================================================
stage_platform() {
    log "platform: P-1..P-4 交给 VPP 的口 / lcp / 全局表 / 内核姿势"
    local n c vif
    for n in 1 2 3 4; do
        c=$(box "$n")
        dx "$c" rm -f /etc/frr/frr.conf 2>/dev/null
        # WAN：地址进 VPP（underlay 出口，隧道要以它为源）。
        vif=$(hand_if_to_vpp "$c" "${WAN[$n]}" 24 1) || return 1
        # 客户口：只交接口，地址留给控制台的第 1 段。
        vif=$(hand_if_to_vpp "$c" "${CUST_IP[$n]}" 24 0) || return 1
        lcp_pair "$c" "$vif"
        # VPP 全局：mpls 表 + overlay 表 + local0 绑 overlay 表（多跳 BFD 查表）。
        vpp "$c" mpls table add 0 >/dev/null
        vpp "$c" show ip fib "$OVERLAY" >/dev/null 2>&1
        vpp "$c" ip table add "$OVERLAY" >/dev/null 2>&1 || true
        vpp "$c" set interface ip table local0 "$OVERLAY" >/dev/null ||
            die "$c: local0 绑 overlay 表 $OVERLAY 失败"
        kernel_no_forwarding "$c"
        # 断言：这一刻盒子里既没有客户口地址也没有隧道，控制台还没说话。
        dx "$c" bash -lc 'if ip -o -4 addr show dev '"$LCP_IF"' | grep -q inet; then
                             echo "lcp TAP 上已经有地址，说明有人手配了" >&2; exit 1; fi' ||
            die "$c: $LCP_IF 提前拿到地址"
        note "$c: $vif + ${LCP_IF}，WAN ${WAN[$n]} 在 VPP，客户口地址未配（等控制台）"
    done

    log "platform: P-7 客户路由器与终端（客户自己的网络，不归控制器）"
    # 站点 12：一台客户路由器和两台盒子跑 OSPF area 0（这是环路的来源）。
    # 站点 3/4：各自跑 eBGP，只播一个 loopback（这是"远端前缀"的来源）。
    for n in 12 3 4; do
        case "$n" in
          12) x=$CUST12_X; box=$SITE_IP1; loop=$CUST12_LOOP; lan=10.66.12.0/24; gw=$VIP ;;
          3)  x=10.76.3.3;  box=10.76.3.2;  loop=$CUST3_LOOP;  lan=10.66.3.0/24; gw=10.76.3.2 ;;
          4)  x=10.76.4.3;  box=10.76.4.2;  loop=$CUST4_LOOP;  lan=10.66.4.0/24; gw=10.76.4.2 ;;
        esac
        if [ "$n" = "12" ]; then
            cat > "$RUN/cust$n.frr.conf" <<EOF
frr defaults traditional
hostname lp-cust$n
log stdout
service integrated-vtysh-config
!
interface lo
 ip address ${loop}/32
!
router ospf
 ospf router-id ${x}
 log-adjacency-changes
 network ${x}/32 area 0
 network ${lan} area 0
 network ${loop}/32 area 0
!
ip route 0.0.0.0/0 ${gw}
!
EOF
        else
            cat > "$RUN/cust$n.frr.conf" <<EOF
frr defaults traditional
hostname lp-cust$n
log stdout
service integrated-vtysh-config
!
interface lo
 ip address ${loop}/32
!
router bgp ${PEER_AS[$n]}
 bgp router-id ${x}
 no bgp ebgp-requires-policy
 neighbor ${box} remote-as ${BOX_AS[$n]}
 neighbor ${box} description lp-cpe$n
 address-family ipv4 unicast
  network ${lan}
  network ${loop}/32
 exit-address-family
!
ip route 0.0.0.0/0 ${box}
!
EOF
        fi
        dx "lp-cust$n" sh -c 'sed -i "s/^zebra=no/zebra=yes/; s/^bgpd=no/bgpd=yes/; s/^ospfd=no/ospfd=yes/" /etc/frr/daemons
            touch /etc/frr/vtysh.conf; chown -R frr:frr /etc/frr' || die "lp-cust$n: daemons 写入失败"
        docker cp "$RUN/cust$n.frr.conf" "lp-cust$n:/etc/frr/frr.conf" >/dev/null
        docker restart "lp-cust$n" >/dev/null || die "lp-cust$n restart 失败"
        local i ok=0
        for i in $(seq 1 60); do
            vtysh "lp-cust$n" -c 'show running-config' 2>/dev/null | grep -qE 'router (ospf|bgp)' &&
                vtysh "lp-cust$n" -c 'show ip route' 2>/dev/null | grep -q "${loop}" && { ok=1; break; }
            sleep 0.5
        done
        [ "$ok" = "1" ] || die "lp-cust$n: 客户路由器的协议/loopback 没就绪（docker logs lp-cust${n}）"
        # docker 给每个网段都塞了一条默认路由（走网桥网关，黑洞）。它距离是 0，
        # 会盖住客户自己写的静态默认，所以删掉；zebra 发现自己装的路由没了会再装回来。
        dx "lp-cust$n" sh -c 'while ip route del default 2>/dev/null; do :; done' || true
        note "lp-cust$n: loopback ${loop}/32，默认路由 → ${gw}（下一跳要等控制台配好地址才通）"
    done

    # 客户终端的默认路由指向自己的客户路由器。
    for pair in "term12 10.66.12.2" "term3 10.66.3.2" "term4 10.66.4.2"; do
        set -- $pair
        dx "lp-$1" sh -c 'while ip route del default 2>/dev/null; do :; done'
        dx "lp-$1" ip route replace default via "$2" || die "lp-$1: 默认路由配不上"
    done
}

# ==================================================================
stage_enroll() {
    log "enroll: 4 台盒子用同一张入网码入网（sr-agent，只身份 + 心跳 + 接口清单）"
    [ -s "$RUN/claim.txt" ] || die "缺 $RUN/claim.txt（由 loop-console.py 的 setup 写，0600）"
    local code
    code=$(cat "$RUN/claim.txt")
    dx lp-ctrl test -s /var/lib/sr-ctrl/ca.pem || die "控制器上没有 CA：先 --stage ctrl"
    local n c
    for n in 1 2 3 4; do
        c=$(box "$n")
        dx "$c" bash -lc "mkdir -p ${IDENT} && chmod 700 ${IDENT}"
        docker cp "$RUN/ca.pem" "$c:${IDENT}/pinned_ca.pem" >/dev/null
        if dx "$c" test -s "${IDENT}/state.json" 2>/dev/null; then
            note "$c: 已有身份 node_id=$(node_id_of "$n")，跳过 enroll"
            continue
        fi
        # 码用位置参数传进容器里的这条命令，不落宿主机 shell 历史；agent 的 argv 里
        # 会有它，所以任何打印 argv 的调试都不能做（sr-agent 的 argv 含入网码）。
        dx -d "$c" bash -lc "exec /usr/local/bin/sr-agent \
            --ctrl ${CTRL_ADDR}:${CTRL_PORT} \
            --ca ${IDENT}/pinned_ca.pem \
            --id ${IDENT} --serial $c \
            --status 127.0.0.1:8089 --vpp-api /run/vpp/api.sock \
            --claim \"\$1\"" sh "$code" >/dev/null || die "$c: agent 起不来"
    done
    local i got
    for n in 1 2 3 4; do
        got=""
        for i in $(seq 1 60); do
            got=$(node_id_of "$n" 2>/dev/null) && [ -n "$got" ] && break
            up "$(box $n)" || die "$(box $n) 的 agent 退出了：docker logs $(box $n)"
            sleep 0.5
        done
        [ -n "$got" ] || { dx "$(box $n)" tail -20 /var/log/sr-agent.log 2>/dev/null; die "$n: 30 秒内没拿到身份"; }
        note "$(box $n): node_id=$got"
    done
}

# ==================================================================
stage_base() {
    log "base: P-5 盒子本地 bootstrap（管道字段，路由内容一律留空）"
    local n c id tenant
    for n in 1 2 3 4; do
        c=$(box "$n")
        id=$(node_id_of "$n") || die "$c: 没有 node_id，先 --stage enroll"
        tenant=$(tenant_of "$n") || die "$c: 读不到 tenant_id（${IDENT}/state.json）"
        # rrs/tunnels/vrfs 留空是**故意的**：这三个键归控制台的第 4 段。
        # sr-route 因此不能在这个阶段起（它要求 rrs 非空），起在 --stage stack。
        cat > "$RUN/cpe$n.route.json" <<EOF
{
  "tenant": "${tenant}",
  "device_id": ${id},
  "identity_dir": "/etc/sdwan/identity",
  "serial": "${c}",
  "hold_sec": 12,
  "tls_server_name": "${RR_ADDR}",
  "rrs": [],
  "tunnels": [],
  "vrfs": [],
  "status_addr": "0.0.0.0:${ROUTE_STATUS[$n]}"
}
EOF
        cat > "$RUN/cpe$n.fib.json" <<EOF
{
  "status_url": "http://127.0.0.1:${ROUTE_STATUS[$n]}/status",
  "status_addr": "0.0.0.0:${FIB_STATUS[$n]}",
  "vpp_socket": "/run/vpp/api.sock",
  "poll_sec": 1,
  "vrfs": [0],
  "tunnels": []
}
EOF
        cat > "$RUN/cpe$n.sr-frr.json" <<EOF
{
  "status_url": "http://127.0.0.1:${ROUTE_STATUS[$n]}/status",
  "zserv_socket": "/run/frr/zserv.api",
  "state_file": "/var/lib/sr-frr/state.json",
  "poll_sec": 1,
  "export_vrfs": [],
  "import_rules": []
}
EOF
        dx "$c" mkdir -p /etc/sdwan ${IDENT} /var/lib/sdwan /var/lib/sr-frr /var/log/frr
        # 身份目录就是 agent 的 --id 目录（sr-route 用同一个 LoadOrCreate 读，
        # 文件名 device_ed25519.key / node_cert.pem / ca_cert.pem 是同一套）。
        # 所以这里**不复制**任何东西：复制出来的第二份身份迟早会和 agent 的那份漂移。
        docker cp "$RUN/cpe$n.route.json"  "$c:/etc/sdwan/route.json"  >/dev/null
        docker cp "$RUN/cpe$n.fib.json"    "$c:/etc/sdwan/fib.json"    >/dev/null
        docker cp "$RUN/cpe$n.sr-frr.json" "$c:/etc/sr-frr.json"       >/dev/null
        # P-6：FRR 进程先起，配置是空壳；真配置由控制台的第 1 段经 frr-reload.py 差量下发。
        dx "$c" bash -lc 'set -e
            sed -i "s/^zebra=no/zebra=yes/; s/^bgpd=no/bgpd=yes/; s/^ospfd=no/ospfd=yes/; s/^staticd=no/staticd=yes/" /etc/frr/daemons
            printf "frr defaults traditional\nhostname %s\nlog file /var/log/frr/frr.log\nservice integrated-vtysh-config\n!\n" "'"$c"'" > /etc/frr/frr.conf
            touch /etc/frr/vtysh.conf; chown -R frr:frr /etc/frr
            /usr/lib/frr/frrinit.sh restart >/dev/null 2>&1 || /usr/lib/frr/frrinit.sh start >/dev/null 2>&1' ||
            die "$c: FRR 起不来（tail -40 /var/log/frr/frr.log）"
        local i
        for i in $(seq 1 40); do vtysh "$c" -c 'show version' >/dev/null 2>&1 && break; sleep 0.5; done
        vtysh "$c" -c 'show version' >/dev/null 2>&1 || die "$c: vtysh 没响应"
        vtysh "$c" -c 'show running-config' | grep -q "^hostname" || die "$c: FRR 空壳配置没应用"
        note "$c: node_id=$id tenant=${tenant}，route/fib/sr-frr bootstrap 就位，FRR 空壳已起"
    done
}

# ==================================================================
stage_agent() {
    log "agent: 给 agent 加上 -apply 与 -tunnels（配置包与隧道从此走控制台）"
    local n c
    for n in 1 2 3 4; do
        c=$(box "$n")
        dx "$c" test -s /etc/sdwan/route.json || die "$c: 先 --stage base"
        stop_proc "$c" sr-agent
        dx -d "$c" bash -lc "exec /usr/local/bin/sr-agent \
            --ctrl ${CTRL_ADDR}:${CTRL_PORT} \
            --ca ${IDENT}/pinned_ca.pem \
            --id ${IDENT} --serial $c \
            --status 127.0.0.1:8089 \
            --vpp-api /run/vpp/api.sock --tunnels \
            --apply --store /var/lib/sdwan/store --route-conf /etc/sdwan/route.json \
            >> /var/log/sr-agent.log 2>&1" || die "$c: agent 起不来"
    done
    local i st
    for n in 1 2 3 4; do
        c=$(box "$n")
        st=""
        for i in $(seq 1 60); do
            st=$(dx "$c" python3 -c 'import urllib.request,json;print(json.load(urllib.request.urlopen("http://127.0.0.1:8089/status",timeout=3))["heartbeats"])' 2>/dev/null)
            [ -n "$st" ] && [ "$st" -ge 1 ] && break
            sleep 1
        done
        [ -n "$st" ] && [ "$st" -ge 1 ] || die "$c: 没有心跳成功（docker exec $c tail -30 /var/log/sr-agent.log）"
        note "$c: 心跳 $st 次，-apply/-tunnels 已开"
    done
}

# ==================================================================
stage_stack() {
    log "stack: 起 sr-route / sr-fib / sr-frr（第一份配置包合并之后才有 rrs）"
    local n c
    for n in 1 2 3 4; do
        c=$(box "$n")
        dx "$c" python3 -c '
import json,sys
d=json.load(open("/etc/sdwan/route.json"))
sys.exit(0 if d.get("rrs") else 1)' 2>/dev/null ||
            die "$c: /etc/sdwan/route.json 里还没有 rrs（先让控制台发布一次并等 agent 应用）"
        # 先把控制器那份包**重放**一遍（= 盒子上 start.sh 的那一步，只落盘不热更）。
        # 非做不可的原因：第 5 段的自动路径要求 sr-frr 活着才肯写 /etc/sr-frr.json，
        # 而 sr-frr 又拒绝带着两张空表启动 —— 首次发布的这个死结见环路报告 §9 G-5。
        dx "$c" /usr/local/bin/sr-apply boot --root /var/lib/sdwan/store \
            --vpp-api /run/vpp/api.sock | sed 's/^/   boot 重放: /' ||
            die "$c: sr-apply boot 重放失败（store 里没有配置包？先 --stage agent 并让控制台发布一次）"
        stop_proc "$c" sr-route; stop_proc "$c" sr-fib; stop_proc "$c" sr-frr
        dx -d "$c" sh -c '/usr/local/bin/sr-route -config /etc/sdwan/route.json > /var/log/sr-route.log 2>&1'
        dx -d "$c" sh -c '/usr/local/bin/sr-fib   -config /etc/sdwan/fib.json   > /var/log/sr-fib.log 2>&1'
        dx -d "$c" sh -c '/usr/local/bin/sr-frr   -config /etc/sr-frr.json     > /var/log/sr-frr.log 2>&1'
    done
    local i ok
    for n in 1 2 3 4; do
        c=$(box "$n")
        ok=0
        for i in $(seq 1 40); do
            dx "$c" bash -lc "curl -sf http://127.0.0.1:${ROUTE_STATUS[$n]}/status >/dev/null &&
                              curl -sf http://127.0.0.1:${FIB_STATUS[$n]}/status  >/dev/null &&
                              pgrep -x sr-frr >/dev/null" && { ok=1; break; }
            sleep 0.5
        done
        [ "$ok" = "1" ] || die "$c: 路由栈没起来（/var/log/sr-route.log、sr-fib.log、sr-frr.log）"
        note "$c: sr-route ${ROUTE_STATUS[$n]} + sr-fib ${FIB_STATUS[$n]} + sr-frr 在跑"
    done
}

# ==================================================================
stage_rr() {
    log "rr: 装控制台签发的 RR 身份并起 sr-rr（身份由 /api/rrs/identity 签发）"
    [ -s "$RUN/rr.crt" ] && [ -s "$RUN/rr.key" ] ||
        die "缺 $RUN/rr.{crt,key}：先跑 loop-console.py 的 rrs 步骤签发身份"
    # lp-rr 镜像里没有 /etc/sr，docker cp 不会自己建目录
    dx lp-rr mkdir -p /etc/sr
    docker cp "$RUN/rr.crt" lp-rr:/etc/sr/rr.crt >/dev/null
    docker cp "$RUN/rr.key" lp-rr:/etc/sr/rr.key >/dev/null
    docker cp "$RUN/ca.pem" lp-rr:/etc/sr/ca.pem >/dev/null
    dx lp-rr bash -lc 'chmod 600 /etc/sr/rr.key'
    local tenant rid
    tenant=$(tenant_of 1) || die "读不到 lp-cpe1 的 tenant_id（先 --stage base）"
    # rr_id 必须等于控制台签发身份时用的库行 id：证书里的 srou://.../rr/<id> 和这个值
    # 不一致时 sr-rr 启动即 Fatal（cmd/sr-rr/main.go:148）。
    rid=$(cat "$RUN/rr-id" 2>/dev/null) || die "缺 $RUN/rr-id：先跑 loop-console.py 的 rrs 步骤"
    cat > "$RUN/rr.json" <<EOF
{
  "rr_id": "${rid}",
  "tenant": "${tenant}",
  "hold_sec": 12,
  "listen": "0.0.0.0:${RR_PORT}",
  "status_addr": "0.0.0.0:${RR_STATUS}",
  "tls": {"cert_file": "/etc/sr/rr.crt", "key_file": "/etc/sr/rr.key", "ca_file": "/etc/sr/ca.pem"}
}
EOF
    docker cp "$RUN/rr.json" lp-rr:/etc/sr/rr.json >/dev/null
    stop_proc lp-rr sr-rr
    dx -d lp-rr sh -c '/usr/local/bin/sr-rr -config /etc/sr/rr.json > /var/log/sr-rr.log 2>&1'
    local i
    for i in $(seq 1 40); do
        dx lp-rr curl -sf "http://127.0.0.1:${RR_STATUS}/status" >/dev/null 2>&1 && break
        sleep 0.5
    done
    dx lp-rr curl -sf "http://127.0.0.1:${RR_STATUS}/status" >/dev/null 2>&1 ||
        die "sr-rr 没起来（docker exec lp-rr tail -40 /var/log/sr-rr.log）"
    note "sr-rr 在 ${RR_ADDR}:${RR_PORT}，状态口 ${RR_STATUS}"
}

# ==================================================================
need_image() { :; }

case "$STAGE" in
  all)       stage_containers; stage_ctrl; stage_vpp; stage_platform;
             echo; echo "下一步：跑 test/lab/loop-console.py setup（建租户 + 授权 + 入网码），再 --stage enroll" ;;
  containers) stage_containers ;;
  ctrl)      stage_ctrl ;;
  vpp)       stage_vpp ;;
  platform)  stage_platform ;;
  enroll)    stage_enroll ;;
  base)      stage_base ;;
  agent)     stage_agent ;;
  stack)     stage_stack ;;
  rr)        stage_rr ;;
esac
