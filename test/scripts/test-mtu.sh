#!/bin/bash
# test-mtu.sh - MTU 的三个数到底各管什么：underlay 记录、隧道记录、超预算时的分片
#
# 这条脚本要钉住的是"隧道 MTU 是不是一个真起作用的数"。这个问题本轮答过一次错的
# 答案（"形同虚设"），错在量了**手建 WG 接口的出生状态**：四个 MTU 槽全 0，等于
# 没有记录，当然无从执行。写对记录之后重测，结论完全反过来 —— VPP 会执行它，
# 只是执行的动作是**按它切内层分片**，而不是拒发。三件事各有断言：
#
#   (1) underlay 记录必须等于内核网卡的真实值。VPP 给 af-packet 的 VIF 默认记
#       L3 9000，容器网卡只有 1500；这个差不表现为"大包被拒"，而是超长外层帧
#       打到 sendto() 上静默失败、这张口的发送侧从此卡死（2026-10-18 带宽基线
#       卡死一次，本轮测量又在 node2 复现一次）。这是三条里最硬的一条。
#   (2) 隧道记录是**会被执行**的那一个：超长的内层在 WG 出口被切成两片，每片
#       自己封一份 WG+SR 头，于是每个外层都不撑破 underlay。
#   (3) 收端在包进 sr-wg4-input **之前**就把分片拼回去了，所以插件永远只看到
#       完整的 SR 头 —— 这条是安全属性，用 Decap_hmac_fail 增 0 来验。
#   (4) 上面三条走的都是"可分片"路径。内层**带 DF 位**时是另一套语义：撞上记录
#       不分片，丢弃并回 ICMP type 3/code 4，让发端自己降 PMTU。这一条 2026-10-24
#       才第一次真测到（VPP 的 ping CLI 没有 DF 开关，得用 tap 接一台内核终端来造包）。
#
# 前置：**必须先跑过 test-e2e.sh --keep**（wg0@51820 单隧道已通、三个容器在跑）。
# 本脚本只发探测包、只在 ASSERT F 里临时改一次 node1 的 wg0 记录并立刻改回来；
# ASSERT I 会临时建一张 tap9 当"终端"再删掉，ASSERT J 会临时在两端各建一张
# tap（tap10/tap11）和两条 192.168.50/60 的 VPP 路由，跑完删掉。
#
# 用法:
#   test/scripts/test-mtu.sh [--keep]
#
# --keep 只多加一轮小包 ping（ASSERT H 里），不改变任何实验：ASSERT F 那个
# "把隧道 MTU 记成 0"的反证两种运行方式都会做，也都会立刻改回来 —— 它动的是
# 实验室里唯一的一条隧道，没有"保留坏状态"这一说。
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
SETUP_C="/workspace/test/scripts/setup-node.sh"

C_NODE1=srwg-node1
C_TRANSIT=srwg-transit
C_NODE2=srwg-node2
ALL_CONTAINERS="${C_NODE1} ${C_TRANSIT} ${C_NODE2}"

N1_UNDERLAY=${N1_UNDERLAY:-10.10.1.2}
N2_UNDERLAY=${N2_UNDERLAY:-10.10.2.2}
VPN_PORT=${VPN_PORT:-51820}
OVERLAY_N1=${OVERLAY_N1:-192.168.99.1}
OVERLAY_N2=${OVERLAY_N2:-192.168.99.2}
PATH1=${PATH1:-1}
IF1=${IF1:-wg0}

# 期望的隧道 MTU：与 setup-node.sh 的 TUNNEL_MTU 默认值、契约里的
# wire.DefaultTunnelMTU 同一个数。断言 B 之后一律用**读回来的**值算，不用这个期望值，
# 免得改了环境变量之后脚本自己骗自己。
EXPECT_TUNNEL_MTU=${EXPECT_TUNNEL_MTU:-1360}

TRACE_DIR=${TRACE_DIR:-/tmp/srwg-mtu}

KEEP=0
for a in "$@"; do
    case "$a" in
      --keep)    KEEP=1 ;;
      -h|--help) awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 0 ;;
      *)         echo "unknown option: $a" >&2; exit 2 ;;
    esac
done

PASS=0
FAIL=0
ok()   { PASS=$((PASS+1)); echo "  [PASS] $*"; }
bad()  { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "== $*"; }
die()  { echo "FATAL: $*" >&2; exit 1; }

dx() { local c=$1; shift; docker exec "${c}" bash -lc "$*"; }
vpp() { dx "$1" "vppctl $2" 2>&1 | tr -d '\r'; }

swidx_of() {   # <container> <iface> -> sw_if_index（空=接口不存在）
    vpp "$1" "show interface $2" | awk -v n="$2" '$1==n {print $2; exit}'
}

# 这台机器上 VPP 实际持有的 underlay VIF 名（可能是 eth0 也可能是 eth1：本轮
# e2e --keep 之后 node2 的就是 host-eth1，脚本一旦写死 host-eth0 就会读空）。
vifs_of() { vpp "$1" "show interface" | awk '$1 ~ /^host-/ {print $1}'; }

# underlay **出口**到底是哪一张：取插件承认的那个，不是"第一张 host-*"。
# `show sr-wg tunnels` 头部列出了 encap 启用的接口，而 encap 只挂在真正的出口网卡上
# 才会改写包 —— 所以这个号就是预算该用哪张口的 MTU 的权威答案。
encap_if_of() {   # <container> -> 如 host-eth0（空=没启用 encap）
    vpp "$1" "show sr-wg tunnels" | awk '/encap enabled on/{getline; print $1; exit}'
}

# MTU 列（形如 1500/0/0/0）取第一格 = L3。只在接口自己的那一行上取：
# `show interface` 表头也含 "MTU (L3/IP4/IP6/MPLS)"，但它不带接口名。
l3_of() {   # <container> <iface>
    vpp "$1" "show interface" | awk -v n="$2" '$1==n {split($4,a,"/"); print a[1]; exit}'
}

tx_of() {   # <container> <iface> -> tx packets 计数（该接口块里第一条 tx packets）
    vpp "$1" "show interface" | awk -v n="$2" '
        $1==n {f=1; next}
        f && $1=="tx" && $2=="packets" {print $3; exit}
        f && NF>=4 && $2 ~ /^[0-9]+$/ && $1 != "rx" {f=0}   # 走到下一个接口了
    '
}

# 内核侧真实 MTU。VPP 的 af-packet VIF 名是 host-<内核名>，去掉这个前缀去问内核。
kernel_mtu_of() {   # <vif 名，如 host-eth0> <container>
    local k=${1#host-}
    [ -n "${k}" ] || return 1
    dx "$2" "cat /sys/class/net/${k}/mtu 2>/dev/null" | tr -d ' \r'
}

# 计数器：setup-node.sh 的 stats 子命令输出 KEY=VALUE 绝对值。
role_of() {
    case "$1" in
      ${C_NODE1}) echo node1 ;; ${C_TRANSIT}) echo transit ;; ${C_NODE2}) echo node2 ;;
    esac
}
stat_of() {   # <container> <Section>_<name> -> 计数
    dx "$1" "bash ${SETUP_C} $(role_of "$1") stats" 2>/dev/null |
        awk -F= -v k="$2" '$1==k {print $2; f=1} END{if(!f) print 0}'
}

