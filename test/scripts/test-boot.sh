#!/bin/bash
# test-boot.sh - 开机自启的判据：一份清单把盒子从"上电"带到"路由面自己在"（O5-d）
#
# 这条脚本回答的是 O5-d 的唯一问题：**盒子从上电（或某个进程死掉）之后，要不要人手敲命令**。
# 判据就是草案 §9 那一行：
#
#   重启盒子后不手敲任何命令，overlay 与路由面自己在；配置变更与进程恢复**永不**重启别的进程。
#
# 三段，各答一半：
#
#   (1) 装盒子：清单、配置、身份、磁盘上那版客户配置都落到**真路径**
#       （/usr/local/lib/sr-box/start.sh、/etc/sr-box/sr-box.conf、/etc/sdwan/*.json、
#       /var/lib/sr-cpe）。然后跑 `check` —— 它必须把每一该跳过（gate 无）和每一该跑
#       （gate 有、进程没起）都说清楚，而且**什么都不启动**。agent 门控的三条分支
#       （身份齐 / 有 ctrl 但没身份 / conf 里没写 ctrl）在这里一次验完。
#   (B1) 热 VPP：`up` 一遍，路由面自己起来、终端 ping 得通；然后**只杀 sr-route**，
#       再 `up` 一遍 —— sr-route 换新 pid，sr-fib 与 sr-agent 与 VPP 一个都不许动；
#       第三遍 `up` 连 sr-route 都不许换。这就是"这份清单不是重启进程的手段"的证据。
#   (B2) 真冷启：`docker restart` 两个容器（VPP 的表/接口/标签、内核路由、tap 全没了，
#       磁盘上的东西全在）。**每台盒子只敲一条命令** `start.sh up`，于是：
#         * VPP 由清单自己拉起（伪终端那条路径 —— 容器里没有 systemd，没别人会做这件事）；
#         * 磁盘上那版配置重放回来（VPP 里 MPLS-VRF:1956 + 内核 table 56 那条 blackhole
#           都是那份 bundle 写的：重启前我盯着它们消失，重启后盯着它们回来）；
#         * RR 还没起来 ⇒ sr-route 照样在跑、状态口照样回话、会话明写着没连上；RR 起来后
#           它自己重连（reconnects≥1）把路由学回来 —— 全程没人碰它；
#         * 隧道 0 条：清单**明说**这一层它恢复不了（wg 口的期望值在控制器上，磁盘上没有
#           那份表），而不是用一句"全部完成"盖掉；
#         * 手工把 wg0 恢复（扮演 sr-agent -tunnels 重新够到控制器）之后，**已经在跑的**
#           sr-route/sr-fib 自己把 FIB 装上、终端又通 —— 还是一个进程都不用重启。
#
# 前置：**必须先跑过 test/scripts/test-e2e.sh --keep**（node1↔node2 的 wg0 在、overlay 通）。
# 本脚本会重启这两个容器的 VPP，收尾时把隧道手工恢复回 e2e 那个形状。
#
# VPN/前缀分配（避开 lab 的 0/10/99/250/1250 与其它脚本的 40/41/42、50/51/53、60）：
#   node1 VPN 56  export 65000:56 import 65000:57  宣告 10.56.0.0/16 标签 1056 色表 10056
#   node2 VPN 57  export 65000:57 import 65000:56  宣告 10.57.0.0/16 标签 1057
#   bundle 的记号：VPP 里 MPLS-VRF:1956/1957（除本脚本没人建它们），内核 table 56/57 里
#   一条 10.5x.255.255/32 blackhole。
# 端口：RR 18700/18701，node1 sr-route 18706，node2 sr-route 18707，node1 agent 18708，
#       node1 的假控制器 18799（故意没人监听：开机时控制器不可达正是这段要的形状）。
#
# 用法:
#   test/scripts/test-boot.sh [--keep]
set -u

N1=srwg-node1
N2=srwg-node2
MODULE=/workspace/controller
WORK=${WORK:-/tmp/sr-boot-e2e}                      # 只在 node1：RR + PKI + bundle
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
START_SRC="${ROOT}/controller/deploy/sr-box/start.sh"
SETUP_C=/workspace/test/scripts/setup-node.sh

# 盒子上的真路径（就是 start.sh 里那些默认值）
BOX_LIB=/usr/local/lib/sr-box
BOX_BIN=/usr/local/bin
BOX_CONF_DIR=/etc/sr-box
BOX_CONF=$BOX_CONF_DIR/sr-box.conf
BOX_ID=/etc/sdwan/identity
BOX_ROUTE=/etc/sdwan/route.json
BOX_FIB=/etc/sdwan/fib.json
BOX_STORE=/var/lib/sr-cpe
BOX_RUN=/run/sr-box
BOX_LOG=/var/log/sr-box

WG_IF=${WG_IF:-wg0}
OVERLAY_N1=${OVERLAY_N1:-192.168.99.1}
OVERLAY_N2=${OVERLAY_N2:-192.168.99.2}
RR_ADDR=${RR_ADDR:-10.10.0.2}       # node1 在管理桥上的地址：两台盒子都从这儿连 RR

VRF_1=${VRF_1:-56}
VRF_2=${VRF_2:-57}
LBL_1=$((1000 + VRF_1))             # 1056
LBL_2=$((1000 + VRF_2))             # 1057
CT_1=$((10000 + VRF_1))             # ColorTable(0, 56) = 10056
PFX_1=${PFX_1:-10.56.0.0/16}
PFX_2=${PFX_2:-10.57.0.0/16}
MK_1=$((1900 + VRF_1))              # bundle 的 MPLS 记号表：1956
MK_2=$((1900 + VRF_2))              # 1957
HB_1=10.56.255.255              # bundle 在内核 table 56 里落的记号路由（blackhole /32）
HB_2=10.57.255.255              # 实测：ip route show 打印 "blackhole 10.56.255.255"，
                                # /32 是被省掉的，所以这两个变量不带掩码，grep 才命中

RR_PORT=${RR_PORT:-18700}
RR_STATUS=${RR_STATUS:-18701}
S1=${S1:-18706}
S2=${S2:-18707}
AS=${AS:-18708}
CTRL_DEAD=${CTRL_DEAD:-127.0.0.1:18799}

