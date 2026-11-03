#!/bin/bash
# test-cross-vpn.sh - 导通（跨 VPN）落到 VPP：表号跟目标走、标签跟来源走（O3 判据）
#
# 这条脚本回答四件事，每件都有一个"看起来对但其实错"的反例：
#
#   (1) node2 在 **VPN 41** 里宣告 10.41.0.0/16，node1 的 **VPN 40** 按 RT 把它
#       导进来。VPP 里这条路由必须装在**表 40**（目标），而压的标签必须是
#       **1041**（= 1000 + 来源 VPN）。反例很自然：装在 40 却压 1040 —— 那等于
#       把"我的 VPN"写进了别人的收端语义里，对端弹栈后查的是它自己的 1040，
#       而 1040 在对端根本没登记。§4.3 第 3 步说的就是这件事。
#   (2) 颜色 0 的那条路径**只存在于主表 40**：§4.2 的公式在 2026-11-03 更正为
#       表号 = 色*1000+主表，颜色 0 折出来就是主表本身。负向钉的是老表号 10040
#       全程为空 —— 改之前那次 lab 观察到的"10000+主表 副本表"不能再出现。
#   (3) **负向（§2.4 静默投递的回归）**：同一个前缀由 node2 的两个 VPN（41 与
#       42）分别送进来 ⇒ 两个标签 ⇒ 不能同桶。断言主表 40 里该前缀只有
#       1 条路径（buckets:1）、标签是 RD 更小的那个源（2/41）、并且 fib.log 里
#       有"择一"的警告。反例是"两条都装，VPP 自己取一个" —— 那 FIB 里没有的那
#       个源就静默消失了，出了故障查不到痕迹。
#   (4) 择一规则不是插入顺序也不是运气：把 41 那份从宣告集合里撤掉，同一个前缀必须
#       **改压 1042**；再把 41 加回来，又回到 1041。三轮下来才说明它跟着 RD 走。
#
# 前置：**必须先跑过 test/scripts/test-e2e.sh --keep**（node1↔node2 的
# wg0@192.168.99.x 单隧道在、通、且 wg0 的 MPLS 弧真的开着）。本脚本只加控制面
# （rr / sr-route x2 / sr-fib x2）与两个 tap 终端，收尾把自己装的 FIB 状态、tap、
# 进程全部撤掉，不动隧道。
#
# VPN 分配（避开 lab 已占用的表号 0/10/99/250/1250）：
#   node1 VPN 40  export 65000:40           import 65000:41, 65000:42
#   node2 VPN 41  export 65000:41           import 65000:40
#   node2 VPN 42  export 65000:42           import 65000:99（永不命中，只用来导出）
# 宣告：node1 在 40 里送 10.40.0.0/16；node2 在 41 里送 10.41.0.0/16，
#       并把 10.44.0.0/16 **同时**放在 41 和 42 下面（这就是"两个来源送同一前缀"）。
#
# 用法:
#   test/scripts/test-cross-vpn.sh [--keep]
set -u

N1=srwg-node1
N2=srwg-node2
MODULE=/workspace/controller
WORK=${WORK:-/tmp/sr-xvpn-e2e}

WG_IF=${WG_IF:-wg0}
OVERLAY_N1=${OVERLAY_N1:-192.168.99.1}
OVERLAY_N2=${OVERLAY_N2:-192.168.99.2}
# node1 内核在管理 bridge（10.10.0.0/24）上的地址：node2 容器里的 sr-fib 从这里
# 拉 node2 那份 RIB。不能用 10.10.1.2 —— 出桥时内核把源地址改写成网桥网关。
N1_CORE=${N1_CORE:-10.10.0.2}

VRF_DST=${VRF_DST:-40}                        # node1 的目标 VPN
VRF_SRC=${VRF_SRC:-41}                        # node2 的来源 VPN（胜出那个）
VRF_SRC2=${VRF_SRC2:-42}                      # node2 的第二个来源 VPN
LBL_DST=$((1000 + VRF_DST))                   # 1040：node1 自己登记的本地标签
LBL_SRC=$((1000 + VRF_SRC))                   # 1041
LBL_SRC2=$((1000 + VRF_SRC2))                 # 1042
LEGACY_CT=$((10000 + VRF_DST))                # 改之前的颜色 0 落点（旧 ColorTable(0,40)=10040）；现在必须全程为空

PFX_DST=${PFX_DST:-10.40.0.0/16}              # node1 在 VPN 40 里宣告
PFX_SRC=${PFX_SRC:-10.41.0.0/16}              # node2 在 VPN 41 里宣告（单源）
PFX_SHARED=${PFX_SHARED:-10.44.0.0/16}        # node2 同时放在 41 与 42（双源）