# VPP 自带的错误计数：<container> <reason 子串> -> 累计值（没有这一行算 0）。
# `show errors` 的行是 "<count> <node> <reason> <severity>"，reason 里带空格，
# 所以按子串匹配整行，不按列取。
err_of() {
    vpp "$1" "show errors" |
        awk -v p="$2" 'index($0,p){print $1; f=1} END{if(!f) print 0}' | head -1
}

# ping 通 = 任一轮拿到回包（冷启动握手要跑完一个 keepalive 周期）。
ping_ok() {   # <container> <dst> <iface> [size N] -> 0=通
    local c=$1 dst=$2 ifn=$3 size=${4:-} out i
    local extra=""; [ -n "${size}" ] && extra="size ${size}"
    for i in 1 2 3; do
        out=$(dx "${c}" "vppctl ping ${dst} source ${ifn} repeat 8 ${extra}" 2>&1)
        echo "${out}" | grep -qE "[0-9]+ sent, [1-9][0-9]* received" && return 0
        sleep 4
    done
    echo "${out}" | tail -2 | sed 's/^/      /' >&2
    return 1
}

# VPP 的 ping `size N` 里，N 是 ICMP 数据长度：内层 IP 总长 = N + 36。
# 这个 +36 是量出来的（size 1324 -> 内层 1360、size 1400 -> 内层 1436），不是推的；
# 断言 C 会拿 trace 里的 `Length:` 反查它，关系一旦变了就大声失败，不会静默放行。
size_for_inner() {
    local s=$(($1 - 36))
    [ "${s}" -ge 1 ] || die "内层目标 ${1} 太小，凑不出 ping size"
    echo "${s}"
}

# 外层总长 = 内层 + WG 封装 + SR 头 + 外层自己的 IP+UDP。
# WG=32、SR=24+6*段数、外层=28，三个数都来自契约包 wire.*（各自都标了实测出处），
# 这里按同一套算式重算一遍：脚本与代码不一致时，断言 C 的 trace 会当场打脸。
outer_at() {   # <内层长度> -> 期望外层 IP 总长
    echo $(($1 + 32 + 24 + 6 * SEGS + 28))
}

dump_all() {
    for c in ${ALL_CONTAINERS}; do
        echo "################ ${c} ################"
        vpp "${c}" "show interface" | grep -vE "^ *$"
        vpp "${c}" "show sr-wg paths"
        vpp "${c}" "show sr-wg stats"
        vpp "${c}" "show errors" | head -30
    done
}

# ==================================================================
info "step 0: 前置检查（需要 test-e2e.sh --keep 的环境）"
# ==================================================================
for c in ${ALL_CONTAINERS}; do
    docker inspect -f '{{.State.Running}}' "${c}" 2>/dev/null | grep -q true ||
        die "容器 ${c} 没在跑，请先 bash test/scripts/test-e2e.sh --keep"
    vpp "${c}" "show version" | grep -q vpp || die "${c}: vppctl 不通（VPP 没起来）"
done
SW0=$(swidx_of "${C_NODE1}" "${IF1}")
[ -n "${SW0}" ] || die "node1 上没有 ${IF1}：先跑 e2e"

MTU_LIVE=$(l3_of "${C_NODE1}" "${IF1}")
[ -n "${MTU_LIVE}" ] || die "读不到 ${IF1} 的 MTU 列"
# 后面所有算式都以**接口上真正记着的**这个数为准：它是断言 B 的对象，也是分片判据。
if [ "${MTU_LIVE}" != "${EXPECT_TUNNEL_MTU}" ]; then
    info "注意：${IF1} 实际记着 ${MTU_LIVE}，与期望 ${EXPECT_TUNNEL_MTU} 不同（断言 B 会失败，其余按实际值算）"
fi

# 表里只有中转跳（relay_count），实际段数 = relay_count + 1：终点段由 encap 从
# 报文外层 dst 现取、不进表，但它那一格 MPLS 标签照样占 6 字节。
RELAYS=$(vpp "${C_NODE1}" "show sr-wg paths" |
    awk -v p="${PATH1}" '$0 ~ ("path_id=" p " ") {match($0, /relay_count=[0-9]+/); print substr($0, RSTART+12, RLENGTH-12); exit}')
[ -n "${RELAYS}" ] || die "读不到 path ${PATH1} 的 relay_count"
SEGS=$((RELAYS + 1))
UNDERLAY_IF=$(encap_if_of "${C_NODE1}")
[ -n "${UNDERLAY_IF}" ] || die "node1 上没查到 encap 启用的接口：e2e 的 SR 三件套没配全"
UNDERLAY_LIVE=$(l3_of "${C_NODE1}" "${UNDERLAY_IF}")
[ -n "${UNDERLAY_LIVE}" ] || die "读不到 ${UNDERLAY_IF} 的 MTU 列"

mkdir -p "${TRACE_DIR}"
ping_ok "${C_NODE1}" "${OVERLAY_N2}" "${IF1}" || die "隧道 1 不通，MTU 测量没有基座"
info "环境就绪：${IF1} 记 ${MTU_LIVE}、underlay ${UNDERLAY_IF} 记 ${UNDERLAY_LIVE}、path ${PATH1} 有 ${SEGS} 段"

# ==================================================================
info "ASSERT A: 每台节点上每个 host-* VIF 的 MTU == 内核网卡的真实值"
# ==================================================================
# 这是三条里唯一"写错就不通"的一条：VPP 记 9000 而网卡 1500 时，它认为不需要分片，
# 于是把超长外层帧直接交给 sendto()，内核失败但不回报 VPP —— 这张口的发送侧从此
# 只收不发，而且计数器不动，是最难查的那种坏。setup-node.sh 现在从
# /sys/class/net/<if>/mtu 同步；这里逐台复核。
for c in ${ALL_CONTAINERS}; do
    VIFS=$(vifs_of "${c}")
    [ -n "${VIFS}" ] || { bad "$(role_of "${c}") 上一个 host-* VIF 都没有"; continue; }
    for v in ${VIFS}; do
        want=$(kernel_mtu_of "${v}" "${c}")
        got=$(l3_of "${c}" "${v}")
        if [ -z "${want}" ]; then
            bad "$(role_of "${c}") ${v}: 内核读不到 ${v#host-} 的 mtu（比对口径没了）"
        elif [ "${want}" = "${got}" ]; then
            ok "$(role_of "${c}") ${v}: VPP ${got} == 内核 ${want}"
        else
            bad "$(role_of "${c}") ${v}: VPP 记 ${got}、内核是 ${want} —— 超长外层帧会在这里静默卡死"
        fi
    done
done

# ==================================================================
info "ASSERT B: 隧道接口的 MTU 记录 == 契约默认值（${EXPECT_TUNNEL_MTU}）"
# ==================================================================
# 手建的 WG 接口出生时四个槽全 0（= 没有 MTU 记录，也就什么都不限制），这正是
# agent 必须写一个非 0 值的理由：断言 C~F 的整套预算，只有在"口上记着数"时才成立。
for c in ${C_NODE1} ${C_NODE2}; do
    got=$(l3_of "${c}" "${IF1}")
    if [ "${got}" = "${EXPECT_TUNNEL_MTU}" ]; then
        ok "$(role_of "${c}") ${IF1} 记 ${got}"
    else
        bad "$(role_of "${c}") ${IF1} 记 '${got}'，期望 ${EXPECT_TUNNEL_MTU}（0 就意味着这条隧道没人管分片）"
    fi
