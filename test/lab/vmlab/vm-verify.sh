#!/bin/bash
# vm-verify.sh - VM 研发台的端到端体检（在 Mac 上跑，ssh 口径）
#
# 容器台 test/lab/verify.sh 的孪生。三处口径必须换，逐条写明为什么：
#
#   1. `docker exec <容器>` ⇒ `ssh CPE1..CPE4` / `ssh cust12|cust3|cust4`
#      （cust 的 ssh 别名走 ProxyJump 到自家盒子，见 ~/.ssh/config）。
#   2. 期望值不再硬抄在脚本里：VM 台的隧道是控制台配的（S6），台账从控制台读
#      （vm-ledger.py → /api/underlay）；只有客户侧 loopback、客户口地址、路由协议
#      是手工表（lab 值手工配，控制器不猜）。wg 接口名从盒子里
#      `show interface addr` 现读，按台账里的本端 overlay 对上。
#   3. 客户视角的 ping 一律用 loopback 作源。overlay 只承载 172.16/16 里的 /32，
#      拿客户局域网地址（172.31.x.2）当源，回程会在对端盒子里黑洞掉 ——
#      2026-11-05 就因为这条误判过一次"六方向全丢包"。
#
# 只读：不改任何配置、不重启任何进程（AGENTS.md §1）。唯一的副作用是 §2 里
# check-selfheal.sh 自己的探测 ping 与 `clear errors`，以及 §9 的 ping 流量。
#
# 判据分两档，不是一档：
#   FAIL —— 计入退出码。任何"没解释"的不通、丢包、形状不对都算。
#   WARN —— 只有一种：§9 客户 ping 有丢包，但同一窗口里盒子的 fib.log 确实记了
#           "下一跳 … 的 BFD 会话不是 Up"（缺陷⑥：亚秒抖动被放大成 ≥1s 黑洞，
#           2026-11-05 实测四台 42–58 次/小时、中位 1s、累计黑洞 1.16%–6.49%）。
#           数字照打、单独计数，不冒充新缺陷；摘不到路的丢包仍是 FAIL。
#
# 本轮没覆盖的两处，点名不静默（文件末尾也打）：
#   - 没有独立终端机（容器台的 lab-termN）：客户视角用 cust 虚拟机的 loopback 代打，
#     所以链路里少一跳（cust 就是"终端 + 客户路由器"两台合一）。
#   - 站点 12 没配 VRRP（S6 拍板这轮不发）：主备切换不在判据里。
#
# 用法：test/lab/vmlab/vm-verify.sh [--quiet] [--no-shape] [节点号...]
#         --quiet     只打 PASS/FAIL/WARN，不打旁证行
#         --no-shape  跳过 §2（check-selfheal.sh 一次 ~15s×8，日常快查可跳）
set -u -o pipefail
# pipefail：ssh_run 内部是 `ssh … | tr -d '\r'`，没有它整条管道的退出码是 tr 的 0，
# ssh 连不上/远端命令失败都会被当成成功（AGENTS.md §2：判据不能靠运气）。

HERE=$(cd "$(dirname "$0")" && pwd)
LEDGER=${VM_LEDGER:-/tmp/vm-verify-ledger.tsv}
WORKDIR=${VM_VERIFY_WORK:-/tmp/vm-verify-work}

VPN=0                    # 客户 VPN ID，直接当 VPP 表号
OVERLAY=250              # 隧道与 overlay 下一跳所在的表，不是 VPN
LABEL=1000               # 标签恒为 LabelFor(来源 VPN)，与装进哪张表无关
ROUTE_PORT=19001         # 盒子里 sr-route 的状态口（VM 台每台一个 loopback，端口不必错开）
FIB_PORT=19011           # 盒子里 sr-fib 的状态口
CTRL_ADDR=192.168.1.4    # 控制面在 Mac 上（S2）
CTRL_PORT=8444
CONSOLE_PORT=8088
RR_STATUS=( "" 19446 19447 )    # rr-a / rr-b 在 Mac 上的状态口（设备口是 18446/18447）
RR_TENANT=tenant-vm
CA_FILE=$HOME/srvmlab/ctrl/data/ca.pem
FIB_LOG=/var/log/sr-box/fib.log

SSHO="-o BatchMode=yes -o ConnectTimeout=8 -o LogLevel=ERROR"

QUIET=0 SHAPE=1 NODES=""
for a in "$@"; do
    case "$a" in
        --quiet) QUIET=1 ;;
        --no-shape) SHAPE=0 ;;
        [1-4]) NODES="$NODES $a" ;;
        *) echo "不认识参数：$a"; exit 2 ;;
    esac
done
[ -n "$NODES" ] || NODES="1 2 3 4"
NODES=$(echo "$NODES" | tr ' ' '\n' | awk 'NF' | sort -nu | tr '\n' ' ')

