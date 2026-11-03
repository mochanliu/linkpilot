#!/bin/bash
# test-escape.sh - 逃生开关（§5）：颜色 0 真死后主表是"消失"还是"切到昂贵颜色"，两条都计时
#
# 这条脚本回答四件事，前两件是 §9 O4 判据逐字要求的，后两件是 §11#2 要求顺手量的：
#
#   (1) 开关**关**（定案的默认形态）：把颜色 0 那条隧道的口真打死，等本机 BFD 判它
#       Down，sr-fib 下一轮把这条路径从计划里剔掉 —— 因为主表只收颜色 0，剔掉之后
#       **主表里这条前缀就不存在了**。打流必断，而且**不会**回落到昂贵颜色。
#       要量的两个数：从前缀在主表消失的角度看，"口 down → 主表没这条前缀"的时长；
#       以及这段时间终端实际丢了多久的包。
#   (2) 开关**开**：同样的动作，主表里这条前缀**始终存在** —— 它本来就一直带着
#       颜色 3 那条路径（pref=230+3）。VPP 原生就把流量抬到存活的最小颜色，
#       恢复后自己回颜色 0。要量的数：口 down → 主表只剩颜色 3 这条路径（形态落实）
#       的时长，以及终端的实际丢包窗口（预期远小于 (1)：不等控制面）。
#       恢复之后再用接口计数器证明流量真的回到了 wg0（不是"看着像"）。
#       注意这里**不用 buckets 数当判据**：实测 preference 不同的两条路径 VPP 放在
#       同一个 dpo 里（靠近的那条是主、远的当备，buckets:1），只有同 pref 才并出
#       两个 bucket。所以开关开的形态证据是 fib-dump 读回的两条 pref + 计数器归因。
#   (3) 改色窗口：**开关关**（生产默认形态）下主表 2 bucket 同色，把 wg1 改成颜色 3
#       （走重启 `-once`，即新进程 plain add 整条替换）打流测实际窗口，
#       并确认改色的那条搬进了颜色表、主表只剩颜色 0。
#       **如实标注**：这一条量的是"整条替换"的代价。Applier 里 §2.2 的先删后加
#       分支（身份相同、只有 pref 变）今天的配置流线**走不到** —— 颜色只在进程
#       启动时读一次，所以它只由单测 `TestApplyDeleteThenAddOnPrefChange` 钉住。
#   (4) 负向：开关开不等于放松"同标签才同桶"，也不等于颜色表停写。颜色表两种
#       情况下都照写（policy 的落点），这里在每一步都复查一遍。
#
# 为什么这一步必须有真 BFD：上一支脚本（test-ecmp-color.sh）里"口 down → bucket
# 变 1"是 VPP 原生算的，本机没跑 BFD，计划不会变。而逃生开关的语义恰恰长在**计划**
# 上（主表收哪些颜色），所以这里必须让 sr-fib 看得见可达性：两端各建两条多跳 BFD
# 会话（每条隧道一对），把颜色 0 那条真打死，BFD 判 Down，路径才会被剔出计划。
#
# 前置：**必须先跑过 test-e2e.sh --keep 与 test-multitunnel.sh --keep**
# （node1↔node2 之间 wg0@192.168.99.x 与 wg1@192.168.100.x 两条隧道都在、都通）。
# 本脚本只在这上面加控制面、BFD 会话与两个 tap 终端；收尾把自己装的 FIB 状态、
# BFD 会话、tap、进程全部撤掉，并把打下来的口拉起来，不动两条隧道本身。
#
# 用法:
#   test/scripts/test-escape.sh [--keep]
set -u

N1=srwg-node1
N2=srwg-node2
MODULE=/workspace/controller
WORK=${WORK:-/tmp/sr-escape-e2e}

WG_IF=${WG_IF:-wg0}
WG2_IF=${WG2_IF:-wg1}
OVERLAY_N1=${OVERLAY_N1:-192.168.99.1}
OVERLAY_N2=${OVERLAY_N2:-192.168.99.2}
OVERLAY2_N1=${OVERLAY2_N1:-192.168.100.1}
OVERLAY2_N2=${OVERLAY2_N2:-192.168.100.2}
# node1 内核在**管理 bridge**（10.10.0.0/24）上的地址：node2 容器里的 sr-fib 从
# 这里拉 node2 那份 RIB。不能用 10.10.1.2 —— 出桥时内核会改写源地址。
N1_CORE=${N1_CORE:-10.10.0.2}

VRF=${VRF:-60}
LABEL=$((1000 + VRF))
LOCAL_PFX=${LOCAL_PFX:-10.50.0.0/16}    # node1 宣告
REMOTE_PFX=${REMOTE_PFX:-10.51.0.0/16}  # node2 宣告，node1 要用两条隧道装进 FIB
COLOR2=${COLOR2:-3}

RR_PORT=${RR_PORT:-18566}
RR_STATUS=${RR_STATUS:-18567}
N1_STATUS=${N1_STATUS:-18581}
N2_STATUS=${N2_STATUS:-18583}

# BFD：50ms x 3 ⇒ 检测约 150ms，再加 sr-fib 一轮 poll_sec=1，就是 §8 要写的口径。
BFD_TX=${BFD_TX:-50000}
BFD_RX=${BFD_RX:-50000}
BFD_MULT=${BFD_MULT:-3}

TAP_N1=${TAP_N1:-tapecs1}
TAP_N2=${TAP_N2:-tapecs2}
TAP_N1_GW=${TAP_N1_GW:-10.50.255.1/30}
TAP_N1_HOST=${TAP_N1_HOST:-10.50.255.2/30}
TAP_N2_GW=${TAP_N2_GW:-10.51.255.1/30}
TAP_N2_HOST=${TAP_N2_HOST:-10.51.255.2/30}

PING_IV_MS=${PING_IV_MS:-100}

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
num()  { echo "       >>> $*"; }
die()  { echo "FATAL: $*" >&2; exit 2; }

