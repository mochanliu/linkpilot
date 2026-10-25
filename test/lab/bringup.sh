#!/bin/bash
# bringup.sh - 三站点 SD-WAN 复杂 lab 的一键搭建（宿主机执行）
#
# 这个 lab 的目的：把已经落地的每一层放进同一张网里，让人可以登进去从终端一路
# 追到对端终端。它**不开发控制器**：按 2026-10-25 的指示，凡是由控制器下发的
# 东西（每隧道端口、overlay /32、隧道计划、VRF/RT、FRR 旁路表号），这里都手写。
# sr-ctrl 只做它现在已经做好的那件事——入网签发设备证书（CPE 拿它跑 sr-route）。
#
# 拓扑（站点 N = 1..3）：
#
#      lab-termN 10.68.N.10 ── lab-frrN ── lab-cpeN ──lab_wan 10.88.1.0/24── lab-transit
#                       lanN 10.68.N.0/24  inN 10.78.N.0/24   wan 10.88.1.1N      10.88.1.3
#
#   CPE 全互联（每对一条隧道，每隧道一个端口——本 lab 两端同号，非必需——
#   一对 overlay /32）：
#     T12  cpe1 wg0 51820 192.168.99.1  <-> cpe2 wg0 51820 192.168.99.2   transit 51821
#     T13  cpe1 wg1 51840 192.168.100.1 <-> cpe3 wg0 51840 192.168.100.2  transit 51841
#     T23  cpe2 wg1 51860 192.168.101.1 <-> cpe3 wg1 51860 192.168.101.2  transit 51861
#   所有 SR 路径都是两跳：本端 -> transit -> 对端。
#
#   路由：每台 CPE 的 inside 口在 VPP table 250（VRF 250，标签 16+250=266），
#   宣告自己站点的 10.68.N.0/24（经 FRR 下一跳 10.78.N.20）。三个 sr-route 经
#   lab-rr 反射成全网表，sr-fib 把远端前缀装成「压 266、走对应 wg 接口」的路由。
#   每台 lab-frrN 的 sr-frr 再把控制面 RIB 导出到 FRR 的 table 1250，并由
#   `redistribute table-direct 1250` 让它出现在 FRR 的 BGP 表里。FRR 是盒子的
#   一部分（lab 里与 VPP 拆成两个容器），真机上再与客户侧路由器跑路由协议。
#
# 管理网 10.99.0.0/24 承载控制面（RR 的 mTLS、sr-route 的 /status、控制器 8444）。
# 数据面的 WAN/LAN IP 都搬进了 VPP，CPE 内核只保留管理地址。
#
# 用法:
#   test/lab/bringup.sh                 # 全量重建并配置
#   test/lab/bringup.sh --stage vpp     # 只跑某一阶段（containers|vpp|enroll|
#                                       #   dataplane|control|frr|terms|all）
#   test/lab/bringup.sh --bfd           # 额外建隧道内多跳 BFD 会话（默认不建）
#   test/lab/bringup.sh --keep          # 不先 down 现有容器（调试用，慎用）
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
COMPOSE="${HERE}/lab-compose.yml"
PROJECT=sdwan-lab
RUN="${HERE}/run"
BIN="${RUN}/bin"
IMAGE=srwg-dev:latest
FRR_IMAGE=quay.io/frrouting/frr:10.7.1

STAGE=all
WITH_BFD=0
KEEP=0
while [ $# -gt 0 ]; do
    case "$1" in
      --stage)    STAGE=${2:-}; shift 2 ;;
      --bfd)      WITH_BFD=1; shift ;;
      --keep)     KEEP=1; shift ;;
      -h|--help)  awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 0 ;;
      *)          echo "unknown option: $1" >&2; exit 2 ;;
    esac
done
case "$STAGE" in containers|vpp|enroll|dataplane|control|frr|terms|all) ;; *)
    echo "unknown stage: $STAGE" >&2; exit 2 ;;
esac

# ---------------- 手写的"控制器下发值" ----------------
WAN_IP=(  "" 10.88.1.11 10.88.1.12 10.88.1.13 )
INSIDE_IP=( "" 10.78.1.2 10.78.2.2 10.78.3.2 )
MGMT_IP=( "" 10.99.0.11 10.99.0.12 10.99.0.13 )
LAN_PFX=( "" 10.68.1.0/24 10.68.2.0/24 10.68.3.0/24 )
FRR_IP=(  "" 10.78.1.20 10.78.2.20 10.78.3.20 )
TRANSIT_WAN=10.88.1.3
VRF=250
ROUTE_STATUS=( "" 19001 19002 19003 )
SR_KEY=00112233445566778899aabbccddeeff
TUNNEL_MTU=1360
MSS=$((TUNNEL_MTU - 44))

RR_ADDR=10.99.0.100
RR_PORT=18446
RR_STATUS=18447
CTRL_ADDR=10.99.0.200
CTRL_PORT=8444
TENANT=tenant-a