PASS=0 FAIL=0 WARNN=0
ok()   { PASS=$((PASS + 1)); printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { FAIL=$((FAIL + 1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
warn() { WARNN=$((WARNN + 1)); printf '  \033[33mWARN\033[0m %s\n' "$*"; }
sect() { echo; echo "== $*"; }
note() { [ "$QUIET" = "1" ] || printf '  .... %s\n' "$*"; }

# ---- 取数：一次 ssh 一条命令；VPP CLI 带 CR，一律剃掉 ----
# -n 是必需的，不是风格：台账的循环体 stdin 接的是 `done < <(grep …)`，
# ssh 不带 -n 会把剩下的台账行当自己的输入读走 —— 于是每个 while 只跑第一行，
# 后面的判据根本没执行过（2026-11-05 实测：§9 六方向互 ping 只打出一条）。
# 本脚本没有任何地方靠 stdin 往盒子里喂数据（远端的 `<` 在引号里，是盒子读的）。
# known_hosts 的 "Warning: Permanently added …" 不受 LogLevel 控制（ERROR/quiet 都照打，
# 2026-11-05 实测），它会混进 2>&1 污染 §5 写出的 JSON（解析器直接报错 = 假红）。
# 这里按行滤掉那一句，同时把 ssh 自己的退出码 return 出去（配 set -o pipefail，
# 链路断了不再被 tr 的 0 掩盖）。
ssh_run() { local h=$1 out rc; shift
    out=$(ssh -n $SSHO "$h" "$*" 2>&1 | tr -d '\r'); rc=$?
    printf '%s\n' "$out" | grep -v '^Warning: Permanently added'
    return $rc; }
vppc()    { local h=$1; shift; ssh_run "$h" "vppctl $*"; }

# ---- 台账读法（vm-ledger.py 的 TSV，$1=记录类型，$2=node）----
lrows()      { awk -F'\t' -v t="$1" -v n="$2" '$1 == t && $2 == n' "$LEDGER"; }
lfld()       { awk -F'\t' -v t="$1" -v n="$2" -v i="$3" '$1 == t && $2 == n {print $i; exit}' "$LEDGER"; }
node_status()  { lfld node "$1" 5; }
node_transit() { lfld node "$1" 6; }
node_deliv_v() { lfld node "$1" 9; }
node_deliv_ok(){ lfld node "$1" 10; }
box_loop()   { lfld box "$1" 4; }      # 本站对客户播的 loopback
box_cust()   { lfld box "$1" 5; }      # 盒内客户口地址
box_peer()   { lfld box "$1" 6; }      # 对面客户机地址
box_ssh()    { lfld box "$1" 7; }      # 客户机 ssh 别名
box_proto()  { lfld box "$1" 8; }      # 客户侧协议 ospf|bgp
tunnels_of() { lrows tun "$1"; }
tun_count()  { tunnels_of "$1" | grep -c .; }

retry() {   # <秒> <命令...>：收敛类断言用，判据本身不变
    local limit=$1 i=0; shift
    while [ "$i" -lt "$((limit * 2))" ]; do
        "$@" >/dev/null 2>&1 && return 0
        sleep 0.5; i=$((i + 1))
    done
    "$@" >/dev/null 2>&1
}

# overlay 表里对端 /32 的名字，以及"本端 overlay → 本机 wg 口名"的映射。
# 口名只有盒子知道（VPP 按创建顺序自动命名），台账只给 overlay 地址。
iface_for_overlay() {   # <show interface addr 的输出> <overlay>
    echo "$1" | awk -v w="$2" '$1 ~ /^wg/ && $0 ~ /\(/ {cur=$1}
                                index($0, "L3 " w "/32") {print cur; exit}'
}

# VPP fib dump 里一段前缀的正文（到下一条顶格行为止）；比 grep -A8 稳，条目长度不固定
fib_block() {   # <prefix> <dump 文件>
    awk -v p="$1" '$0 == p { f = 1; next } f && /^[^[:space:]]/ { f = 0 } f' "$2";
}

# 整表 dump（show ip fib table N）只打**承载链**，没有 path-list；要知道「行里装了哪几条
# 路径、各自 pref 多少、resolved 没有」必须单前缀查询。判前缀行是防 LPM 回落：精确行被删时
# show ip fib 会打印覆盖它的那条（实测回落到 0.0.0.0/0 管理网默认路由），那不是我们的路径集
# （exp-285 的采样器就这么被污染过一枪）。行不在 ⇒ 输出空，由调用方判红。
fib_paths() {   # <host> <prefix>；输出「下一跳 pref R|U」
    local out
    out=$(vppc "$1" show ip fib table "$VPN" "$2") || return 0
    printf '%s\n' "$out" | awk -v p="$2" '
        $1 == p && $2 ~ /^fib:/ { f = 1; next }
        f && /^[^[:space:]]/ { f = 0 }
        f && /path:\[/ && /pref=/ {
            match($0,/pref=[0-9]+/); pr=substr($0,RSTART+5,RLENGTH-5)
            r=($0 ~ /oper-flags:resolved/) ? "R" : "U"
            if (getline nl) {split(nl,a," ");
                if (a[1] ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/) printf "%s %s %s\n", a[1], pr, r}
        }'
}

# 远端前缀的 FIB 断言不能"抓一次就判"：BFD 在这台共享 CPU 的宿主上实测每分钟会漏掉
# 1–3 次检测（六会话同时掉，~1s 自己恢复），sr-fib 按规矩把那条路径撤掉、下一轮再装
# 回来（缺陷⑥）。先把 dump 存下来供逐条断言，10s 仍不收敛才算红。
fib_ready() {   # <host> <node>
    local h=$1 n=$2 out pfx
    out=$(vppc "$h" show ip fib table "$VPN")
    [ -n "$out" ] || return 1
    for pfx in $(lrows rib "$n" | cut -f3); do
        echo "$out" | grep -q "^$pfx" || return 1
    done
    printf '%s\n' "$out" > "$WORKDIR/fib-$h"
    for pfx in $(lrows rib "$n" | cut -f3); do
        fib_block "$pfx" "$WORKDIR/fib-$h" | grep -q "dpo-drop" && return 1
    done
    return 0
}

# overlay 两跳 ping：必须校验发包数。vppctl ping 不带 source 会直接报
# "no egress interface"，然后以 "0 sent, 0 received, 0% packet loss" 收尾 ——
# 只 grep 丢包率会把没发出去的包当成通。
PING_OUT=
overlay_ping() {   # <host> <对端 overlay> <本端 wg 口>
    PING_OUT=$(vppc "$1" ping "$2" source "$3" repeat 3 interval 0.2)
    echo "$PING_OUT" | grep -Eq '(^|[^0-9])3 sent, 3 received'
}

# BFD 会话：条数 = 隧道数、且全部 Up。多跳是硬约束（绑接口的会话会 dpo-drop）。
bfd_all_up() {   # <host> <want>
    local s d
    s=$(vppc "$1" show bfd sessions | grep -c 'Hop Type')
    d=$(vppc "$1" show bfd sessions | grep 'Session state' | grep -cvE 'Up +Up')
    [ "$s" = "$2" ] && [ "${d:-0}" = 0 ]
}

# ---- 摘路计数（缺陷⑥的证据）----
# 不跨时钟对齐（盒子跑 UTC、Mac 跑 +08:00，8 小时差）：改用 fib.log 的行号增量，
# 窗口前后各取一次 wc -l，中间那段才属于这次 ping。
log_mark() { ssh_run "$1" "wc -l < $FIB_LOG 2>/dev/null" | tr -d ' \r' | tail -1; }
log_withdrawals() {   # <host> <起始行号> → 窗口内 "BFD 会话不是 Up" 的条数
    local n0
    n0=$(ssh_run "$1" "tail -n +$((${2:-0} + 1)) $FIB_LOG 2>/dev/null | grep -c 'BFD 会话不是 Up'")
    echo "${n0:-0}"
}

# ---- 控制面状态对账：Go 吐的是紧凑 JSON，用解析器读，不 grep ----
# 程序体在 vm-state.py（不在这里用 heredoc）：heredoc 要占 stdin，而本脚本的 stdin
# 在 while-read 循环里就是台账 —— 少一段程序体时它执行空程序、退出码 0，十条判据全成
# 假绿（2026-11-05 抓到过，note 全是 0 字节）。判据与旁证都由那个文件说话。
route_check() {   # <node> <host>；证据行写到 $WORKDIR/route-<node>.note
    local n=$1 h=$2
    ssh_run "$h" "curl -sf http://127.0.0.1:$ROUTE_PORT/status" > "$WORKDIR/route-$n.json" || {
        echo "盒子里 curl :$ROUTE_PORT/status 没答（sr-route 没在跑？）" > "$WORKDIR/route-$n.note"; return 1; }
    python3 "$HERE/vm-state.py" route "$WORKDIR/route-$n.json" "$LEDGER" "$n" \
        > "$WORKDIR/route-$n.note" 2>&1
}
fib_check() {     # <node> <host>；证据行写到 $WORKDIR/fib-<node>.note
    local n=$1 h=$2
    ssh_run "$h" "curl -sf http://127.0.0.1:$FIB_PORT/status" > "$WORKDIR/fib-$n.json" || {
        echo "盒子里 curl :$FIB_PORT/status 没答（sr-fib 没在跑？）" > "$WORKDIR/fib-$n.note"; return 1; }
    python3 "$HERE/vm-state.py" fib "$WORKDIR/fib-$n.json" "$LEDGER" "$n" "$VPN" \
        > "$WORKDIR/fib-$n.note" 2>&1
}
rr_check() {      # <1|2>；证据行写到 $WORKDIR/rr-<r>.note
    local r=$1
    curl -sf "http://127.0.0.1:${RR_STATUS[$r]}/status" > "$WORKDIR/rr-$r.json" 2>/dev/null || {
        echo "Mac 上 curl http://127.0.0.1:${RR_STATUS[$r]}/status 没答（rr$r 没在跑？）" \
            > "$WORKDIR/rr-$r.note"; return 1; }
    python3 "$HERE/vm-state.py" rr "$WORKDIR/rr-$r.json" "$RR_TENANT" "$(grep -c '^node' "$LEDGER")" \
        > "$WORKDIR/rr-$r.note" 2>&1
}

if [ ! -s "$LEDGER" ]; then
    echo "先生成台账：$HERE/vm-ledger.py > $LEDGER"
    exit 2
fi
mkdir -p "$WORKDIR"
echo "===== VM 台端到端体检（节点 ${NODES}；台账 ${LEDGER}，$(grep -c . "$LEDGER") 行）====="

sect "1. 主机、VPP、插件与盒内进程"
for n in $NODES; do
    h=CPE$n
    ssh_run "$h" "hostname" | grep -q "^$h$" && ok "$h: ssh 可达" || bad "$h: ssh 不通"
    vppc "$h" show version | grep -q "vpp v" && ok "$h: VPP 在运行" || bad "$h: VPP 不可用"
    plug=$(vppc "$h" show plugins)
    for p in sr_wg_plugin.so srmpls_plugin.so wireguard_plugin.so \
             crypto_openssl_plugin.so linux_cp_plugin.so linux_nl_plugin.so; do
        echo "$plug" | grep -q "$p" && ok "$h: $p 已加载" || bad "$h: $p 缺失"
    done
    for d in sr-agent sr-route sr-fib sr-frr; do
        pid=$(ssh_run "$h" "pidof $d" | awk '{print $1}')
        [ -n "$pid" ] && ok "$h: $d 在跑（pid ${pid}）" || bad "$h: $d 没在跑"
    done
    # 问题6（#270，§9.18）：VPP 主线程落哪一核是这台盒子里最大的一根时延开关。
    # 不写 cpu 段时落点由内核开机随手给（四台里三台拿到 vCPU1，客户 overlay 从 8~12ms
    # 抬到 22~26ms，主机 ping 管理口从 ~2ms 抬到 5~7ms），所以启动配置里必须钉住，
    # 而且判据要看 VPP 自报的落点 —— 只看配置等于"我写了所以生效了"（§2 证据规则）。
    conf=$(ssh_run "$h" "grep -c 'main-core 0' /root/vmlab/startup-vmlab.conf")
    core=$(vppc "$h" show threads | awk '$2 == "vpp_main" {print $7; exit}')
    if [ "${conf:-0}" != "0" ] && [ "${core:-x}" = "0" ]; then
        ok "$h: VPP 主线程在 Core 0（启动配置钉了 main-core 0）"
    else
        bad "$h: VPP 主线程 Core=${core:-读不到}（期望 0；配置里 main-core 行数=${conf:-读不到}）"
    fi
done
for c in cust12 cust3 cust4; do
    ssh_run "$c" "hostname" | grep -q "^$c$" && ok "$c: ssh 可达（ProxyJump 走自家盒子）" \
        || bad "$c: ssh 不通"
    ssh_run "$c" "pidof zebra bgpd ospfd" | grep -q "[0-9]" \
        && ok "$c: FRR 守护进程在跑" || bad "$c: FRR 没在跑"
done

sect "2. 接管形状（盒内跑 check-selfheal.sh 的 C1–C7，含缺陷②的 ALLMULTI）"
if [ "$SHAPE" = 0 ]; then
    note "--no-shape：跳过（要单跑：ssh CPE1 /root/vmlab/check-selfheal.sh <口> <CIDR> <TAP> <对端> 标签）"
else
    for n in $NODES; do
        h=CPE$n
        conf=$(ssh_run "$h" "cat /root/vmlab/box.conf")
        for side in CUST WAN; do
            IF=$(echo "$conf"   | awk -F= -v k="IF_$side"   '$1 == k {print $2}')
            CIDR=$(echo "$conf" | awk -F= -v k="CIDR_$side" '$1 == k {print $2}')
            TAP=$(echo "$conf"  | awk -F= -v k="TAP_$side"  '$1 == k {print $2}')
            PEER=$(echo "$conf" | awk -F= -v k="PEER_$side" '$1 == k {print $2}')
            [ -n "$IF" ] && [ -n "$TAP" ] || { bad "$h: box.conf 里缺 $side 那一对参数"; continue; }
            out=$(ssh_run "$h" "/root/vmlab/check-selfheal.sh $IF $CIDR $TAP $PEER $h-$side")
            nf=$(echo "$out" | grep -c '^  FAIL')
            if [ "${nf:-0}" = 0 ] && echo "$out" | grep -q '结论：全部 PASS'; then
                ok "$h: ${side}（$IF ↔ ${TAP}）形状 13 条全 PASS"
            else
                bad "$h: ${side}（$IF ↔ ${TAP}）形状 $nf 条 FAIL"
                echo "$out" | grep '^  FAIL' | sed 's/^/       /'
            fi
        done
    done
fi

sect "3. 隧道层（台账对盒内：口名 / overlay / 表 $OVERLAY / MTU / 端口 / 绑定 / peer / BFD）"
for n in $NODES; do
    h=CPE$n
    rows=$(tunnels_of "$n")
    want=$(tun_count "$n")
    [ -n "$rows" ] || { bad "$h: 台账里这台一条隧道都没有"; continue; }
    VP_IF=$(vppc "$h" show interface)
    ADDR=$(vppc "$h" show interface addr)
    PORTS=$(vppc "$h" show sr-wg ports)
    PATHS=$(vppc "$h" show sr-wg paths)
    TUND=$(vppc "$h" show sr-wg tunnels)
    PEERS=$(vppc "$h" show wireguard peer)
    vpnports=$(echo "$PORTS" | awk '/^vpn/{i=index($0,": "); s=substr($0,i+2); gsub(/ /,"",s); print s}')
    case ",$vpnports," in
        *",$(node_transit "$n"),"*) bad "$h: 中转端口 $(node_transit "$n") 混进了 vpn 集合" ;;
        *) ok "$h: 中转端口 $(node_transit "$n") 没占用 vpn 集合" ;;
    esac
    echo "$PORTS" | grep -q "(已启用)" && ok "$h: transit 已启用" || bad "$h: transit 未启用"
    echo "$PORTS" | grep '^transit' | grep -Eq "(^|[^0-9])$(node_transit "$n")([^0-9]|$)" \
        && ok "$h: 中转端口 $(node_transit "$n") 已登记" \
        || bad "$h: 中转端口 $(node_transit "$n") 不在 transit 集合（$(echo "$PORTS" | grep '^transit')）"
    OUTIF=$(echo "$rows" | head -1 | cut -f7)
    if echo "$TUND" | grep -q "encap enabled on" && echo "$TUND" | grep -q "  $OUTIF "; then
        ok "$h: encap 挂在 ${OUTIF}（客户口的包才进得了隧道）"
    else
        bad "$h: encap 没挂在 ${OUTIF}（SR 路径不会生效）"
    fi
    while IFS=$'\t' read -r _ nn tid lov lport laddr oiface pnn pov pport paddr color tmtu; do
        [ -n "$tid" ] || continue
        wg=$(iface_for_overlay "$ADDR" "$lov")
        [ -n "$wg" ] || { bad "$h: $tid 的本端 overlay $lov 在 VPP 里没有对应 wg 口"; continue; }
        echo "$VP_IF" | awk -v x="$wg" '$1 == x && $3 == "up" {f = 1} END {exit !f}' \
            && ok "$h: $tid → ${wg}（对端 ${pov}）口在且 up" || bad "$h: $tid → $wg 不存在或没 up"
        echo "$ADDR" | grep -A1 "^$wg (" | grep -q "L3 $lov/32 ip4 table-id $OVERLAY" \
            && ok "$h: $wg 持有 $lov/32 且在 overlay 表 $OVERLAY" \
            || bad "$h: $wg 上不是「$lov/32 + table-id ${OVERLAY}」"
        mtu=$(echo "$VP_IF" | awk -v x="$wg" '$1 == x {split($4, m, "/"); print m[1]; exit}')
        [ "${mtu:-0}" = "$tmtu" ] && ok "$h: $wg MTU=${mtu}（台账 ${tmtu}）" \
            || bad "$h: $wg MTU=${mtu:-无}，台账 $tmtu"
        case ",$vpnports," in
            *",$lport,"*) ok "$h: $wg 的监听端口 $lport 在 vpn 集合" ;;
            *) bad "$h: $wg 的监听端口 $lport 不在 vpn 集合 '$vpnports'" ;;
        esac
        bind=$(echo "$TUND" | grep "($wg)")
        pid=$(echo "$bind" | sed -n 's/.*path_id=\([0-9]*\).*/\1/p')
        if echo "$bind" | grep -q "src_port=$lport" && echo "$bind" | grep -q "usable"; then
            ok "$h: $wg 绑定行 src_port=$lport 且 usable"
        else
            bad "$h: $wg 的绑定行不对：${bind:-没有这一行}"
        fi
        [ -n "$pid" ] && echo "$PATHS" | grep -q "path_id=$pid " \
            && ok "$h: $wg 的路径 $pid 在路径表里（active）" \
            || bad "$h: $wg 的路径 '$pid' 不在路径表里"
        ep="endpoint:\[$laddr:$lport->$paddr:$pport\]"
        echo "$PEERS" | grep -q "$ep $wg" \
            && ok "$h: peer 端点 $laddr:${lport}→$paddr:${pport}（${wg}）与台账一致" \
            || bad "$h: 没有 '$laddr:${lport}→$paddr:$pport' 这条 peer（${wg}）"
        echo "$PEERS" | grep "$ep" | grep -q "flags: 2" \
            && ok "$h: $wg 的 peer established" || bad "$h: $wg 的 peer 不是 flags: 2"
        echo "$PEERS" | grep -A3 "$ep" | grep -q "allowed-ips: 0.0.0.0/0" \
            && ok "$h: $wg 的 allowed-ips 是常量 0.0.0.0/0（不参与转发）" \
            || bad "$h: $wg 的 allowed-ips 不是 0.0.0.0/0"
        [ "$color" = 0 ] && ok "$h: $tid 颜色 0（落主表 ${VPN}，无颜色表）" \
            || note "$h: $tid 颜色 $color —— 落颜色表 $((color * 1000 + VPN))；主表收不收它由逃生开关决定（承载集在 §6 判）"
    done <<EOF
$rows
EOF
    npeer=$(echo "$PEERS" | grep -c 'flags: 2')
    [ "$npeer" = "$want" ] && ok "$h: peer established $npeer 条 = 台账 $want 条" \
        || bad "$h: peer established $npeer 条，台账 $want 条"
    usable=$(echo "$TUND" | sed -n 's/.*binding(s) configured, \([0-9]*\) usable.*/\1/p')
    [ "${usable:-0}" = "$want" ] && ok "$h: 隧道绑定可用 $usable 条" \
        || bad "$h: 隧道绑定可用 ${usable:-0} 条，台账 $want 条"
    if retry 20 bfd_all_up "$h" "$want"; then
        ok "$h: BFD 多跳会话 $want 条且全 Up"
    else
        bad "$h: BFD 会话不是「$want 条且全 Up」（实测 $(vppc "$h" show bfd sessions | grep -c 'Hop Type') 条、非 Up $(vppc "$h" show bfd sessions | grep 'Session state' | grep -cvE 'Up +Up') 条）"
    fi
done

sect "4. overlay 可达性（VPP 自己发起的 ping，两跳）"
for n in $NODES; do
    h=CPE$n
    ADDR=$(vppc "$h" show interface addr)
    while IFS=$'\t' read -r _ nn tid lov lport laddr oiface pnn pov pport paddr color tmtu; do
        [ -n "$tid" ] || continue
        wg=$(iface_for_overlay "$ADDR" "$lov")
        if retry 20 overlay_ping "$h" "$pov" "${wg:-wg-none}"; then
            ok "$h → ${pov}（cpe${pnn}，source ${wg:-?}）通"
            note "$(echo "$PING_OUT" | grep 'round-trip' | tail -1)"
        else
            bad "$h → ${pov}（cpe${pnn}，source ${wg:-?}）不通：$(echo "$PING_OUT" | tail -1)"
        fi
    done <<EOF
$(tunnels_of "$n")
EOF
done

sect "5. 控制面（Mac 上的 sr-ctrl / 双 RR + 盒内身份 / RIB / 隧道表）"
[ -s "$CA_FILE" ] && ok "sr-ctrl CA 已生成（${CA_FILE}）" || bad "sr-ctrl 没有 CA：$CA_FILE"
nc -z -w 2 "$CTRL_ADDR" "$CTRL_PORT" 2>/dev/null && ok "设备入口 $CTRL_ADDR:$CTRL_PORT 在听" \
    || bad "设备入口 $CTRL_ADDR:$CTRL_PORT 不通"
curl -sf -o /dev/null "http://127.0.0.1:$CONSOLE_PORT/" && ok "控制台 127.0.0.1:$CONSOLE_PORT 在答" \
    || bad "控制台 127.0.0.1:$CONSOLE_PORT 没答"
for r in 1 2; do
    if rr_check "$r"; then
        ok "rr$r: $(cat "$WORKDIR/rr-$r.note")"
    else
        bad "rr$r: $(cat "$WORKDIR/rr-$r.note" 2>/dev/null || echo 取不到 /status)"
    fi
done
for n in $NODES; do
    h=CPE$n
    for f in device_ed25519.key node_cert.pem ca_cert.pem state.json wg_x25519.key; do
        ssh_run "$h" "test -s /etc/sdwan/identity/$f && echo Y" | grep -q "^Y$" \
            && ok "$h: identity/$f 已落盘" || bad "$h: identity/$f 缺失"
    done
    st=$(node_status "$n")
    [ "$st" = online ] && ok "$h: 控制台侧 status=online" || bad "$h: 控制台侧 status=$st"
    dv=$(node_deliv_v "$n"); do_=$(node_deliv_ok "$n")
    if [ "$do_" = true ]; then
        ok "$h: 隧道下发回执 ok=true（设备侧版本 ${dv}，控制台已确认）"
        # 缺陷④的反证位：回执 ok 但盒内一条隧道都没有时，页面会显示"已下发"。
        # 上面 §3 的口名/绑定计数已经封住这条路，这里只把两者并排放出来好核。
        note "$h: 回执 v$dv + 盒内隧道 $(tun_count "$n") 条实际在位（两个口径一起看）"
    else
        bad "$h: 隧道下发回执 ok=${do_}（版本 ${dv}）—— 页面会显示未确认"
    fi
    if retry 30 route_check "$n" "$h"; then
        ok "$h: sr-route 的 RIB/local 与台账逐条一致 —— $(cat "$WORKDIR/route-$n.note")"
    else
        bad "$h: sr-route 状态不符（$(cat "$WORKDIR/route-$n.note" 2>/dev/null | tail -1)）"
    fi
    if retry 20 fib_check "$n" "$h"; then
        ok "$h: sr-fib 的隧道表/vrfs/逃生与台账一致 —— $(cat "$WORKDIR/fib-$n.note")"
    else
        bad "$h: sr-fib 状态不符（$(cat "$WORKDIR/fib-$n.note" 2>/dev/null | tail -1)）"
    fi
done

sect "6. VPP FIB（远端 loopback 压 $LABEL 走 wg；本地前缀交回客户机；表 $OVERLAY 只有隧道）"
for n in $NODES; do
    h=CPE$n
    retry 20 fib_ready "$h" "$n" || bad "$h: 10s 内 FIB 没收敛（远端前缀缺失或 dpo-drop）"
    [ -f "$WORKDIR/fib-$h" ] || { bad "$h: 没有可用的 FIB dump，本节其余判据跳过"; continue; }
    while IFS=$'\t' read -r _ _ pfx nhs; do
        [ -n "$pfx" ] || continue
        block=$(fib_block "$pfx" "$WORKDIR/fib-$h")
        if [ -z "$block" ]; then bad "$h: table $VPN 缺远端 $pfx"; continue; fi
        ok "$h: table $VPN 有远端 ${pfx}（sr-fib 直接装 VPP）"
        echo "$block" | grep -qE "mpls-label\[@[0-9]+\]:\[$LABEL:" \
            && ok "$h: $pfx 压标签 $LABEL" || bad "$h: $pfx 没压标签 $LABEL"
        # 路径集与承载集必须分开判。旧口径是「RIB 里每个下一跳都要有 mpls via」，
        # 那只在所有腿同 pref（ECMP）时成立：`mpls via` 只出现在承载链里，而承载链
        # 只装最优 preference 的那条。颜色一分开（exp-285 把 t13 改成颜色 2 ⇒ pref 10/12
        # 主备），备路**装在 path-list 里且 resolved、但不承载**，旧口径就把正确的主备
        # 形状判红了（2026-11-09 实测 FAIL 1 条）。
        # 整表 dump（${block}）里根本没有 path:/forwarding: 这些行，路径集只能单前缀问。
        # 新口径两条，比旧的严：① 每个 RIB 下一跳都要有 resolved 的路径装在行里；
        # ② 承载集必须**恰好等于**最优 pref 的那组路径（同 pref 并列就是 ECMP，全都要承载）。
        paths=$(fib_paths "$h" "$pfx")
        carrying=$(echo "$block" | awk '/mpls via/{print $4}' | sort)
        for nh in $(echo "$nhs" | tr ',' ' '); do
            line=$(echo "$paths" | awk -v n="$nh" '$1==n{print; exit}')
            if [ -z "$line" ]; then
                bad "$h: $pfx 的 path-list 里没有下一跳 $nh 的路径"
            elif [ "${line##* }" != R ]; then
                bad "$h: $pfx 下一跳 $nh 的路径不是 resolved（${line}）"
            else
                ok "$h: $pfx 下一跳 $nh 的路径已装且 resolved（pref=$(echo "$line" | awk '{print $2}')）"
            fi
        done
        best=$(echo "$paths" | awk '$3=="R"{print $2}' | sort -n | head -1)
        if [ -z "$best" ]; then
            bad "$h: $pfx 没有任何 resolved 路径"
        else
            want=$(echo "$paths" | awk -v b="$best" '$3=="R" && $2==b {print $1}' | sort)
            if [ "$want" = "$carrying" ]; then
                ok "$h: $pfx 承载集 == 最优 pref=$best 的路径集（$(echo $carrying | tr '\n' ' ')）"
            else
                bad "$h: $pfx 承载集与最优 pref=$best 不符：应承载 $(echo $want | tr '\n' ' ')，实际 $(echo $carrying | tr '\n' ' ')"
            fi
        fi
        echo "$block" | grep -q dpo-drop && bad "$h: $pfx 是 dpo-drop" || ok "$h: $pfx 不是 dpo-drop"
    done <<EOF
$(lrows rib "$n")
EOF
    lb=$(box_loop "$n"); cust=$(box_cust "$n"); peer=$(box_peer "$n")
    ADDR=$(vppc "$h" show interface addr)
    xc=$(echo "$ADDR" | awk -v w="$cust" '$1 ~ /^host-/ && $0 ~ /\(/ {cur=$1}
                                         index($0, "L3 " w "/") {print cur; exit}')
    [ -n "$xc" ] || { bad "$h: VPP 里没有客户口地址 ${cust}（本地前缀无从核对）"; continue; }
    one=$(vppc "$h" show ip fib table "$VPN" "$lb/32")
    echo "$one" | grep -q "via $peer $xc" \
        && ok "$h: 本地 $lb/32 在 VPP 里经 $xc 交给客户机 $peer" \
        || bad "$h: 本地 $lb/32 没指向 $peer $xc"
    echo "$one" | grep -q "mpls-label" && bad "$h: 本地 $lb/32 被压了标签（本地前缀不该走隧道）" \
        || ok "$h: 本地 $lb/32 不打标签"
    echo "$one" | grep -q dpo-drop && bad "$h: 本地 $lb/32 是 dpo-drop" || ok "$h: 本地 $lb/32 不是 dpo-drop"
    ovf=$(vppc "$h" show ip fib table "$OVERLAY")
    while IFS=$'\t' read -r _ nn tid lov lport laddr oiface pnn pov pport paddr color tmtu; do
        [ -n "$tid" ] || continue
        echo "$ovf" | grep -q "^$pov/32" && ok "$h: overlay 表有对端 $pov/32" \
            || bad "$h: overlay 表缺对端 $pov/32"
    done <<EOF
$(tunnels_of "$n")
EOF
    echo "$ovf" | grep -qE "^(172\.16\.|172\.31\.)" && bad "$h: overlay 表 $OVERLAY 里混进了客户前缀" \
        || ok "$h: 表 $OVERLAY 只有隧道与 overlay，没有客户前缀"
    # 表号公式 = 颜色*1000+主表号；1250 唯一的合法来源是"VPN 250 配了颜色 1"，而 250
    # 是 overlay 传输域、三处闸门都拒绝它当 VPN ⇒ 真 VPP 里不该有 1250。出现只有两种
    # 解释：有人绕开闸门装了 VPN 250，或者退役的 ZAPI 停车场没退干净。
    vppc "$h" show ip fib table 1250 | grep -q "fib_index" \
        && bad "$h: VPP table 1250 还在（退役停车场没退干净，或 250 被当成 VPN 用了）" \
        || ok "$h: VPP table 1250 不存在"
done

sect "7. 正向导出：sr-route RIB → sr-frr 注 K 路由（出口 lo）→ redistribute → 客户机学到"
for n in $NODES; do
    h=CPE$n; c=$(box_ssh "$n"); proto=$(box_proto "$n"); peer=$(box_peer "$n"); cust=$(box_cust "$n")
    ssh_run "$h" "vtysh -c 'show running-config'" | grep -q "redistribute kernel route-map SDWAN2CPE-vpn$VPN" \
        && ok "$h: FRR 有 redistribute kernel route-map SDWAN2CPE-vpn${VPN}（第 5 段落进去了）" \
        || bad "$h: FRR 缺 redistribute kernel route-map（正向导出这条链没接上）"
    case "$proto" in
        ospf) adj=$(ssh_run "$h" "vtysh -c 'show ip ospf neighbor'")
              echo "$adj" | grep -q "$peer" && echo "$adj" | grep -q "Full" \
                  && ok "$h: OSPF 与客户机 $peer 全相邻（Full）" || bad "$h: OSPF 与 $peer 没到 Full"
              ;;
        bgp)  ssh_run "$h" "vtysh -c 'show bgp summary'" | grep -q "^$peer " \
                  && ok "$h: BGP 与客户机 $peer 建立" || bad "$h: BGP 与 $peer 没建立"
              # 会话的本地源必须是客户口上那份地址（linux-cp 的 TAP），证明 BGP 没走内核转发。
              ssh_run "$h" "vtysh -c 'show bgp neighbors $peer'" | grep -q "Local host: $cust," \
                  && ok "$h: BGP 本地源是 ${cust}（走盒内 TAP）" || bad "$h: BGP 本地源不是 $cust"
              ;;
    esac
    while IFS=$'\t' read -r _ _ pfx _; do
        [ -n "$pfx" ] || continue
        k=$(ssh_run "$h" "ip route show $pfx")
        echo "$k" | grep -q "dev lo" && ok "$h: 内核主表 $pfx 出口是 lo（linux-nl 不镜像进 VPP）" \
            || bad "$h: 内核主表 $pfx 不是 lo 出口：${k:-没有这条路由}"
        r=$(ssh_run "$h" "vtysh -c 'show ip route $pfx'")
        echo "$r" | grep -q 'Known via "kernel"' \
            && ok "$h: zebra 里 $pfx 是 kernel 路由（sr-frr 注入的那条）" \
            || bad "$h: zebra 里 $pfx 不是 kernel 路由：$(echo "$r" | sed -n '2p')"
        # 回灌取证（不判红）：客户协议会把同一条前缀再学回来。K 的距离 0 更优 ⇒
        # 装表的是 lo 那条、VPP 用的是 sr-fib 的隧道路径，所以回灌没改变转发；
        # 这一条就是 docs 里"防环规则待拍"的现场数字，2026-11-01 实测过挡不住的场景。
        oline=$(echo "$r" | grep -oE '"(kernel|ospf|bgp|static)"' | tr '\n' ' ')
        note "$h: $pfx 在 zebra 的候选来源 = ${oline:-无}"
    done <<EOF