# 计时结果收集：最后统一打进文档口径（§8/§9/§11），不要只散在日志里。
RESULTS=${RESULTS:-/tmp/sr-escape-timings.txt}
: > "$RESULTS"
record() {   # <key> <value>
    echo "$1=$2" | tee -a "$RESULTS"
    num "$1 = $2"
}

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
mpls_dropped() {   # <container> <iface> —— 该口的 mpls-input 弧还挂在丢弃路径上
    vpp "$1" "show interface features $2" |
        awk '/^mpls-input:/{getline; exit !/mpls-not-enabled/}'
}

dump_table() {   # <container> <table> <prefix> —— 读回该表里这条前缀（无则空）
    dx "$1" "$WORK/bin/fib-dump -table $2 -prefix $3" 2>/dev/null
}
of() { dump_table "$N1" "$VRF" "$REMOTE_PFX"; }
path_n() {   # <container> <table> <prefix> —— 该表该前缀里有几条路径
    dump_table "$1" "$2" "$3" | grep -c '  via '
}
# wait_fn 要的是"函数名 + 参数"，每轮重新求值；直接传 `test "$(path_n ...)" = 2`
# 会在调用点展开成一次性的 test 1 = 2，之后每轮都判同一个死值 —— 假等或假过。
path_n_is() { [ "$(path_n "$1" "$2" "$3")" = "$4" ]; }
has_path() {   # <container> <table> <prefix> <next_hop> <preference>
    dump_table "$1" "$2" "$3" | grep -F "via $4 " | grep -q "preference=$5"
}
has_prefix() {   # <container> <table> <prefix>
    [ -n "$(dump_table "$1" "$2" "$3")" ]
}
buckets_of() {   # <container> <table> <prefix>
    vpp "$1" "show ip fib table $2" | awk -v p="$3" '$1==p {f=1; next}
        f && /dpo-load-balance/ {
            if (match($0, /buckets:[0-9]+/)) print substr($0, RSTART+8, RLENGTH-8);
            exit}'
}
mpls_count() {   # <container> <iface> —— 该口经过的 MPLS 包计数（证明流量走了哪条）
    # 没有 mpls 行时打印 0 而不是空：空值进到 $(( )) 就是 "syntax error: operand expected"。
    vpp "$1" "show interface $2" | awk '$1=="mpls" {print $2; f=1; exit} END {if (!f) print 0}'
}
color_table() { echo $(( $1 * 1000 + $2 )); }   # <color> <main>（§4.2 公式，脚本侧独立算；2026-11-03 去掉 +10000 段基址）
LEGACY_COLOR0=$((10000 + VRF))                 # 改之前的颜色 0 落点，现在必须一直是空的

wait_fn() {   # <label> <timeout_s> <fn> [args...]
    local label=$1 limit=$2 i=0
    shift 2
    while [ "$i" -lt $((limit * 5)) ]; do
        if "$@"; then ok "$label"; return 0; fi
        sleep 0.2
        i=$((i + 1))
    done
    bad "$label (超时)"
    of | head -8 >&2
    return 1
}

# 容器内轮询计时：t0 与每一次判定都在 node1 容器里取（GNU date 有毫秒）。
# 判定不在宿主机拼字符串再 eval：那些前缀/下一跳里全是引号和 $，穿过两层 shell
# 迟早散架。形态判定集中在容器里的 chk.sh（见 §1），这里只传"检查名 + 参数"。
measure_ms() {   # <timeout_s> <chk.sh 的参数...> —— 打印通过所花 ms；超时打印 TIMEOUT
    local limit=$1; shift
    dx "$N1" "
        t0=\$(date +%s%3N)
        while :; do
            if $WORK/chk.sh $*; then echo \$(( \$(date +%s%3N) - t0 )); exit 0; fi
            now=\$(date +%s%3N)
            if [ \$(( now - t0 )) -ge $(( limit * 1000 )) ]; then echo TIMEOUT; exit 1; fi
            sleep 0.02
        done"
}

ping_ok()   { dx "$1" "ping -c2 -W1 -i0.2 $2" >/dev/null 2>&1; }
wait_ping() {   # <dst> <label> [timeout_s]
    local dst=$1 label=$2 limit=${3:-20} i=0
    while [ "$i" -lt $((limit * 2)) ]; do
        if ping_ok "$N1" "$dst"; then ok "$label"; return 0; fi
        sleep 0.5
        i=$((i + 1))
    done
    bad "$label (不通)"
    dx "$N1" "ping -c2 -W1 -i0.2 $dst" 2>&1 | tail -3 >&2
    of | head -6 >&2
    return 1
}
wait_no_ping() {   # <dst> <label> —— 连续两次都不通才算（避免抢在收敛前误判通过）
    local dst=$1 label=$2
    sleep 1
    if ping_ok "$N1" "$dst" || ping_ok "$N1" "$dst"; then
        bad "$label（还能 ping 通）"
        of | head -6 >&2
        return 1
    fi
    ok "$label"
}