done

# ==================================================================
info "ASSERT C: 预算内一个整包 —— 外层长度 = 内层 + WG32 + SR($((24 + 6 * SEGS))) + 28，且一片不切"
# ==================================================================
# 这一组是**校准**：如果 ping size -> 内层长度 的 +36 关系变了，或者 wire.* 里某个
# 每包开销数变了，这里会第一个发现（trace 里的数与算式对不上）。
f_run() {   # <容器> <期望内层> <标签> <trace 文件> <repeat 次数>
    # 分两行赋值：`local x=1 y=$(f $x)` 里的 $x 展开发生在 local 生效**之前**，
    # 拿到的是外层的同名变量（set -u 下直接 unbound），这条踩过一次。
    local c=$1 inner=$2 tag=$3 tf=$4 rep=$5
    local size=$(size_for_inner "${inner}")
    vpp "${c}" "clear trace" >/dev/null
    vpp "${c}" "trace add af-packet-input 200" >/dev/null
    dx "${C_NODE1}" "vppctl ping ${OVERLAY_N2} source ${IF1} repeat ${rep} size ${size} >/dev/null 2>&1"
    sleep 1
    vpp "${c}" "show trace max 600" > "${tf}" 2>&1
    vpp "${c}" "clear trace" >/dev/null
    echo "      ${tag}: ping size ${size} x${rep} -> 期望内层 ${inner}"
    # 空 trace 会让后面所有"没出现分片/没出现第二片"的判据**空洞地成立**，
    # 那是最坏的一种绿（本轮第一轮就撞过：helper 报错导致 trace 全空，8 个断言
    # 里有 3 个照样 PASS）。所以这里先把"确实收到了包"钉死。
    if ! grep -q "Wireguard input:" "${tf}"; then
        bad "${tag} 的 trace 里一个 WG 包都没有（${tf}）—— 这轮判据全部作废，先查 ping 是否送达"
        return 1
    fi
    return 0
}

# WG 解封之后每个包自己在 trace 里报长度。只取 `Wireguard input:` 块里那一行
# `Length: N`，输出一行一个数 —— 上层用 sum/count/max 算分片不变量，不能先去重。
wg_lengths() {   # <trace 文件> -> 每行一个内层长度（只算带载荷的数据包）
    # "Wireguard input:" 块里混着三种包：真数据、keepalive（Type: Data, Length: 0）、
    # 握手（Type: Handshake ..., Length: 148）。后两种跟这段的被测载荷无关，混进来会
    # 让"片数 == REP"这种等式随机失败（2026-10-24 一次 ASSERT F 就这么假红过）。
    awk '/Wireguard input:/{f=1; typ=""; next}
         f && /Type:/ {typ=$0; next}
         f && /Length:/ {gsub(/[^0-9]/,"",$2); if (typ ~ /Data/ && $2+0 > 0) print $2; f=0}' "$1"
}
len_stats() {   # <长度列表(每行一个)> -> "<个数> <总和> <最大值>"
    awk '{n++; s+=$1; if ($1+0>m) m=$1+0} END {print n+0, s+0, m+0}'
}

# 外层 IP 头的分片标志行数：只有紧跟在 `<n1> -> <n2>` 之后的那个 fragment id 行算外层。
outer_frag_lines() {   # <trace 文件> -> 带 flags 的外层分片行数
    awk -v a="${N1_UNDERLAY}" -v b="${N2_UNDERLAY}" '
        index($0, "UDP: " a " -> " b) {o=1; next}
        o && /fragment id/ { if ($0 ~ /flags/) c++; o=0 }
        END {print c+0}' "$1"
}

# 外层 IP 总长的最大值。同一段头里 `tos ..., length N,` 只出现在外层与内层两处，
# 而内层是 ICMP 头，所以这里只数 `<n1> -> <n2>` 之后紧跟的那一行。
outer_max_len() {   # <trace 文件>
    awk -v a="${N1_UNDERLAY}" -v b="${N2_UNDERLAY}" '
        index($0, "UDP: " a " -> " b) {o=1; next}
        o && match($0, /length [0-9]+/) {
            s=substr($0, RSTART+7, RLENGTH-7)+0; if (s>m) m=s; o=0
        }
        END {print m+0}' "$1"
}

T_EXACT=${MTU_LIVE}                     # 预算内：内层正好等于隧道 MTU
f_run "${C_NODE2}" "${T_EXACT}" "整包" "${TRACE_DIR}/c-exact.txt" 2
LENS=$(wg_lengths "${TRACE_DIR}/c-exact.txt")
NF=$(outer_frag_lines "${TRACE_DIR}/c-exact.txt")
OMAX=$(outer_max_len "${TRACE_DIR}/c-exact.txt")
read -r NN NSUM NMAX <<<"$(printf '%s\n' "${LENS}" | len_stats)"
WANT_OUTER=$(outer_at "${T_EXACT}")
echo "      内层长度：${NN} 个包、最大 ${NMAX}、全部取值 $(echo "${LENS}" | tr '\n' ' ')"
echo "      外层最大 IP 总长: ${OMAX}（算式期望 ${WANT_OUTER}）、外层分片行 ${NF}"
[ "${NN}" -ge 1 ] && [ "${NMAX}" = "${T_EXACT}" ] &&
    [ "$(printf '%s\n' "${LENS}" | sort -u | wc -l | tr -d ' ')" = "1" ] &&
    ok "每个内层都正好 ${T_EXACT}（ping size $(size_for_inner "${T_EXACT}") + 36 的关系成立）" ||
    bad "内层长度不是清一色 ${T_EXACT}：取值 $(echo "${LENS}" | tr '\n' ' ')（size->内层 的关系变了，后面的算式都不可信）"
[ "${OMAX}" = "${WANT_OUTER}" ] &&
    ok "外层 ${OMAX} == 内层 ${T_EXACT} + 32 + SR($((24 + 6 * SEGS))) + 28：契约里每包开销的算式与真机一致" ||
    bad "外层是 ${OMAX}、算式说 ${WANT_OUTER}：开销数与真机不符（查 wire.go 的 WGDataOverheadBytes / SRFixedBytes / SRSegmentBytes）"
[ "${OMAX}" -le "${UNDERLAY_LIVE}" ] &&
    ok "外层 ${OMAX} <= underlay ${UNDERLAY_LIVE}：预算内的包根本不需要分片" ||
    bad "预算内的包已经把外层撑到 ${OMAX} > ${UNDERLAY_LIVE}：预算算错了"
[ "${NF}" = "0" ] && ok "外层零分片标志" || bad "预算内的包出现了 ${NF} 个外层分片"