$(lrows rib "$n")
EOF
    # 客户机那侧才是"正向导出"的落点：远端 loopback 得从协议里学到。
    cr=$(ssh_run "$c" "vtysh -c 'show ip route'")
    while IFS=$'\t' read -r _ _ pfx _; do
        [ -n "$pfx" ] || continue
        if echo "$cr" | grep -qE "^[$( [ "$proto" = ospf ] && echo O || echo B)]>?\*?[[:space:]]+$pfx([[:space:]]|\$)"; then
            ok "$c: 客户侧 $proto 学到远端 ${pfx}（正向导出落到了客户路由表）"
        else
            bad "$c: 客户侧没学到远端 $pfx"
        fi
    done <<EOF
$(lrows rib "$n")
EOF
done

sect "8. 反向导入：客户机 loopback → 盒内 FRR → sr-frr → sr-route local"
for n in $NODES; do
    h=CPE$n; proto=$(box_proto "$n"); peer=$(box_peer "$n"); lb=$(box_loop "$n")
    r=$(ssh_run "$h" "vtysh -c 'show ip route $lb/32'")
    if echo "$r" | grep -qE "Known via \"$proto\""; then
        ok "$h: 客户 loopback $lb/32 在 zebra 里是 $proto 路由"
    else
        bad "$h: $lb/32 不是 $proto 路由：$(echo "$r" | sed -n '1,2p' | tr '\n' ' ')"
    fi
    echo "$r" | grep -q "$peer" && ok "$h: $lb/32 的下一跳是客户机 $peer" \
        || bad "$h: $lb/32 的下一跳不是 $peer"
    # 反向上报的落点：sr-route 的 local 里必须正好是本站 loopback（第 5 节按集合核过）。
    # 远端前缀在内核主表还得是 K（出口 lo），不能被客户协议那条改写 —— 2026-11-01 实测
    # preference 10+颜色挡不住回灌，这条不能省。
    while IFS=$'\t' read -r _ _ pfx _; do
        [ -n "$pfx" ] || continue
        ssh_run "$h" "ip route show $pfx" | grep -q "dev lo" \
            && ok "$h: 远端 $pfx 在主表是 K/lo（没被客户侧回灌改写）" \
            || bad "$h: 远端 $pfx 在主表不是 K/lo（可能被回灌）"
    done <<EOF