# a b aif aport a_overlay bif bport b_overlay path_a path_b transit_port
# 端口是「每隧道一个」，但两端不必同号：encap 的隧道身份来自绑定表
# (对端 endpoint, 对端 dst_port)，与本机 vpn_ports 无关；vpn_ports 只决定
# 收包进 sr-wg4-input 还是 wg4-input。这里两端写成同号只是方便对表；
# 非对称端口的回归在 test/scripts/test-multitunnel.sh（51830/51840）。
TUNNELS="1 2 wg0 51820 192.168.99.1  wg0 51820 192.168.99.2  1 3 51821
1 3 wg1 51840 192.168.100.1 wg0 51840 192.168.100.2 2 5 51841
2 3 wg1 51860 192.168.101.1 wg1 51860 192.168.101.2 4 6 51861"

log()  { echo "== $*"; }
die()  { echo "FATAL: $*" >&2; exit 2; }
note() { echo "   $*"; }

dx()  { docker exec "$@"; }
vpp() { local c=$1; shift; docker exec "$c" vppctl "$@" 2>&1 | tr -d '\r'; }

vpp_if_exists() { vpp "$1" show interface | awk -v n="$2" '$1==n {f=1} END{exit !f}'; }
vpp_swidx()     { vpp "$1" show interface "$2" | awk -v n="$2" '$1==n {print $2; exit}'; }
vpp_addr_has()  { vpp "$1" show interface addr | grep -q "L3 $2/"; }
vpp_fib_has()   { vpp "$1" show ip fib "$2" | grep -q "^$2"; }
vpp_ifname_for_ip() {
    vpp "$1" show interface addr |
        awk -v want="$2" '
            /:[[:space:]]*$/     { n=$1; next }
            index($0, "L3 "want"/") { print n; exit }'
}
wg_pubkey() {
    vpp "$1" show wireguard interface |
        awk -v ifn="$2" '
            $2==ifn { for(i=1;i<=NF;i++) if ($i ~ /^public-key:/)
                      { sub(/^public-key:/,"",$i); print $i; exit } }'
}
wg_peer_has() { vpp "$1" show wireguard peer | grep -q "$2"; }

kernel_if_for_ip() { docker exec "$1" ip -o -4 addr show | awk -v w="$2" '{split($4,a,"/"); if (a[1]==w) {print $2; exit}}'; }
hint_if_for_ip()   { docker exec "$1" awk -v w="$2" '$1==w {print $2; exit}' /tmp/lab-ifstate 2>/dev/null; }

# 幂等：把内核网卡交给 VPP，回显 VPP 接口名。IP 由 take_ip 单独搬。
ensure_host_if() {
    local c=$1 ip=$2 pfx=$3 ifn vif kmac mtu
    ifn=$(kernel_if_for_ip "$c" "$ip")
    if [ -z "$ifn" ]; then
        ifn=$(hint_if_for_ip "$c" "$ip")
        [ -n "$ifn" ] || die "$c: 内核上没有 $ip，也没有 /tmp/lab-ifstate 记录——重跑 --stage containers"
        docker exec "$c" ip addr add "$ip/$pfx" dev "$ifn" ||
            die "$c: 恢复 $ip/$pfx 到 $ifn 失败"
    fi
    vif="host-$ifn"
    vpp_if_exists "$c" "$vif" ||
        docker exec "$c" vppctl create host-interface name "$ifn" >/dev/null ||
        die "$c: create host-interface $ifn 失败"
    kmac=$(docker exec "$c" cat "/sys/class/net/$ifn/address")
    vpp "$c" set interface mac address "$vif" "$kmac" >/dev/null
    vpp "$c" set interface state "$vif" up >/dev/null
    mtu=$(docker exec "$c" cat "/sys/class/net/$ifn/mtu")
    vpp "$c" set interface mtu packet "$mtu" "$vif" >/dev/null
    echo "$vif"
}