RR_PORT=${RR_PORT:-18586}
RR_STATUS=${RR_STATUS:-18587}
N1_STATUS=${N1_STATUS:-18591}
N2_STATUS=${N2_STATUS:-18592}

# tap 终端的 /30 必须落在**宣告前缀内部**（这就是前缀选 /16 而不是 /24 的原因：
# 上一版用 10.41.0.0/24 配 10.41.255.0/30，内核里 `ip route replace 10.41.0.0/24
# via ...` 装得上，但 ping 10.41.255.2 直接 connect: Network is unreachable ——
# 报的是"没路由"，看着像数据面坏了，其实是宿主机自己出不了门）。
# 同时也避开 underlay 的 10.10.1/24、10.10.2/24。
TAP_N1=${TAP_N1:-tapxv1}
TAP_N2=${TAP_N2:-tapxv2}
TAP_N1_GW=${TAP_N1_GW:-10.40.255.1/30}
TAP_N1_HOST=${TAP_N1_HOST:-10.40.255.2/30}
TAP_N2_GW=${TAP_N2_GW:-10.41.255.1/30}
TAP_N2_HOST=${TAP_N2_HOST:-10.41.255.2/30}

KEEP=0
for a in "$@"; do
    case "$a" in
      --keep) KEEP=1 ;;
      -h|--help) awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 0 ;;
      *) echo "unknown option: $a" >&2; exit 2 ;;
    esac
done

PASS=0
FAIL=0
ok()   { PASS=$((PASS+1)); echo "  [PASS] $*"; }
bad()  { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "== $*"; }
die()  { echo "FATAL: $*" >&2; exit 2; }

dx()  { docker exec "$1" bash -lc "$2"; }
vpp() { docker exec "$1" bash -lc "vppctl $2" 2>/dev/null | tr -d '\r'; }

# push_local <container> <status-port> <body-json> <label> —— 扮演 sr-frr：sr-route
# 只从 PUT /local-prefixes 收本端前缀。进程刚起时端口还没 listen，curl 直接失败
# 不是被测行为，所以要重试。
push_local() {
    local c=$1 port=$2 body=$3 label=$4 i=0
    while [ "$i" -lt 50 ]; do
        if dx "$c" "curl -sf --max-time 2 -X PUT http://127.0.0.1:$port/local-prefixes \
            -H 'Content-Type: application/json' -d '$body'" >/dev/null; then
            ok "$label"
            return 0
        fi
        sleep 0.2
        i=$((i+1))
    done
    bad "$label（超时）"
    return 1
}

# ---------------------------------------------------------------- 断言工具

has_iface() { vpp "$1" "show interface $2" | awk -v n="$2" '$1==n {f=1} END {exit !f}'; }

# 重建的 WG 口开不了 mpls-input（引用计数在删口时不清零，sw_if_index 又被复用）。
# 症状是"overlay 能 ping、FIB 也装得好，但带标签的流量在收端全丢"，所以先验它。
mpls_dropped() {
    vpp "$1" "show interface features $2" |
        awk '/^mpls-input:/{getline; exit !/mpls-not-enabled/}'
}

fib_block() {   # <container> <table> <prefix> —— 该表里这条前缀的整块
    vpp "$1" "show ip fib table $2" | awk -v p="$3" '$1==p {f=1; print; next}
        f && /^[^[:space:]]/ {exit}
        f {print}'
}
route_in() {   # <container> <table> <prefix>
    vpp "$1" "show ip fib table $2" | grep -q "^$3"
}
buckets_of() {   # <container> <table> <prefix>
    vpp "$1" "show ip fib table $2" | awk -v p="$3" '$1==p {f=1; next}
        f && /dpo-load-balance/ {
            if (match($0, /buckets:[0-9]+/)) print substr($0, RSTART+8, RLENGTH-8);
            exit}'
}
have_buckets() {   # <want> <container> <table> <prefix>
    [ "$(buckets_of "$2" "$3" "$4")" = "$1" ]
}
labeled_via() {   # <container> <table> <prefix> <label> <next_hop> —— 压这个标签、走这个口
    local b
    b=$(fib_block "$1" "$2" "$3")
    printf '%s\n' "$b" | grep -qE "mpls-label\[@[0-9]+\]:\[$4:" &&
        printf '%s\n' "$b" | grep -qF "mpls via $5 $WG_IF"
}
not_labeled() {   # <container> <table> <prefix> <label> —— 这条前缀不压这个标签
    ! fib_block "$1" "$2" "$3" | grep -qE "mpls-label\[@[0-9]+\]:\[$4:"
}
label_in() {   # <container> <label> —— 本地标签有没有登记
    vpp "$1" "show mpls fib" | grep -q "^$2:"
}