# ==================================================================
info "ASSERT D: 预算算术 —— 隧道 MTU 必须 <= TunnelMTUBudget(underlay, 段数)"
# ==================================================================
# 与 wire.TunnelMTUBudget 同一个式子：budget = underlay - 28 - 32 - (24+6*seg)。
# 这里只验实验室这一档；契约包里有整张表（含 0 值与最坏 8 段）。
BUDGET=$((UNDERLAY_LIVE - 28 - 32 - (24 + 6 * SEGS)))
echo "      underlay ${UNDERLAY_LIVE} / ${SEGS} 段 -> 预算 ${BUDGET}，隧道记 ${MTU_LIVE}"
[ "${MTU_LIVE}" -le "${BUDGET}" ] &&
    ok "隧道 MTU ${MTU_LIVE} 在 ${SEGS} 段预算 ${BUDGET} 之内" ||
    bad "隧道 MTU ${MTU_LIVE} 超了 ${SEGS} 段预算 ${BUDGET}：每个分片都要再背一份头，外层会二次分片"
# 默认值必须扛得住本实验室最坏的路径（8 段），否则它就不是一个"可以不问用户"的默认。
WORST=$((UNDERLAY_LIVE - 28 - 32 - (24 + 6 * 8)))
[ "${EXPECT_TUNNEL_MTU}" -le "${WORST}" ] &&
    ok "默认 ${EXPECT_TUNNEL_MTU} <= 最坏 8 段预算 ${WORST}：路径被控制器加长也不会破" ||
    bad "默认 ${EXPECT_TUNNEL_MTU} 在 ${UNDERLAY_LIVE} 的 underlay 上撑不过 8 段（预算 ${WORST}）"

# ==================================================================
info "ASSERT E: 超预算的内层被**按隧道 MTU 切片**，而且外层一片不超"
# ==================================================================
# 这是本轮纠正错判的那条测量。早先记的"wg0 记 1360 照样放过 1436"量的其实是
# MTU 0/0/0/0 的出生状态；写对记录之后重测，VPP 在 WG 出口把内层切成两片
# （实测 1356 + 100），每片自己封一份 WG+SR 头，于是外层 1452 与 196 都 <= 1500。
# 也就是说：**隧道 MTU 是生效的，生效动作是分片，不是拒发。**
T_OVER=$((MTU_LIVE + 76))               # 1436：比 MTU_LIVE 多出一小截，正好落在"必须切"的那一侧
REP=2
f_run "${C_NODE2}" "${T_OVER}" "超预算" "${TRACE_DIR}/e-over.txt" "${REP}"
LENS=$(wg_lengths "${TRACE_DIR}/e-over.txt")
NF=$(outer_frag_lines "${TRACE_DIR}/e-over.txt")
OMAX=$(outer_max_len "${TRACE_DIR}/e-over.txt")
read -r NN NSUM NMAX <<<"$(printf '%s\n' "${LENS}" | len_stats)"
echo "      内层长度：${NN} 个 WG 包（ping ${REP} 轮）、最大 ${NMAX}、总和 ${NSUM}"
echo "      外层最大 IP 总长: ${OMAX}、外层分片行 ${NF}"
ping_ok "${C_NODE1}" "${OVERLAY_N2}" "${IF1}" "${T_OVER}" &&
    ok "超预算的内层 ${T_OVER} 仍然送达（VPP 不拒发）" ||
    bad "超预算 ${T_OVER} 没送达：分片把隧道打断了（不是'静默旁路'就是被丢）"
# 每个分片都自带一份完整的 20 B IP 头，而原包只有一份 —— 所以
#   总和 == 轮数 * 原长 + 20 * (片数 - 轮数)
# 这个不变量不依赖 VPP 的取整规则（它把首片切到 1356 而不是 1360），比硬记 1356/100 稳。
[ "${NSUM}" = "$((REP * T_OVER + 20 * (NN - REP)))" ] &&
    ok "分片长度自洽：${NSUM} == ${REP}*${T_OVER} + 20*($NN-${REP})，一片不多一片不少" ||
    bad "分片长度不自洽：${NSUM} 对不上 ${REP}*${T_OVER} + 20*($NN-${REP}) = $((REP * T_OVER + 20 * (NN - REP)))（有片丢失或重复计数）"
[ "${NMAX}" -le "${MTU_LIVE}" ] &&
    ok "没有一片内层超过隧道 MTU ${MTU_LIVE}（最大 ${NMAX}）：这条记录真的在执行" ||
    bad "有一片内层 ${NMAX} > 隧道 MTU ${MTU_LIVE}：分片没按记录来，回到'形同虚设'那个结论"
[ "${NN}" -gt "${REP}" ] &&
    ok "${REP} 个 ${T_OVER} 包变成了 ${NN} 个 WG 包：内层确实被切了" ||
    bad "${REP} 个包还是 ${NN} 个 WG 包：超预算却没切片，隧道 MTU 没被执行"
[ "${NF}" = "0" ] &&
    ok "外层一片都没分（分片行 0、最大 ${OMAX} <= ${UNDERLAY_LIVE}）：内层先切，就轮不到外层" ||
    bad "外层出现了 ${NF} 个分片、最大 ${OMAX}：内层切分没兜住，分片被推到 underlay 上"

# 收端重组发生在本插件之前：内层两片各自有完整 SR 头，拼回来的是内层。
grep -q "ip4-local-full-reassembly" "${TRACE_DIR}/e-over.txt" &&
    ok "收端用 ip4-local-full-reassembly 把内层两片拼回原包" ||
    bad "trace 里没有重组节点：内层分片没被拼回来，那 ping 是怎么通的？"
# 中转侧也顺手钉一枚：隧道 1 的路径经过 transit，中转看到的是自己那个端口。
vpp "${C_TRANSIT}" "clear trace" >/dev/null
vpp "${C_TRANSIT}" "trace add af-packet-input 200" >/dev/null
dx "${C_NODE1}" "vppctl ping ${OVERLAY_N2} source ${IF1} repeat 2 size $(size_for_inner "${T_OVER}") >/dev/null 2>&1"
sleep 1
vpp "${C_TRANSIT}" "show trace max 400" > "${TRACE_DIR}/e-transit.txt" 2>&1
vpp "${C_TRANSIT}" "clear trace" >/dev/null
grep -q "ip4-frag" "${TRACE_DIR}/e-transit.txt" &&
    echo "      注：中转侧出现了 ip4-frag —— 分片是中转自己要处理的（它按 underlay 转发）" ||
    echo "      中转侧没有 ip4-frag：外层始终没超 underlay，中转不需要分片（预期）"

# ==================================================================
info "ASSERT F: 把隧道记录改成 0（= 没有记录），超长内层就不再被切，外层于是二次分片"
# ==================================================================
# 这条是"为什么 agent 必须写一个非 0 值"的反证：同一个 1436 的内层，口上记 1360 时
# 切成两片、外层 1452/196；口上没记录时整包封出去，外层 1532 > 1500，只能由 underlay
# 来切。实验室里 VPP 会在收端 ip4-input 之前把外层拼回来（所以 sr-wg4-input 仍只见到
# 完整 SR 头），但公网上的中间设备常直接丢单片 UDP —— 这就是预算要卡在隧道 MTU 上、
# 而不是指望"反正会分片"的原因。
# 这个实验只在 node1 的 ${IF1} 上动一下，改完立刻回 ${MTU_LIVE}；两种运行方式都做清理。
H0=$(stat_of "${C_NODE2}" Decap_hmac_fail)
vpp "${C_NODE1}" "set interface mtu packet 0 ${IF1}" | sed 's/^/    /'
ZRO=$(l3_of "${C_NODE1}" "${IF1}")
[ "${ZRO}" = "0" ] && ok "MTU=0 被 VPP 照单收下并把记录清空（所以 0 必须由客户端拦掉）" ||
    bad "写 0 之后 MTU 列是 '${ZRO}'：VPP 的行为变了，契约里那条'0 等于取消记录'要复核"