$(lrows rib "$n")
EOF
done
for c in cust12 cust3 cust4; do
    ssh_run "$c" "ip -o -4 addr show dev lo" | grep -qE "172\.16\.[0-9]+\.[0-9]+/32" \
        && ok "$c: lo 上有本站 loopback /32（拍板 5：客户 IP 由 cust 的 loopback 播）" \
        || bad "$c: lo 上没有 172.16 的 loopback"
done

sect "9. 客户视角互 ping（loopback→loopback，0% 丢包才算过）"
# 前后各取一次：sr-wg 的封装/解封装计数、内核 Ip:Forwarding、四台的摘路行号
C0=""; F0=""; M0=""
i=1
for n in $NODES; do
    h=CPE$n
    st=$(vppc "$h" show sr-wg stats)
    C0="$C0 $h:$(echo "$st" | awk '/Encap:/{e=1} e&&/ok:/{print $2; exit}')/$(echo "$st" | awk '/Decap:/{d=1} d&&/ok:/{print $2; exit}')"
    F0="$F0 $h=$(ssh_run "$h" "awk '/^Ip:/{n++; if(n==2){print \$2; exit}}' /proc/net/snmp")"
    M0="$M0 $h=$(log_mark "$h")"
    i=$((i + 1))
done
while IFS=$'\t' read -r _ c src dst; do
    [ -n "$c" ] || continue
    out=$(ssh_run "$c" "ping -c 20 -i 0.2 -W1 -I $src $dst")
    loss=$(echo "$out" | grep -oE '[0-9]+% packet loss')
    rtt=$(echo "$out" | tail -1 | sed 's/.*= //')
    if [ "$loss" = "0% packet loss" ]; then
        ok "${c}（源 ${src}）→ ${dst}：0% loss，rtt $rtt"
    else
        acc=""; tw=0
        for n in $NODES; do
            h=CPE$n
            m0=$(echo "$M0" | tr ' ' '\n' | awk -F= -v x="$h" '$1 == x {print $2}')
            w=$(log_withdrawals "$h" "$m0")
            acc="$acc $h:$w"
            tw=$((tw + w))
        done
        if [ "$tw" -gt 0 ]; then
            warn "${c}（源 ${src}）→ $dst 丢包（${loss}）：同窗口四台摘路条数$acc —— 归因缺陷⑥（#262）"
        else
            bad "${c}（源 ${src}）→ $dst 丢包（${loss}，rtt ${rtt}）：同窗口四台一条摘路都没有${acc}（没解释的丢包按回归算）"
        fi
    fi