# 把内核上的 ip 搬进 VPP（可选放某张 table），并从内核摘掉。
take_ip() {
    local c=$1 vif=$2 ip=$3 pfx=$4 table=${5:-} kif
    kif=${vif#host-}
    if [ -n "$table" ]; then
        vpp "$c" set interface ip table "$vif" "$table" >/dev/null
    fi
    vpp_addr_has "$c" "$ip" || vpp "$c" set interface ip address "$vif" "$ip/$pfx" >/dev/null
    docker exec "$c" ip addr del "$ip/$pfx" dev "$kif" 2>/dev/null || true
    docker exec "$c" sh -c "grep -q '^$ip ' /tmp/lab-ifstate 2>/dev/null ||
                            echo '$ip $kif $pfx' >> /tmp/lab-ifstate"
}

wg_create() {   # <cpe> <if> <listen-port> <src-ip> <overlay/32>
    local c=$1 ifn=$2 port=$3 src=$4 ov=$5
    vpp_if_exists "$c" "$ifn" ||
        docker exec "$c" vppctl wireguard create listen-port "$port" generate-key src "$src" >/dev/null ||
        die "$c: wireguard create $ifn 失败"
    vpp_addr_has "$c" "$ov" || vpp "$c" set interface ip address "$ifn" "$ov/32" >/dev/null
    vpp "$c" set interface state "$ifn" up >/dev/null
    vpp "$c" set interface mpls "$ifn" enable >/dev/null
    vpp "$c" set interface mtu packet "$TUNNEL_MTU" "$ifn" >/dev/null
    vpp "$c" set interface tcp-mss-clamp "$ifn" ip4 rx ip4-mss "$MSS" ip6 disable >/dev/null
}

tunnel_rows() { printf '%s\n' "$TUNNELS" | while read -r a b aif aport aov bif bport bov pa pb tport; do
    [ -z "${a:-}" ] && continue
    echo "$a $b $aif $aport $aov $bif $bport $bov $pa $pb $tport"
done; }

# ==================================================================
stage_containers() {
    log "containers: 生成 FRR 配置并重建 ${PROJECT}"
    docker image inspect "$IMAGE" >/dev/null 2>&1 || die "缺少镜像 $IMAGE（先跑 test/docker 的构建或 test-e2e）"
    docker image inspect "$FRR_IMAGE" >/dev/null 2>&1 || die "缺少镜像 $FRR_IMAGE：docker pull $FRR_IMAGE"

    mkdir -p "$RUN" "$BIN"
    for n in 1 2 3; do
        mkdir -p "$RUN/frr$n"
        cat > "$RUN/frr$n/frr.conf" <<EOF
frr defaults traditional
hostname lab-frr$n
log stdout
service integrated-vtysh-config
!
router bgp 6500$n
 bgp router-id 10.99.0.2$n
 no bgp ebgp-requires-policy
 address-family ipv4 unicast
  redistribute table-direct 1250
 exit-address-family
!
! 本 lab 未配置 eBGP 邻居（客户路由器未建模）；真机在此与客户路由器建邻。
! 盒内 FRR 的默认路由指向 VPP inside（10.78.N.2）；远端明细由 sr-frr 导出的
! table 1250 经 BGP 呈现，不进内核主表。客户侧只需要一条默认路由指向盒子的 LAN 口。
ip route 0.0.0.0/0 ${INSIDE_IP[$n]}
!
EOF
    done

    if [ "$KEEP" != "1" ]; then
        docker compose -f "$COMPOSE" -p "$PROJECT" down -v --remove-orphans >/dev/null 2>&1 || true
    fi
    docker compose -f "$COMPOSE" -p "$PROJECT" up -d --no-build || die "docker compose up 失败"
    for c in lab-cpe1 lab-cpe2 lab-cpe3 lab-transit lab-rr lab-ctrl lab-frr1 lab-frr2 lab-frr3 lab-term1 lab-term2 lab-term3; do
        docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null | grep -q true ||
            die "$c 没起来"
    done

    # FRR 的配置在容器内落盘后再 restart：镜像的 docker-start 会处理 /etc/frr
    # 的属主与 vtysh.conf，只读 bind mount 会把这套初始化弄坏。
    for n in 1 2 3; do
        docker cp "$RUN/frr$n/frr.conf" "lab-frr$n:/etc/frr/frr.conf" >/dev/null
        docker exec "lab-frr$n" sh -c 'sed -i "s/^bgpd=no/bgpd=yes/" /etc/frr/daemons
            touch /etc/frr/vtysh.conf; chown -R frr:frr /etc/frr' || die "lab-frr$n: 写 FRR 配置失败"
        docker restart "lab-frr$n" >/dev/null || die "lab-frr$n: restart 失败"
    done
    note "12 个 lab-* 容器已运行，FRR 已带 bgpd 重启"
}

# ==================================================================
stage_vpp() {
    log "vpp: 启动 4 个数据面实例（3 CPE + transit）"
    [ -f "$ROOT/vpp-plugin/build/lib/vpp_plugins/sr_wg_plugin.so" ] ||
        die "缺 sr_wg_plugin.so：先跑一次 test/scripts/test-e2e.sh 或在容器里编译插件"
    for c in lab-cpe1 lab-cpe2 lab-cpe3 lab-transit; do
        docker exec "$c" bash -lc 'pkill -9 -x vpp_main; pkill -9 -f "script -qefc vpp"; sleep 1; rm -f /run/vpp/*.sock' >/dev/null 2>&1
        docker exec -d "$c" bash -lc "bash /workspace/test/docker/start-node.sh ${c}"
    done
    for c in lab-cpe1 lab-cpe2 lab-cpe3 lab-transit; do
        local i
        for i in $(seq 1 90); do
            docker exec "$c" vppctl show version >/dev/null 2>&1 && break
            sleep 1
        done
        docker exec "$c" vppctl show version >/dev/null 2>&1 ||
            die "$c: VPP 没起来（docker exec $c tail -40 /var/log/vpp/vpp.log）"
        docker exec "$c" vppctl show plugins | grep -q sr_wg_plugin.so ||
            die "$c: sr_wg 插件没加载"
        note "$c: VPP + sr_wg OK"
    done
}

# ==================================================================
stage_enroll() {
    log "enroll: 编译二进制、起控制器、三台 CPE 入网"
    docker exec lab-cpe1 bash -lc '
        set -e; cd /workspace/controller; mkdir -p /tmp/lab-bin
        for b in sr-rr sr-route sr-fib sr-frr sr-ctrl; do
            CGO_ENABLED=0 go build -o /tmp/lab-bin/$b ./cmd/$b
        done' || die "编译失败"
    for b in sr-rr sr-route sr-fib sr-frr sr-ctrl; do
        docker cp "lab-cpe1:/tmp/lab-bin/$b" "$BIN/$b" >/dev/null || die "取 $b 失败"
    done
    docker cp "$BIN/sr-rr"   lab-rr:/usr/local/bin/sr-rr
    docker cp "$BIN/sr-ctrl" lab-ctrl:/usr/local/bin/sr-ctrl
    for n in 1 2 3; do
        docker cp "$BIN/sr-route" "lab-cpe$n:/usr/local/bin/sr-route"
        docker cp "$BIN/sr-fib"   "lab-cpe$n:/usr/local/bin/sr-fib"
        docker cp "$BIN/sr-frr"   "lab-frr$n:/usr/local/bin/sr-frr"
    done

    log "enroll: 启动 sr-ctrl（CA + 8444/mTLS）"
    docker exec lab-ctrl bash -lc 'pkill -f /usr/local/bin/sr-ctrl 2>/dev/null || true'
    docker exec -d lab-ctrl sh -c "mkdir -p /var/lib/sr-ctrl && /usr/local/bin/sr-ctrl \
        -data /var/lib/sr-ctrl -listen 0.0.0.0:${CTRL_PORT} \
        -tls-san ${CTRL_ADDR},10.88.1.200,lab-ctrl -rr rr-a=${RR_ADDR}:${RR_PORT} \
        > /var/log/sr-ctrl.log 2>&1"
    local i
    for i in $(seq 1 30); do
        docker exec lab-ctrl test -f /var/lib/sr-ctrl/ca.pem && break
        sleep 0.5
    done
    docker exec lab-ctrl test -f /var/lib/sr-ctrl/ca.pem || die "sr-ctrl 没生成 CA（看 /var/log/sr-ctrl.log）"
    docker cp lab-ctrl:/var/lib/sr-ctrl/ca.pem "$RUN/ca.pem" >/dev/null
    docker cp lab-ctrl:/var/lib/sr-ctrl/ca.key "$RUN/ca.key" >/dev/null
    note "控制器 CA 就绪（$RUN/ca.pem）"

    for n in 1 2 3; do
        local c=lab-cpe$n
        docker cp "$RUN/ca.pem" "$c:/opt/vbond/ca.pem" >/dev/null 2>&1 || {
            docker exec "$c" mkdir -p /opt/vbond
            docker cp "$RUN/ca.pem" "$c:/opt/vbond/ca.pem" >/dev/null
        }
        docker exec -e VBOND_URL="https://${CTRL_ADDR}:${CTRL_PORT}" -e VBOND_CA=/opt/vbond/ca.pem \
            "$c" bash -lc "/workspace/controller/test/scripts/vbond-client.sh enroll $c claim-a $c" >/dev/null ||
            die "$c: 入网失败"
        local node_id
        node_id=$(docker exec "$c" python3 -c \
            'import json;print(json.load(open("/opt/vbond/'"$c"'.enroll.json"))["node_id"])' 2>/dev/null)
        [ -n "$node_id" ] || die "$c: 入网响应里没有 node_id"
        docker exec "$c" bash -lc "set -e
            mkdir -p /etc/sdwan/identity
            install -m600 /opt/vbond/$c.key /etc/sdwan/identity/device_ed25519.key
            install -m644 /opt/vbond/$c.pem /etc/sdwan/identity/node_cert.pem
            install -m644 /opt/vbond/ca.pem /etc/sdwan/identity/ca_cert.pem
            printf '{\"serial\":\"%s\",\"key_gen\":0,\"tenant_id\":\"%s\",\"node_id\":%s}' \
                $c $TENANT $node_id > /etc/sdwan/identity/state.json" || die "$c: 写身份目录失败"
        note "$c: tenant=${TENANT} node=${node_id}"
    done

    log "enroll: 用控制器 CA 给 RR 签服务端证书（SAN 10.99.0.100 / 10.88.1.100 / lab-rr）"
    docker exec lab-cpe1 bash -lc 'rm -rf /tmp/rr-pki && mkdir -p /tmp/rr-pki'
    docker cp "$RUN/ca.pem" lab-cpe1:/tmp/rr-pki/ca.pem >/dev/null
    docker cp "$RUN/ca.key" lab-cpe1:/tmp/rr-pki/ca.key >/dev/null
    docker exec lab-cpe1 bash -lc '
        set -e; cd /tmp/rr-pki
        openssl genpkey -algorithm ED25519 -out rr.key 2>/dev/null
        openssl req -new -key rr.key -subj "/CN=lab-rr" -out rr.csr 2>/dev/null
        cat > server.ext <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=IP:10.99.0.100,IP:10.88.1.100,DNS:lab-rr
EOF
        openssl x509 -req -in rr.csr -CA ca.pem -CAkey ca.key -CAcreateserial \
            -days 30 -extfile server.ext -out rr.crt 2>/dev/null' || die "RR 证书签发失败"
    docker exec lab-rr mkdir -p /etc/sr
    docker cp lab-cpe1:/tmp/rr-pki/rr.crt "$RUN/rr.crt" >/dev/null
    docker cp lab-cpe1:/tmp/rr-pki/rr.key "$RUN/rr.key" >/dev/null
    docker cp "$RUN/rr.crt" lab-rr:/etc/sr/rr.crt >/dev/null
    docker cp "$RUN/rr.key" lab-rr:/etc/sr/rr.key >/dev/null
    docker cp "$RUN/ca.pem" lab-rr:/etc/sr/ca.pem >/dev/null

    cat > "$RUN/rr.json" <<EOF
{
  "rr_id": "rr-a",
  "tenant": "$TENANT",
  "hold_sec": 12,
  "listen": "0.0.0.0:${RR_PORT}",
  "status_addr": "0.0.0.0:${RR_STATUS}",
  "tls": {"cert_file": "/etc/sr/rr.crt", "key_file": "/etc/sr/rr.key", "ca_file": "/etc/sr/ca.pem"}
}
EOF
    docker cp "$RUN/rr.json" lab-rr:/etc/sr/rr.json >/dev/null
    docker exec lab-rr bash -lc 'pkill -f /usr/local/bin/sr-rr 2>/dev/null || true'
    docker exec -d lab-rr sh -c '/usr/local/bin/sr-rr -config /etc/sr/rr.json > /var/log/sr-rr.log 2>&1'
    for i in $(seq 1 20); do
        docker exec lab-rr curl -sf "http://127.0.0.1:${RR_STATUS}/status" >/dev/null 2>&1 && break
        sleep 0.5
    done
    docker exec lab-rr curl -sf "http://127.0.0.1:${RR_STATUS}/status" >/dev/null 2>&1 ||
        die "sr-rr 没起来（docker exec lab-rr cat /var/log/sr-rr.log）"
    note "rr-a 在 ${RR_ADDR}:${RR_PORT}（状态 ${RR_STATUS}）"
}

# ==================================================================
stage_dataplane() {
    log "dataplane: CPE 的 underlay / inside / 隧道（每隧道一个端口 + overlay /32）"
    local n c wan ins
    for n in 1 2 3; do
        c=lab-cpe$n
        vpp "$c" sr-wg key set 1 "$SR_KEY" >/dev/null
        vpp "$c" mpls table add 0 >/dev/null
        vpp "$c" ip table add "$VRF" >/dev/null
        wan=$(ensure_host_if "$c" "${WAN_IP[$n]}" 24)
        take_ip "$c" "$wan" "${WAN_IP[$n]}" 24
        ins=$(ensure_host_if "$c" "${INSIDE_IP[$n]}" 24)
        take_ip "$c" "$ins" "${INSIDE_IP[$n]}" 24 "$VRF"
        note "$c: wan=$wan(${WAN_IP[$n]}) inside=$ins(${INSIDE_IP[$n]}, table $VRF)"
    done

    # 先建所有 WG 接口（每个接口自己 generate-key；同对端的多条隧道不能用同一把
    # 远端公钥，VPP 的 peer 池跨接口去重）。
    local a b aif aport aov bif bport bov pa pb tport
    while read -r a b aif aport aov bif bport bov pa pb tport; do
        wg_create "lab-cpe$a" "$aif" "$aport" "${WAN_IP[$a]}" "$aov"
        wg_create "lab-cpe$b" "$bif" "$bport" "${WAN_IP[$b]}" "$bov"
    done < <(tunnel_rows) || die "建 WG 接口失败"

    # 端口登记必须排在 wireguard create 之后：建接口会把 listen-port 抢回 wg4-input。
    for n in 1 2 3; do
        c=lab-cpe$n
        while read -r a b aif aport aov bif bport bov pa pb tport; do
            [ "$a" = "$n" ] && vpp "$c" set sr-wg port add vpn "$aport" >/dev/null
            [ "$b" = "$n" ] && vpp "$c" set sr-wg port add vpn "$bport" >/dev/null
        done < <(tunnel_rows)
        vpp "$c" set sr-wg rebind-ports >/dev/null
        note "$c: vpn 端口集合 = $(vpp "$c" show sr-wg ports | awk '/^vpn/{i=index($0,": "); s=substr($0,i+2); sub(/ .*/,"",s); print s}')"
    done

    # SR path / 按 (endpoint,dst-port) 的绑定 / underlay 出口的 encap 挂点。
    while read -r a b aif aport aov bif bport bov pa pb tport; do
        local ca=lab-cpe$a cb=lab-cpe$b sw
        vpp "$ca" sr-wg path del "$pa" >/dev/null 2>&1
        vpp "$ca" sr-wg path add "$pa" via "${WAN_IP[$b]}:$bport" via "${TRANSIT_WAN}:$tport" hmac-id 1 >/dev/null ||
            die "$ca: path $pa 下发失败"
        sw=$(vpp_swidx "$ca" "$aif")
        [ -n "$sw" ] || die "$ca: 取不到 $aif 的 sw_if_index"
        vpp "$ca" set sr-wg tunnel "$sw" path "$pa" endpoint "${WAN_IP[$b]}" dst-port "$bport" >/dev/null ||
            die "$ca: 绑定 $aif 失败"
        vpp "$cb" sr-wg path del "$pb" >/dev/null 2>&1
        vpp "$cb" sr-wg path add "$pb" via "${WAN_IP[$a]}:$aport" via "${TRANSIT_WAN}:$tport" hmac-id 1 >/dev/null ||
            die "$cb: path $pb 下发失败"
        sw=$(vpp_swidx "$cb" "$bif")
        [ -n "$sw" ] || die "$cb: 取不到 $bif 的 sw_if_index"
        vpp "$cb" set sr-wg tunnel "$sw" path "$pb" endpoint "${WAN_IP[$a]}" dst-port "$aport" >/dev/null ||
            die "$cb: 绑定 $bif 失败"
    done < <(tunnel_rows)
    for n in 1 2 3; do
        c=lab-cpe$n
        local uvif uidx
        uvif=$(vpp_ifname_for_ip "$c" "${WAN_IP[$n]}")
        [ -n "$uvif" ] || die "$c: 找不到持有 ${WAN_IP[$n]} 的 VPP 接口"
        uidx=$(vpp_swidx "$c" "$uvif")
        vpp "$c" set sr-wg encap enable "$uidx" >/dev/null
        note "$c: encap 挂在 $uvif(sw_if_index=$uidx)"
    done

    # peer 一定最后加：peer 一加 WG 立刻发 handshake，encap 必须先挂好。
    while read -r a b aif aport aov bif bport bov pa pb tport; do
        local ca=lab-cpe$a cb=lab-cpe$b puba pubb
        pubb=$(wg_pubkey "$cb" "$bif")
        puba=$(wg_pubkey "$ca" "$aif")
        [ -n "$pubb" ] && [ -n "$puba" ] || die "取不到 $aif/$bif 的 WG 公钥"
        wg_peer_has "$ca" "$pubb" ||
            vpp "$ca" wireguard peer add "$aif" public-key "$pubb" endpoint "${WAN_IP[$b]}" \
                allowed-ip 0.0.0.0/0 allowed-ip ::/0 dst-port "$bport" persistent-keepalive 5 >/dev/null ||
            die "$ca: 加 peer 失败"
        wg_peer_has "$cb" "$puba" ||
            vpp "$cb" wireguard peer add "$bif" public-key "$puba" endpoint "${WAN_IP[$a]}" \
                allowed-ip 0.0.0.0/0 allowed-ip ::/0 dst-port "$aport" persistent-keepalive 5 >/dev/null ||
            die "$cb: 加 peer 失败"
        vpp_fib_has "$ca" "$bov/32" || vpp "$ca" ip route add "$bov/32" via "$aif" >/dev/null
        vpp_fib_has "$cb" "$aov/32" || vpp "$cb" ip route add "$aov/32" via "$bif" >/dev/null
    done < <(tunnel_rows)

    log "dataplane: transit（三个 per-tunnel transit 端口）"
    vpp lab-transit sr-wg key set 1 "$SR_KEY" >/dev/null
    vpp lab-transit mpls table add 0 >/dev/null
    wan=$(ensure_host_if lab-transit "$TRANSIT_WAN" 24)
    take_ip lab-transit "$wan" "$TRANSIT_WAN" 24
    vpp lab-transit set sr-wg transit enable port 51821 >/dev/null
    vpp lab-transit set sr-wg port add transit 51841 >/dev/null
    vpp lab-transit set sr-wg port add transit 51861 >/dev/null
    vpp lab-transit set sr-wg rebind-ports >/dev/null
    note "lab-transit: $(vpp lab-transit show sr-wg ports | awk '/^transit/{i=index($0,": "); s=substr($0,i+2); sub(/ .*/,"",s); print s}')"
}

# ==================================================================
stage_control() {
    log "control: sr-route（宣告 10.68.N.0/24）+ sr-fib（table $VRF，标签 266）"
    local n c node_id a b aif aport aov bif bport bov pa pb tport tun route_tun fib_tun
    for n in 1 2 3; do
        c=lab-cpe$n
        node_id=$(docker exec "$c" sed -n 's/.*"node_id":\([0-9]*\).*/\1/p' /etc/sdwan/identity/state.json | head -1)
        [ -n "$node_id" ] || die "$c: 没有身份（先跑 --stage enroll）"
        route_tun=""; fib_tun=""
        while read -r a b aif aport aov bif bport bov pa pb tport; do
            [ -z "${a:-}" ] && continue
            if [ "$a" = "$n" ]; then
                route_tun+="{\"peer_device_id\": $b, \"next_hop\": \"$bov\"},"
                fib_tun+="{\"next_hop\": \"$bov\", \"interface\": \"$aif\"},"
            fi
            if [ "$b" = "$n" ]; then
                route_tun+="{\"peer_device_id\": $a, \"next_hop\": \"$aov\"},"
                fib_tun+="{\"next_hop\": \"$aov\", \"interface\": \"$bif\"},"
            fi
        done <<< "$TUNNELS"
        route_tun=${route_tun%,}; fib_tun=${fib_tun%,}

        cat > "$RUN/cpe$n.route.json" <<EOF
{
  "tenant": "$TENANT",
  "device_id": $node_id,
  "identity_dir": "/etc/sdwan/identity",
  "serial": "lab-cpe$n",
  "hold_sec": 12,
  "tls_server_name": "$RR_ADDR",
  "rrs": [{"id": "rr-a", "addr": "$RR_ADDR:$RR_PORT"}],
  "tunnels": [$route_tun],
  "vrfs": [{"id": $VRF, "export_rts": ["65000:$VRF"], "import_rts": ["65000:$VRF"]}],
  "local_prefixes": [{"vrf_id": $VRF, "prefix": "${LAN_PFX[$n]}"}],
  "status_addr": "0.0.0.0:${ROUTE_STATUS[$n]}"
}
EOF
        cat > "$RUN/cpe$n.fib.json" <<EOF
{
  "status_url": "http://127.0.0.1:${ROUTE_STATUS[$n]}/status",
  "vpp_socket": "/run/vpp/api.sock",
  "poll_sec": 1,
  "vrfs": [$VRF],
  "tunnels": [$fib_tun]
}
EOF
        docker cp "$RUN/cpe$n.route.json" "$c:/etc/sdwan/route.json" >/dev/null
        docker cp "$RUN/cpe$n.fib.json" "$c:/etc/sdwan/fib.json" >/dev/null
        docker exec "$c" bash -lc 'pkill -f /usr/local/bin/sr-route 2>/dev/null || true; pkill -f /usr/local/bin/sr-fib 2>/dev/null || true'
        docker exec -d "$c" sh -c '/usr/local/bin/sr-route -config /etc/sdwan/route.json > /var/log/sr-route.log 2>&1'
    done

    local i remote
    for n in 1 2 3; do
        c=lab-cpe$n
        for i in $(seq 1 40); do
            docker exec "$c" curl -sf --max-time 2 "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" >/dev/null 2>&1 && break
            sleep 0.5
        done
        docker exec "$c" curl -sf --max-time 2 "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" >/dev/null 2>&1 ||
            die "$c: sr-route 状态口没起来（cat /var/log/sr-route.log）"
    done
    # 等三台都从 RR 收到另外两个站点。Go 的 encoding/json 是紧凑输出（冒号后没有
    # 空格），所以这里必须容忍 0..n 个空格，别按"好看"的 JSON 写死。
    for n in 1 2 3; do
        c=lab-cpe$n
        for i in $(seq 1 60); do
            local have=0
            for remote in 1 2 3; do
                [ "$remote" = "$n" ] && continue
                docker exec "$c" curl -sf "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" |
                    grep -Eq "\"prefix\":[[:space:]]*\"${LAN_PFX[$remote]}\"" && have=$((have + 1))
            done
            [ "$have" = "2" ] && break
            sleep 0.5
        done
        docker exec "$c" curl -sf "http://127.0.0.1:${ROUTE_STATUS[$n]}/status" |
            grep -Eq "\"active_rr\":[[:space:]]*0" ||
            die "$c: 没有和 rr-a 同步（cat /var/log/sr-route.log）"
        note "$c: RIB 已收齐另两个站点"
    done

    for n in 1 2 3; do
        c=lab-cpe$n
        docker exec -d "$c" sh -c '/usr/local/bin/sr-fib -config /etc/sdwan/fib.json > /var/log/sr-fib.log 2>&1'
    done
    for n in 1 2 3; do
        c=lab-cpe$n
        for i in $(seq 1 40); do
            local got=0
            for remote in 1 2 3; do
                [ "$remote" = "$n" ] && continue
                vpp "$c" show ip fib table "$VRF" | grep -q "^${LAN_PFX[$remote]}" && got=$((got + 1))
            done
            [ "$got" = "2" ] && break
            sleep 0.5
        done
        for remote in 1 2 3; do
            [ "$remote" = "$n" ] && continue
            vpp "$c" show ip fib table "$VRF" | grep -q "^${LAN_PFX[$remote]}" ||
                die "$c: table $VRF 里没有 ${LAN_PFX[$remote]}（cat /var/log/sr-fib.log）"
        done
        # 站点自己的客户网就在 FRR 后面：控制面/数据面都不该猜这条，手写。
        # 下一跳必须**绑到 inside 接口**。只写 `via <nh>` 时 VPP 会做递归查表，
        # 而递归发生在 table 0 —— inside 接口和它的 /24 都在 table 250，那里
        # 既没有接口也没有邻居，路径解析成 dpo-drop（实测：ping 100% 丢，FIB
        # 还显示路由"存在"）。写成 attached-nexthop 才走该表的邻居表。
        local ins
        ins=$(vpp_ifname_for_ip "$c" "${INSIDE_IP[$n]}")
        [ -n "$ins" ] || die "$c: 找不到持有 ${INSIDE_IP[$n]} 的 VPP 接口"
        vpp "$c" ip route del table "$VRF" "${LAN_PFX[$n]}" via "${FRR_IP[$n]}" "$ins" >/dev/null 2>&1 || true
        vpp "$c" ip route del table "$VRF" "${LAN_PFX[$n]}" via "${FRR_IP[$n]}" >/dev/null 2>&1 || true
        vpp "$c" ip route add table "$VRF" "${LAN_PFX[$n]}" via "${FRR_IP[$n]}" "$ins" >/dev/null ||
            die "$c: 下发本地 ${LAN_PFX[$n]} via ${FRR_IP[$n]}/$ins 失败"
        vpp "$c" show ip fib table "$VRF" "${LAN_PFX[$n]}" | grep -q "dpo-drop" &&
            die "$c: 本地 ${LAN_PFX[$n]} 解析成 dpo-drop"
        note "$c: FIB 已装好（含本地 10.68.$n.0/24 via ${FRR_IP[$n]}/$ins）"
    done
}

# ==================================================================
stage_frr() {
    log "frr: sr-frr 把控制面 RIB 导出到 FRR table 1250（再由 BGP 重分布）"
    local n c i
    for n in 1 2 3; do
        c=lab-frr$n
        for i in $(seq 1 40); do
            docker exec "$c" vtysh -c 'show version' >/dev/null 2>&1 && break
            sleep 0.5
        done
        docker exec "$c" vtysh -c 'show version' >/dev/null 2>&1 || die "$c: FRR 没起来"
        docker exec "$c" vtysh -c 'show running-config' | grep -q 'redistribute table-direct 1250' ||
            die "$c: frr.conf 没应用（检查 run/frr$n/frr.conf 与 /etc/frr/daemons）"
        # docker 给容器塞了一条 kernel 默认路由（distance 0），zebra 不会用
        # static(distance 1) 覆盖它——流量会跑去网桥网关而不是 CPE。删掉它，
        # zebra 下一轮重算就会把 frr.conf 里的默认路由装进内核。
        docker exec "$c" sh -c 'ip route del default 2>/dev/null || true'
        for i in $(seq 1 20); do
            docker exec "$c" ip route show default | grep -q "${INSIDE_IP[$n]}" && break
            sleep 0.5
        done
        docker exec "$c" ip route show default | grep -q "${INSIDE_IP[$n]}" ||
            die "$c: 内核默认路由没有指向 ${INSIDE_IP[$n]}（docker 的默认路由没清理干净？）"
        cat > "$RUN/frr$n.sr-frr.json" <<EOF
{
  "status_url": "http://${MGMT_IP[$n]}:${ROUTE_STATUS[$n]}/status",
  "zserv_socket": "/run/frr/zserv.api",
  "state_file": "/var/lib/sr-frr/state.json",
  "poll_sec": 1,
  "distance": 1,
  "metric": 20,
  "vrfs": [{"vrf_id": $VRF, "table_id": 1250}]
}
EOF
        docker cp "$RUN/frr$n.sr-frr.json" "$c:/etc/sr-frr.json" >/dev/null
        docker exec "$c" bash -lc 'mkdir -p /var/lib/sr-frr; pkill -f /usr/local/bin/sr-frr 2>/dev/null || true'
        docker exec -d "$c" sh -c '/usr/local/bin/sr-frr -config /etc/sr-frr.json > /var/log/sr-frr.log 2>&1'
    done
    local remote have
    for n in 1 2 3; do
        c=lab-frr$n
        for i in $(seq 1 60); do
            have=0
            for remote in 1 2 3; do
                [ "$remote" = "$n" ] && continue
                docker exec "$c" vtysh -c 'show ip route table 1250' |
                    grep -q "${LAN_PFX[$remote]}" && have=$((have + 1))
            done
            [ "$have" = "2" ] && break
            sleep 0.5
        done
        [ "$have" = "2" ] || die "$c: table 1250 没收到两个远端前缀（cat /var/log/sr-frr.log）"
        note "$c: table 1250 有另两个站点，BGP 里 $(docker exec "$c" vtysh -c 'show bgp ipv4 unicast' | grep -c '\*>') 条"
    done
}

# ==================================================================
stage_terms() {
    log "terms: 终端默认路由（客户侧不用知道 SD-WAN 内部怎么走）"
    local n
    for n in 1 2 3; do
        docker exec "lab-term$n" ip route replace default via 10.68.$n.2
        note "lab-term$n: default via 10.68.$n.2"
    done
}

# ==================================================================
stage_bfd() {
    log "bfd: 每对 overlay 之间建**多跳**会话（绑接口的会话会把 overlay 路由打成 dpo-drop）"
    local a b aif aport aov bif bport bov pa pb tport
    while read -r a b aif aport aov bif bport bov pa pb tport; do
        local ca=lab-cpe$a cb=lab-cpe$b
        vpp "$ca" bfd udp session add multihop local-addr "$aov" peer-addr "$bov" \
            desired-min-tx 300000 required-min-rx 300000 detect-mult 3 >/dev/null
        vpp "$cb" bfd udp session add multihop local-addr "$bov" peer-addr "$aov" \
            desired-min-tx 300000 required-min-rx 300000 detect-mult 3 >/dev/null
        note "$ca($aov) <-> $cb($bov)"
    done < <(tunnel_rows)
}

# ==================================================================
case "$STAGE" in
  containers) stage_containers ;;
  vpp)        stage_vpp ;;
  enroll)     stage_enroll ;;
  dataplane)  stage_dataplane ;;
  control)    stage_control ;;
  frr)        stage_frr ;;
  terms)      stage_terms ;;
  all)
        stage_containers
        stage_vpp
        stage_enroll
        stage_dataplane
        stage_control
        stage_frr
        stage_terms
        [ "$WITH_BFD" = "1" ] && stage_bfd || true
        ;;
esac

if [ "$STAGE" = "all" ] || [ "$STAGE" = "dataplane" ]; then
    echo
    log "完成。先跑 test/lab/verify.sh 看绿灯；登录方式见 test/lab/README.md"
fi