f_run "${C_NODE2}" "${T_OVER}" "无记录" "${TRACE_DIR}/f-zero.txt" "${REP}"
LENS=$(wg_lengths "${TRACE_DIR}/f-zero.txt")
NF=$(outer_frag_lines "${TRACE_DIR}/f-zero.txt")
read -r FN FSUM NMAX <<<"$(printf '%s\n' "${LENS}" | len_stats)"
WANT=$(outer_at "${T_OVER}")
echo "      内层长度：${FN} 个 WG 包、最大 ${NMAX}（无记录 => 整包，不该有第二片）"
echo "      外层分片行 ${NF}；这个内层对应的外层本该是 ${WANT} B，超过 underlay ${UNDERLAY_LIVE}"
# 注意这里不看外层 IP 头的 length 字段：分片之后每一片的头里写的是**自己**的长度，
# 1532 那个数已经不在包上了，所以判据取"有没有分片标志"，不取最大值。
[ "${NMAX}" = "${T_OVER}" ] && [ "${FN}" = "${REP}" ] &&
    ok "没有 MTU 记录时 ${T_OVER} 的内层整包封了出去（${REP} 轮 = ${FN} 个 WG 包）：对照 E，那条记录就是分片依据" ||
    bad "无记录仍然切了片（${FN} 个包、最大 ${NMAX}）：分片依据不是接口记录，早先的结论还得再改"
[ "${WANT}" -gt "${UNDERLAY_LIVE}" ] &&
    ok "算式说外层要 ${WANT} > underlay ${UNDERLAY_LIVE}：这条隧道此刻只能靠外层分片活着" ||
    bad "算式给出的外层 ${WANT} 并不超 ${UNDERLAY_LIVE}：这个反例不成立，检查 outer_at"
[ "${NF}" -ge 1 ] &&
    ok "外层确实带分片标志（${NF} 行）：分片被推到了 underlay 上，正是预算要防的那件事" ||
    bad "外层没出现分片标志：VPP 换了别的处理方式（直接丢？），请复核 ${TRACE_DIR}/f-zero.txt"
grep -q "ip4-local-full-reassembly" "${TRACE_DIR}/f-zero.txt" &&
    ok "外层分片在收端被重组回来（所以才谈得上'实验室里看不出问题'）" ||
    bad "外层分片没被重组：那这条隧道在分片场景下是直接不通的，比预期更糟"
H1=$(stat_of "${C_NODE2}" Decap_hmac_fail)
[ "${H1}" = "${H0}" ] &&
    ok "分片照样没打到插件上：Decap_hmac_fail ${H0} -> ${H1}（收端先重组再派发，安全属性成立）" ||
    bad "出现 hmac_fail 增量 $((H1 - H0))：有半截 SR 头进了插件，安全属性破了，必须查"
vpp "${C_NODE1}" "set interface mtu packet ${MTU_LIVE} ${IF1}" | sed 's/^/    /'
RESTORED=$(l3_of "${C_NODE1}" "${IF1}")
if [ "${RESTORED}" = "${MTU_LIVE}" ]; then
    ok "实验结束已把 ${IF1} 恢复成 ${MTU_LIVE}"
else
    bad "${IF1} 没恢复（现在是 '${RESTORED}'）！手工执行: vppctl set interface mtu packet ${MTU_LIVE} ${IF1}"
fi

# ==================================================================
info "ASSERT G: 一整轮大包之后，node1 的 underlay 发送侧没被卡死"
# ==================================================================
# 上面 E/F 专门发超长包，就是冲着当年那个"sendto 静默失败、计数器不再动"的现场去的：
# 修好 underlay 记录（断言 A）之后，同样的包只会被分片，不该把这张口打死。
# 判据取两条：tx 计数在动 + 没有 sendto 失败项。
TX1=$(tx_of "${C_NODE1}" "${UNDERLAY_IF}")
dx "${C_NODE1}" "vppctl ping ${OVERLAY_N2} source ${IF1} repeat 6 size $(size_for_inner "${T_OVER}") >/dev/null 2>&1"
sleep 1
TX2=$(tx_of "${C_NODE1}" "${UNDERLAY_IF}")
ERRS=$(vpp "${C_NODE1}" "show errors" | grep -iE "sendto|af_packet.*fail" || true)
echo "      ${UNDERLAY_IF} tx packets: ${TX1} -> ${TX2}"
if [ -n "${TX1}" ] && [ -n "${TX2}" ] && [ "${TX2}" -gt "${TX1}" ]; then
    ok "发送侧还在动（+ $((TX2 - TX1))）：af-packet 没被超长包卡住"
else
    bad "发送侧计数器没涨（${TX1} -> ${TX2}）：这就是 2026-10-18 那次卡死的症状，查 underlay MTU"
fi
[ -z "${ERRS}" ] && ok "show errors 里没有 sendto/af-packet 失败项" ||
    bad "show errors 出现发送侧失败：$(echo "${ERRS}" | tr '\n' ' | ')"

# ==================================================================
info "ASSERT H: 收尾复核 —— 记录没被实验改坏，隧道仍按预算工作"
# ==================================================================
if [ "${KEEP}" = "1" ]; then
    info "--keep：额外留下一轮 64B/1400B 的 trace 供人工排查（${TRACE_DIR}）"
    ping_ok "${C_NODE1}" "${OVERLAY_N2}" "${IF1}" 64 && ok "--keep 期间隧道仍通（小包）" || bad "小包都不通了"
fi
for c in ${C_NODE1} ${C_NODE2}; do
    got=$(l3_of "${c}" "${IF1}")
    [ "${got}" = "${EXPECT_TUNNEL_MTU}" ] &&
        ok "$(role_of "${c}") ${IF1} 仍是 ${got}" ||
        bad "$(role_of "${c}") ${IF1} 变成了 '${got}'"
    # 复核的是 A  Enumerate 出来的同一批 VIF，不是硬写的 "host-eth0"：
    # node2 的 underlay 口在 e2e --keep 之后是 host-eth1，写死名字只会读空并
    # 报一个假的"MTU 不一致"（本轮就这么撞过一次）。
    for v in $(vifs_of "${c}"); do
        k=$(kernel_mtu_of "${v}" "${c}")
        val=$(l3_of "${c}" "${v}")
        [ "${val}" = "${k}" ] && ok "$(role_of "${c}") ${v} 仍与内核一致（${val}）" ||
            bad "$(role_of "${c}") ${v} ${val} != 内核 ${k}"
    done
done
H2=$(stat_of "${C_NODE2}" Decap_hmac_fail)
[ "${H2}" = "${H1}" ] && ok "最后一轮复核没引入新的 hmac_fail（${H2}）" ||
    bad "复核阶段 hmac_fail 涨到 ${H2}（上一步 ${H1}）"