# 后台打流：固定间隔、固定包数，收包序号缺失的最长连续段 = 断流窗口。
# 用 awk 把 PING_IV_MS 换成秒：`-i 0.1` 只有 root 能用，容器里就是 root。
PING_BG=""
ping_start() {   # <name> [count]
    local name=$1 count=${2:-100} iv
    iv=$(awk -v ms="$PING_IV_MS" 'BEGIN { printf "%.3f", ms / 1000 }')
    docker exec "$N1" bash -lc "ping -i $iv -c $count ${TAP_N2_HOST%/*} > $WORK/$name.log 2>&1" &
    PING_BG=$!
}
# 收尾必须发生在**父 shell**：`ping_analyze X | tee … | sed …` 的左边跑在子 shell 里，
# 子 shell 的作业表没有这个 PID，`wait` 立刻返回失败 —— 于是读到一份还在写的日志
# （本轮实测：统计行还没落笔，判读拿到"没有 packets transmitted"）。
ping_wait() {
    [ -n "$PING_BG" ] || return 0
    wait "$PING_BG" 2>/dev/null
    PING_BG=""
}
ping_analyze() {   # <name> —— 输出 loss 个数与最长断流 ms（调用前先 ping_wait）
    local name=$1 sent recv seqs maxrun
    seqs=$(dx "$N1" "grep -o 'icmp_seq=[0-9]*' $WORK/$name.log | sed 's/.*=//' | sort -n" 2>/dev/null)
    sent=$(dx "$N1" "grep -o '[0-9]* packets transmitted' $WORK/$name.log | awk '{print \$1}'" 2>/dev/null)
    recv=$(printf '%s\n' "$seqs" | grep -c '^[0-9]')
    # 量不到就明说，绝不退化成"0 个包丢 = 断流窗口 0ms"：上一轮 pings 一条没发出去
    # （内核侧 tap 没起来），打流读数就成了 sent=0 recv=0 窗口 0ms，看着像通过。
    if [ -z "$sent" ]; then
        echo "断流窗口=量不到（$WORK/$name.log 里没有 'packets transmitted' 统计行，ping 没跑起来；日志尾：$(dx "$N1" "tr '\n' '|' < $WORK/$name.log" 2>/dev/null | tail -c 160)）"
        return 0
    fi
    maxrun=$(printf '%s\n' "$seqs" | awk -v sent="$sent" -v iv="$PING_IV_MS" '
        NF==0 {next}
        {n=$1+0; g=n-prev-1; if (g>max) max=g; prev=n}
        END {if (sent-prev > max) max=sent-prev; print max*iv}')
    echo "sent=$sent recv=$recv loss=$((sent - recv)) 断流窗口=${maxrun}ms（±${PING_IV_MS}ms，按最长连续缺失序号段计）"
}
# 判读放在父 shell：ping_analyze 在管道左边，跑在子 shell 里，它 bad() 也计不上数。
ping_verdict() {   # <tee 出来的文件> <记录名>
    local s
    s=$(grep -o '断流窗口=.*' "$1" 2>/dev/null)
    case "$s" in
      断流窗口=量不到*) bad "打流量不到：$s" ;;
      "")               bad "打流没有产出读数（见 $1）" ;;
      *)                record "$2" "$s" ;;
    esac
}

tap_create() {   # <container> <tap> <vpp addr/mask> <host addr/mask>
    local c=$1 t=$2 gw=$3 host=$4
    if has_iface "$c" "$t"; then die "$c 上已有接口 $t：先确认它不是别人的再删"; fi
    vpp "$c" "create tap if-name $t host-if-name $t host-ip4-addr $host host-mtu-size 1500" >/dev/null || die "$c 建 $t 失败"
    vpp "$c" "set interface state $t up" >/dev/null || die "$c 拉起 $t 失败"
    vpp "$c" "set interface ip table $t $VRF" >/dev/null || die "$c 把 $t 放进表 $VRF 失败"
    vpp "$c" "set interface ip address $t $gw" >/dev/null || die "$c 给 $t 配 $gw 失败"
    dx "$c" "ip link set $t up" >/dev/null || die "$c 内核侧拉起 $t 失败"
    # 内核侧必须真的有 $host 这个地址：后面 `ip route replace <前缀> via <VPP 侧 gw>`
    # 靠它才能把包递进 tap。上一轮这里写成 || true，tap 没配上地址 → 内核
    # "Network is unreachable" → 后台 ping 一条都没发出去 → 打流读数伪装成 0ms 通过。
    dx "$c" "ip -4 -o addr show dev $t" 2>/dev/null | grep -qF "${host%/*}" ||
        die "$c 内核侧 $t 上没有地址 ${host%/*}（VPP 的 host-ip4-addr 没落地）"
}
tap_del() {
    vpp "$1" "delete tap $2" >/dev/null 2>&1
    dx "$1" "ip link del $2" >/dev/null 2>&1 || true
}

bfd_add() {   # <container> <local> <peer>
    vpp "$1" "bfd udp session add multihop local-addr $2 peer-addr $3 desired-min-tx $BFD_TX required-min-rx $BFD_RX detect-mult $BFD_MULT" >/dev/null
}
bfd_del() {   # <container> <local> <peer>
    vpp "$1" "bfd udp session del multihop local-addr $2 peer-addr $3" >/dev/null 2>&1 || true
}
# 状态只能从 API dump 读（CLI 只打印计数），借 internal/vppapi 里那个现成探针。
bfd_wait_up() {   # <local> <peer>
    dx "$N1" "cd $MODULE && SRWG_VPP_SOCK=/run/vpp/api.sock \
        SRWG_OVERLAY_LOCAL=$1 SRWG_OVERLAY_PEER=$2 \
        SRWG_BFD_WANT=up SRWG_BFD_WAIT_MS=8000 \
        go test ./internal/vppapi -run TestLiveBFDState -count=1" 2>&1 | grep -q "^ok"
}

