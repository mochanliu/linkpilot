#!/bin/bash
# test-refresh.sh - 不重启任何进程，让路由跟着本机隧道表与 VPN 表重新算（O5 判据）
#
# 这条脚本回答的是 O5 的唯一问题：**"后补的配置"到底要不要重启进程**。以前要，
# 因为装不进表的路由哪儿都没留（§2.5 那条"必须重启"）。现在每个会话留一份
# adj-RIBs-in，本机表一变就就地重算，于是下面每一段都只是 curl + 轮询：
#
#   (A) **空隧道表能开会话**。这是 O5-a 顺手修掉的那个 bug 的实测回归：空表以前
#       序列化成 `"tunnels":null`，RR 判"tunnels message without a table"直接关
#       会话 ⇒ 一台没配隧道的设备一上线就掉线。现在必须以 `[]` 上线。
#   (B) node1 加上到 node2 的隧道 ⇒ RR 开始反射 ⇒ node1 **收到**了 node2 的前缀，
#       但本机没有 VPN import 它的 RT ⇒ 它不丢，而是留在 pending 里，原因写着
#       rt_mismatch。反例是旧行为：这一条会被直接丢弃，之后无论配什么都查不到。
#       为什么这里判的是 rt_mismatch 而不是 no_path：RR **只按双向邻接反射**，本机
#       没隧道时它一条都不发，所以 no_path 只有在"隧道刚被撤走、RR 的撤回还没到"
#       那一瞬存在 —— 拿它当判据就是拿竞态的一个侧面当判据。稳定形态由 route 包的
#       单测钉；本脚本只在阶段 E 断言那个瞬时最终会被撤回收干净（待决清单回空）。
#   (C) **一次 PUT /vrfs 加 import RT ⇒ 此前已经收到的那条前缀进表**，而且
#       `received` 与 `imported` 一动不动 —— 这两个数不变就是"没有再问 RR"的证据，
#       也是"本机重算不许改写事件计数器"这条设计规矩在真进程里的样子。
#   (D) sr-fib 下一轮轮询把它落到 VPP（表 50、压 **1051=来源 VPN** 的标签），
#       两端 tap 终端 ping 通 ⇒ 学到的东西真的在转发，不是只在 JSON 里。
#   (E) 撤掉隧道 ⇒ 前缀离表、VPP 撤路由、终端 ping 断；再加回来 ⇒ 又学到又装上。
#       来回两次，全程零重启。
#   (F) 负向：**删掉一个 VPN ⇒ 它上面的本地前缀必须显式撤回**（node2 那边看得见
#       它消失），而不是留在 RR 里靠一份没人配置过的 RT 继续被导入；同时 node1 的
#       local_skips 要报 vrf_unconfigured —— 配了但说不出口的前缀必须可见。
#   (G) 收尾把五个进程的 pid + 启动时刻和刚启动时比一遍。只比 pid 会被复用骗，
#       所以两个一起比；这是本脚本标题那句"没重启过"的唯一硬证据。
#
# 前置：**必须先跑过 test/scripts/test-e2e.sh --keep**（node1↔node2 的 wg0 单隧道
# 在、通、且 wg0 的 MPLS 弧真的开着）。本脚本只加控制面与两个 tap，收尾把自己
# 装的 FIB 状态、tap、进程全部撤掉，不动隧道。
#
# VPN/前缀分配（避开 lab 的 0/10/99/250/1250 与其它脚本的 40/41/42、60）：
#   node1 VPN 50  export 65000:50  import 65000:99  →  阶段 C 改成 import 65000:51
#   node1 VPN 53  export 65000:53  （只导出，专门给阶段 F 删）
#   node2 VPN 51  export 65000:51  import 65000:50, 65000:53
# 宣告：node1 在 50 里送 10.50.0.0/16、在 53 里送 10.53.0.0/16；node2 在 51 里送
#       10.51.0.0/16。
#
# 一件事必须先说清楚：**PUT 改的是内存里那张表，磁盘上的配置文件不会跟着变**。
# 所以这个 lab 跑完之后如果谁重启 sr-route，它会回到 rr.json/node1.json 里那份
# 隧道表（空的）。这不是本脚本的漏洞，而是"配置以谁为准"还没接线 —— 那属于
# 草案里控制器发布 → 设备侧 applier 那一层，不在 O5 的判据里。
#
# 用法:
#   test/scripts/test-refresh.sh [--keep]
set -u

N1=srwg-node1
N2=srwg-node2
MODULE=/workspace/controller
WORK=${WORK:-/tmp/sr-refresh-e2e}