ping_ok "${C_NODE1}" "${OVERLAY_N2}" "${IF1}" && ok "隧道 1 仍然通" || bad "跑完 MTU 测试隧道不通了"

# ==================================================================
info "ASSERT I: 内层 DF=1 撞上隧道记录 —— 丢弃 + 回 ICMP frag-needed，不分片"
# ==================================================================
# 前面 A~H 全部走"可分片"路径：VPP 的 ping CLI 根本没有 DF 开关（实测 `ping ... df`
# 报 unknown input），所以内层 DF 位一次都没被真正测过。这里换注入路径：
# `create tap` 会同时建出 VPP 侧和**同一容器 netns 里的 host 侧**设备，内核于是就是
# 一台接在 CPE LAN 口的终端，`ping -M do` 能造出真带 DF 的内层包。
#
# 这条只判"发端 CPE 怎么处理一个它自己装不下的 DF 包"，不依赖回包：实验室里
# 转发流量的**回程**还有一处未定位的问题（收端 wg4-input 把转发包 punt 给宿主，
# 把本插件的 encap 关掉也一样复现，所以不是 SR 层的事），另记任务。
DF_TAP=tap9
DF_LAN=192.168.50
vpp "${C_NODE1}" "delete tap ${DF_TAP}" >/dev/null 2>&1
dx "${C_NODE1}" "ip route flush cache" >/dev/null 2>&1
TAP_NEW=$(vpp "${C_NODE1}" "create tap if-name ${DF_TAP} host-if-name ${DF_TAP} host-ip4-addr ${DF_LAN}.1/24 host-mtu-size 1500")
if [ "${TAP_NEW}" != "${DF_TAP}" ]; then
    bad "建不出 ${DF_TAP}（'${TAP_NEW}'），DF 这条无法判定"
else
    vpp "${C_NODE1}" "set interface state ${DF_TAP} up"                   >/dev/null
    vpp "${C_NODE1}" "set interface mtu 1500 ${DF_TAP}"                   >/dev/null
    vpp "${C_NODE1}" "set interface ip address ${DF_TAP} ${DF_LAN}.2/24"  >/dev/null
    dx "${C_NODE1}" "ip route add ${OVERLAY_N2}/32 via ${DF_LAN}.2 dev ${DF_TAP}" >/dev/null 2>&1
    dx "${C_NODE1}" "ip route flush cache" >/dev/null 2>&1

    # ---- 控制组：DF=1 且内层**正好**等于记录（Linux 的 -s 是 ICMP 数据，内层 = s + 28）----
    SZ_FIT=$((MTU_LIVE - 28))
    D0=$(stat_of "${C_NODE2}" Decap_ok)
    vpp "${C_NODE1}" "clear trace" >/dev/null
    vpp "${C_NODE1}" "trace add tap-input 200" >/dev/null
    dx "${C_NODE1}" "ping -c ${REP} -W 2 -M do -s ${SZ_FIT} -I ${DF_TAP} ${OVERLAY_N2}" >/dev/null 2>&1
    sleep 1
    vpp "${C_NODE1}" "show trace max 300" > "${TRACE_DIR}/i-fit.txt" 2>&1
    D1=$(stat_of "${C_NODE2}" Decap_ok)
    NFIT=$(grep -c "ip4-frag" "${TRACE_DIR}/i-fit.txt")
    DFI=$(grep -c "DONT_FRAGMENT" "${TRACE_DIR}/i-fit.txt")
    # 外层头的分片行：只在 underlay 发送节点的块里取，避免把内层那一行算进来
    OFI=$(awk -v n="${UNDERLAY_IF}-tx" 'index($0,n){f=1} f && /fragment id/{print; f=0}' "${TRACE_DIR}/i-fit.txt")
    echo "      内层正好 ${MTU_LIVE}（DF=1）：node2 Decap_ok ${D0} -> ${D1}，ip4-frag 出现 ${NFIT} 次"
    [ "${D1}" -gt "${D0}" ] && [ "${NFIT}" = "0" ] &&
        ok "DF 的内层等于记录（${MTU_LIVE}）时仍然整包封出去，没有分片（Decap_ok +$((D1 - D0))）" ||
        bad "DF 的内层等于记录却没整包出去（Decap_ok ${D0} -> ${D1}，ip4-frag ${NFIT}）"
    [ "${DFI}" -ge 1 ] &&
        ok "注入口看到内层 DF 位（${DFI} 行 DONT_FRAGMENT）：这条注入路径确实带 DF，不是空跑" ||
        bad "trace 里内层没有 DONT_FRAGMENT：注入的包没带 DF，下面两组都作废"
    if printf '%s' "${OFI}" | grep -q "flags"; then
        bad "外层 IP 头带了 DF（${OFI}）：内层的 DF 漏到了 underlay"
    else
        ok "外层 IP 头没有 DF：内层 DF 不外溢，PMTUD 只在隧道内生效"
    fi

    # ---- 实验组：DF=1 且内层超记录 ----
    # Linux 会按收到的 ICMP 缓存 PMTU，缓存住之后连包都不发（local error），
    # 所以每轮先 flush cache，否则量到的是内核行为而不是 VPP 行为。
    SZ_OVER=$((MTU_LIVE + 40))          # 内层 = 记录 + 68
    dx "${C_NODE1}" "ip route flush cache" >/dev/null 2>&1
    E0=$(err_of "${C_NODE1}" "ip4 MTU exceeded and DF set")
    I0=$(err_of "${C_NODE1}" "destination unreachable response")
    vpp "${C_NODE1}" "clear trace" >/dev/null
    vpp "${C_NODE1}" "trace add tap-input 200" >/dev/null
    OUT_OVER=$(dx "${C_NODE1}" "ping -c ${REP} -W 2 -M do -s ${SZ_OVER} -I ${DF_TAP} ${OVERLAY_N2} 2>&1")
    sleep 1
    vpp "${C_NODE1}" "show trace max 300" > "${TRACE_DIR}/i-over.txt" 2>&1
    E1=$(err_of "${C_NODE1}" "ip4 MTU exceeded and DF set")
    I1=$(err_of "${C_NODE1}" "destination unreachable response")
    D2=$(stat_of "${C_NODE2}" Decap_ok)
    NOVER=$(grep -c "ip4-frag" "${TRACE_DIR}/i-over.txt")
    # "一个片都没进隧道"只能看 node1 自己有没有把包交给 WG/SR。node2 的 Decap_ok
    # 不能当判据：WG 的 keepalive 走同一条路，随时会在这个窗口里 +1（第一次就撞上）。
    NO_ENC=$(grep -cE "wg4-output-tun|sr-wg4-encap|${UNDERLAY_IF}-tx" "${TRACE_DIR}/i-over.txt")
    echo "      DF 超记录（内层 $((SZ_OVER + 28)) > ${MTU_LIVE}）：ip4-input DF 丢弃 ${E0} -> ${E1}、ip4-icmp-error ${I0} -> ${I1}；node2 Decap_ok ${D1} -> ${D2}（含 keepalive，仅供参考）"
    printf '%s\n' "${OUT_OVER}" | grep -q "Frag needed and DF set (mtu = ${MTU_LIVE})" &&
        ok "发端真的收到 ICMP frag-needed，且报的 MTU 就是隧道记录 ${MTU_LIVE}（不是 underlay 的 ${UNDERLAY_LIVE}）" ||
        bad "发端没收到 'Frag needed and DF set (mtu = ${MTU_LIVE})'：$(printf '%s' "${OUT_OVER}" | tail -2 | tr '\n' ' ')"
    [ "${E1}" -gt "${E0}" ] &&
        ok "VPP 丢弃了装不下的 DF 包（ip4-input: ip4 MTU exceeded and DF set +$((E1 - E0))）" ||
        bad "DF 包没被计成 'MTU exceeded and DF set'（${E0} -> ${E1}）：它可能被分片了"
    [ "${I1}" -gt "${I0}" ] &&
        ok "VPP 发了 ICMP 目的不可达（ip4-icmp-error +$((I1 - I0))）：frag-needed 不是缺的" ||
        bad "ip4-icmp-error 没涨（${I0} -> ${I1}）：发端看到的 ICMP 来源要复核"
    [ "${NO_ENC}" = "0" ] &&
        ok "node1 没有把包交给 WG/SR（trace 里 wg4-output-tun / sr-wg4-encap / ${UNDERLAY_IF}-tx 均为 0）：拒发在封装之前" ||
        bad "trace 里出现封装或发送 ${NO_ENC} 行：DF 包被放进了隧道"
    [ "${NOVER}" = "0" ] &&
        ok "trace 里没有 ip4-frag：这是拒发，不是'切了再丢'" ||
        bad "trace 出现 ip4-frag ${NOVER} 次：VPP 先分片了，DF 语义没被执行"

    dx "${C_NODE1}" "ip route flush cache" >/dev/null 2>&1
    vpp "${C_NODE1}" "delete tap ${DF_TAP}" >/dev/null 2>&1
    if [ -z "$(swidx_of "${C_NODE1}" "${DF_TAP}")" ] && [ -z "$(dx "${C_NODE1}" "ip -o link show ${DF_TAP} 2>/dev/null")" ]; then
        ok "实验结束 ${DF_TAP} 的 VPP 侧与内核侧都已删除"
    else
        bad "${DF_TAP} 有残留（sw_if_index='$(swidx_of "${C_NODE1}" "${DF_TAP}")'）：手工执行 vppctl delete tap ${DF_TAP}"
    fi
    ping_ok "${C_NODE1}" "${OVERLAY_N2}" "${IF1}" && ok "DF 实验之后隧道仍然通" || bad "DF 实验把隧道弄不通了"