status_hops() {   # node1 的 RIB 里该前缀解析出的下一跳（排序去重：比的是集合）
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
cleanup() {
    if [ "$KEEP" = 1 ]; then
        info "--keep：进程、BFD、tap 与 FIB 状态都留着（人工拆：sr-fib -reset、删 tap、杀 $WORK/*.pid）"
        info "计时结果在 $RESULTS"
        return
    fi
    info "收尾：拉起被打下来的口、撤 tap、撤 BFD 会话、停进程、撤 FIB 状态"
    vpp "$N1" "set interface state $WG_IF up" >/dev/null 2>&1
    vpp "$N1" "set interface state $WG2_IF up" >/dev/null 2>&1
    tap_del "$N1" "$TAP_N1"
    tap_del "$N2" "$TAP_N2"
    bfd_del "$N1" "$OVERLAY_N1" "$OVERLAY_N2"
    bfd_del "$N1" "$OVERLAY2_N1" "$OVERLAY2_N2"
    bfd_del "$N2" "$OVERLAY_N2" "$OVERLAY_N1"
    bfd_del "$N2" "$OVERLAY2_N2" "$OVERLAY2_N1"
    kill_one "$N2" fib
    for n in fib node1 node2 rr; do kill_one "$N1" "$n"; done
    sleep 0.3
    dx "$N1" "$WORK/bin/sr-fib -config $WORK/fib.json -reset" >/dev/null 2>&1 || true
    dx "$N2" "$WORK/bin/sr-fib -config $WORK/fib2.json -reset" >/dev/null 2>&1 || true
    dx "$N1" "rm -rf $WORK" >/dev/null 2>&1 || true
    dx "$N2" "rm -rf $WORK" >/dev/null 2>&1 || true
    info "计时结果留在 $RESULTS"
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
        mpls_dropped "$c" "$i" && die "$c 的 $i 还挂在 mpls-not-enabled 上（被删过又重建的隧道，enable 对它已是空操作）：重启 VPP、e2e 与 multitunnel 各只跑一次再来"
    done
done
for p in "$RR_PORT" "$RR_STATUS" "$N1_STATUS" "$N2_STATUS"; do
    dx "$N1" "ss -ltn" 2>/dev/null | grep -q ":$p " && die "node1 上端口 $p 已被占用：上一轮的 sr-* 还活着，先杀干净（pgrep -af 'sr-(rr|route|fib)'）"
done
ok "两条隧道都在、都通，四条（2 端 x 2 口）收端 MPLS 都真的开着，四个端口都空"

CT0=$(color_table 0 "$VRF")
CT2=$(color_table "$COLOR2" "$VRF")
info "   表号按 §4.2 公式（2026-11-03 更正：颜色*1000+主表号）：主表 $VRF，颜色 0 → $CT0（=主表），颜色 $COLOR2 → $CT2；本地标签 $LABEL"

# ---------------------------------------------------------------- 1 配置

info "1. 准备 $WORK：PKI、配置、二进制（两端各一份 sr-fib 配置，逃生开关同一个值）"
dx "$N1" "rm -rf $WORK && mkdir -p $WORK/bin $WORK/pki $WORK/identity" || die "建 $WORK 失败"

docker exec -i -e WORK="$WORK" -e RR_PORT="$RR_PORT" -e RR_STATUS="$RR_STATUS" \
    -e N1_STATUS="$N1_STATUS" -e N2_STATUS="$N2_STATUS" -e N1_CORE="$N1_CORE" \
    -e VRF="$VRF" -e LABEL="$LABEL" \
    -e OVERLAY_N1="$OVERLAY_N1" -e OVERLAY_N2="$OVERLAY_N2" \
    -e OVERLAY2_N1="$OVERLAY2_N1" -e OVERLAY2_N2="$OVERLAY2_N2" \
    -e WG_IF="$WG_IF" -e WG2_IF="$WG2_IF" \
    -e LOCAL_PFX="$LOCAL_PFX" -e REMOTE_PFX="$REMOTE_PFX" -e COLOR2="$COLOR2" \
    "$N1" bash -s <<'EOS' || die "写配置失败"
set -e
cd "$WORK/pki"
gen_key() { openssl genpkey -algorithm ED25519 -out "$1" 2>/dev/null; }
gen_key ca.key
openssl req -x509 -new -key ca.key -days 30 -subj "/CN=sr-escape-ca" -out ca.crt 2>/dev/null
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

# 两端都宣告**两条**隧道：逃生开关是租户级的（§5.2 拍板），两台盒子同一个值。
# 反向只有一条路的话，"开关开着流量不断"这条就分不清是正向兜住了还是反向本来就活。
for n in 1 2; do
    if [ "$n" = 1 ]; then DEV=1; PEER=2; SA="$OVERLAY_N1"; SB="$OVERLAY2_N1"; PA="$OVERLAY_N2"; PB="$OVERLAY2_N2"; LP="$LOCAL_PFX"; RP="$REMOTE_PFX"; ST="127.0.0.1:$N1_STATUS"; else
        DEV=2; PEER=1; SA="$OVERLAY_N2"; SB="$OVERLAY2_N2"; PA="$OVERLAY_N1"; PB="$OVERLAY2_N1"; LP="$REMOTE_PFX"; RP="$LOCAL_PFX"; ST="0.0.0.0:$N2_STATUS"; fi
    cat > "$WORK/node$n.json" <<EOF
{
  "tenant": "tenant-a",
  "device_id": $DEV,
  "identity_dir": "$WORK/identity/node$DEV",
  "serial": "node$n",
  "hold_sec": 6,
  "rrs": [{"id": "rr-a", "addr": "127.0.0.1:$RR_PORT"}],
  "tunnels": [{"peer_device_id": $PEER, "next_hop": "$PA"},
              {"peer_device_id": $PEER, "next_hop": "$PB"}],
  "vrfs": [{"id": $VRF, "export_rts": ["65000:100"], "import_rts": ["65000:100"]}],
  "status_addr": "$ST"
}
EOF
done

# write_fib <c0> <c1> <escape> <outfile> <status_url> <peer_nh> <peer_nh2> <if1> <if2>
# 逃生开关的键名 = 草案 §3.1 的 escape_all_colors；配置读侧用了 DisallowUnknownFields，
# 名字对不上会当场报错，不会静默按"关"跑。
write_fib() {
    cat > "$4" <<EOF
{
  "status_url": "$5",
  "vpp_socket": "/run/vpp/api.sock",
  "poll_sec": 1,
  "escape_all_colors": $3,
  "vrfs": [$VRF],
  "tunnels": [{"next_hop": "$6", "interface": "$8", "color": $1},
              {"next_hop": "$7", "interface": "$9", "color": $2}]
}
EOF
}

# 基线：wg0 颜色 0、wg1 颜色 COLOR2，开关**关**（定案的默认形态）。
write_fib 0 "$COLOR2" false "$WORK/fib.json" \
    "http://127.0.0.1:$N1_STATUS/status" "$OVERLAY_N2" "$OVERLAY2_N2" "$WG_IF" "$WG2_IF"

# node2 那份必须**逐字另写**：它的下一跳是对端的 overlay 地址（node1 的 .99.1 /
# .100.1）。上一轮这里图省事，拿 node1 那份只 sed 改状态口，于是 node2 的隧道表里
# 装的是它自己的地址，sr-fib 报"下一跳不在隧道表里"、反向路由一条没装，
# 之后每一步 ping 全断 —— 11 条 FAIL 全是这一个错衍生出来的。
write_fib 0 "$COLOR2" false "$WORK/fib2.json" \
    "http://$N1_CORE:$N2_STATUS/status" "$OVERLAY_N1" "$OVERLAY2_N1" "$WG_IF" "$WG2_IF"

cat > "$WORK/start.sh" <<'EOF'
#!/bin/bash
set -e
cd "$(dirname "$0")"
nohup ./bin/sr-rr    -config rr.json    > rr.log    2>&1 & echo $! > rr.pid
nohup ./bin/sr-route -config node1.json > node1.log 2>&1 & echo $! > node1.pid
nohup ./bin/sr-route -config node2.json > node2.log 2>&1 & echo $! > node2.pid
EOF
chmod +x "$WORK/start.sh"

# 起一份 sr-fib：$1=配置文件 $2=日志 $3=pid 文件（都要显式传，别靠默认 —— 上一轮
# 就是靠默认名留下了孤儿进程占着端口）。
cat > "$WORK/start-fib.sh" <<'EOF'
#!/bin/bash
set -e
cd "$(dirname "$0")"
nohup ./bin/sr-fib -config "$1" > "$2" 2>&1 & echo $! > "$3"
EOF
chmod +x "$WORK/start-fib.sh"

# chk.sh 是计时循环用的形态判定（只在 node1 跑，参数都是本机表号与前缀）。
# 为什么写在这而不是宿主机传字符串进去：见 measure_ms 的注释。
cat > "$WORK/chk.sh" <<EOF
#!/bin/bash
WORK_DIR=\$(cd "\$(dirname "\$0")" && pwd)
D() { "$WORK/bin/fib-dump" -table "\$1" -prefix "\$2"; }
case "\$1" in
  # 主表里这条前缀又回来了（开关关 + 颜色 0 恢复的形态）
  present)  [ -n "\$(D "\$2" "\$3")" ] ;;
  # 主表里这条前缀彻底没有（开关关 + 颜色 0 全 down 的形态）
  absent)   [ -z "\$(D "\$2" "\$3")" ] ;;
  # 只剩一条路径，而且是"<下一跳> + preference=<pref>"那一条（开关开的切换）
  one_path) out=\$(D "\$2" "\$3")
            [ "\$(printf '%s' "\$out" | grep -c '  via ')" = 1 ] &&
            printf '%s' "\$out" | grep -F "via \$4 " | grep -q "preference=\$5" ;;
  two_path) [ "\$(D "\$2" "\$3" | grep -c '  via ')" = 2 ] ;;
  *) exit 2 ;;