TAP_N1=${TAP_N1:-tapbt1}
TAP_N2=${TAP_N2:-tapbt2}
TAP_N1_GW=${TAP_N1_GW:-10.56.255.1/30}
TAP_N1_HOST=${TAP_N1_HOST:-10.56.255.2/30}
TAP_N2_GW=${TAP_N2_GW:-10.57.255.1/30}
TAP_N2_HOST=${TAP_N2_HOST:-10.57.255.2/30}

PASS=0
FAIL=0
ok()   { PASS=$((PASS+1)); echo "  [PASS] $*"; }
bad()  { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "== $*"; }
die()  { echo "FATAL: $*" >&2; exit 2; }

KEEP=0
for a in "$@"; do
    case "$a" in
      --keep) KEEP=1 ;;
      -h|--help) awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 0 ;;
      *) echo "unknown option: $a" >&2; exit 2 ;;
    esac
done
[ -f "$START_SRC" ] || die "找不到被验的清单：$START_SRC"

dx()  { docker exec "$1" bash -lc "$2"; }
vpp() { docker exec "$1" bash -lc "vppctl $2" 2>/dev/null | tr -d '\r'; }

# ---------------------------------------------------------------- 断言工具

has_iface() { vpp "$1" "show interface $2" | awk -v n="$2" '$1==n {f=1} END {exit !f}'; }
mpls_dropped() {
    vpp "$1" "show interface features $2" |
        awk '/^mpls-input:/{getline; exit !/mpls-not-enabled/}'
}
route_in()    { vpp "$1" "show ip fib table $2" | grep -q "^$3"; }
no_route_in() { ! route_in "$1" "$2" "$3"; }
mpls_table()  { vpp "$1" "show mpls fib" | grep -q "^MPLS-VRF:$2,"; }
label_in()    { vpp "$1" "show mpls fib" | grep -q "^$2:"; }
kroute()      { dx "$1" "ip route show table $2" | grep -q "$3"; }   # 内核路由表，不是 VPP 的
fib_block() {
    vpp "$1" "show ip fib table $2" | awk -v p="$3" '$1==p {f=1; print; next}
        f && /^[^[:space:]]/ {exit}
        f {print}'
}
labeled_via() {   # <container> <table> <prefix> <label> <next_hop>
    local b
    b=$(fib_block "$1" "$2" "$3")
    printf '%s\n' "$b" | grep -qE "mpls-label\[@[0-9]+\]:\[$4:" &&
        printf '%s\n' "$b" | grep -qF "mpls via $5 $WG_IF"
}

ping_ok() { dx "$1" "ping -c2 -W1 -i0.2 $2" >/dev/null 2>&1; }
wait_ping() {   # <container> <dst> <label> [timeout_s]
    local c=$1 dst=$2 label=$3 limit=${4:-20} i=0
    while [ "$i" -lt $((limit * 2)) ]; do
        if ping_ok "$c" "$dst"; then ok "$label"; return 0; fi
        sleep 0.5
        i=$((i + 1))
    done
    bad "$label (不通)"
    dx "$c" "ping -c2 -W1 -i0.2 $dst" 2>&1 | tail -3 >&2
    return 1
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
# 终端那一侧。B2 之后 VPP 和内核都是新的，所以这段要能重入。
term_setup() {
    tap_create "$N1" "$TAP_N1" "$VRF_1" "$TAP_N1_GW" "$TAP_N1_HOST"
    tap_create "$N2" "$TAP_N2" "$VRF_2" "$TAP_N2_GW" "$TAP_N2_HOST"
    dx "$N1" "ip route replace $PFX_2 via ${TAP_N1_GW%/*} dev $TAP_N1" || die "node1 内核路由失败"
    dx "$N2" "ip route replace $PFX_1 via ${TAP_N2_GW%/*} dev $TAP_N2" || die "node2 内核路由失败"
}
term_ping() {   # <这段在什么时候>
    wait_ping "$N1" "${TAP_N2_HOST%/*}" "node1 终端 -> 表 $VRF_1 -> 标签 $LBL_2 -> $WG_IF -> node2 表 $VRF_2（$1）" 25
    wait_ping "$N2" "${TAP_N1_HOST%/*}" "反向也通（$1）" 25
}

# ---------------------------------------------------------------- 状态口读法

status_json() { dx "$1" "curl -sf --max-time 2 http://127.0.0.1:$2/status"; }
wait_json() {   # <container> <port> <python expr over d> <label> [timeout_s]
    local c=$1 port=$2 expr=$3 label=$4 limit=${5:-25} i=0
    while [ "$i" -lt $((limit * 5)) ]; do
        if status_json "$c" "$port" |
            python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if ($expr) else 1)" 2>/dev/null; then
            ok "$label"
            return 0
        fi
        sleep 0.2
        i=$((i + 1))
    done
    bad "$label (超时)"
    status_json "$c" "$port" | python3 -m json.tool 2>/dev/null | head -40 >&2
    return 1
}
field() {   # <container> <port> <python expr over d> —— 取不到就印 -1，让后面的比较明确失败
    local v
    v=$(status_json "$1" "$2" |
        python3 -c "import json,sys; d=json.load(sys.stdin); print($3)" 2>/dev/null)
    [ -n "$v" ] || v=-1
    printf '%s\n' "$v"
}

# ---------------------------------------------------------------- 清单的三个入口

# 清单每行是 "步名  门控  现状"（%-11s %-4s %s），所以步名和门控之间是补齐的空格，
# 按字面匹配必须用正则，不能用 grep -F。
has_re()   { printf '%s\n' "$1" | grep -qE -- "$2"; }
has_line() { printf '%s\n' "$1" | grep -qF -- "$2"; }
run_box() {   # <container> <up|check|stop> -> 设 BOX_RC / BOX_OUT，并把清单原文打出来
    BOX_OUT=$(docker exec "$1" bash -lc "$BOX_LIB/start.sh $2" 2>&1)
    BOX_RC=$?
    printf '%s\n' "$BOX_OUT" | sed 's/^/    | /'
    return $BOX_RC
}
box_pid() {   # <container> <step> —— pid + 内核记录的启动时刻（/proc/<pid>/stat 第 22 字段）
    dx "$1" "p=\$(cat $BOX_RUN/$2.pid 2>/dev/null); [ -n \"\$p\" ] && awk '{print \$1\"/\"\$22}' /proc/\$p/stat || echo none"
}
wait_vppctl() {   # <container> <limit_s>
    local c=$1 limit=$2 i=0
    while [ "$i" -lt $((limit * 2)) ]; do
        vpp "$c" "show version" | grep -q 'vpp v' && return 0
        sleep 0.5
        i=$((i + 1))
    done
    return 1
}