fi

# ==================================================================
info "ASSERT J: TCP MSS 钳制 —— 两端 RX 钳制把穿隧道的 SYN 改成 隧道MTU-44"
# ==================================================================
# 这条补的是"MTU 记录怎么影响 TCP"的落点。内层超了记录时 VPP 会切片（ASSERT E），
# 但 TCP 端点并不知道，LAN 终端按 1500 谈出 MSS=1460 才是最省事的路径，于是大包在
# WG 出口被切，每片再背一份 SR/WG 头。真实 LAN 不会替 CPE 记住隧道 MTU，所以要么
# 发端收到 ICMP frag-needed 自己降（ASSERT I），要么在入口把 MSS 改小 —— 后者就是
# mss_clamp 插件，MSS = 隧道 MTU - 44（20 IP + 20 TCP + 4 B VPN MPLS 标签）。
#
# 方向实测结论（2026-10-24，wg0 上 A/B/A 三轮）：TX 钳制**完全无效** —— WG 接口的
# ip4-output arc 上 wg4-output-tun 排在 tcp-mss-clamping-ip4-out 之前，SYN 原样带着
# 1460 出去；RX 钳制生效，**包括从别的接口转发进来的包**。所以正确配置是两端各自
# 在自己 wg0 的 RX 上钳，setup-node.sh 的 wg-create 已经这么配了。
#
# 判据两条：
#   (a) 配置：两端 wg0 的明细是 ip4: <MTU-44> [RX]（TX 或 disabled 都算红）；
#   (b) 功能：在 node1 接一台内核"终端"发真 SYN，node2 接一台"终端"收 —— 出去时
#       MSS 是 1460（终端按自己 1500 的 LAN 谈的），到 node2 已经是 1316。
#       只断言收端等于 MTU-44 不够：如果 SYN 根本没走隧道，那条断言也会"通过"，
#       所以必须同时看到发端那条更大的 MSS。
J_TAP_A=tap10
J_TAP_B=tap11
J_LAN_A=192.168.50
J_LAN_B=192.168.60
MSS_EXPECT=$((MTU_LIVE - 44))
J_A_OUT="${TRACE_DIR}/j-syn-a.txt"
J_B_OUT="${TRACE_DIR}/j-syn-b.txt"

# ---- (a) 两端的配置 ----
for jc in "${C_NODE1}" "${C_NODE2}"; do
    jl=$(vpp "${jc}" "show interface tcp-mss-clamp ${IF1}")
    echo "      $(role_of "${jc}") ${IF1}: ${jl}"
    printf '%s' "${jl}" | grep -q "ip4: ${MSS_EXPECT} \[RX\]" &&
        ok "$(role_of "${jc}") ${IF1} 上配着 RX 钳制 ${MSS_EXPECT}（= ${MTU_LIVE} - 44）" ||
        bad "$(role_of "${jc}") ${IF1} 的钳制不是 'ip4: ${MSS_EXPECT} [RX]'（'${jl}'）—— TX 方向实测不生效；手工修：vppctl set interface tcp-mss-clamp ${IF1} ip4 rx ip4-mss ${MSS_EXPECT} ip6 disable"
done

# ---- (b) 功能：真 SYN 穿隧道 ----
# 判"路由本来就有没有"，只删自己加的那条，别把环境里的东西删掉。
j_add_route() {   # <container> <prefix> <via> -> 0=本次新增
    if vpp "$1" "show ip fib $2" | grep -q "CLI refs"; then return 1; fi
    vpp "$1" "ip route add $2 via $3" >/dev/null 2>&1 || return 2
    return 0
}
for jc in "${C_NODE1}" "${C_NODE2}"; do
    vpp "${jc}" "delete tap ${J_TAP_A}" >/dev/null 2>&1
    vpp "${jc}" "delete tap ${J_TAP_B}" >/dev/null 2>&1
done
TAPA_NEW=$(vpp "${C_NODE1}" "create tap if-name ${J_TAP_A} host-if-name ${J_TAP_A} host-ip4-addr ${J_LAN_A}.1/24 host-mtu-size 1500")
TAPB_NEW=$(vpp "${C_NODE2}" "create tap if-name ${J_TAP_B} host-if-name ${J_TAP_B} host-ip4-addr ${J_LAN_B}.1/24 host-mtu-size 1500")
if [ "${TAPA_NEW}" != "${J_TAP_A}" ] || [ "${TAPB_NEW}" != "${J_TAP_B}" ]; then
    bad "建不出终端 tap（node1 '${TAPA_NEW}' / node2 '${TAPB_NEW}'），功能那条无法判定"