WG_IF=${WG_IF:-wg0}
OVERLAY_N1=${OVERLAY_N1:-192.168.99.1}
OVERLAY_N2=${OVERLAY_N2:-192.168.99.2}
# node1 内核在管理 bridge（10.10.0.0/24）上的地址：node2 容器里的 sr-fib 从这里
# 拉 node2 那份 RIB。不能用 10.10.1.2 —— 出桥时内核把源地址改写成网桥网关。
N1_CORE=${N1_CORE:-10.10.0.2}

VRF_DST=${VRF_DST:-50}                        # node1 的 VPN（既导出也导入）
VRF_LEAK=${VRF_LEAK:-53}                       # 只导出，阶段 F 删掉它
VRF_SRC=${VRF_SRC:-51}                         # node2 的 VPN
LBL_DST=$((1000 + VRF_DST))                    # 1050
LBL_SRC=$((1000 + VRF_SRC))                    # 1051
LBL_LEAK=$((1000 + VRF_LEAK))                  # 1053
CT_DST=$((10000 + VRF_DST))                    # ColorTable(0, 50) = 10050

PFX_DST=${PFX_DST:-10.50.0.0/16}               # node1 在 VPN 50 里宣告
PFX_LEAK=${PFX_LEAK:-10.53.0.0/16}             # node1 在 VPN 53 里宣告（阶段 F）
PFX_SRC=${PFX_SRC:-10.51.0.0/16}               # node2 在 VPN 51 里宣告

RR_PORT=${RR_PORT:-18686}
RR_STATUS=${RR_STATUS:-18687}
N1_STATUS=${N1_STATUS:-18691}
N2_STATUS=${N2_STATUS:-18692}

# tap 终端的 /30 必须落在**宣告前缀内部**（前缀选 /16 而不是 /24 的原因见
# test-cross-vpn.sh 的同名注释：内核报 "Network is unreachable" 会伪装成数据面坏了）。
TAP_N1=${TAP_N1:-taprf1}
TAP_N2=${TAP_N2:-taprf2}
TAP_N1_GW=${TAP_N1_GW:-10.50.255.1/30}
TAP_N1_HOST=${TAP_N1_HOST:-10.50.255.2/30}
TAP_N2_GW=${TAP_N2_GW:-10.51.255.1/30}
TAP_N2_HOST=${TAP_N2_HOST:-10.51.255.2/30}

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
dxi() { docker exec -i "$1" bash -lc "$2"; }
vpp() { docker exec "$1" bash -lc "vppctl $2" 2>/dev/null | tr -d '\r'; }

# ---------------------------------------------------------------- 断言工具

has_iface() { vpp "$1" "show interface $2" | awk -v n="$2" '$1==n {f=1} END {exit !f}'; }

# 重建的 WG 口开不了 mpls-input（引用计数在删口时不清零，sw_if_index 又被复用）。
# 症状是"overlay 能 ping、FIB 也装得好，但带标签的流量在收端全丢"，所以先验它。
mpls_dropped() {
    vpp "$1" "show interface features $2" |
        awk '/^mpls-input:/{getline; exit !/mpls-not-enabled/}'
}