# ---------------------------------------------------------------- 清理

kill_harness() {   # <container> <name> —— 只杀 harness 自己起的（RR）；盒子的进程归 start.sh stop
    dx "$1" "[ -f $WORK/$2.pid ] && kill \$(cat $WORK/$2.pid) 2>/dev/null; rm -f $WORK/$2.pid" >/dev/null 2>&1
}
port_busy() { dx "$1" "ss -ltn" 2>/dev/null | grep -q ":$2 "; }

cleanup() {
    info "收尾"
    if [ "$KEEP" = 1 ]; then
        info "--keep：两台盒子上的东西全留着 —— $BOX_RUN/*.pid 是进程名册，"
        info "     start.sh check 随时能看现状；harness 侧的 RR 与 $WORK 也留着"
        info "     手工拆：docker exec $N1 $BOX_LIB/start.sh stop；"
        info "     撤 FIB：docker exec <盒子> $BOX_BIN/sr-fib -config $BOX_FIB -reset"
        return
    fi
    tap_del "$N1" "$TAP_N1"
    tap_del "$N2" "$TAP_N2"
    dx "$N1" "ip route del blackhole $HB_1/32 table $VRF_1" >/dev/null 2>&1 || true
    dx "$N2" "ip route del blackhole $HB_2/32 table $VRF_2" >/dev/null 2>&1 || true
    dx "$N1" "$BOX_LIB/start.sh stop" >/dev/null 2>&1 || true
    dx "$N2" "$BOX_LIB/start.sh stop" >/dev/null 2>&1 || true
    kill_harness "$N1" rr
    # sr-fib 撤干净自己装过的表/标签/颜色表，然后盒子上就没有本脚本的痕迹了
    dx "$N1" "$BOX_BIN/sr-fib -config $BOX_FIB -reset" >/dev/null 2>&1 || true
    dx "$N2" "$BOX_BIN/sr-fib -config $BOX_FIB -reset" >/dev/null 2>&1 || true
    rm_box "$N1"
    rm_box "$N2"
    info "     留下的两处：MPLS-VRF:$MK_1/$MK_2 是空标签表，VPP 没有删它的 CLI（重启即消）；"
    info "     RR 从没装进 $BOX_BIN —— 它是基础设施不是盒子的一部分，一直在 $WORK/bin 里"
}
rm_box() {   # <container>
    dx "$1" "rm -rf $WORK $BOX_CONF_DIR $BOX_ID $BOX_ROUTE $BOX_FIB $BOX_STORE $BOX_RUN $BOX_LOG $BOX_LIB /etc/frr;
        rmdir /etc/sdwan 2>/dev/null;
        rm -f $BOX_BIN/sr-route $BOX_BIN/sr-fib $BOX_BIN/sr-apply" >/dev/null 2>&1 || true
}
# 上一轮死在半路会留下这些；只有确实像本脚本的残留才清，否则是别人在用这张表。
clean_residue() {   # <container> <table> <label> <prefix>
    if vpp "$1" "show ip fib table $3" | grep -q "^ipv4-VRF:$3"; then
        if label_in "$1" "$4" || vpp "$1" "show ip fib table $3" | grep -q "^$5"; then
            info "清理 $1 表 $3 的上一轮残留"
            dx "$1" "$BOX_BIN/sr-fib -config $BOX_FIB -reset" >/dev/null 2>&1 || true
        else
            die "$1 的表 $3 已被别的进程占用（既无标签 $4 也无前缀 $5）"
        fi
    fi
}

trap cleanup EXIT
trap 'exit 130' INT TERM

# ---------------------------------------------------------------- 0 前置

info "0. 前置：wg0 与 overlay（需要先跑 test/scripts/test-e2e.sh --keep）"
for c in "$N1" "$N2"; do
    docker ps --format '{{.Names}}' | grep -qx "$c" || die "$c 没在跑"
    wait_vppctl "$c" 20 || die "$c 的 VPP 没在跑：先跑 test-e2e.sh --keep"
    has_iface "$c" "$WG_IF" || die "$c 没有 $WG_IF：先跑 test/scripts/test-e2e.sh --keep"
    mpls_dropped "$c" "$WG_IF" && die "$c 的 $WG_IF 还挂在 mpls-not-enabled 上（它是被删掉又重建的口）：重启 VPP、e2e 只跑一次再来"
    dx "$c" "ls $BOX_RUN $BOX_STORE $BOX_CONF $BOX_LIB 2>/dev/null" | grep -q . &&
        die "$c 上已经有盒子状态（$BOX_CONF / $BOX_STORE / $BOX_RUN / $BOX_LIB）：上一轮没收尾，先手工 rm 掉这几个再跑"
done
vpp "$N1" "ping $OVERLAY_N2 repeat 3 interval 0.2" | grep -q '0% packet loss' || die "node1 -> node2 overlay 不通"
vpp "$N2" "ping $OVERLAY_N1 repeat 3 interval 0.2" | grep -q '0% packet loss' || die "node2 -> node1 overlay 不通"
for p in "$RR_PORT" "$RR_STATUS" "$S1" "$AS"; do port_busy "$N1" "$p" && die "node1 端口 $p 被占用"; done
port_busy "$N2" "$S2" && die "node2 端口 $S2 被占用"
clean_residue "$N1" "$VRF_1" "$LBL_1" "$PFX_2"
clean_residue "$N2" "$VRF_2" "$LBL_2" "$PFX_1"
ok "两台盒子都在跑、wg0 的 MPLS 弧真开着、overlay 双向可达、端口与表都空着"

# ---------------------------------------------------------------- 1 装盒子