dump_of() {   # <container> <table> <prefix> —— 从 ip_route_details 读回每条路径
    dx "$1" "$WORK/bin/fib-dump -table $2 -prefix $3" 2>/dev/null
}
path_label() {   # <container> <table> <prefix> <label> —— fib-dump 读回的标签里有它
    dump_of "$1" "$2" "$3" | grep -qE "标签=\[[^]]*\b$4\b"
}
path_only() {   # <container> <table> <prefix> —— 恰好一条路径
    [ "$(dump_of "$1" "$2" "$3" | grep -c '  via ')" = 1 ]
}

wait_fn() {   # <label> <timeout_s> <fn> [args...]
    local label=$1 limit=$2 i=0
    shift 2
    while [ "$i" -lt $((limit * 5)) ]; do
        if "$@"; then ok "$label"; return 0; fi
        sleep 0.2
        i=$((i + 1))
    done
    bad "$label (超时)"
    vpp "$N1" "show ip fib table $VRF_DST" | head -16 >&2
    dump_of "$N1" "$VRF_DST" "$PFX_SHARED" >&2
    return 1
}

ping_ok()   { dx "$1" "ping -c2 -W1 -i0.2 $2" >/dev/null 2>&1; }
wait_ping() {   # <container> <dst> <label> [timeout_s]
    local c=$1 dst=$2 label=$3 limit=${4:-15} i=0
    while [ "$i" -lt $((limit * 2)) ]; do
        if ping_ok "$c" "$dst"; then ok "$label"; return 0; fi
        sleep 0.5
        i=$((i + 1))
    done
    bad "$label (不通)"
    dx "$c" "ping -c2 -W1 -i0.2 $dst" 2>&1 | tail -3 >&2
    vpp "$c" "show ip fib table $VRF_DST" | head -12 >&2
    return 1
}

tap_create() {   # <container> <tap> <table> <vpp addr/mask> <host addr/mask>
    local c=$1 t=$2 tbl=$3 gw=$4 host=$5
    if has_iface "$c" "$t"; then die "$c 上已有接口 $t：先确认它不是别人的再删"; fi
    vpp "$c" "create tap if-name $t host-if-name $t host-ip4-addr $host host-mtu-size 1500" >/dev/null || die "$c 建 $t 失败"
    vpp "$c" "set interface state $t up" >/dev/null || die "$c 拉起 $t 失败"
    vpp "$c" "set interface ip table $t $tbl" >/dev/null || die "$c 把 $t 放进表 $tbl 失败（sr-fib 还没建表？）"
    vpp "$c" "set interface ip address $t $gw" >/dev/null || die "$c 给 $t 配 $gw 失败"
    dx "$c" "ip link set $t up" >/dev/null 2>&1 || true
}
tap_del() {
    vpp "$1" "delete tap $2" >/dev/null 2>&1
    dx "$1" "ip link del $2" >/dev/null 2>&1 || true
}

status_json() { dx "$N1" "curl -sf --max-time 2 http://127.0.0.1:$1/status"; }
wait_json() {   # <port> <python expr over d> <label> [timeout_s]
    local port=$1 expr=$2 label=$3 limit=${4:-25} i=0
    while [ "$i" -lt $((limit * 5)) ]; do
        if status_json "$port" |
            python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if ($expr) else 1)" 2>/dev/null; then
            ok "$label"
            return 0
        fi
        sleep 0.2
        i=$((i + 1))
    done
    bad "$label (超时)"
    status_json "$port" | python3 -m json.tool 2>/dev/null | head -40 >&2
    return 1
}
# ---------------------------------------------------------------- 清理