route_in() {   # <container> <table> <prefix>
    vpp "$1" "show ip fib table $2" | grep -q "^$3"
}
no_route_in() { ! route_in "$1" "$2" "$3"; }
fib_block() {   # <container> <table> <prefix> —— 该表里这条前缀的整块
    vpp "$1" "show ip fib table $2" | awk -v p="$3" '$1==p {f=1; print; next}
        f && /^[^[:space:]]/ {exit}
        f {print}'
}
labeled_via() {   # <container> <table> <prefix> <label> <next_hop> —— 压这个标签、走这个口
    local b
    b=$(fib_block "$1" "$2" "$3")
    printf '%s\n' "$b" | grep -qE "mpls-label\[@[0-9]+\]:\[$4:" &&
        printf '%s\n' "$b" | grep -qF "mpls via $5 $WG_IF"
}
label_in() {   # <container> <label> —— 本地标签有没有登记
    vpp "$1" "show mpls fib" | grep -q "^$2:"
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
    status_json "$N1_STATUS" | python3 -m json.tool 2>/dev/null | head -40 >&2
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
# 负向的连通性判据：真断了才算数，但只等一个短窗口，别把"还没断"当成故障。
wait_no_ping() {   # <container> <dst> <label> [timeout_s]
    local c=$1 dst=$2 label=$3 limit=${4:-15} i=0
    while [ "$i" -lt $((limit * 2)) ]; do
        if ! ping_ok "$c" "$dst"; then ok "$label"; return 0; fi
        sleep 0.5
        i=$((i + 1))
    done
    bad "$label (还通着)"
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

# ---------------------------------------------------------------- 状态口读法

status_json() { dx "$N1" "curl -sf --max-time 2 http://127.0.0.1:$1/status"; }
rr_json()     { dx "$N1" "curl -sf --max-time 2 http://127.0.0.1:$RR_STATUS/status"; }

# wait_json <port> <python expr over d> <label> [timeout_s]
wait_json() {
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

# field <port> <python expr over d> —— 取一个标量留着后面比（计数器要跨阶段比大小）。
# 取不到就印 -1：宁可让后面的比较明确失败（"received 没涨 -1 → …"），也不要让
# [ -gt ] 拿到空串、报一条 "unary operator expected" 把真正的失败盖住。
field() {
    local v
    v=$(status_json "$1" |
        python3 -c "import json,sys; d=json.load(sys.stdin); print($2)" 2>/dev/null)
    [ -n "$v" ] || v=-1
    printf '%s\n' "$v"
}

# put <port> <path> <body> —— PUT 一张表进去。这里只送合法 body（三类非法输入
# 由 cmd/sr-route/main_test.go 钉住），所以非 200 一律当脚本失败，并把响应体
# 原样打出来：这道 seam 的失败信息就是给读脚本的人看的。
put() {   # <port> <path> <body>
    local port=$1 path=$2 body=$3 code
    code=$(printf '%s' "$body" | dxi "$N1" \
        "curl -s -o $WORK/put-resp.json -w '%{http_code}' -X PUT --data-binary @- http://127.0.0.1:$port$path")
    [ "$code" = 200 ] && return 0
    echo "PUT $path 返回 $code：" >&2
    dx "$N1" "cat $WORK/put-resp.json" >&2 2>/dev/null
    return 1
}

# ---------------------------------------------------------------- 清理

kill_one() {   # <container> <name>
    dx "$1" "if [ -f $WORK/$2.pid ]; then kill \$(cat $WORK/$2.pid) 2>/dev/null || true; rm -f $WORK/$2.pid; fi" >/dev/null 2>&1
}
port_busy() {   # <port> —— node1 上有没有人在听这个口
    dx "$N1" "ss -ltn" 2>/dev/null | grep -q ":$1 "
}
cleanup() {
    if [ "$KEEP" = 1 ]; then
        info "--keep：进程、tap 与 FIB 状态都留着（人工拆：sr-fib -reset、删 tap、杀 $WORK/*.pid）"
        info "     注意内存里的隧道表/VPN 表已被 PUT 改过，与 $WORK/*.json 不一致：重启 sr-route 会回到文件那份"
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

# ---------------------------------------------------------------- 1 配置与启动

info "1. 在 $N1 准备 $WORK（PKI、node1 空隧道表、二进制）"
dx "$N1" "rm -rf $WORK && mkdir -p $WORK/bin $WORK/pki $WORK/identity" || die "建 $WORK 失败"

docker exec -i -e WORK="$WORK" -e RR_PORT="$RR_PORT" -e RR_STATUS="$RR_STATUS" \
    -e N1_STATUS="$N1_STATUS" -e N2_STATUS="$N2_STATUS" \
    -e VRF_DST="$VRF_DST" -e VRF_LEAK="$VRF_LEAK" -e VRF_SRC="$VRF_SRC" \
    -e PFX_DST="$PFX_DST" -e PFX_LEAK="$PFX_LEAK" -e PFX_SRC="$PFX_SRC" \
    -e OVERLAY_N1="$OVERLAY_N1" -e OVERLAY_N2="$OVERLAY_N2" -e WG_IF="$WG_IF" \
    "$N1" bash -s <<'EOS' || die "写配置失败"
set -e
cd "$WORK/pki"
gen_key() { openssl genpkey -algorithm ED25519 -out "$1" 2>/dev/null; }
gen_key ca.key
openssl req -x509 -new -key ca.key -days 30 -subj "/CN=sr-refresh-ca" -out ca.crt 2>/dev/null
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

# node1 **开机就没有隧道**：阶段 A 要的就是这个形状。两个 VPN 都 import 一个永不
# 命中的 RT，所以阶段 B 收到的一定是 pending，不是"装上了再拆"。
cat > "$WORK/node1.json" <<EOF
{
  "tenant": "tenant-a",
  "device_id": 1,
  "identity_dir": "$WORK/identity/node1",
  "serial": "node1",
  "hold_sec": 6,
  "rrs": [{"id": "rr-a", "addr": "127.0.0.1:$RR_PORT"}],
  "tunnels": [],
  "vrfs": [{"id": $VRF_DST, "export_rts": ["65000:50"], "import_rts": ["65000:99"]},
           {"id": $VRF_LEAK, "export_rts": ["65000:53"]}],
  "local_prefixes": [{"vrf_id": $VRF_DST, "prefix": "$PFX_DST"},
                     {"vrf_id": $VRF_LEAK, "prefix": "$PFX_LEAK"}],
  "status_addr": "127.0.0.1:$N1_STATUS"
}
EOF

# node2 有到 node1 的隧道，所以阶段 B 一加就成双向邻接；它 import 50 与 53，
# 于是 node1 的两份宣告都会在 node2 的表 51 里出现 —— 阶段 F 看的就是 53 那份消失。
cat > "$WORK/node2.json" <<EOF
{
  "tenant": "tenant-a",
  "device_id": 2,
  "identity_dir": "$WORK/identity/node2",
  "serial": "node2",
  "hold_sec": 6,
  "rrs": [{"id": "rr-a", "addr": "127.0.0.1:$RR_PORT"}],
  "tunnels": [{"peer_device_id": 1, "next_hop": "$OVERLAY_N1"}],
  "vrfs": [{"id": $VRF_SRC, "export_rts": ["65000:51"], "import_rts": ["65000:50", "65000:53"]}],
  "local_prefixes": [{"vrf_id": $VRF_SRC, "prefix": "$PFX_SRC"}],
  "status_addr": "0.0.0.0:$N2_STATUS"
}
EOF

# 阶段 C 与 F 用的两份 VPN 表：整表替换，所以每次都得把另一个 VPN 一起写全。
cat > "$WORK/vrfs-C.json" <<EOF
{"vrfs":[{"id":$VRF_DST,"export_rts":["65000:50"],"import_rts":["65000:51"]},
         {"id":$VRF_LEAK,"export_rts":["65000:53"]}]}
EOF
cat > "$WORK/vrfs-F.json" <<EOF
{"vrfs":[{"id":$VRF_DST,"export_rts":["65000:50"],"import_rts":["65000:51"]}]}
EOF
cat > "$WORK/vrfs-restore.json" <<EOF
{"vrfs":[{"id":$VRF_DST,"export_rts":["65000:50"],"import_rts":["65000:51"]},
         {"id":$VRF_LEAK,"export_rts":["65000:53"],"import_rts":[]}]}
EOF

cat > "$WORK/tunnels-one.json" <<EOF
{"tunnels":[{"peer_device_id":2,"next_hop":"$OVERLAY_N2"}]}
EOF
cat > "$WORK/tunnels-none.json" <<'EOF'
{"tunnels":[]}
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

# 进程身份条：pid + 内核记录的启动时刻（/proc/<pid>/stat 第 22 字段，jiffies）。
# 阶段 G 用它证明"全程没重启"。只看 pid 不够 —— pid 会被复用；只看启动时刻不够
# —— 解释起来要看时钟源，两个一起相等才是我要的那句话。
cat > "$WORK/stamp.sh" <<'EOF'
#!/bin/bash
cd "$(dirname "$0")"
p=$(cat "$1.pid" 2>/dev/null)
[ -n "$p" ] || { echo missing; exit 1; }
[ -f "/proc/$p/stat" ] || { echo dead; exit 1; }
awk '{print $1 "/" $22}' "/proc/$p/stat"
EOF
chmod +x "$WORK/stamp.sh"
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
cat > "$WORK/stamp.sh" <<'EOF'
#!/bin/bash
cd "$(dirname "$0")"
p=$(cat "$1.pid" 2>/dev/null)
[ -n "$p" ] || { echo missing; exit 1; }
[ -f "/proc/$p/stat" ] || { echo dead; exit 1; }
awk '{print $1 "/" $22}' "/proc/$p/stat"
EOF
chmod +x "$WORK/stamp.sh"
EOS2

dx "$N1" "cd $MODULE && go build -o $WORK/bin/sr-rr ./cmd/sr-rr &&
    go build -o $WORK/bin/sr-route ./cmd/sr-route &&
    go build -o $WORK/bin/sr-fib ./cmd/sr-fib" || die "node1 编译失败"
dx "$N2" "cd $MODULE && go build -o $WORK/bin/sr-fib ./cmd/sr-fib" || die "node2 编译失败"
ok "三个二进制已编译（sr-route 带 PUT /tunnels 与 PUT /vrfs）"

# 上一轮死在半路会留下表；只有它确实像本脚本的残留才清。
clean_residue() {   # <container> <fib 配置> <table> <label> <prefix>
    if vpp "$1" "show ip fib table $3" | grep -q "^ipv4-VRF:$3"; then
        if label_in "$1" "$4" || vpp "$1" "show ip fib table $3" | grep -q "^$5"; then
            info "清理 $1 表 $3 的上一轮残留"
            dx "$1" "$WORK/bin/sr-fib -config $WORK/$2 -reset" >/dev/null 2>&1 || true
        else
            die "$1 的表 $3 已被别的进程占用（既无标签 $4 也无前缀 $5）"
        fi
    fi
}
clean_residue "$N1" fib.json "$VRF_DST" "$LBL_DST" "$PFX_SRC"
clean_residue "$N2" fib2.json "$VRF_SRC" "$LBL_SRC" "$PFX_DST"

for p in "$RR_PORT" "$RR_STATUS" "$N1_STATUS" "$N2_STATUS"; do
    port_busy "$p" && die "node1 上端口 $p 已被占用：上一轮的 sr-* 还活着，先杀干净（pgrep -af 'sr-(rr|route|fib)'）"
done

info "2. 启动 rr / sr-route x2 / sr-fib（node1 那份隧道表是空的）"
dx "$N1" "bash $WORK/start.sh" || die "启动失败"
sleep 0.5
dx "$N2" "bash $WORK/start-fib.sh" || die "node2 sr-fib 启动失败"

stamp_rr=$(dx "$N1" "$WORK/stamp.sh rr")
stamp_n1=$(dx "$N1" "$WORK/stamp.sh node1")
stamp_n2=$(dx "$N1" "$WORK/stamp.sh node2")
stamp_f1=$(dx "$N1" "$WORK/stamp.sh fib")
stamp_f2=$(dx "$N2" "$WORK/stamp.sh fib")
for s in "$stamp_rr" "$stamp_n1" "$stamp_n2" "$stamp_f1" "$stamp_f2"; do
    case "$s" in
      */[0-9]*) ;;
      *) die "取不到进程身份条（$s）：启动脚本没写 pid？" ;;
    esac
done
ok "五个进程的 pid + 启动时刻已记下，阶段 G 回来比"

# ---------------------------------------------------------------- A 空隧道表

info "(A) node1 开机零隧道：会话必须活着（这条是 null 隧道表的实测回归）"
wait_json "$N1_STATUS" \
    "any(s['connected'] and s['synced'] for s in d['sessions']) and d['active_rr'] >= 0" \
    "node1 的 RR 会话 connected + synced：空隧道表没有让 RR 关会话" 30
if dx "$N1" "grep -q 'tunnels message without a table' $WORK/node1.log" 2>/dev/null; then
    bad "node1.log 里有 'tunnels message without a table'（空表又被序列化成 null 了）"
else
    ok "日志里没有那条错误：空隧道表是以 [] 上线的，不是 null"
fi
wait_json "$N1_STATUS" "d['active_rib_routes'] == 0 and d['pending_total'] == 0" \
    "本机既没装任何路由，也没有待决的（没人反射给一个零隧道的设备）" 20
rr_json | python3 -c "
import json,sys
d = json.load(sys.stdin)
sys.exit(0 if d['tunnels'].get('1') == 0 and d['tunnels'].get('2') == 1 else 1)" 2>/dev/null &&
    ok "RR 侧看到的隧道表条数：device 1 = 0，device 2 = 1（双向邻接不成立，所以此时谁都不反射）" ||
    bad "RR 侧的隧道表和预期不符：$(rr_json | python3 -c 'import json,sys; print(json.load(sys.stdin)["tunnels"])' 2>/dev/null)"

# ---------------------------------------------------------------- B 加隧道

info "(B) PUT /tunnels 加上到 node2 的隧道 —— 不重启，让 RR 开始反射"
recv_b_before=$(field "$N1_STATUS" "d['received']")
put "$N1_STATUS" "/tunnels" "$(dx "$N1" "cat $WORK/tunnels-one.json")" ||
    die "PUT /tunnels 失败（阶段 B）"
ok "PUT /tunnels = 200"
wait_json "$N1_STATUS" \
    "d['tunnels'] == [{'peer_device_id': 2, 'next_hop': '$OVERLAY_N2'}]" \
    "读回的隧道表就是 PUT 进去那份" 15
wait_json "$N1_STATUS" \
    "d['pending_total'] == 1 and d['pending'][0]['reason'] == 'rt_mismatch' and
     d['pending'][0]['prefix'] == '$PFX_SRC' and d['pending'][0]['device_id'] == 2" \
    "node2 的前缀**收到了但装不进表**，留在 pending 里且原因写着 rt_mismatch（旧代码这里直接丢弃）" 30
wait_json "$N1_STATUS" "d['active_rib_routes'] == 0" \
    "它没有偷偷进表（RT 不匹配就是没装上）" 10
wait_fn "VPP 表 $VRF_DST 里也没有 $PFX_SRC（pending 不该惊动数据面）" 10 \
    no_route_in "$N1" "$VRF_DST" "$PFX_SRC"
wait_json "$N2_STATUS" \
    "[(r['vrf_id'], r['prefix']) for r in d['rib'].get('$VRF_SRC', []) if r['prefix'] == '$PFX_DST'] ==
     [($VRF_DST, '$PFX_DST')]" \
    "邻接双向成立后 node2 学到了 $PFX_DST（node2 的 VPN $VRF_SRC 本来就 import 65000:50）" 30
recv_b=$(field "$N1_STATUS" "d['received']")
imported_b=$(field "$N1_STATUS" "d['imported']")
mismatch_b=$(field "$N1_STATUS" "d['dropped_rt_mismatch']")
[ "$recv_b" -gt "$recv_b_before" ] &&
    ok "received 从 $recv_b_before 涨到 $recv_b：这一段确实是从 RR **收**来的" ||
    bad "received 没涨（$recv_b_before → $recv_b）：RR 没把 node2 的路由送过来"

# ---------------------------------------------------------------- C 改 import

info "(C) PUT /vrfs 给 VPN $VRF_DST 加上 import 65000:51 —— 已收到的路由就地进表"
put "$N1_STATUS" "/vrfs" "$(dx "$N1" "cat $WORK/vrfs-C.json")" || die "PUT /vrfs 失败（阶段 C）"
ok "PUT /vrfs = 200"
wait_json "$N1_STATUS" \
    "[(r['vrf_id'], r['prefix']) for r in d['rib'].get('$VRF_DST', [])] == [($VRF_SRC, '$PFX_SRC')]" \
    "$PFX_SRC 进了表 $VRF_DST（来源记的是 VPN $VRF_SRC，装的是本机 VPN $VRF_DST）" 20
wait_json "$N1_STATUS" \
    "d['pending_total'] == 0 and d['active_rib_routes'] == 1" \
    "待决清单空了：它不是被重新学一遍，是从待决里搬进表里" 15
recv_c=$(field "$N1_STATUS" "d['received']")
imported_c=$(field "$N1_STATUS" "d['imported']")
mismatch_c=$(field "$N1_STATUS" "d['dropped_rt_mismatch']")
if [ "$recv_c" = "$recv_b" ] && [ "$imported_c" = "$imported_b" ] && [ "$mismatch_c" = "$mismatch_b" ]; then
    ok "received=$recv_c、imported=$imported_c、dropped_rt_mismatch=$mismatch_c 与阶段 B 完全相同："
    info "     ⇒ 这条路由是**本机改表**装进来的，一个字节都没再问 RR；计数器记的是每一帧做了什么，不被重算改写"
else
    bad "本机重算改写了事件计数器：B($recv_b/$imported_b/$mismatch_b) → C($recv_c/$imported_c/$mismatch_c)"
fi
wait_json "$N1_STATUS" \
    "d['rib']['$VRF_DST'][0]['targets'] == [$VRF_DST] and
     d['rib']['$VRF_DST'][0]['next_hops'] == ['$OVERLAY_N2']" \
    "下一跳是本机隧道表算出来的（wire 里没有下一跳），目标表是 RT 命中的那张" 15

# ---------------------------------------------------------------- D 落到 VPP

info "(D) sr-fib 下一次轮询把它装进 VPP，两个 tap 终端真的 ping"
wait_fn "表 $VRF_DST 里出现 $PFX_SRC" 15 route_in "$N1" "$VRF_DST" "$PFX_SRC"
wait_fn "它压 $LBL_SRC（= 1000 + 来源 VPN $VRF_SRC）经 $WG_IF 发往 $OVERLAY_N2" 10 \
    labeled_via "$N1" "$VRF_DST" "$PFX_SRC" "$LBL_SRC" "$OVERLAY_N2"
wait_fn "颜色表 $CT_DST 里也有它（色*10000+主表的双写）" 15 route_in "$N1" "$CT_DST" "$PFX_SRC"
label_in "$N1" "$LBL_DST" &&
    ok "node1 登记了自己的本地标签 $LBL_DST（node2 送进来的流量弹栈后查表 $VRF_DST）" ||
    bad "node1 没登记本地标签 $LBL_DST"
# 反向那份是 node2 的 sr-fib 装的：它的输入是 node2 的 sr-route 表，而那张表在
# 阶段 B 之后就没再动过 —— 所以这一段证明"对端改隧道"也会一路跟到对端的对端。
wait_fn "node2 的表 $VRF_SRC 里装上 $PFX_DST（它那侧的 sr-fib 也在不重启地跟）" 20 \
    route_in "$N2" "$VRF_SRC" "$PFX_DST"
label_in "$N2" "$LBL_SRC" &&
    ok "node2 登记了本地标签 $LBL_SRC（node1 压进来的包弹栈后查表 $VRF_SRC）" ||
    bad "node2 没登记 $LBL_SRC：node1 送来的带标签包在 node2 无处可查"

tap_create "$N1" "$TAP_N1" "$VRF_DST" "$TAP_N1_GW" "$TAP_N1_HOST"
tap_create "$N2" "$TAP_N2" "$VRF_SRC" "$TAP_N2_GW" "$TAP_N2_HOST"
dx "$N1" "ip route replace $PFX_SRC via ${TAP_N1_GW%/*} dev $TAP_N1" || die "node1 内核路由失败"
dx "$N2" "ip route replace $PFX_DST via ${TAP_N2_GW%/*} dev $TAP_N2" || die "node2 内核路由失败"
wait_ping "$N1" "${TAP_N2_HOST%/*}" "node1 终端 -> 表 $VRF_DST -> 标签 $LBL_SRC -> $WG_IF -> node2 表 $VRF_SRC" 20
wait_ping "$N2" "${TAP_N1_HOST%/*}" "反向也通（表 $VRF_SRC -> 标签 $LBL_DST -> node1 表 $VRF_DST）" 20

# ---------------------------------------------------------------- E 撤了再加

info "(E) PUT /tunnels 撤掉隧道：前缀离表、FIB 撤、终端断 —— 还是不重启"
# 撤隧道这段的证据必须分两头看，而且得先说清**为什么不看 node1 的 withdrawn**：
# 本机 PUT 一生效，这条路由就先从表里搬进待决（没路径了）；等 RR 的撤回到达时它
# 早已不在表里，而 withdrawn 只记"真的从表里退了"的那次 —— 所以 node1 这边注定是
# 平的。会涨的计数器在 node2：它那边这条前缀是装进表的，只有收到 RR 的撤回才 +1。
withdrawn_e_before=$(field "$N2_STATUS" "d['withdrawn']")
w_n1_before=$(field "$N1_STATUS" "d['withdrawn']")
put "$N1_STATUS" "/tunnels" "$(dx "$N1" "cat $WORK/tunnels-none.json")" ||
    die "PUT /tunnels（清空）失败"
wait_json "$N1_STATUS" "d['active_rib_routes'] == 0" "$PFX_SRC 离开表 $VRF_DST" 20
wait_json "$N2_STATUS" "d['withdrawn'] > $withdrawn_e_before" \
    "node2 的 withdrawn 从 $withdrawn_e_before 涨上去：对端是收到 RR 的**显式撤回**的，不是等老化" 25
wait_json "$N1_STATUS" "d['pending_total'] == 0" \
    "本机待决清单也清空了：RR 撤了就不再替它记着（adj-RIBs-in 不是永久缓存）" 25
w_n1=$(field "$N1_STATUS" "d['withdrawn']")
[ "$w_n1" = "$w_n1_before" ] &&
    ok "node1 的 withdrawn 没动（$w_n1）：只是被本机扣下的路由，不该算成一次撤回" ||
    bad "node1 的 withdrawn 被本机重算改写了：$w_n1_before → $w_n1"
wait_fn "VPP 表 $VRF_DST 里这条路由被 sr-fib 撤掉" 20 no_route_in "$N1" "$VRF_DST" "$PFX_SRC"
wait_no_ping "$N1" "${TAP_N2_HOST%/*}" "node1 终端打不通了（控制面一撤，数据面跟着撤）" 20
wait_json "$N2_STATUS" \
    "'$PFX_DST' not in [r['prefix'] for r in d['rib'].get('$VRF_SRC', [])]" \
    "node2 那边也失去 $PFX_DST：RR 是按隧道表重算的，不是按会话断开" 30

put "$N1_STATUS" "/tunnels" "$(dx "$N1" "cat $WORK/tunnels-one.json")" ||
    die "PUT /tunnels（加回）失败"
wait_json "$N1_STATUS" \
    "[(r['vrf_id'], r['prefix']) for r in d['rib'].get('$VRF_DST', [])] == [($VRF_SRC, '$PFX_SRC')]" \
    "隧道加回来，$PFX_SRC 又进表" 30
wait_fn "VPP 表 $VRF_DST 又装上，压的仍是 $LBL_SRC" 20 \
    labeled_via "$N1" "$VRF_DST" "$PFX_SRC" "$LBL_SRC" "$OVERLAY_N2"
wait_ping "$N1" "${TAP_N2_HOST%/*}" "终端又通了（一撤一加，零重启）" 20

# ---------------------------------------------------------------- F 删 VPN

info "(F) 负向：删掉 VPN $VRF_LEAK ⇒ 它上面的本地前缀必须**显式撤回**"
wait_json "$N2_STATUS" \
    "($VRF_LEAK, '$PFX_LEAK') in [(r['vrf_id'], r['prefix']) for r in d['rib'].get('$VRF_SRC', [])]" \
    "删之前 node2 的表 $VRF_SRC 里有 $PFX_LEAK（来源 VPN $VRF_LEAK）" 20
w_f_before=$(field "$N2_STATUS" "d['withdrawn']")
put "$N1_STATUS" "/vrfs" "$(dx "$N1" "cat $WORK/vrfs-F.json")" || die "PUT /vrfs 失败（阶段 F）"
wait_json "$N1_STATUS" \
    "d['local_skips_total'] == 1 and d['local_skips'][0]['reason'] == 'vrf_unconfigured' and
     d['local_skips'][0]['vrf_id'] == $VRF_LEAK and d['local_skips'][0]['prefix'] == '$PFX_LEAK'" \
    "本机把它报成 local_skips/vrf_unconfigured：配了但当前 VPN 表说不出口，必须看得见" 15
wait_json "$N1_STATUS" \
    "($VRF_LEAK, '$PFX_LEAK') not in [(p['vrf_id'], p['prefix']) for p in d['local']]" \
    "它已从本机宣告集合里消失（不是等老化，是本机主动撤的）" 10
wait_json "$N2_STATUS" "d['withdrawn'] > $w_f_before" \
    "node2 的 withdrawn 从 $w_f_before 涨上去：对端收到的是一条 MsgWithdraw" 25
wait_json "$N2_STATUS" \
    "'$PFX_LEAK' not in [r['prefix'] for r in d['rib'].get('$VRF_SRC', [])]" \
    "对端 node2 的表里 $PFX_LEAK 也没了：**留着它等于用一份没人配置的 RT 继续被导入**" 30
wait_fn "node2 的 VPP 表 $VRF_SRC 里这条前缀也被撤掉" 20 no_route_in "$N2" "$VRF_SRC" "$PFX_LEAK"
wait_json "$N1_STATUS" \
    "[r['prefix'] for r in d['rib'].get('$VRF_DST', [])] == ['$PFX_SRC']" \
    "同一份 VPN 表里 VPN $VRF_DST 的导入没受影响（整表替换不是全部推倒）" 15

put "$N1_STATUS" "/vrfs" "$(dx "$N1" "cat $WORK/vrfs-restore.json")" ||
    die "PUT /vrfs（恢复）失败"
wait_json "$N2_STATUS" \
    "($VRF_LEAK, '$PFX_LEAK') in [(r['vrf_id'], r['prefix']) for r in d['rib'].get('$VRF_SRC', [])]" \
    "VPN $VRF_LEAK 加回来，$PFX_LEAK 又被宣告（不重启就恢复）" 30
wait_json "$N1_STATUS" "d['local_skips_total'] == 0" "local_skips 回到空" 15

# ---------------------------------------------------------------- G 进程身份

info "(G) 全程没重启：把五个身份条和启动时比一遍"
same=0
for pair in "rr:$stamp_rr" "node1:$stamp_n1" "node2:$stamp_n2" "fib:$stamp_f1"; do
    name=${pair%%:*}; want=${pair#*:}
    got=$(dx "$N1" "$WORK/stamp.sh $name" 2>/dev/null)
    if [ "$got" = "$want" ]; then
        PASS=$((PASS + 1)); echo "  [PASS] $name 仍是同一个进程（$got）"
    else
        FAIL=$((FAIL + 1)); echo "  [FAIL] $name 换了：$want → $got"
    fi
done
got=$(dx "$N2" "$WORK/stamp.sh fib" 2>/dev/null)
if [ "$got" = "$stamp_f2" ]; then
    PASS=$((PASS + 1)); echo "  [PASS] node2 的 sr-fib 仍是同一个进程（$got）"
else
    FAIL=$((FAIL + 1)); echo "  [FAIL] node2 的 sr-fib 换了：$stamp_f2 → $got"
fi
info "     阶段 F 之后 node2 仍活着、node1 的导入仍生效：改表改的是内存里那两份表"

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