info "1. 把盒子装到**真路径**（清单、两张表、身份、磁盘上那版客户配置）"
for c in "$N1" "$N2"; do
    dx "$c" "cd $MODULE && go build -o $BOX_BIN/sr-route ./cmd/sr-route &&
        go build -o $BOX_BIN/sr-fib ./cmd/sr-fib &&
        go build -o $BOX_BIN/sr-apply ./cmd/sr-apply &&
        go build -o $BOX_BIN/sr-agent ./cmd/sr-agent &&
        mkdir -p $BOX_LIB $BOX_CONF_DIR $BOX_ID $BOX_LOG" || die "$c 编译/建目录失败"
    docker cp "$START_SRC" "$c:$BOX_LIB/start.sh" >/dev/null || die "拷 start.sh 到 $c 失败"
    dx "$c" "chmod +x $BOX_LIB/start.sh"
done
ok "清单在 $BOX_LIB/start.sh，sr-route/sr-fib/sr-apply 在 $BOX_BIN（两台都是）"

# PKI 在 node1 生成：同一份 CA 既签 RR 的服务证书，也签两台盒子的客户端证书。
# 盒子身份目录里的文件名与 sr-agent 落盘的那套逐字相同 —— sr-route 复用的就是
# agent.LoadOrCreate，所以这一份身份同时喂给 sr-route 和 sr-agent。
dx "$N1" "rm -rf $WORK && mkdir -p $WORK/bin $WORK/pki" || die "建 $WORK 失败"
docker exec -i -e WORK="$WORK" -e RR_PORT="$RR_PORT" -e RR_STATUS="$RR_STATUS" \
    -e RR_ADDR="$RR_ADDR" -e S1="$S1" -e AS="$AS" -e CTRL_DEAD="$CTRL_DEAD" \
    -e VRF_1="$VRF_1" -e VRF_2="$VRF_2" -e PFX_1="$PFX_1" -e PFX_2="$PFX_2" \
    -e MK_1="$MK_1" -e HB_1="$HB_1" -e OVERLAY_N2="$OVERLAY_N2" \
    -e BOX_ID="$BOX_ID" -e BOX_ROUTE="$BOX_ROUTE" -e BOX_FIB="$BOX_FIB" -e BOX_CONF="$BOX_CONF" \
    -e BOX_STORE="$BOX_STORE" -e BOX_BIN="$BOX_BIN" -e MODULE="$MODULE" \
    "$N1" bash -s <<'EOS' || die "写 node1 配置失败"
set -e
cd "$WORK/pki"
gen_key() { openssl genpkey -algorithm ED25519 -out "$1" 2>/dev/null; }
gen_key ca.key
openssl req -x509 -new -key ca.key -days 30 -subj "/CN=sr-boot-ca" -out ca.crt 2>/dev/null
gen_key rr.key
openssl req -new -key rr.key -subj "/CN=rr" -out rr.csr 2>/dev/null
printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=IP:%s,IP:127.0.0.1,DNS:localhost\n' "$RR_ADDR" > server.ext
openssl x509 -req -in rr.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 \
    -extfile server.ext -out rr.crt 2>/dev/null