else
    for p in "${C_NODE1}:${J_TAP_A}:${J_LAN_A}.2" "${C_NODE2}:${J_TAP_B}:${J_LAN_B}.2"; do
        jc=${p%%:*}; rest=${p#*:}; jt=${rest%%:*}; jip=${rest##*:}
        vpp "${jc}" "set interface state ${jt} up" >/dev/null
        vpp "${jc}" "set interface mtu 1500 ${jt}" >/dev/null
        vpp "${jc}" "set interface ip address ${jt} ${jip}/24" >/dev/null
    done
    # 终端（内核侧 tap10）靠 .2 当网关。隧道里只能下**/32 主机路由**：WG 是点对点
    # 接口、没有 ARP，`ip route add <前缀>/24 via wg0` 会被 VPP 建成 attached/glean，
    # 转发时 dpo-drop（实测：包在 ip4-lookup 就丢，show ip fib 里是 dpo-drop，症状是
    # 隧道 ping 不通但没有任何错误计数）。overlay 路由本来也是 /32，正好一致。
    dx "${C_NODE1}" "ip route add ${J_LAN_B}.0/24 via ${J_LAN_A}.2 dev ${J_TAP_A}" >/dev/null 2>&1
    dx "${C_NODE2}" "ip route add ${J_LAN_A}.0/24 via ${J_LAN_B}.2 dev ${J_TAP_B}" >/dev/null 2>&1
    dx "${C_NODE1}" "ip route flush cache" >/dev/null 2>&1
    dx "${C_NODE2}" "ip route flush cache" >/dev/null 2>&1
    j_add_route "${C_NODE1}" "${J_LAN_B}.1/32" "${IF1}"; J_R1=$?
    j_add_route "${C_NODE2}" "${J_LAN_A}.1/32" "${IF1}"; J_R2=$?
    [ "${J_R1}" = "0" ] || bad "node1 加 ${J_LAN_B}.1/32 via ${IF1} 失败"
    [ "${J_R2}" = "0" ] || bad "node2 加 ${J_LAN_A}.1/32 via ${IF1} 失败"

    # 两端各抓一个 SYN：发端证明"终端确实按 1500 谈 MSS"，收端证明"到这一侧已被改写"。
    # tcpdump 的 stdout 落到宿主上的 TRACE_DIR（与其余 trace 证据同一处），不是在容器里重定向。
    dx "${C_NODE1}" "timeout 10 tcpdump -i ${J_TAP_A} -n -l -c 1 -v 'tcp[tcpflags] & tcp-syn != 0'" > "${J_A_OUT}" 2>/dev/null &
    JPID_A=$!
    dx "${C_NODE2}" "timeout 10 tcpdump -i ${J_TAP_B} -n -l -c 1 -v 'tcp[tcpflags] & tcp-syn != 0'" > "${J_B_OUT}" 2>/dev/null &
    JPID_B=$!
    sleep 1
    # 内核终端发一个真 SYN：没有监听者，收到 RST 就结束。
    dx "${C_NODE1}" "timeout 3 bash -c 'exec 3<>/dev/tcp/${J_LAN_B}.1/8080'" >/dev/null 2>&1
    wait "${JPID_A}" 2>/dev/null
    wait "${JPID_B}" 2>/dev/null

    MSS_A=$(grep -o 'mss [0-9]*' "${J_A_OUT}" 2>/dev/null | awk '{print $2}' | head -1)
    MSS_B=$(grep -o 'mss [0-9]*' "${J_B_OUT}" 2>/dev/null | awk '{print $2}' | head -1)
    echo "      SYN 的 MSS：node1 发出 ${MSS_A:-<没抓到>} -> node2 收到 ${MSS_B:-<没抓到>}（期望 ${MTU_LIVE} 的 LAN 值是 1460、隧道值是 ${MSS_EXPECT}）"
    if [ "${MSS_A:-0}" -gt "${MSS_EXPECT}" ] 2>/dev/null; then
        ok "SYN 离开终端时 MSS=${MSS_A}（大于隧道值）：对照组成立，终端并不知道隧道 MTU"
    else
        bad "发端 SYN 的 MSS 是 '${MSS_A:-<没抓到>}'（应大于 ${MSS_EXPECT}）：抓包或注入路径有问题，下面收端那条不能单独作数"
    fi
    [ "${MSS_B:-0}" = "${MSS_EXPECT}" ] &&
        ok "SYN 到 node2 时 MSS 已被 RX 钳制改成 ${MSS_B}（= ${MTU_LIVE} - 44）" ||
        bad "SYN 到 node2 的 MSS 是 '${MSS_B:-<没抓到>}'，期望 ${MSS_EXPECT}：钳制没生效或方向配错了"

    # ---- 收拾：只删自己加的 ----
    dx "${C_NODE1}" "ip route del ${J_LAN_B}.0/24 via ${J_LAN_A}.2 dev ${J_TAP_A}" >/dev/null 2>&1
    dx "${C_NODE2}" "ip route del ${J_LAN_A}.0/24 via ${J_LAN_B}.2 dev ${J_TAP_B}" >/dev/null 2>&1
    [ "${J_R1}" = "0" ] && vpp "${C_NODE1}" "ip route del ${J_LAN_B}.1/32" >/dev/null 2>&1
    [ "${J_R2}" = "0" ] && vpp "${C_NODE2}" "ip route del ${J_LAN_A}.1/32" >/dev/null 2>&1
    vpp "${C_NODE1}" "delete tap ${J_TAP_A}" >/dev/null 2>&1
    vpp "${C_NODE2}" "delete tap ${J_TAP_B}" >/dev/null 2>&1
    if [ -z "$(swidx_of "${C_NODE1}" "${J_TAP_A}")" ] && [ -z "$(swidx_of "${C_NODE2}" "${J_TAP_B}")" ] &&
       [ -z "$(dx "${C_NODE1}" "ip -o link show ${J_TAP_A} 2>/dev/null")" ] &&
       [ -z "$(dx "${C_NODE2}" "ip -o link show ${J_TAP_B} 2>/dev/null")" ]; then
        ok "实验结束 ${J_TAP_A}/${J_TAP_B} 的 VPP 侧与内核侧都已删除"
    else
        bad "终端 tap 有残留：手工执行 vppctl delete tap ${J_TAP_A}（node1）/ ${J_TAP_B}（node2）"
    fi
    ping_ok "${C_NODE1}" "${OVERLAY_N2}" "${IF1}" && ok "MSS 实验之后隧道仍然通" || bad "MSS 实验把隧道弄不通了"
fi

echo
echo "=========================================================="
echo " trace 证据: ${TRACE_DIR}/{c-exact,e-over,f-zero,e-transit,i-fit,i-over,j-syn-a,j-syn-b}.txt"
echo " 实测结论：内层 = ping size + 36；外层 = 内层 + 32 + (24+6*${SEGS}) + 28"
echo "          隧道 MTU 由 VPP 执行，执行动作是按它切内层分片（不是拒发）"
echo "          内层带 DF 时不切：丢弃 + 回 ICMP type3/code4，MTU 报的就是隧道记录"
echo " RESULT: ${PASS} passed, ${FAIL} failed"
echo "=========================================================="
[ "${FAIL}" != "0" ] && { echo; info "dumping diagnostics (FAIL)"; dump_all; }
[ "${FAIL}" = "0" ]