esac
EOF
chmod +x "$WORK/chk.sh"
EOS

# ---------------------------------------------------------------- 2 二进制与启动

info "2. 编译并启动 rr / sr-route x2"
dx "$N1" "cd $MODULE && go build -o $WORK/bin/sr-rr ./cmd/sr-rr &&
    go build -o $WORK/bin/sr-route ./cmd/sr-route &&
    go build -o $WORK/bin/sr-fib ./cmd/sr-fib &&
    go build -o $WORK/bin/fib-dump ./tools/fib-dump" || die "node1 编译失败"
# node2 也要 fib-dump：§4 用 has_prefix "$N2" 判反向前缀有没有装上，而它走的是
# $WORK/bin/fib-dump（上一轮只给 node2 编了 sr-fib，那条判定永远为空 ⇒ 假 FAIL）。
dx "$N2" "cd $MODULE && mkdir -p $WORK/bin &&
    go build -o $WORK/bin/sr-fib ./cmd/sr-fib &&
    go build -o $WORK/bin/fib-dump ./tools/fib-dump" || die "node2 编译失败"
# node2 那份 fib2.json 在 §1 的 heredoc 里**逐字另写**（下一跳是 node1 的 overlay
# 地址），这里只是原样拷进容器：颜色和逃生开关必须两端逐字一致（§5.2 拍的是租户级），
# 所以拷文件而不是就地生成。
dx "$N1" "cat $WORK/fib2.json" | docker exec -i "$N2" bash -lc "mkdir -p $WORK && cat > $WORK/fib2.json" ||
    die "node2 的 fib2.json 没拷过去"
docker cp "$N1:$WORK/start-fib.sh" /tmp/sr-escape-startfib.sh >/dev/null 2>&1 || die "取不到 start-fib.sh"
docker cp /tmp/sr-escape-startfib.sh "$N2:$WORK/start-fib.sh" >/dev/null 2>&1 || die "拷不过去 start-fib.sh"
dx "$N2" "chmod +x $WORK/start-fib.sh"
# 上一轮死在半路会留下这张表；只有它确实像本脚本的残留才清，否则是别人在用。
if vpp "$N1" "show ip fib table $VRF" | grep -q "^ipv4-VRF:$VRF"; then
    if vpp "$N1" "show mpls fib" | grep -q "^$LABEL:" || \
       dump_table "$N1" "$VRF" "$REMOTE_PFX" | grep -q .; then
        info "清理 $N1 上本脚本的残留（标签 $LABEL 或前缀 $REMOTE_PFX）"
        dx "$N1" "$WORK/bin/sr-fib -config $WORK/fib.json -reset" >/dev/null 2>&1 || true
    else
        die "$N1 的 VRF $VRF 已被别的进程占用（既无标签 $LABEL 也无前缀 $REMOTE_PFX）"
    fi
fi

dx "$N1" "bash $WORK/start.sh" || die "启动失败"
sleep 0.5

info "   推本端前缀（扮演 sr-frr 的 PUT /local-prefixes）"
push_local "$N1" "$N1_STATUS" "$LOCAL_PFX" "node1 宣告 $LOCAL_PFX@vrf$VRF" || die "node1 前缀推不进去"
push_local "$N2" "$N2_STATUS" "$REMOTE_PFX" "node2 宣告 $REMOTE_PFX@vrf$VRF" || die "node2 前缀推不进去"

if [ "$(status_hops)" != "$HOPS_WANT" ]; then
    i=0
    while [ "$i" -lt 125 ] && [ "$(status_hops)" != "$HOPS_WANT" ]; do sleep 0.2; i=$((i + 1)); done
fi
[ "$(status_hops)" = "$HOPS_WANT" ] &&
    ok "RIB：$REMOTE_PFX 解析出两条下一跳（$OVERLAY_N2 / $OVERLAY2_N2）" ||
    bad "RIB 下一跳不是两条（期望 '$HOPS_WANT'，得到 '$(status_hops)'）"

# ---------------------------------------------------------------- 3 BFD 会话