for n in 1 2; do
    d="$WORK/id$n"; mkdir -p "$d"
    gen_key "$d/device_ed25519.key"
    openssl req -new -key "$d/device_ed25519.key" -subj "/CN=node$n" -out "node$n.csr" 2>/dev/null
    printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature\nextendedKeyUsage=clientAuth\nsubjectAltName=URI:srou://tenant-a/node/%s\n' "$n" > client.ext
    openssl x509 -req -in "node$n.csr" -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 \
        -extfile client.ext -out "$d/node_cert.pem" 2>/dev/null
    cp ca.crt "$d/ca_cert.pem"
    printf '{"serial":"node%s","key_gen":0,"tenant_id":"tenant-a","node_id":%s}\n' "$n" "$n" > "$d/state.json"
    # WG 私钥（32 字节）：真盒子上是 agent 生成的，这里补一把，免得 agent 起来才发现缺。
    head -c 32 /dev/urandom > "$d/wg_x25519.key"
    chmod 600 "$d"/*.key
done
rm -rf "$BOX_ID" && mkdir -p "$BOX_ID"
cp -a "$WORK/id1/." "$BOX_ID/"

cat > "$BOX_ROUTE" <<EOF
{
  "tenant": "tenant-a",
  "device_id": 1,
  "identity_dir": "$BOX_ID",
  "serial": "node1",
  "hold_sec": 6,
  "rrs": [{"id": "rr-a", "addr": "$RR_ADDR:$RR_PORT"}],
  "tunnels": [{"peer_device_id": 2, "next_hop": "$OVERLAY_N2"}],
  "vrfs": [{"id": $VRF_1, "export_rts": ["65000:$VRF_1"], "import_rts": ["65000:$VRF_2"]}],
  "local_prefixes": [{"vrf_id": $VRF_1, "prefix": "$PFX_1"}],
  "status_addr": "127.0.0.1:$S1"
}
EOF
cat > "$BOX_FIB" <<EOF
{
  "status_url": "http://127.0.0.1:$S1/status",
  "vpp_socket": "/run/vpp/api.sock",
  "poll_sec": 1,
  "vrfs": [$VRF_1],
  "tunnels": [{"next_hop": "$OVERLAY_N2", "interface": "wg0"}]
}
EOF
# 这份 conf 就是"这台盒子该怎么开机"的全部答案：lab 与真盒子的差别只写在这里。
cat > "$BOX_CONF" <<EOF
# sr-box 开机清单配置（test-boot.sh 生成）。没写的键用 start.sh 里的默认值。
SR_BOX_BIN=$BOX_BIN
# 容器里没有 systemd，VPP 归这份清单启动；真盒子上写 SR_BOX_VPP_START=no，让 vpp.service 管。
SR_BOX_VPP_START=auto
SR_BOX_VPP_LAUNCHER=/workspace/test/docker/start-node.sh
# 控制器故意指向一个没人听的口：开机时控制器不可达，正是 B2 那段要的形状。
SR_BOX_AGENT_CTRL=$CTRL_DEAD
SR_BOX_AGENT_ID=$BOX_ID
SR_BOX_AGENT_STATUS=http://127.0.0.1:$AS/status
SR_BOX_STATUS_WAIT=40
EOF

# 装在磁盘上那版客户配置：vpp 段建两张 MPLS 标签表（0 是真的要，$MK_1 是本脚本的记号），
# boot 段在内核 table $VRF_1 里落一条 blackhole。两段都幂等，所以这份清单可以反复跑。
# 这一遍要 --frr-reload true：这台盒子上没有 FRR，apply 结尾那条"重启 FRR"在容器里必然
# 失败（systemctl 不存在）。这是 #88 那层的真实缺口，本脚本只记一笔，不顺手改代码。
cat > "$WORK/bundle.json" <<EOF
{
  "node_id": 1,
  "version": 1,
  "artifacts": {
    "frr": "",
    "vpp": "mpls table add 0\nmpls table add $MK_1\n",
    "boot": "#!/bin/sh\nset -e\nip route replace blackhole $HB_1/32 table $VRF_1\n"
  }
}
EOF
$BOX_BIN/sr-apply apply --bundle "$WORK/bundle.json" --root "$BOX_STORE" --frr-reload true

cd "$MODULE" && go build -o "$WORK/bin/sr-rr" ./cmd/sr-rr
cat > "$WORK/rr.json" <<EOF
{
  "rr_id": "rr-a",
  "tenant": "tenant-a",
  "hold_sec": 6,
  "listen": "0.0.0.0:$RR_PORT",
  "status_addr": "0.0.0.0:$RR_STATUS",
  "tls": {"cert_file": "$WORK/pki/rr.crt", "key_file": "$WORK/pki/rr.key", "ca_file": "$WORK/pki/ca.crt"}
}
EOF
cat > "$WORK/start-rr.sh" <<'EOF'
#!/bin/bash
cd "$(dirname "$0")"
nohup ./bin/sr-rr -config rr.json > rr.log 2>&1 & echo $! > rr.pid
EOF
chmod +x "$WORK/start-rr.sh"
EOS

# node2 那一半：身份从宿主机中转拷过去（只拷那份目录，CA 私钥留在 node1 的 $WORK 里）。
TMPID=$(mktemp -d) || die "建宿主机临时目录失败"
docker cp "$N1:$WORK/id2" "$TMPID/id2" >/dev/null || die "拷 node2 身份失败"
# 目的地是个已存在的目录（第 1 段 mkdir -p 过），docker cp 会把源目录**套进**它里面
# 变成 /etc/sdwan/identity/id2 —— 必须用 /. 这份"目录内容"写法。
docker cp "$TMPID/id2/." "$N2:$BOX_ID" >/dev/null || die "装 node2 身份失败"
rm -rf "$TMPID"

docker exec -i -e WORK="$WORK" -e RR_ADDR="$RR_ADDR" -e RR_PORT="$RR_PORT" -e S2="$S2" \
    -e VRF_1="$VRF_1" -e VRF_2="$VRF_2" -e PFX_1="$PFX_1" -e PFX_2="$PFX_2" \
    -e MK_2="$MK_2" -e HB_2="$HB_2" -e OVERLAY_N1="$OVERLAY_N1" \
    -e BOX_ID="$BOX_ID" -e BOX_ROUTE="$BOX_ROUTE" -e BOX_FIB="$BOX_FIB" -e BOX_CONF="$BOX_CONF" \
    -e BOX_STORE="$BOX_STORE" -e BOX_BIN="$BOX_BIN" \
    "$N2" bash -s <<'EOS2' || die "写 node2 配置失败"
set -e
mkdir -p "$WORK"
cat > "$BOX_ROUTE" <<EOF
{
  "tenant": "tenant-a",
  "device_id": 2,
  "identity_dir": "$BOX_ID",
  "serial": "node2",
  "hold_sec": 6,
  "rrs": [{"id": "rr-a", "addr": "$RR_ADDR:$RR_PORT"}],
  "tunnels": [{"peer_device_id": 1, "next_hop": "$OVERLAY_N1"}],
  "vrfs": [{"id": $VRF_2, "export_rts": ["65000:$VRF_2"], "import_rts": ["65000:$VRF_1"]}],
  "local_prefixes": [{"vrf_id": $VRF_2, "prefix": "$PFX_2"}],
  "status_addr": "127.0.0.1:$S2"
}
EOF
cat > "$BOX_FIB" <<EOF
{
  "status_url": "http://127.0.0.1:$S2/status",
  "vpp_socket": "/run/vpp/api.sock",
  "poll_sec": 1,
  "vrfs": [$VRF_2],
  "tunnels": [{"next_hop": "$OVERLAY_N1", "interface": "wg0"}]
}
EOF
# node2 故意**不写** SR_BOX_AGENT_CTRL：清单得能在这个分支上把"为什么跳过"说清楚。
cat > "$BOX_CONF" <<EOF
SR_BOX_BIN=$BOX_BIN
SR_BOX_VPP_START=auto
SR_BOX_VPP_LAUNCHER=/workspace/test/docker/start-node.sh
SR_BOX_STATUS_WAIT=40
EOF
cat > "$WORK/bundle.json" <<EOF
{
  "node_id": 2,
  "version": 1,
  "artifacts": {
    "frr": "",
    "vpp": "mpls table add 0\nmpls table add $MK_2\n",
    "boot": "#!/bin/sh\nset -e\nip route replace blackhole $HB_2/32 table $VRF_2\n"
  }
}
EOF
$BOX_BIN/sr-apply apply --bundle "$WORK/bundle.json" --root "$BOX_STORE" --frr-reload true
EOS2

for c in "$N1" "$N2"; do
    dx "$c" "readlink $BOX_STORE/current" | grep -qx versions/1 ||
        die "$c 的 $BOX_STORE/current 没指向 versions/1"
    mpls_table "$c" "$MK_1" || mpls_table "$c" "$MK_2" || die "$c 装完 v1 却没有对应的 MPLS 记号表"
done
ok "磁盘上那版（v1）已装好、并且当场生效：current -> versions/1，两台都有自己那张记号表"

# ---------------------------------------------------------------- 1b check（只读）

info "1b. start.sh check：只说现状，一个进程都不起"
run_box "$N1" check || bad "node1 的 check 返回非 0（check 是只读的，不该失败）"
has_re "$BOX_OUT" "^route[[:space:]]+有.*配置在，进程没跑" &&
    ok "node1：route 门控「有」、现状「配置在，进程没跑」—— 该跑而没跑的东西要看得见" ||
    bad "node1 的 check 没把 route 写成「配置在，进程没跑」"
has_re "$BOX_OUT" "^apply[[:space:]]+有.*v1 / apply / ok" &&
    ok "node1：apply 那行报的是磁盘上那版的真实结论（v1 / apply / ok），不是猜的" ||
    bad "node1 的 check 没报出 apply v1 / apply / ok：$(printf '%s' "$BOX_OUT" | grep '^apply' || echo 无 apply 行)"
has_re "$BOX_OUT" "^frr[[:space:]]+无" && ok "node1：frr 门控「无」——这台盒子不跑 FRR，跳过不是失败" || bad "node1 的 check 没说 frr 无"
has_re "$BOX_OUT" "^bridge[[:space:]]+无" && ok "node1：bridge(sr-frr) 门控「无」，同样没报失败" || bad "node1 的 check 没说 bridge 无"
has_re "$BOX_OUT" "^tunnels.*1 条" &&
    ok "node1：tunnels 行如实报 1 条（wg0 是 e2e 建的，不是清单建的）" || bad "node1 的 check 没报 tunnels 1 条"
[ -z "$(dx "$N1" "pgrep -x sr-route; pgrep -x sr-fib; pgrep -x sr-agent")" ] &&
    ok "check 之后一个进程都没多起来 —— 它真的只读" || bad "check 启动了进程：那就不是 check"

# 空 conf + 只给控制器地址不给身份目录 ⇒ agent 门控的第二条分支
node2_branch=$(docker exec -e SR_BOX_CONF=/dev/null -e SR_BOX_AGENT_CTRL="$CTRL_DEAD" \
    -e SR_BOX_AGENT_ID=/var/lib/sr-box-nope "$N2" bash -lc "$BOX_LIB/start.sh check" 2>&1)
has_line "$node2_branch" "没 state.json" &&
    ok "换掉 conf、只给 ctrl 不给身份 ⇒ agent 报「没 state.json（未入网）」（conf 被 source 这条也顺带验了）" ||
    bad "agent 门控的第二条分支没说出来：$(printf '%s' "$node2_branch" | grep agent || echo 无 agent 行)"
run_box "$N2" check || bad "node2 的 check 返回非 0"
has_re "$BOX_OUT" "^agent[[:space:]]+无.*SR_BOX_AGENT_CTRL" &&
    ok "node2：conf 里没写 ctrl ⇒ agent 门控「无」并点名缺哪个键" ||
    bad "node2 的 check 没说 agent 门控无 / 缺 SR_BOX_AGENT_CTRL"

# ---------------------------------------------------------------- 1c RR 就位

# RR 是盒子的**对端基础设施**，不是盒子的一部分：它从没装进 $BOX_BIN，一直待在
# $WORK/bin 里，由 harness 起、由 harness 杀（B2 那一段就是要在它不在场的时候开机）。
# 这一段的职责只是把"第 2 段要用的对端"立起来 —— 第 2 段因此量的是清单，不是 RR。
info "1c. harness 起 RR（它是盒子外面的对端，不归 start.sh 管）"
dx "$N1" "bash $WORK/start-rr.sh" || die "RR 起不来"
wait_json "$N1" "$RR_STATUS" "d is not None" "RR 的状态口在回话" 25

# ---------------------------------------------------------------- 2 一遍 up（热 VPP）

info "2. 第一次 up：清单把路由面自己带起来（VPP 是热的，不该被碰）"
run_box "$N1" up && ok "node1 up 退出码 0" || bad "node1 up 退出码 $BOX_RC"
has_re "$BOX_OUT" "^vpp.*已在" && ok "node1：vpp 那一步是「已在」，没有重启热着的 VPP" || bad "node1：vpp 步骤不说「已在」"
has_line "$BOX_OUT" "重放完成" && ok "node1：apply 重放了磁盘上那版" || bad "node1：apply 没报重放完成"
has_re "$BOX_OUT" "^frr[[:space:]]+SKIP" && has_re "$BOX_OUT" "^bridge[[:space:]]+SKIP" &&
    ok "node1：frr/bridge 两步各有各的一行，跳过的也留在结论里" || bad "node1：跳过的步骤没出现在 up 的输出里"
run_box "$N2" up && ok "node2 up 退出码 0" || bad "node2 up 退出码 $BOX_RC"

wait_json "$N1" "$S1" "any(s['connected'] and s['synced'] for s in d['sessions'])" \
    "node1 的 sr-route 连上 RR 并同步完" 40
wait_json "$N1" "$S1" "d['active_rib_routes'] == 1 and d['rib']['$VRF_1'][0]['prefix'] == '$PFX_2'" \
    "表 $VRF_1 里学到 $PFX_2（node2 的宣告）" 30
wait_json "$N2" "$S2" "d['active_rib_routes'] == 1 and d['rib']['$VRF_2'][0]['prefix'] == '$PFX_1'" \
    "node2 那边学到 $PFX_1：清单跑完的是**两台**盒子" 30
wait_fn "node1 VPP 表 $VRF_1 里 $PFX_2 压 $LBL_2 经 $WG_IF" 25 \
    labeled_via "$N1" "$VRF_1" "$PFX_2" "$LBL_2" "$OVERLAY_N2"
wait_fn "颜色表 $CT_1 里也有它" 25 route_in "$N1" "$CT_1" "$PFX_2"
status_json "$N1" "$AS" | python3 -m json.tool >/dev/null 2>&1 &&
    ok "node1：sr-agent 由清单拉起，$AS 的状态口在回话" || bad "node1：sr-agent 的状态口没回话"
wait_json "$N1" "$AS" "d['last_ok_unix'] == 0 and d['last_err']" \
    "状态口里明写着「心跳一次都没成」：控制器不可达时 agent 照样在跑，失败是写出来的，不是猜的" 40
run_box "$N1" check || bad "up 之后的 check 返回非 0"
has_re "$BOX_OUT" "^route[[:space:]]+有.*状态口在回话" &&
    ok "check 现在把 route 报成「状态口在回话」—— 同一条清单，up 前后说的是两件不同的事" ||
    bad "up 之后的 check 没把 route 报成在回话"

term_setup
term_ping "第一次 up"

# ---------------------------------------------------------------- B1 进程死一个

info "B1. 只杀 sr-route，再跑一遍 up —— 清单不许重启别人"
p1_route=$(box_pid "$N1" route)
p1_fib=$(box_pid "$N1" fib)
p1_agent=$(box_pid "$N1" agent)
for s in "$p1_route" "$p1_fib" "$p1_agent"; do
    case "$s" in */[0-9]*) ;; *) die "取不到进程身份条（$s）：$BOX_RUN/*.pid 没写？" ;; esac
