#!/bin/bash
# test-ecmp-color.sh - 同对端两条同色隧道落成两个 bucket，改色则搬进颜色表（O2 + O3 判据）
#
# 这条脚本回答五件事，每件都有一个"看起来对但其实错"的反例：
#
#   (1) 本机隧道表里对同一个对端有**两条**隧道时，控制面反射过来的前缀会不会
#       只剩一条路径？协议 v3 的下一跳由本机隧道表解析（前缀本身不带下一跳），
#       route 包里"同对端并行隧道"是合法输入，但合法不等于解析得出来。
#   (2) 两条颜色 0 的隧道在 VPP 里是不是**并用**：`buckets:2` 只是形态，真正的
#       证据是每条路径的 preference。`show ip fib` 不打印 preference，所以这一步
#       用 fib-dump 从 ip_route_details 读回 230/230。
#   (3) 把其中一条 wg 口 `set interface state down`，FIB 是不是自己收成
#       `buckets:1` 而且流量不断；同时我们的计划里那条路径**还在**（没有 BFD
#       就没有可达性信息，fail-open）。这两条一起才说明"bucket 变 1"是 VPP
#       原生算的，不是我们的控制面循环干的 —— O4 的逃生开关靠的就是这个机制。
#   (4) 恢复那条口，bucket 自己回到 2。
#
# 第 (5) 件事是 O3-b 的判据，也是"逃生开关关着"（定案）那条规则的实证：
#       把 wg1 改成颜色 3 再重启 sr-fib，**不是**"主表里那条路径的 preference
#       变了"，而是**搬表** —— 主表 $VRF 只留颜色 0 的那条（buckets 回 1），
#       改色的那条只存在于 ColorTable(3,$VRF)=3*1000+10000+$VRF（§4.2 公式）。
#       同时颜色 0 那条在主表和 ColorTable(0,$VRF) 里各有一份（双写是定案）。
#       反例长这样：两条路径都留在主表、只差 preference —— 那是逃生开关**打开**
#       时的形态，归 O4，不是现在这份代码该产出的。
#
# 为什么用"重启 sr-fib"来改色：新进程的上一份计划是空的，所以走原地 add（整条
# 替换）而不是 §2.2 的先删后加；这一步同时回答"重启改色生不生效"。
#
# 前置：**必须先跑过 test-e2e.sh --keep 与 test-multitunnel.sh --keep**
# （node1↔node2 之间 wg0@192.168.99.x 与 wg1@192.168.100.x 两条隧道都在、都通）。
# 本脚本只在这上面加控制面（rr / sr-route x2 / sr-fib x2）与两个 tap 终端，
# 收尾把自己装的 FIB 状态、tap、进程全部撤掉，不动两条隧道。
#
# 用法:
#   test/scripts/test-ecmp-color.sh [--keep]
set -u

N1=srwg-node1
N2=srwg-node2
MODULE=/workspace/controller
WORK=${WORK:-/tmp/sr-ecmp-e2e}

WG_IF=${WG_IF:-wg0}
WG2_IF=${WG2_IF:-wg1}
OVERLAY_N1=${OVERLAY_N1:-192.168.99.1}
OVERLAY_N2=${OVERLAY_N2:-192.168.99.2}
OVERLAY2_N2=${OVERLAY2_N2:-192.168.100.2}
# node1 内核在**管理 bridge**（10.10.0.0/24）上的地址：node2 容器里的 sr-fib 从
# 这里拉 node2 那份 RIB。不能用 10.10.1.2 —— 出桥时内核会把源地址改写成网桥
# 网关（docker-compose.yml 里那条实测注释），连不上不说，还很难查。
N1_CORE=${N1_CORE:-10.10.0.2}