done < <(grep '^ping' "$LEDGER")

sect "10. 数据面计数器与内核不转发（§9 前后对比）"
i=1
for n in $NODES; do
    h=CPE$n
    pair=$(echo "$C0" | tr ' ' '\n' | awk -v x="$h" 'index($0, x ":") == 1 {print; exit}')
    e0=$(echo "$pair" | cut -d: -f2 | cut -d/ -f1); d0=$(echo "$pair" | cut -d: -f2 | cut -d/ -f2)
    st=$(vppc "$h" show sr-wg stats)
    e1=$(echo "$st" | awk '/Encap:/{e=1} e&&/ok:/{print $2; exit}')
    d1=$(echo "$st" | awk '/Decap:/{d=1} d&&/ok:/{print $2; exit}')
    [ "$((e1 - e0))" -gt 0 ] && ok "$h: Encap ok $e0 → ${e1}（跑完客户 ping 在涨 +$((e1 - e0))）" \
        || bad "$h: Encap ok $e0 → $e1 没涨（客户流量没进隧道）"
    [ "$((d1 - d0))" -gt 0 ] && ok "$h: Decap ok $d0 → ${d1}（+$((d1 - d0))）" \
        || bad "$h: Decap ok $d0 → $d1 没涨（没收到隧道里的包）"
    f0=$(echo "$F0" | tr ' ' '\n' | awk -F= -v x="$h" '$1 == x {print $2}')
    f1=$(ssh_run "$h" "awk '/^Ip:/{n++; if(n==2){print \$2; exit}}' /proc/net/snmp")
    [ "$f1" = "2" ] && ok "$h: 内核 Ip:Forwarding=2（MIB 口径 notForwarding）" \
        || bad "$h: Ip:Forwarding=${f1}（期望 2 = notForwarding）"
    [ -n "$f0" ] && [ "$f1" = "$f0" ] \
        && ok "$h: 跑完客户流量 Ip:Forwarding 没变（$f0 → ${f1}，内核没参与转发）" \
        || bad "$h: Ip:Forwarding 从 ${f0:-?} 变成 ${f1:-?}（内核参与了转发）"
    [ "$(ssh_run "$h" "cat /proc/sys/net/ipv4/ip_forward")" = "0" ] \
        && ok "$h: 全局 ip_forward=0" || bad "$h: 全局 ip_forward 不是 0"
    fw=$(ssh_run "$h" 'for i in /sys/class/net/*; do [ -d "$i" ] || continue
            i=$(basename "$i"); [ "$i" = lo ] && continue
            cat "/proc/sys/net/ipv4/conf/$i/forwarding" 2>/dev/null; done' | sort -u | tr '\n' ' ')
    [ "$fw" = "0 " ] && ok "$h: 所有非 lo 接口 forwarding=0" || bad "$h: 仍有接口 forwarding≠0（'$fw'）"
    # VM 台与容器台的一处已知差别：这轮没有独立管理通道（拍板 4），盒内核留了一条默认
    # 路由给 ssh/心跳。它和"客户流量不走内核"不矛盾（上面三条判据锁住了），所以报数不判红。
    note "$h: 内核默认路由 = $(ssh_run "$h" "ip route show default" | head -1)"
    i=$((i + 1))
done

echo
echo "================================"
printf 'PASS=%d  FAIL=%d  WARN=%d\n' "$PASS" "$FAIL" "$WARNN"
[ "$WARNN" = 0 ] || echo "  WARN 只可能是 §9 里能归因到缺陷⑥（#262：BFD 抖动 + sr-fib 无抑制摘路）的窗口，条数已逐条打印。"
echo "本轮没覆盖：① 独立终端机（客户视角用 cust 的 loopback 代打，链路里少一跳）；"
echo "            ② 站点 12 的 VRRP（S6 拍板这轮不发，主备切换不在判据里）。"
[ "$FAIL" = 0 ]