done
vpp_pid=$(dx "$N1" "pgrep -x vpp_main | head -1")
[ -n "$vpp_pid" ] || die "node1 上没有 vpp_main"
dx "$N1" "kill \$(cat $BOX_RUN/route.pid)" >/dev/null || die "杀 sr-route 失败"
sleep 1
[ -z "$(dx "$N1" "pgrep -x sr-route")" ] || die "sr-route 没死干净"
ok "node1 的 sr-route 被杀掉（模拟进程崩了）"

run_box "$N1" up && ok "第二遍 up 退出码 0" || bad "第二遍 up 退出码 $BOX_RC"
p2_route=$(box_pid "$N1" route)
[ "$p2_route" != "$p1_route" ] && [ "$p2_route" != none ] &&
    ok "sr-route 补上了（$p1_route → $p2_route）：清单只补它缺的那一步" ||
    bad "sr-route 没被补起来（$p1_route → $p2_route）"
p2_fib=$(box_pid "$N1" fib); p2_agent=$(box_pid "$N1" agent)
[ "$p2_fib" = "$p1_fib" ] && ok "sr-fib 的 pid+启动时刻没变（$p2_fib）：补一个进程不等于重启一排" ||
    bad "sr-fib 被重启了：$p1_fib → $p2_fib"