VRF=${VRF:-100}
LABEL=$((1000 + VRF))
REMOTE_PFX=${REMOTE_PFX:-10.20.0.0/16}   # node2 宣告、node1 要用两条隧道装进 FIB
LOCAL_PFX=${LOCAL_PFX:-10.10.0.0/16}     # node1 宣告，只用来让反向有路
COLOR2=${COLOR2:-3}                       # 观测段把第二条隧道的颜色换成它

RR_PORT=${RR_PORT:-18556}
RR_STATUS=${RR_STATUS:-18557}
N1_STATUS=${N1_STATUS:-18571}
N2_STATUS=${N2_STATUS:-18572}

TAP_N1=${TAP_N1:-tapecmp1}
TAP_N2=${TAP_N2:-tapecmp2}
TAP_N1_GW=${TAP_N1_GW:-10.10.255.1/30}
TAP_N1_HOST=${TAP_N1_HOST:-10.10.255.2/30}
TAP_N2_GW=${TAP_N2_GW:-10.20.255.1/30}
TAP_N2_HOST=${TAP_N2_HOST:-10.20.255.2/30}

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

# push_local <container> <status-port> <prefix> <label> —— 扮演 sr-frr：sr-route 只从
# PUT /local-prefixes 收本端前缀。进程刚起时端口还没 listen，curl 直接失败不是被测
# 行为，所以要重试。
push_local() {
    local c=$1 port=$2 prefix=$3 label=$4 i=0
    while [ "$i" -lt 50 ]; do
        if dx "$c" "curl -sf --max-time 2 -X PUT http://127.0.0.1:$port/local-prefixes \
            -H 'Content-Type: application/json' \
            -d '{\"prefixes\":[{\"vrf_id\":$VRF,\"prefix\":\"$prefix\"}]}'" >/dev/null; then
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
fib_table() { vpp "${1:-$N1}" "show ip fib table $VRF"; }
route_in()  { fib_table "$1" | grep -q "^$2"; }
label_in()  { vpp "${1:-$N1}" "show mpls fib" | grep -q "^$LABEL:"; }

# mpls-input 走的是 feature arc：接口没"开 MPLS"时弧上挂的是 mpls-not-enabled 这条
# 丢弃路径。set interface mpls ... enable 只在引用计数 0<->1 翻转时才改弧（VPP 26.06
# src/vnet/mpls/interface.c），而计数在接口删除时**不清零**、sw_if_index 又会被复用
# —— 所以"删掉再建"的那条隧道，enable 是静默空操作，弧永远停在丢弃路径上。
# 症状很阴：overlay 地址能 ping（那是普通 IP），FIB 也正常装两个 bucket，但带标签的
# VPN 流量在收端 mpls-input 全丢。跑测之前先验这一点，别让它伪装成"ECMP 不通"。
mpls_dropped() {   # <container> <iface> -> 该接口的 mpls-input 弧还在丢弃路径上
    vpp "$1" "show interface features $2" |
        awk '/^mpls-input:/{getline; exit !/mpls-not-enabled/}'
}

buckets_of() {   # <container> <prefix> -> 该前缀 dpo-load-balance 的 buckets 数
    fib_table "$1" | awk -v p="$2" '$1==p {f=1; next}
        f && /dpo-load-balance/ {
            if (match($0, /buckets:[0-9]+/)) print substr($0, RSTART+8, RLENGTH-8);
            exit}'
}
# 一条 2-bucket 路由在 show ip fib 里占 6~7 行，用 grep -A6 会正好切掉第二条路径。
entry_block() {   # <container> <prefix> —— 该前缀的整块，到下一条表项（顶格行）为止
    fib_table "$1" | awk -v p="$2" '$1==p {f=1; print; next}
        f && /^[^[:space:]]/ {exit}
        f {print}'
}
have_buckets() {   # <want> <container> <prefix>
    [ "$(buckets_of "$2" "$3")" = "$1" ]
}

dump_table() {   # <table> <prefix> —— 读回任意表里这条前缀（没有该前缀时输出为空）
    dx "$N1" "$WORK/bin/fib-dump -table $1 -prefix $2" 2>/dev/null
}
dump_of() {   # 读回 preference —— show ip fib 打不出来，只能走 ip_route_details
    dump_table "$VRF" "$REMOTE_PFX"
}
path_pref() {   # <next_hop> <preference>
    dump_of | grep -F "via $1 " | grep -q "preference=$2"
}
path_count() {
    [ "$(dump_of | grep -c '  via ')" = "$1" ]
}

# §4.2 的表号公式在脚本侧独立算一遍：这一步要断言的正是"sr-fib 落的是不是
# 这张表"，复用它的输出就成了自己给自己判卷。
color_table() { echo $(( $1 * 1000 + 10000 + $2 )); }   # <color> <main>
table_has() {   # <table> <prefix>
    [ -n "$(dump_table "$1" "$2")" ]
}
table_path() {   # <table> <next_hop> <preference> —— 固定看 $REMOTE_PFX
    dump_table "$1" "$REMOTE_PFX" | grep -F "via $2 " | grep -q "preference=$3"
}
table_buckets() {   # <table> <prefix>
    vpp "$N1" "show ip fib table $1" | awk -v p="$2" '$1==p {f=1; next}
        f && /dpo-load-balance/ {
            if (match($0, /buckets:[0-9]+/)) print substr($0, RSTART+8, RLENGTH-8);
            exit}'
}
table_block() {   # <table> <prefix> —— 该表里这条前缀的整块
    vpp "$N1" "show ip fib table $1" | awk -v p="$2" '$1==p {f=1; print; next}
        f && /^[^[:space:]]/ {exit}
        f {print}'
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
    fib_table "$N1" | head -14 >&2
    dump_of | head -6 >&2
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
    fib_table "$c" | head -10 >&2
    return 1
}

tap_create() {   # <container> <tap> <vpp addr/mask> <host addr/mask>
    local c=$1 t=$2 gw=$3 host=$4
    if has_iface "$c" "$t"; then die "$c 上已有接口 $t：先确认它不是别人的再删"; fi
    vpp "$c" "create tap if-name $t host-if-name $t host-ip4-addr $host host-mtu-size 1500" >/dev/null || die "$c 建 $t 失败"
    vpp "$c" "set interface state $t up" >/dev/null || die "$c 拉起 $t 失败"
    # 表必须先由 sr-fib 建好，否则这里回 "no such table"。
    vpp "$c" "set interface ip table $t $VRF" >/dev/null || die "$c 把 $t 放进表 $VRF 失败（sr-fib 还没建表？）"
    vpp "$c" "set interface ip address $t $gw" >/dev/null || die "$c 给 $t 配 $gw 失败"
    dx "$c" "ip link set $t up" >/dev/null 2>&1 || true
}
tap_del() {
    vpp "$1" "delete tap $2" >/dev/null 2>&1
    dx "$1" "ip link del $2" >/dev/null 2>&1 || true
}

status_hops() {   # node1 的 RIB 里该前缀解析出的下一跳
    # 排序去重：线上规范序是"按下一跳字符串字典序"（100.2 排在 99.2 前面），
    # 这里要断言的是**集合**，不是顺序。
    dx "$N1" "curl -sf --max-time 2 http://127.0.0.1:$N1_STATUS/status" |
        python3 -c "
import json,sys
d = json.load(sys.stdin)
for r in d['rib'].get('$VRF', []):
    if r['prefix'] == '$REMOTE_PFX':
        print(','.join(sorted(set(r['next_hops']))))
        break
" 2>/dev/null
}
HOPS_WANT=$(printf '%s\n' "$OVERLAY_N2" "$OVERLAY2_N2" | LC_ALL=C sort | paste -sd, -)

# ---------------------------------------------------------------- 清理

kill_one() {   # <container> <name>
    dx "$1" "if [ -f $WORK/$2.pid ]; then kill \$(cat $WORK/$2.pid) 2>/dev/null || true; rm -f $WORK/$2.pid; fi" >/dev/null 2>&1
}
fib_tail() {   # <file> —— 这份 sr-fib 日志里最后一次收敛的那一行
    dx "$N1" "grep 收敛 $WORK/$1 | tail -1" 2>/dev/null
}
tunnels_line() {
    dx "$N1" "grep -o 'tunnels.*' $WORK/fib.json | tr -d '\n' | tail -c 220"
}
cleanup() {
    if [ "$KEEP" = 1 ]; then
        info "--keep：进程、tap 与 FIB 状态都留着（人工拆：sr-fib -reset、删 tap、杀 $WORK/*.pid）"
        return
    fi
    info "收尾：拉起被 down 的口、撤 tap、停进程、撤 FIB 状态"
    vpp "$N1" "set interface state $WG2_IF up" >/dev/null 2>&1
    tap_del "$N1" "$TAP_N1"
    tap_del "$N2" "$TAP_N2"
    kill_one "$N2" fib
    for n in fib node1 node2 rr; do kill_one "$N1" "$n"; done
    sleep 0.3
    dx "$N1" "$WORK/bin/sr-fib -config $WORK/fib.json -reset" >/dev/null 2>&1 || true
    dx "$N2" "$WORK/bin/sr-fib -config $WORK/fib2.json -reset" >/dev/null 2>&1 || true
    dx "$N1" "rm -rf $WORK" >/dev/null 2>&1 || true
    dx "$N2" "rm -rf $WORK" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ---------------------------------------------------------------- 0 前置

info "0. 前置：node1↔node2 两条隧道（test-e2e.sh --keep + test-multitunnel.sh --keep）"
for c in "$N1" "$N2"; do
    for i in "$WG_IF" "$WG2_IF"; do
        has_iface "$c" "$i" || die "$c 上没有 $i：这两条隧道要由 e2e / multitunnel 建"
    done
done
vpp "$N1" "ping $OVERLAY_N2 repeat 3 interval 0.2"  | grep -q '0% packet loss' || die "隧道 1 不通"
vpp "$N1" "ping $OVERLAY2_N2 repeat 3 interval 0.2" | grep -q '0% packet loss' || die "隧道 2 不通"
for c in "$N1" "$N2"; do
    for i in "$WG_IF" "$WG2_IF"; do
        mpls_dropped "$c" "$i" && die "$c 的 $i 还挂在 mpls-not-enabled 上：它是被删掉又重建的隧道，enable 对它已经是空操作。重启 VPP、e2e 与 multitunnel 各只跑一次（中途别重跑）再来"
    done
done
ok "两条隧道都在、都通，且四条（2 端 x 2 口）收端 MPLS 都真的开着"

# ---------------------------------------------------------------- 1 配置

info "1. 在 $N1 准备 $WORK（PKI、配置、四个二进制）"
dx "$N1" "rm -rf $WORK && mkdir -p $WORK/bin $WORK/pki $WORK/identity" || die "建 $WORK 失败"

docker exec -i -e WORK="$WORK" -e RR_PORT="$RR_PORT" -e RR_STATUS="$RR_STATUS" \
    -e N1_STATUS="$N1_STATUS" -e N2_STATUS="$N2_STATUS" -e N1_CORE="$N1_CORE" \
    -e VRF="$VRF" -e LABEL="$LABEL" \
    -e OVERLAY_N1="$OVERLAY_N1" -e OVERLAY_N2="$OVERLAY_N2" \
    -e OVERLAY2_N2="$OVERLAY2_N2" -e WG_IF="$WG_IF" -e WG2_IF="$WG2_IF" \
    -e REMOTE_PFX="$REMOTE_PFX" -e LOCAL_PFX="$LOCAL_PFX" \
    "$N1" bash -s <<'EOS' || die "写配置失败"
set -e
cd "$WORK/pki"
gen_key() { openssl genpkey -algorithm ED25519 -out "$1" 2>/dev/null; }
gen_key ca.key
openssl req -x509 -new -key ca.key -days 30 -subj "/CN=sr-ecmp-ca" -out ca.crt 2>/dev/null
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

# node1 对**同一个对端**宣告两条隧道 —— 这正是"同对端并行隧道"的输入形态。
cat > "$WORK/node1.json" <<EOF
{
  "tenant": "tenant-a",
  "device_id": 1,
  "identity_dir": "$WORK/identity/node1",
  "serial": "node1",
  "hold_sec": 6,
  "rrs": [{"id": "rr-a", "addr": "127.0.0.1:$RR_PORT"}],
  "tunnels": [{"peer_device_id": 2, "next_hop": "$OVERLAY_N2"},
              {"peer_device_id": 2, "next_hop": "$OVERLAY2_N2"}],
  "vrfs": [{"id": $VRF, "export_rts": ["65000:100"], "import_rts": ["65000:101"]}],
  "status_addr": "127.0.0.1:$N1_STATUS"
}
EOF

# node2 只留一条隧道：反向若也 ECMP，会有一半回包从被 down 掉的那条口走，
# 就分不清"bucket 少了一个"和"回包换了条路"。本脚本测的是 node1 这一侧。
cat > "$WORK/node2.json" <<EOF
{
  "tenant": "tenant-a",
  "device_id": 2,
  "identity_dir": "$WORK/identity/node2",
  "serial": "node2",
  "hold_sec": 6,
  "rrs": [{"id": "rr-a", "addr": "127.0.0.1:$RR_PORT"}],
  "tunnels": [{"peer_device_id": 1, "next_hop": "$OVERLAY_N1"}],
  "vrfs": [{"id": $VRF, "export_rts": ["65000:101"], "import_rts": ["65000:100"]}],
  "status_addr": "0.0.0.0:$N2_STATUS"
}
EOF

# 颜色写在盒子自己的隧道表里：这就是 O2 里"控制器那一列"要到设备上的落点
# （眼下由这份文件承担）。两条都写颜色，缺省那条是 0。
write_fib() {   # <c0> <c1> <outfile>
    cat > "$3" <<EOF
{
  "status_url": "http://127.0.0.1:$N1_STATUS/status",
  "vpp_socket": "/run/vpp/api.sock",
  "poll_sec": 1,
  "vrfs": [$VRF],
  "tunnels": [{"next_hop": "$OVERLAY_N2", "interface": "$WG_IF", "color": $1},
              {"next_hop": "$OVERLAY2_N2", "interface": "$WG2_IF", "color": $2}]
}
EOF
}
write_fib 0 0 "$WORK/fib.json"

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

# node2 的 sr-fib：VPP 在另一个容器，RIB 从 node1 内核口上的 node2 状态口拉。
docker exec -i -e WORK="$WORK" -e VRF="$VRF" -e N1_CORE="$N1_CORE" -e N2_STATUS="$N2_STATUS" \
    -e OVERLAY_N1="$OVERLAY_N1" -e WG_IF="$WG_IF" "$N2" bash -s <<'EOS2' || die "写 node2 配置失败"
set -e
rm -rf "$WORK"
mkdir -p "$WORK"
cat > "$WORK/fib2.json" <<EOF
{
  "status_url": "http://$N1_CORE:$N2_STATUS/status",
  "vpp_socket": "/run/vpp/api.sock",
  "poll_sec": 1,
  "vrfs": [$VRF],
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

# 上一轮死在半路会留下这张表；只有它确实像本脚本的残留才清。
if fib_table "$N1" | grep -q "^ipv4-VRF:$VRF"; then
    if label_in "$N1" || route_in "$N1" "$REMOTE_PFX"; then
        info "清理 $N1 上一轮残留（标签 $LABEL 或前缀 $REMOTE_PFX）"
        dx "$N1" "$WORK/bin/sr-fib -config $WORK/fib.json -reset" >/dev/null 2>&1 || true
    else
        die "$N1 的 VRF $VRF 已被别的进程占用（既无标签 $LABEL 也无前缀 $REMOTE_PFX）"
    fi
fi

# ---------------------------------------------------------------- 2 跑起来

info "2. 启动 rr / sr-route x2 / sr-fib，看控制面解析出几条下一跳"
dx "$N1" "bash $WORK/start.sh" || die "启动失败"
sleep 0.5

info "   推本端前缀（扮演 sr-frr 的 PUT /local-prefixes）"
push_local "$N1" "$N1_STATUS" "$LOCAL_PFX" "node1 宣告 $LOCAL_PFX@vrf$VRF" || die "node1 前缀推不进去"
push_local "$N1" "$N2_STATUS" "$REMOTE_PFX" "node2 宣告 $REMOTE_PFX@vrf$VRF" || die "node2 前缀推不进去"

i=0
while [ "$i" -lt 125 ]; do
    [ "$(status_hops)" = "$HOPS_WANT" ] && break
    sleep 0.2
    i=$((i + 1))
done
if [ "$(status_hops)" = "$HOPS_WANT" ]; then
    ok "RIB：$REMOTE_PFX 解析出两条下一跳（$OVERLAY_N2 / $OVERLAY2_N2，同对端并行隧道都留下了）"
else
    bad "RIB 下一跳不是两条（期望 '$HOPS_WANT'，得到 '$(status_hops)'）"
fi

# ---------------------------------------------------------------- 3 落 FIB

info "3. (1)(2) 同色两条隧道 → buckets:2，且两条路径的 preference 都是 230"
wait_fn "VRF $VRF 里出现 $REMOTE_PFX" 10 route_in "$N1" "$REMOTE_PFX" || die "路由没装上"
wait_fn "读回两条路径" 10 path_count 2
wait_fn "$REMOTE_PFX 是 2 个 bucket" 15 have_buckets 2 "$N1" "$REMOTE_PFX"
wait_fn "$WG_IF 那条 preference=230（颜色 0）" 10 path_pref "$OVERLAY_N2" 230
wait_fn "$WG2_IF 那条 preference=230（同色才是并用）" 10 path_pref "$OVERLAY2_N2" 230
entry_block "$N1" "$REMOTE_PFX" | grep -q "mpls via $OVERLAY2_N2 $WG2_IF" &&
    ok "第二条路径确实是 $WG2_IF → $OVERLAY2_N2 且压标签 $LABEL" ||
    bad "第二条路径不是 $WG2_IF"
info "     show ip fib 该前缀整块："
entry_block "$N1" "$REMOTE_PFX" | sed 's/^/       /'
info "     fib-dump 读回："
dump_of | sed 's/^/       /'

# ---------------------------------------------------------------- 4 真流量

info "4. 两端各建一个 tap 终端，让这条两 bucket 的路由真的转发"
dx "$N2" "bash $WORK/start-fib.sh" || die "node2 sr-fib 启动失败"
wait_fn "node2 的 VRF 里出现 $LOCAL_PFX" 20 route_in "$N2" "$LOCAL_PFX"
tap_create "$N1" "$TAP_N1" "$TAP_N1_GW" "$TAP_N1_HOST"
tap_create "$N2" "$TAP_N2" "$TAP_N2_GW" "$TAP_N2_HOST"
dx "$N1" "ip route replace $REMOTE_PFX via ${TAP_N1_GW%/*} dev $TAP_N1" || die "node1 内核路由失败"
dx "$N2" "ip route replace $LOCAL_PFX via ${TAP_N2_GW%/*} dev $TAP_N2" || die "node2 内核路由失败"
wait_ping "$N1" "${TAP_N2_HOST%/*}" "终端 → 两个 bucket → 隧道 → 对端终端" 20

# ---------------------------------------------------------------- 5 打一条口

info "5. (3) 在 VPP 里 down 掉 $WG2_IF：bucket 自己变 1，流量不断"
conv=$(dx "$N1" "grep -c 收敛 $WORK/fib.log")
vpp "$N1" "set interface state $WG2_IF down" >/dev/null || die "down $WG2_IF 失败"
wait_fn "$REMOTE_PFX 只剩 1 个 bucket" 15 have_buckets 1 "$N1" "$REMOTE_PFX"
entry_block "$N1" "$REMOTE_PFX" | grep -q "mpls via $OVERLAY_N2 $WG_IF" &&
    ok "活下来的是 $WG_IF → $OVERLAY_N2" || bad "活下来的不是 $WG_IF"
# 为什么用"sr-fib 有没有再下发"来判：本机没跑 BFD，所以它的计划不会因为口
# down 而变。若 bucket 变 1 的同时 sr-fib 一次都没动，那 1 就是 VPP 自己算的 ——
# 这正是 O4 逃生开关要依赖的机制。
conv_after=$(dx "$N1" "grep -c 收敛 $WORK/fib.log")
if [ "$conv_after" = "$conv" ]; then
    ok "sr-fib 一次都没再下发：bucket 变 1 是 VPP 原生重算，不是我们撤的路径"
else
    bad "sr-fib 在口 down 后又收敛了（$conv → $conv_after）：那 1 个 bucket 是我们改的，不是 VPP 算的"
fi
wait_ping "$N1" "${TAP_N2_HOST%/*}" "一条隧道 down 之后 ping 仍然通" 20

info "6. (4) 恢复 $WG2_IF：bucket 回到 2，不需要任何控制面动作"
vpp "$N1" "set interface state $WG2_IF up" >/dev/null || die "up $WG2_IF 失败"
wait_fn "$REMOTE_PFX 又是 2 个 bucket" 15 have_buckets 2 "$N1" "$REMOTE_PFX"
wait_ping "$N1" "${TAP_N2_HOST%/*}" "恢复后 ping 仍通" 20

# ---------------------------------------------------------------- 7 改色搬表

info "7. (5) 改色 $WG2_IF → 颜色 $COLOR2：主表只留颜色 0，改色的那条搬进颜色表"
CT0=$(color_table 0 "$VRF")
CT2=$(color_table "$COLOR2" "$VRF")
info "     表号按 §4.2 公式：颜色 0 → $CT0，颜色 $COLOR2 → $CT2"
# 只改 $WG2_IF 那一行：两条隧道的颜色都是 0，按内容匹配会把两条一起换掉。
dx "$N1" "sed -i '/$WG2_IF/s/\"color\": 0}/\"color\": $COLOR2}/' $WORK/fib.json" >/dev/null
info "     fib.json 的隧道段：$(tunnels_line)"
kill_one "$N1" fib
dx "$N1" "$WORK/bin/sr-fib -config $WORK/fib.json -once > $WORK/recolor.log 2>&1" ||
    bad "改色后的 sr-fib -once 退出非 0（见 recolor.log）"
info "     收敛计数：$(fib_tail recolor.log)"

info "     主表 $VRF 读回："
dump_of | sed 's/^/       /'
wait_fn "主表 $VRF 只剩 1 个 bucket" 15 have_buckets 1 "$N1" "$REMOTE_PFX"
path_pref "$OVERLAY_N2" 230 &&
    ok "主表里活下来的是 $WG_IF 那条，preference 仍是 230（颜色 0 不动）" ||
    bad "主表里那条 230 不在了"
if dump_table "$VRF" "$REMOTE_PFX" | grep -qF "via $OVERLAY2_N2 "; then
    bad "改色的 $OVERLAY2_N2 还在主表 $VRF：逃生开关关着时主表只收颜色 0"
else
    ok "改色的 $OVERLAY2_N2 已不在主表 $VRF（负向：改色是搬表，不是留在主表换 pref）"
fi

info "     颜色表 $CT2 读回："
dump_table "$CT2" "$REMOTE_PFX" | sed 's/^/       /'
wait_fn "颜色表 $CT2 里有 $REMOTE_PFX" 15 table_has "$CT2" "$REMOTE_PFX"
table_path "$CT2" "$OVERLAY2_N2" $((230 + COLOR2)) &&
    ok "该表里这条路径 preference=$((230 + COLOR2))（= 230 + 颜色，$WG2_IF 那条）" ||
    bad "颜色表 $CT2 里这条路径的 preference 不是 $((230 + COLOR2))"
table_block "$CT2" "$REMOTE_PFX" | grep -qE "mpls-label\[@[0-9]+\]:\[$LABEL:" &&
    table_block "$CT2" "$REMOTE_PFX" | grep -qF "mpls via $OVERLAY2_N2 $WG2_IF" &&
    ok "颜色表里同样压标签 $LABEL 走 $WG2_IF（表号跟着颜色走，标签跟着来源 VPN 走）" ||
    bad "颜色表 $CT2 里这条路径不是带标签 $LABEL 走 $WG2_IF"
if dump_table "$CT2" "$REMOTE_PFX" | grep -qF "via $OVERLAY_N2 "; then
    bad "颜色表 $CT2 里混进了 $WG_IF（颜色 0）：一张颜色表只收自己那个颜色"
else
    ok "颜色表 $CT2 里只有颜色 $COLOR2 那条（颜色 0 没串进来）"
fi
table_path "$CT0" "$OVERLAY_N2" 230 &&
    ok "颜色 0 是双写：主表 $VRF 与颜色表 $CT0 各有一份同一条路径" ||
    bad "颜色表 $CT0 里没有颜色 0 那条（§4.3 第 5 步要求颜色 0 也建自己的表）"
wait_ping "$N1" "${TAP_N2_HOST%/*}" "改色搬表之后仍然可转发（主表还剩一条颜色 0）" 20

info "8. (5) 反向：颜色改回 0 再重启，主表自己回到 2 个 bucket"
dx "$N1" "sed -i '/$WG2_IF/s/\"color\": $COLOR2}/\"color\": 0}/' $WORK/fib.json" >/dev/null
kill_one "$N1" fib
dx "$N1" "$WORK/bin/sr-fib -config $WORK/fib.json -once > $WORK/recolor2.log 2>&1" ||
    bad "改回颜色 0 的 sr-fib -once 退出非 0（见 recolor2.log）"
wait_fn "主表 $VRF 又回到 2 个 bucket" 15 have_buckets 2 "$N1" "$REMOTE_PFX"
path_pref "$OVERLAY2_N2" 230 &&
    ok "$WG2_IF 那条回到 230：新进程的原地 add（整条替换）把颜色写回了 VPP" ||
    bad "$WG2_IF 那条没回到 230（原地 add 没换掉 VPP 存的 preference）"
# 颜色表 13xxx 里的旧条目**不会**被这次重启删掉：新进程的上一份计划是空的，
# Apply 的删除轮只能删它自己装过的东西。这不是缺陷，是 -reset 的职责边界，
# 打个数留给读日志的人（真要清干净由收尾的 sr-fib -reset 扫颜色表）。
info "     （已知行为）颜色表 $CT2 里改色那轮的旧路径仍在：$(dump_table "$CT2" "$REMOTE_PFX" | grep -c '  via ') 条；新进程删不到没装过的表"
wait_ping "$N1" "${TAP_N2_HOST%/*}" "改回颜色 0 之后仍然可转发" 20

echo
echo "== 结果：$PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
    for f in fib node1 node2 rr recolor recolor2; do
        echo "---- $f.log" >&2
        dx "$N1" "tail -20 $WORK/$f.log" >&2 2>/dev/null || true
    done
    exit 1
fi