info "3. 建四条多跳 BFD 会话（每条隧道一对）：逃生开关的语义长在**计划**上，"
info "   而计划只认 BFD 给的可达性；没有会话 = 没有信息 = fail-open，测不出切换。"
bfd_add "$N1" "$OVERLAY_N1"  "$OVERLAY_N2"
bfd_add "$N1" "$OVERLAY2_N1" "$OVERLAY2_N2"
bfd_add "$N2" "$OVERLAY_N2"  "$OVERLAY_N1"
bfd_add "$N2" "$OVERLAY2_N2" "$OVERLAY2_N1"
bfd_wait_up "$OVERLAY_N1" "$OVERLAY_N2" && ok "颜色 0 那条（$OVERLAY_N1 ↔ $OVERLAY_N2）BFD Up" || bad "颜色 0 的 BFD 到不了 Up"
bfd_wait_up "$OVERLAY2_N1" "$OVERLAY2_N2" && ok "颜色 $COLOR2 那条（$OVERLAY2_N1 ↔ $OVERLAY2_N2）BFD Up" || bad "颜色 $COLOR2 的 BFD 到不了 Up"

info "   起两端 sr-fib（开关**关**）"
dx "$N1" "bash $WORK/start-fib.sh fib.json fib.log fib.pid" || die "node1 sr-fib 启动失败"
dx "$N2" "bash $WORK/start-fib.sh fib2.json fib.log fib.pid" || die "node2 sr-fib 启动失败"

# ---------------------------------------------------------------- 4 开关关：基线形态

info "4. 开关关（定案默认）：主表只收颜色 0，颜色 $COLOR2 只在颜色表 $CT2 里"
wait_fn "主表 $VRF 里出现 $REMOTE_PFX" 15 has_prefix "$N1" "$VRF" "$REMOTE_PFX" || die "路由没装上"
wait_fn "主表 $VRF 只有 1 条路径" 15 path_n_is "$N1" "$VRF" "$REMOTE_PFX" 1
has_path "$N1" "$VRF" "$REMOTE_PFX" "$OVERLAY_N2" 230 &&
    ok "主表里那条是颜色 0（$OVERLAY_N2，pref 230）" || bad "主表里颜色 0 那条不在或 pref 不对"
if dump_table "$N1" "$VRF" "$REMOTE_PFX" | grep -qF "via $OVERLAY2_N2 "; then
    bad "开关关着，$OVERLAY2_N2（颜色 $COLOR2）却进了主表"
else
    ok "负向：开关关 ⇒ 颜色 $COLOR2 不在主表（§4.3 第 5 步）"
fi
if has_prefix "$N1" "$LEGACY_COLOR0" "$REMOTE_PFX"; then
    bad "表 $LEGACY_COLOR0 里出现了 $REMOTE_PFX：+10000 段基址已退役，颜色 0 不该再产生副本表"
else
    ok "负向：表 $LEGACY_COLOR0 是空的 ⇒ 颜色 0 只在主表 $VRF 里有一份（不再双写）"