[ "$p2_agent" = "$p1_agent" ] && ok "sr-agent 没被重启（$p2_agent）" || bad "sr-agent 被重启了：$p1_agent → $p2_agent"
[ "$vpp_pid" = "$(dx "$N1" "pgrep -x vpp_main | head -1")" ] && ok "VPP 没被重启" || bad "VPP 被清单重启了"

wait_json "$N1" "$S1" "any(s['connected'] and s['synced'] for s in d['sessions']) and d['active_rib_routes'] == 1" \
    "新 sr-route 自己连回 RR 又学到了（没人替它重启谁）" 40
wait_fn "VPP 表 $VRF_1 里的 $PFX_2 又回来了（老的 sr-fib 下一轮轮询自己补上）" 25 \
    labeled_via "$N1" "$VRF_1" "$PFX_2" "$LBL_2" "$OVERLAY_N2"
term_ping "sr-route 崩过又补回来之后"

run_box "$N1" up >/dev/null && ok "第三遍 up 退出码 0（没得补的时候它就该什么都不做）" || bad "第三遍 up 退出码 $BOX_RC"
p3_route=$(box_pid "$N1" route); p3_fib=$(box_pid "$N1" fib); p3_agent=$(box_pid "$N1" agent)
if [ "$p3_route" = "$p2_route" ] && [ "$p3_fib" = "$p2_fib" ] && [ "$p3_agent" = "$p2_agent" ]; then
    ok "第三遍 up 一个 pid 都没变（$p3_route / $p3_fib / $p3_agent）：幂等"
else
    bad "第三遍 up 动了已经在跑的进程（$p2_route,$p2_fib,$p2_agent → $p3_route,$p3_fib,$p3_agent）"
fi

# ---------------------------------------------------------------- B2 真冷启

info "B2. docker restart 两个容器：内存里的一切都没了，磁盘上的都还在"
kill_harness "$N1" rr      # RR 也"死一次"：盒子该能在自己的基础设施之前起来
docker restart "$N1" "$N2" >/dev/null || die "重启容器失败"
sleep 2
for c in "$N1" "$N2"; do
    dx "$c" "vppctl show version" 2>/dev/null | grep -q 'vpp v' &&
        bad "$c 重启后 VPP 还在（那这就不算冷启）" ||
        ok "$c：VPP 没了 —— 容器里没有 systemd，PID 1 只是个 bash，没人会替它起来"
done
mpls_table "$N1" "$MK_1" && bad "node1：MPLS 表 $MK_1 居然还在" || ok "node1：bundle 建过的 MPLS 表 $MK_1 随 VPP 一起没了"
kroute "$N1" "$VRF_1" "$HB_1" && bad "node1：内核 table $VRF_1 的 $HB_1 居然还在" || ok "node1：bundle 写的那条内核路由也没了"
has_iface "$N1" "$WG_IF" && bad "node1：wg0 居然还在" || ok "node1：wg0 没了 —— 隧道这一层只能等控制器，清单不假装能恢复它"
for c in "$N1" "$N2"; do
    dx "$c" "test -x $BOX_BIN/sr-route && test -x $BOX_LIB/start.sh && test -e $BOX_ROUTE && test -e $BOX_CONF && readlink $BOX_STORE/current" |
        grep -qx versions/1 &&
        ok "$c：磁盘上的都在 —— 清单、两张表、conf、current -> versions/1（冷启前后就差在内存）" ||
        bad "$c：磁盘上的东西不全"
done

info "B2a. 每台盒子只敲一条命令：start.sh up（此时 RR 还没起来）"
run_box "$N1" up && ok "node1 up 退出码 0（RR 不在场，清单不该因此失败）" || bad "node1 up 退出码 $BOX_RC"
has_re "$BOX_OUT" "^vpp.*起来了" &&
    ok "node1：vpp 那一步写着「起来了」—— 容器里没人管 VPP，是这份清单把它拉起来的" ||
    bad "node1：VPP 不是清单拉起来的（输出里没有「起来了」）"
wait_vppctl "$N1" 30 || die "node1 的 VPP 起不来，后面没法测"
vpp "$N1" "show plugins" | grep -q sr_wg_plugin.so &&
    ok "node1：新 VPP 里 sr_wg 插件在（启动器那条伪终端路径没把插件弄丢）" || bad "node1：插件没加载"