kill_one() {   # <container> <name>
    dx "$1" "if [ -f $WORK/$2.pid ]; then kill \$(cat $WORK/$2.pid) 2>/dev/null || true; rm -f $WORK/$2.pid; fi" >/dev/null 2>&1
}
port_busy() {   # <port> —— node1 上有没有人在听这个口
    dx "$N1" "ss -ltn" 2>/dev/null | grep -q ":$1 "
}
warn_line() {   # 最近一条 sr-fib 警告
    dx "$N1" "grep 警告 $WORK/$1 | tail -1" 2>/dev/null
}
cleanup() {
    if [ "$KEEP" = 1 ]; then
        info "--keep：进程、tap 与 FIB 状态都留着（人工拆：sr-fib -reset、删 tap、杀 $WORK/*.pid）"
        return
    fi
    info "收尾：撤 tap、停进程、撤两端 FIB 状态（含颜色表）"
    tap_del "$N1" "$TAP_N1"
    tap_del "$N2" "$TAP_N2"
    kill_one "$N2" fib
    for n in fib node2 node1 rr; do kill_one "$N1" "$n"; done
    sleep 0.3
    dx "$N1" "$WORK/bin/sr-fib -config $WORK/fib.json -reset" >/dev/null 2>&1 || true
    dx "$N2" "$WORK/bin/sr-fib -config $WORK/fib2.json -reset" >/dev/null 2>&1 || true
    dx "$N1" "rm -rf $WORK" >/dev/null 2>&1 || true
    dx "$N2" "rm -rf $WORK" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ---------------------------------------------------------------- 0 前置

info "0. 前置：wg0 与 overlay（需要先跑 test-e2e.sh --keep）"
for c in "$N1" "$N2"; do
    has_iface "$c" "$WG_IF" || die "$c 没有 $WG_IF：先跑 test/scripts/test-e2e.sh --keep"
    mpls_dropped "$c" "$WG_IF" && die "$c 的 $WG_IF 还挂在 mpls-not-enabled 上（它是被删掉又重建的口）：重启 VPP、e2e 只跑一次再来"
done
vpp "$N1" "ping $OVERLAY_N2 repeat 3 interval 0.2" | grep -q '0% packet loss' || die "node1 -> node2 overlay 不通"
vpp "$N2" "ping $OVERLAY_N1 repeat 3 interval 0.2" | grep -q '0% packet loss' || die "node2 -> node1 overlay 不通"
ok "wg0 存在、overlay 双向可达、两端收口 MPLS 都真开着"

# ---------------------------------------------------------------- 1 配置

info "1. 在 $N1 准备 $WORK（PKI、三 VPN 配置、二进制）"
dx "$N1" "rm -rf $WORK && mkdir -p $WORK/bin $WORK/pki $WORK/identity" || die "建 $WORK 失败"

docker exec -i -e WORK="$WORK" -e RR_PORT="$RR_PORT" -e RR_STATUS="$RR_STATUS" \
    -e N1_STATUS="$N1_STATUS" -e N2_STATUS="$N2_STATUS" \
    -e VRF_DST="$VRF_DST" -e VRF_SRC="$VRF_SRC" -e VRF_SRC2="$VRF_SRC2" \
    -e PFX_DST="$PFX_DST" -e PFX_SRC="$PFX_SRC" -e PFX_SHARED="$PFX_SHARED" \
    -e OVERLAY_N1="$OVERLAY_N1" -e OVERLAY_N2="$OVERLAY_N2" -e WG_IF="$WG_IF" \
    "$N1" bash -s <<'EOS' || die "写配置失败"
set -e
cd "$WORK/pki"
gen_key() { openssl genpkey -algorithm ED25519 -out "$1" 2>/dev/null; }
gen_key ca.key
openssl req -x509 -new -key ca.key -days 30 -subj "/CN=sr-xvpn-ca" -out ca.crt 2>/dev/null
gen_key rr.key
openssl req -new -key rr.key -subj "/CN=rr" -out rr.csr 2>/dev/null
printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=IP:127.0.0.1,DNS:localhost\n' > server.ext
openssl x509 -req -in rr.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 \
    -extfile server.ext -out rr.crt 2>/dev/null
for n in 1 2; do
    d="$WORK/identity/node$n"
    mkdir -p "$d"
    gen_key "$d/device_ed25519.key"
    openssl req -new -key "$d/device_ed25519.key" -subj "/CN=node$n" -out "node$n.csr" 2>/dev/null
    printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature\nextendedKeyUsage=clientAuth\nsubjectAltName=URI:srou://tenant-a/node/%s\n' "$n" > client.ext
    openssl x509 -req -in "node$n.csr" -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 \
        -extfile client.ext -out "$d/node_cert.pem" 2>/dev/null
    cp ca.crt "$d/ca_cert.pem"
    printf '{"serial":"node%s","key_gen":0,"tenant_id":"tenant-a","node_id":%s}\n' "$n" "$n" > "$d/state.json"
done

cat > "$WORK/rr.json" <<EOF
{
  "rr_id": "rr-a",
  "tenant": "tenant-a",
  "hold_sec": 6,
  "listen": "127.0.0.1:$RR_PORT",
  "status_addr": "127.0.0.1:$RR_STATUS",
  "tls": {"cert_file": "$WORK/pki/rr.crt", "key_file": "$WORK/pki/rr.key", "ca_file": "$WORK/pki/ca.crt"}
}
EOF

# node1：只有一个 VPN 40，但它 import 两个来源 RT —— "导通"就这一处配置。
cat > "$WORK/node1.json" <<EOF
{
  "tenant": "tenant-a",
  "device_id": 1,
  "identity_dir": "$WORK/identity/node1",
  "serial": "node1",
  "hold_sec": 6,
  "rrs": [{"id": "rr-a", "addr": "127.0.0.1:$RR_PORT"}],
  "tunnels": [{"peer_device_id": 2, "next_hop": "$OVERLAY_N2"}],
  "vrfs": [{"id": $VRF_DST, "export_rts": ["65000:40"], "import_rts": ["65000:41", "65000:42"]}],
  "status_addr": "127.0.0.1:$N1_STATUS"
}
EOF

# node2：VPN 41 与 42 都送 $PFX_SHARED —— 同一个前缀两个来源，就是负向那段的输入。
# 42 的 import 挂一个永不命中的 RT：它只负责导出，免得 node2 的 RIB 多出键 42。
# 宣告哪个集合不在配置里：启动后由 push_local 扮演 sr-frr 推进去，第 (4) 段换的
# 就是推的那份集合（撤回 41 的那一条），进程本身不重启。
cat > "$WORK/node2.json" <<EOF
{
  "tenant": "tenant-a",
  "device_id": 2,
  "identity_dir": "$WORK/identity/node2",
  "serial": "node2",
  "hold_sec": 6,
  "rrs": [{"id": "rr-a", "addr": "127.0.0.1:$RR_PORT"}],
  "tunnels": [{"peer_device_id": 1, "next_hop": "$OVERLAY_N1"}],
  "vrfs": [{"id": $VRF_SRC, "export_rts": ["65000:41"], "import_rts": ["65000:40"]},
           {"id": $VRF_SRC2, "export_rts": ["65000:42"], "import_rts": ["65000:99"]}],
  "status_addr": "0.0.0.0:$N2_STATUS"
}
EOF

cat > "$WORK/fib.json" <<EOF
{
  "status_url": "http://127.0.0.1:$N1_STATUS/status",
  "vpp_socket": "/run/vpp/api.sock",
  "poll_sec": 1,
  "vrfs": [$VRF_DST],
  "tunnels": [{"next_hop": "$OVERLAY_N2", "interface": "$WG_IF"}]
}
EOF

cat > "$WORK/start.sh" <<'EOF'
#!/bin/bash
set -e
cd "$(dirname "$0")"
nohup ./bin/sr-rr    -config rr.json    > rr.log    2>&1 & echo $! > rr.pid
nohup ./bin/sr-route -config node1.json > node1.log 2>&1 & echo $! > node1.pid
nohup ./bin/sr-route -config node2.json > node2.log 2>&1 & echo $! > node2.pid
nohup ./bin/sr-fib   -config fib.json   > fib.log   2>&1 & echo $! > fib.pid
EOF
chmod +x "$WORK/start.sh"
EOS

# node2 的 sr-fib：VPP 在它自己的容器里，RIB 从 node1 内核口上的 node2 状态口拉。
docker exec -i -e WORK="$WORK" -e VRF_SRC="$VRF_SRC" -e N1_CORE="$N1_CORE" \
    -e N2_STATUS="$N2_STATUS" -e OVERLAY_N1="$OVERLAY_N1" -e WG_IF="$WG_IF" \
    "$N2" bash -s <<'EOS2' || die "写 node2 配置失败"
set -e
rm -rf "$WORK"
mkdir -p "$WORK/bin"
cat > "$WORK/fib2.json" <<EOF
{
  "status_url": "http://$N1_CORE:$N2_STATUS/status",
  "vpp_socket": "/run/vpp/api.sock",
  "poll_sec": 1,
  "vrfs": [$VRF_SRC],
  "tunnels": [{"next_hop": "$OVERLAY_N1", "interface": "$WG_IF"}]
}
EOF
cat > "$WORK/start-fib.sh" <<'EOF'
#!/bin/bash
set -e
cd "$(dirname "$0")"
nohup ./bin/sr-fib -config fib2.json > fib.log 2>&1 & echo $! > fib.pid
EOF
chmod +x "$WORK/start-fib.sh"
EOS2

dx "$N1" "cd $MODULE && go build -o $WORK/bin/sr-rr ./cmd/sr-rr &&
    go build -o $WORK/bin/sr-route ./cmd/sr-route &&
    go build -o $WORK/bin/sr-fib ./cmd/sr-fib &&
    go build -o $WORK/bin/fib-dump ./tools/fib-dump" || die "node1 编译失败"
dx "$N2" "cd $MODULE && go build -o $WORK/bin/sr-fib ./cmd/sr-fib" || die "node2 编译失败"
ok "四个二进制已编译"

# 上一轮死在半路会留下表；只有它确实像本脚本的残留才清。
clean_residue() {   # <container> <fib 配置文件> <table> <label> <prefix>
    if vpp "$1" "show ip fib table $3" | grep -q "^ipv4-VRF:$3"; then
        if label_in "$1" "$4" || route_in "$1" "$3" "$5"; then
            info "清理 $1 表 $3 的上一轮残留"
            dx "$1" "$WORK/bin/sr-fib -config $WORK/$2 -reset" >/dev/null 2>&1 || true
        else
            die "$1 的表 $3 已被别的进程占用（既无标签 $4 也无前缀 $5）"
        fi
    fi
}
clean_residue "$N1" fib.json "$VRF_DST" "$LBL_DST" "$PFX_SRC"
clean_residue "$N2" fib2.json "$VRF_SRC" "$LBL_SRC" "$PFX_DST"

# ---------------------------------------------------------------- 2 控制面

info "2. 启动 rr / sr-route x2 / sr-fib"
# 先确认三个口没人听：上一轮没杀干净的进程会让这一轮的失败完全看不出来
# （node2 的 sr-route 报 bind: address already in use 但照常跑 RR 会话，
#  node2 的 sr-fib 拉不到 RIB，症状却是"对端路由没装上"）。
for p in "$RR_PORT" "$N1_STATUS" "$N2_STATUS"; do
    port_busy "$p" && die "node1 上端口 $p 已被占用：上一轮的 sr-* 还活着，先杀干净（pgrep -af 'sr-(rr|route|fib)'）"
done
dx "$N1" "bash $WORK/start.sh" || die "启动失败"
sleep 0.5

info "   推本端前缀（扮演 sr-frr 的 PUT /local-prefixes）"
push_local "$N1" "$N1_STATUS" \
    "{\"prefixes\":[{\"vrf_id\":$VRF_DST,\"prefix\":\"$PFX_DST\"}]}" \
    "node1 宣告 $PFX_DST@vrf$VRF_DST" || die "node1 前缀推不进去"
push_local "$N1" "$N2_STATUS" \
    "{\"prefixes\":[{\"vrf_id\":$VRF_SRC,\"prefix\":\"$PFX_SRC\"},{\"vrf_id\":$VRF_SRC,\"prefix\":\"$PFX_SHARED\"},{\"vrf_id\":$VRF_SRC2,\"prefix\":\"$PFX_SHARED\"}]}" \
    "node2 宣告 $PFX_SRC@vrf$VRF_SRC 与 $PFX_SHARED@vrf$VRF_SRC/vrf$VRF_SRC2" || die "node2 前缀推不进去"

wait_json "$N1_STATUS" \
    "sorted([(r['vrf_id'], r['prefix']) for r in d['rib'].get('$VRF_DST', [])]) == [($VRF_SRC, '$PFX_SRC'), ($VRF_SRC, '$PFX_SHARED'), ($VRF_SRC2, '$PFX_SHARED')]" \
    "node1 的 RIB：目标表 $VRF_DST 下三条（$PFX_SRC 来自 $VRF_SRC；$PFX_SHARED 同时来自 $VRF_SRC 和 $VRF_SRC2）"

# ---------------------------------------------------------------- 3 正向落表

info "(1) 跨 VPN：装在表 $VRF_DST（目标），压标签 $LBL_SRC（来源）"
wait_fn "表 $VRF_DST 里出现 $PFX_SRC" 10 route_in "$N1" "$VRF_DST" "$PFX_SRC"
wait_fn "该路由压 $LBL_SRC（= 1000 + 来源 VPN $VRF_SRC）经 $WG_IF 发往 $OVERLAY_N2" 10 \
    labeled_via "$N1" "$VRF_DST" "$PFX_SRC" "$LBL_SRC" "$OVERLAY_N2"
not_labeled "$N1" "$VRF_DST" "$PFX_SRC" "$LBL_DST" &&
    ok "它压的不是目标 VPN 自己的标签 $LBL_DST（装在谁的表 ≠ 压谁的标签）" ||
    bad "路由压了 $LBL_DST：目标 VPN 的标签被当成出口，对端弹栈后无处可查"
label_in "$N1" "$LBL_DST" &&
    ok "node1 只登记自己那个 VPN 的本地标签 $LBL_DST" ||
    bad "node1 没登记本地标签 $LBL_DST（收端弹栈后查不到表）"

info "(2) 颜色 0 的落点就是主表 $VRF_DST（表号 = 色*1000+主表，2026-11-03 更正）：只有一份，不再有 10000+主表 副本"
[ "$(buckets_of "$N1" "$VRF_DST" "$PFX_SRC")" = "1" ] &&
    ok "主表 $VRF_DST 里这条前缀只有 1 个 bucket（单隧道、单份路径）" ||
    bad "主表 $VRF_DST 里 $PFX_SRC 的 bucket 数不是 1：$(buckets_of "$N1" "$VRF_DST" "$PFX_SRC")"
if route_in "$N1" "$LEGACY_CT" "$PFX_SRC"; then
    bad "表 $LEGACY_CT 里出现了 $PFX_SRC：颜色 0 不该再产生 10000+主表 的副本表"
else
    ok "负向：表 $LEGACY_CT 是空的（lab 里那张 10040 副本不再出现）"
fi
info "     主表读回：$(dump_of "$N1" "$VRF_DST" "$PFX_SRC" | tr '\n' ' ')"
info "     老落点 $LEGACY_CT 读回：$(dump_of "$N1" "$LEGACY_CT" "$PFX_SRC" | tr '\n' ' ')"

info "反向：node2 的表 $VRF_SRC 里装 node1 的 $PFX_DST，压 $LBL_DST"
dx "$N2" "bash $WORK/start-fib.sh" || die "node2 sr-fib 启动失败"
wait_fn "node2 的表 $VRF_SRC 里出现 $PFX_DST" 20 route_in "$N2" "$VRF_SRC" "$PFX_DST"
wait_fn "node2 的路由压 $LBL_DST 经 $WG_IF 发往 $OVERLAY_N1" 15 \
    labeled_via "$N2" "$VRF_SRC" "$PFX_DST" "$LBL_DST" "$OVERLAY_N1"
label_in "$N2" "$LBL_SRC" &&
    ok "node2 登记了本地标签 $LBL_SRC（node1 送进来的那条弹栈后查表 $VRF_SRC）" ||
    bad "node2 没登记 $LBL_SRC"
# 本地前缀不由 sr-fib 自装（#63 去掉的路径）：node2 表里不该有自己的 10.41.0.0/16。
route_in "$N2" "$VRF_SRC" "$PFX_SRC" &&
    bad "node2 的表里有自己宣告的 $PFX_SRC（自装前缀那条路径已经删掉了）" ||
    ok "node2 的表里没有自己宣告的 $PFX_SRC（本地前缀不自装，收端只认对端送进来的）"

# ---------------------------------------------------------------- 4 真报文

info "(1) 两端各建一个 tap 终端，跨 VPN 的标签路由真的在转发"
tap_create "$N1" "$TAP_N1" "$VRF_DST" "$TAP_N1_GW" "$TAP_N1_HOST"
tap_create "$N2" "$TAP_N2" "$VRF_SRC" "$TAP_N2_GW" "$TAP_N2_HOST"
dx "$N1" "ip route replace $PFX_SRC via ${TAP_N1_GW%/*} dev $TAP_N1" || die "node1 内核路由失败"
dx "$N2" "ip route replace $PFX_DST via ${TAP_N2_GW%/*} dev $TAP_N2" || die "node2 内核路由失败"
wait_ping "$N1" "${TAP_N2_HOST%/*}" "node1 终端 -> 表 $VRF_DST -> 标签 $LBL_SRC -> $WG_IF -> node2 表 $VRF_SRC" 20
wait_ping "$N2" "${TAP_N1_HOST%/*}" "反向也通（表 $VRF_SRC -> 标签 $LBL_DST -> node1 表 $VRF_DST）" 20

# ---------------------------------------------------------------- 5 择一

info "(3) 负向：$PFX_SHARED 由 VPN $VRF_SRC 与 $VRF_SRC2 同时送进来 ⇒ 不能同桶"
wait_fn "表 $VRF_DST 里有 $PFX_SHARED" 15 route_in "$N1" "$VRF_DST" "$PFX_SHARED"
wait_fn "它只有 1 个 bucket（两个标签不能同桶）" 15 have_buckets 1 "$N1" "$VRF_DST" "$PFX_SHARED"
wait_fn "读回也只有 1 条路径" 10 path_only "$N1" "$VRF_DST" "$PFX_SHARED"
path_label "$N1" "$VRF_DST" "$PFX_SHARED" "$LBL_SRC" &&
    ok "留下的是 RD 更小那个源（device 2 / VPN $VRF_SRC → 标签 $LBL_SRC）" ||
    bad "留下的不是 $LBL_SRC（RD 择一没按 来源<<8|VPN 比）"
path_label "$N1" "$VRF_DST" "$PFX_SHARED" "$LBL_SRC2" &&
    bad "$LBL_SRC2 那条也装进了 FIB（§2.4 的静默投递回来了）" ||
    ok "输掉的那个源（$LBL_SRC2）没有偷偷进 FIB"
warn_line fib.log | grep -q 择一 &&
    ok "fib.log 里有择一警告：$(warn_line fib.log | sed 's/.*警告: //' | cut -c1-110)…" ||
    bad "被丢掉的那个源没有任何痕迹"

# ---------------------------------------------------------------- 6 择一跟着 RD

info "(4) 撤掉 VPN $VRF_SRC 那份 $PFX_SHARED：同一前缀必须改压 $LBL_SRC2"
# 盒子上的"撤回"是 sr-frr 重新推一份集合，不是重启 sr-route：这里照做。
push_local "$N1" "$N2_STATUS" \
    "{\"prefixes\":[{\"vrf_id\":$VRF_SRC,\"prefix\":\"$PFX_SRC\"},{\"vrf_id\":$VRF_SRC2,\"prefix\":\"$PFX_SHARED\"}]}" \
    "node2 只留 vrf$VRF_SRC2 送 $PFX_SHARED" || die "node2 前缀集推不进去"
wait_json "$N1_STATUS" \
    "[r['vrf_id'] for r in d['rib'].get('$VRF_DST', []) if r['prefix'] == '$PFX_SHARED'] == [$VRF_SRC2]" \
    "node1 的 RIB 里 $PFX_SHARED 只剩来源 $VRF_SRC2（41 那份已被撤回）" 30
wait_fn "FIB 里 $PFX_SHARED 仍只有 1 条路径" 15 path_only "$N1" "$VRF_DST" "$PFX_SHARED"
wait_fn "标签换成了 $LBL_SRC2" 15 path_label "$N1" "$VRF_DST" "$PFX_SHARED" "$LBL_SRC2"
path_label "$N1" "$VRF_DST" "$PFX_SHARED" "$LBL_SRC" &&
    bad "$LBL_SRC 还留着：择一跟着的是插入顺序不是 RD" ||
    ok "$LBL_SRC 那份已经不在了（赢家随来源变化重算，不是先到先得）"

info "(4) 把 VPN $VRF_SRC 那份加回来：又回到 $LBL_SRC 胜出"
push_local "$N1" "$N2_STATUS" \
    "{\"prefixes\":[{\"vrf_id\":$VRF_SRC,\"prefix\":\"$PFX_SRC\"},{\"vrf_id\":$VRF_SRC,\"prefix\":\"$PFX_SHARED\"},{\"vrf_id\":$VRF_SRC2,\"prefix\":\"$PFX_SHARED\"}]}" \
    "node2 恢复 vrf$VRF_SRC 也送 $PFX_SHARED" || die "node2 前缀集推不进去"
wait_json "$N1_STATUS" \
    "sorted([(r['vrf_id'], r['prefix']) for r in d['rib'].get('$VRF_DST', []) if r['prefix'] == '$PFX_SHARED']) == [($VRF_SRC, '$PFX_SHARED'), ($VRF_SRC2, '$PFX_SHARED')]" \
    "node1 的 RIB 里 $PFX_SHARED 又回到两个来源" 30
wait_fn "FIB 里 $PFX_SHARED 又压回 $LBL_SRC" 15 path_label "$N1" "$VRF_DST" "$PFX_SHARED" "$LBL_SRC"
path_label "$N1" "$VRF_DST" "$PFX_SHARED" "$LBL_SRC2" &&
    bad "$LBL_SRC2 也进来了" || ok "$LBL_SRC2 那条被留在 FIB 之外（三轮一致：赢家只由 RD 决定）"
warn_line fib.log | grep -q 择一 && ok "警告仍在（这一轮又丢了一次 $LBL_SRC2）" || bad "这一轮没有择一警告"

echo
echo "== 结果：$PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
    for f in fib node1 node2 rr; do
        echo "---- $f.log" >&2
        dx "$N1" "tail -25 $WORK/$f.log" >&2 2>/dev/null || true
    done
    echo "---- node2 fib.log" >&2
    dx "$N2" "tail -25 $WORK/fib.log" >&2 2>/dev/null || true
    exit 1
fi