fi
wait_fn "颜色表 $CT2 有颜色 $COLOR2 那条 pref $((230 + COLOR2))" 15 has_path "$N1" "$CT2" "$REMOTE_PFX" "$OVERLAY2_N2" $((230 + COLOR2))
info "   两端 tap 终端，把这条路由真的用起来"
wait_fn "node2 的 VRF 里出现 $LOCAL_PFX" 20 has_prefix "$N2" "$VRF" "$LOCAL_PFX"
tap_create "$N1" "$TAP_N1" "$TAP_N1_GW" "$TAP_N1_HOST"
tap_create "$N2" "$TAP_N2" "$TAP_N2_GW" "$TAP_N2_HOST"
dx "$N1" "ip route replace $REMOTE_PFX via ${TAP_N1_GW%/*} dev $TAP_N1" || die "node1 内核路由失败"
dx "$N2" "ip route replace $LOCAL_PFX via ${TAP_N2_GW%/*} dev $TAP_N2" || die "node2 内核路由失败"
DST=${TAP_N2_HOST%/*}
wait_ping "$DST" "开关关、两色都在：终端 → 主表颜色 0 → 隧道 → 对端终端" 20

# ---------------------------------------------------------------- 5 开关关：颜色 0 真死

info "5. (1) 开关关：把 $WG_IF（颜色 0）真打死，BFD 判 Down ⇒ 主表这条前缀**不存在**"
info "   打流覆盖整个切换（口 down 之前起，之后继续跑到包数够）"
ping_start a-off-down 120
sleep 1
dx "$N1" "vppctl 'set interface state $WG_IF down' >/dev/null"
ABSENT=$(measure_ms 25 absent "$VRF" "$REMOTE_PFX")
case "$ABSENT" in
  TIMEOUT) bad "开关关：主表 $VRF 里 $REMOTE_PFX 25s 还没消失" ;;
  *) record "开关关.主表前缀消失" "${ABSENT}ms（从 $WG_IF down 起算，含 BFD 检测 + 一轮 poll）" ;;
esac
if has_prefix "$N1" "$CT2" "$REMOTE_PFX"; then
    ok "颜色表 $CT2 照写：开关只改主表，不改颜色表（policy 落点）"
else
    bad "颜色表 $CT2 里这条前缀没了：开关不该动颜色表"
fi
# 这里原来断言"颜色 0 那张颜色表也空了"（双写时代要两份各删一次）。表号公式去掉
# +10000 之后 $CT0 就是主表 $VRF，再判一次等于把上面的 ABSENT 重说一遍；换成钉
# 老落点：路径被剔出计划时，不该有一副本表躲在后面没撤。
if has_prefix "$N1" "$LEGACY_COLOR0" "$REMOTE_PFX"; then
    bad "表 $LEGACY_COLOR0 里还留着 $REMOTE_PFX：颜色 0 早就不该产生副本表"
else
    ok "负向：颜色 0 的路径只从主表 $VRF 撤走（表 $LEGACY_COLOR0 全程是空的）"
fi
wait_no_ping "$DST" "开关关：主表没这条前缀 ⇒ 打流断，不回落到昂贵颜色 $COLOR2（§5 语义）"
num "开关关的这一段 fib.log 尾部："
dx "$N1" "grep -E '收敛|警告' $WORK/fib.log | tail -3" | sed 's/^/       /'
ping_wait; ping_analyze a-off-down | tee /tmp/sr-escape-a-off.log | sed 's/^/       /'
ping_verdict /tmp/sr-escape-a-off.log "开关关.打流结果"

info "6. 恢复 $WG_IF：BFD 回 Up ⇒ 前缀自己回到主表（开关关的恢复侧）"
dx "$N1" "vppctl 'set interface state $WG_IF up' >/dev/null"
R1=$(measure_ms 25 present "$VRF" "$REMOTE_PFX")
case "$R1" in
  TIMEOUT) bad "开关关：恢复后主表 25s 还没把前缀加回来" ;;
  *) record "开关关.主表前缀回来" "${R1}ms" ;;
esac
wait_fn "回来的那条是颜色 0（pref 230）" 15 has_path "$N1" "$VRF" "$REMOTE_PFX" "$OVERLAY_N2" 230
wait_ping "$DST" "开关关：恢复后打流又通" 20

# ---------------------------------------------------------------- 7 开关开

info "7. (2) 开关**开**：同一份隧道表，只是 escape_all_colors=true"
dx "$N1" "sed -i 's/\"escape_all_colors\": false/\"escape_all_colors\": true/' $WORK/fib.json" >/dev/null
dx "$N2" "sed -i 's/\"escape_all_colors\": false/\"escape_all_colors\": true/' $WORK/fib2.json" >/dev/null
grep -o '"escape_all_colors": *[a-z]*' <(dx "$N1" "cat $WORK/fib.json") | head -1 | sed 's/^/       node1 /'
grep -o '"escape_all_colors": *[a-z]*' <(dx "$N2" "cat $WORK/fib2.json") | head -1 | sed 's/^/       node2 /'
kill_one "$N1" fib
kill_one "$N2" fib
# 新进程上一份计划是空的 ⇒ 走整条替换的 plain add（§2.2），不是先删后加。
dx "$N1" "bash $WORK/start-fib.sh fib.json fib-on.log fib.pid" || die "node1 sr-fib（开）启动失败"
dx "$N2" "bash $WORK/start-fib.sh fib2.json fib-on.log fib.pid" || die "node2 sr-fib（开）启动失败"
wait_fn "主表 $VRF 里有 2 条路径" 20 path_n_is "$N1" "$VRF" "$REMOTE_PFX" 2
has_path "$N1" "$VRF" "$REMOTE_PFX" "$OVERLAY_N2" 230 &&
    has_path "$N1" "$VRF" "$REMOTE_PFX" "$OVERLAY2_N2" $((230 + COLOR2)) &&
    ok "开关开：主表同时收颜色 0（230）与颜色 $COLOR2（$((230 + COLOR2))），VPP 按 pref 排" ||
    bad "开关开：主表两条路径的 pref 不对"
# 这里**不**断言 buckets:2 —— 实测（node1，本轮）：preference 不同的两条路径 VPP
# 放在同一个 dpo 里（buckets:1，靠的那条是主、另一条是备），只有同 pref 才并出
# 两个 bucket。所以开关开这个形态的判据是 fib-dump 读回的两条 pref（上面那条），
# buckets 数只在 §10 同色对照里才有区分力。
info "   VPP 侧 buckets：$(buckets_of "$N1" "$VRF" "$REMOTE_PFX")（不同 pref ⇒ 同一 dpo 的主/备，不是 2 个 bucket）"
wait_fn "颜色表 $CT2 仍然只有颜色 $COLOR2（开关不撤颜色表）" 15 has_path "$N1" "$CT2" "$REMOTE_PFX" "$OVERLAY2_N2" $((230 + COLOR2))
wait_ping "$DST" "开关开：基线打流仍通（此时走颜色 0）" 20

# ---------------------------------------------------------------- 8 开关开：颜色 0 真死

info "8. (2) 开关开：把 $WG_IF（颜色 0）真打死 —— 主表这条前缀必须**一直在**，只把路径换成颜色 $COLOR2"
ping_start b-on-down 120
sleep 1
dx "$N1" "vppctl 'set interface state $WG_IF down' >/dev/null"
ONE=$(measure_ms 25 one_path "$VRF" "$REMOTE_PFX" "$OVERLAY2_N2" "$((230 + COLOR2))")
case "$ONE" in
  TIMEOUT) bad "开关开：主表 25s 还没收成只有颜色 $COLOR2 那一条" ;;
  *) record "开关开.主表换成最小存活颜色" "${ONE}ms（从 $WG_IF down 起算；对照开关关的 ${ABSENT}ms）" ;;
esac
if has_prefix "$N1" "$VRF" "$REMOTE_PFX"; then
    ok "开关开：主表这条前缀**没消失过**（轮询到形态变化时它一直在）"
else
    bad "开关开：主表这条前缀没了 —— 逃生开关的整个意义就是别让它没"
fi
wait_ping "$DST" "开关开：颜色 0 死后打流**仍然通**（走颜色 $COLOR2，不回黑洞）" 20
# VPP 原生那一下（bucket 自己从 2 变 1）先于控制面：这是 §5.2 "主体是 VPP 原生
# preference，不是我们的控制面循环" 的直接证据。
info "   VPP 侧 buckets：$(buckets_of "$N1" "$VRF" "$REMOTE_PFX")（开关开时前后都是 1，见 §7 的说明；这一段"只剩活着的颜色"由上面 one_path 轮询和下面的打流证明）"
num "开关开的这一段打流："
ping_wait; ping_analyze b-on-down | tee /tmp/sr-escape-b-on.log | sed 's/^/       /'
ping_verdict /tmp/sr-escape-b-on.log "开关开.打流结果"

info "9. (2) 恢复：$WG_IF 回来 ⇒ 主表回到 2 条路径，而且流量真的回到颜色 0"
M0=$(mpls_count "$N1" "$WG_IF"); M1=$(mpls_count "$N1" "$WG2_IF")
dx "$N1" "vppctl 'set interface state $WG_IF up' >/dev/null"
TWO=$(measure_ms 25 two_path "$VRF" "$REMOTE_PFX")
case "$TWO" in
  TIMEOUT) bad "开关开：恢复后主表 25s 没回到 2 条路径" ;;
  *) record "开关开.回到颜色 0（形态）" "${TWO}ms" ;;
esac
has_path "$N1" "$VRF" "$REMOTE_PFX" "$OVERLAY_N2" 230 &&
    ok "颜色 0 那条带 pref 230 回来了：VPP 会自己把流量抬回去（§2.1）" ||
    bad "颜色 0 那条没回来"
wait_ping "$DST" "恢复后打流通" 20
sleep 1
dx "$N1" "ping -c 10 -W1 -i0.2 $DST" >/dev/null 2>&1
N0=$(mpls_count "$N1" "$WG_IF"); N1C=$(mpls_count "$N1" "$WG2_IF")
d0=$((N0 - M0)); d1=$((N1C - M1))
info "   10 个包的 MPLS 计数增量：$WG_IF(颜色 0)=+$d0，$WG2_IF(颜色 $COLOR2)=+$d1"
[ "$d0" -gt "$d1" ] && [ "$d0" -ge 10 ] &&
    ok "流量真的回到颜色 0（$WG_IF 涨 $d0，$WG2_IF 涨 $d1）—— 不是只看着像" ||
    bad "恢复后流量没回颜色 0（$WG_IF 涨 $d0，$WG2_IF 涨 $d1）"

# ---------------------------------------------------------------- 10 改色窗口

info "10. (3) 改色窗口：回到定案默认（开关**关**）、两条都颜色 0（主表 buckets:2 同 pref），"
info "    把 $WG2_IF 改成颜色 $COLOR2 走重启 -once，打流量窗口。"
info "    为什么这一段要在开关关上量：生产默认就是关（§5.2），改色的形态是**搬表**；"
info "    开着量的话主表两条路径身份不变、只是 pref 变，量的就不是同一种代价了。"
# 两条隧道的颜色都拍回 0，同时把开关拍回 false：整份配置用 python 重写，别用 sed
# 挑行 —— 隧道行长得一样，sed 一漂就把两条一起改，测出来的就不是"改色"了。
dx "$N1" "python3 - <<PY
import json
p='$WORK/fib.json'
d=json.load(open(p))
d['escape_all_colors']=False
d['tunnels'][0]['color']=0
d['tunnels'][1]['color']=0
json.dump(d,open(p,'w'),indent=2)
PY" >/dev/null || die "重写 fib.json（两条都颜色 0、开关关）失败"
dx "$N2" "sed -i 's/\"escape_all_colors\": true/\"escape_all_colors\": false/' $WORK/fib2.json" >/dev/null
kill_one "$N1" fib
kill_one "$N2" fib
# node2 也要跟着回到开关关：两端逃生开关是租户级，测试中途留一开一关，
# 反向路径的形态就和正向不是一回事了。这里重启是在基线阶段（还没开始打流）。
dx "$N2" "bash $WORK/start-fib.sh fib2.json fib-off2.log fib.pid" || die "node2 sr-fib（开关关）重启失败"
dx "$N1" "$WORK/bin/sr-fib -config $WORK/fib.json -once > $WORK/both0.log 2>&1" || bad "两条颜色 0 的 sr-fib -once 退出非 0"
wait_fn "主表 $VRF 回到 2 条路径" 20 path_n_is "$N1" "$VRF" "$REMOTE_PFX" 2
dump_table "$N1" "$VRF" "$REMOTE_PFX" | sed 's/^/       /'
info "   同色 0 时 VPP buckets：$(buckets_of "$N1" "$VRF" "$REMOTE_PFX")（同 pref 才并 bucket —— 与 §7 的对照）"
wait_ping "$DST" "两条同色 0：打流通" 20

ping_start c-recolor 120
sleep 1
dx "$N1" "python3 - <<PY
import json
p='$WORK/fib.json'
d=json.load(open(p))
d['tunnels'][1]['color']=$COLOR2
json.dump(d,open(p,'w'),indent=2)
PY" >/dev/null || die "改色写文件失败"
dx "$N1" "$WORK/bin/sr-fib -config $WORK/fib.json -once > $WORK/recolor.log 2>&1" || bad "改色的 sr-fib -once 退出非 0"
wait_fn "改色后主表只剩颜色 0 那条（搬表）" 20 path_n_is "$N1" "$VRF" "$REMOTE_PFX" 1
has_path "$N1" "$VRF" "$REMOTE_PFX" "$OVERLAY_N2" 230 &&
    ok "主表里留下的正是颜色 0（$OVERLAY_N2，pref 230）" || bad "主表留下的不是颜色 0 那条"
wait_fn "改色的那条搬进颜色表 $CT2（pref $((230 + COLOR2))）" 15 has_path "$N1" "$CT2" "$REMOTE_PFX" "$OVERLAY2_N2" $((230 + COLOR2))
num "改色这一段打流："
ping_wait; ping_analyze c-recolor | tee /tmp/sr-escape-c-recolor.log | sed 's/^/       /'
ping_verdict /tmp/sr-escape-c-recolor.log "改色(开关关,重启 plain add).打流结果"
info "   如实标注：上面这个窗口量的是**新进程 plain add（整条替换）**的代价。"
info "   Applier 的先删后加分支（§2.2）要求同一进程里'身份相同、只有 pref 变'，"
info "   而颜色只在进程启动时读一次 ⇒ 今天的配置流线走不到它，只有单测钉住。"
wait_ping "$DST" "改色之后仍可转发（主表还剩颜色 0 那条）" 20

# ---------------------------------------------------------------- 11 汇总

echo
info "计时汇总（写进 §8/§9/§11 的数）："
sed 's/^/       /' "$RESULTS"
echo
echo "== 结果：$PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
    for f in fib-on both0 recolor node1 node2 rr; do
        echo "---- $f.log" >&2
        dx "$N1" "tail -25 $WORK/$f.log" >&2 2>/dev/null || true
    done
    echo "---- node2 fib-on.log" >&2
    dx "$N2" "tail -25 $WORK/fib-on.log" >&2 2>/dev/null || true
    exit 1
fi