mpls_table "$N1" "$MK_1" && ok "node1：MPLS 表 $MK_1 回来了 —— 磁盘上那版落进了刚启动的 VPP，一个字节都没问控制器" ||
    bad "node1：apply 没把 MPLS 表 $MK_1 建回来"
kroute "$N1" "$VRF_1" "$HB_1" && ok "node1：内核 table $VRF_1 里 $HB_1 也回来了" || bad "node1：内核段没重放回来"
wait_json "$N1" "$S1" "d['device_id'] == 1" "node1：sr-route 在跑，RR 没起来也照样把状态口开开了" 40
wait_json "$N1" "$S1" "all(not s['connected'] for s in d['sessions']) and d['sessions'][0]['last_error']" \
    "会话明写着没连上、而且 last_error 写着为什么 —— 不是「看起来一切正常」" 20
has_line "$BOX_OUT" "磁盘上没有那份表" &&
    ok "node1：清单把「隧道 0 条、这一层我恢复不了」写在结论里（诚实的缺口比假装的完成有用）" ||
    bad "node1：tunnels 那两行诚实文案没出现"
run_box "$N2" up && ok "node2 也是同一条命令、退出码 0" || bad "node2 up 退出码 $BOX_RC"
mpls_table "$N2" "$MK_2" && ok "node2：MPLS 表 $MK_2 回来了" || bad "node2：MPLS 表 $MK_2 没回来"

info "B2b. RR 起来 —— 已经开完机的两台盒子自己接上，谁都不用再碰"
dx "$N1" "bash $WORK/start-rr.sh" || die "RR 起不来"
wait_json "$N1" "$S1" "any(s['connected'] and s['synced'] for s in d['sessions'])" \
    "node1 自己重连上了 RR" 40
wait_json "$N2" "$S2" "any(s['connected'] and s['synced'] for s in d['sessions'])" \
    "node2 也是" 40
rc1=$(field "$N1" "$S1" "d['sessions'][0]['reconnects']")
[ "$rc1" -ge 1 ] 2>/dev/null && ok "node1 的 reconnects=$rc1（≥1）：那条连接是它自己重试出来的" ||
    bad "node1 的 reconnects=$rc1：它没重试过？"
wait_json "$N1" "$S1" "d['active_rib_routes'] == 1" "node1 重新学到 $PFX_2" 30
wait_json "$N2" "$S2" "d['active_rib_routes'] == 1" "node2 重新学到 $PFX_1" 30
if no_route_in "$N1" "$VRF_1" "$PFX_2"; then
    ok "但 VPP 表 $VRF_1 里还装不上：没有隧道就没有下一跳，sr-fib 只能等着（它没报错，也没重启）"
else
    bad "隧道不在场时表 $VRF_1 里居然有 $PFX_2"
fi

info "B2c. 手工把 wg0 恢复（这一段扮演的是控制器 + sr-agent -tunnels）"
# 命令序列与 test-e2e.sh step 3 逐字相同；transit 没重启过，所以不动它。
p_route=$(box_pid "$N1" route); p_fib=$(box_pid "$N1" fib)
q_route=$(box_pid "$N2" route); q_fib=$(box_pid "$N2" fib)
dx "$N1" "bash $SETUP_C node1 init" >/dev/null 2>&1 || die "node1 init 失败"
dx "$N2" "bash $SETUP_C node2 init" >/dev/null 2>&1 || die "node2 init 失败"
PUB1=$(dx "$N1" "bash $SETUP_C node1 wg-create" 2>/dev/null | tail -1)
PUB2=$(dx "$N2" "bash $SETUP_C node2 wg-create" 2>/dev/null | tail -1)
[ -n "$PUB1" ] && [ -n "$PUB2" ] || die "读不到 WG 公钥"
dx "$N1" "bash $SETUP_C node1 peer $PUB2" >/dev/null 2>&1 || die "node1 peer 失败"
dx "$N2" "bash $SETUP_C node2 peer $PUB1 nosr" >/dev/null 2>&1 || die "node2 peer 失败"
vpp "$N1" "ping $OVERLAY_N2 repeat 5 interval 0.2" | grep -q '0% packet loss' ||
    die "恢复后 overlay 不通（这已经不是清单的问题了）"
ok "wg0 回来了、overlay 又通"

wait_fn "已经在跑的 sr-fib 自己把 $PFX_2 装进表 $VRF_1（它每轮都重新查接口，不缓存 sw_if_index）" 30 \
    labeled_via "$N1" "$VRF_1" "$PFX_2" "$LBL_2" "$OVERLAY_N2"
wait_fn "node2 那边同样（表 $VRF_2 里 $PFX_1 压 $LBL_1）" 30 \
    labeled_via "$N2" "$VRF_2" "$PFX_1" "$LBL_1" "$OVERLAY_N1"
if [ "$(box_pid "$N1" route)" = "$p_route" ] && [ "$(box_pid "$N1" fib)" = "$p_fib" ] &&
   [ "$(box_pid "$N2" route)" = "$q_route" ] && [ "$(box_pid "$N2" fib)" = "$q_fib" ]; then
    ok "四个进程的 pid+启动时刻一个没变：冷启之后隧道姗姗来迟，靠的是这些进程自己在跟，不是重启"
else
    bad "冷启后隧道回来时有人被重启了（$p_route/$p_fib → $(box_pid "$N1" route)/$(box_pid "$N1" fib)）"
fi
run_box "$N1" check >/dev/null
has_re "$BOX_OUT" "^tunnels.*1 条" &&
    ok "node1 的 check 现在报 tunnels 1 条（那两行诚实文案换成了一行计数）" ||
    bad "node1 的 check 没报 tunnels 1 条"
term_setup
term_ping "冷启 + 隧道姗姗来迟之后"

echo
echo "== 结果：$PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
    for c in "$N1" "$N2"; do
        echo "---- $c 的清单日志" >&2
        dx "$c" "for f in $BOX_LOG/*.log; do echo \"[\$f]\"; tail -20 \"\$f\"; done" >&2 2>/dev/null || true
        echo "---- $c VPP" >&2
        vpp "$c" "show mpls fib" | head -8 >&2
    done
    echo "---- RR" >&2
    dx "$N1" "tail -20 $WORK/rr.log" >&2 2>/dev/null || true
    exit 1
fi
